package services

import (
	"context"
	"encoding/json"
	"log"

	"dylaris-core/models"
	"dylaris-core/store"
	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"
)

// SetupResultService turns what an installer FOUND into the rows the panel
// shows.
//
// Every other installer is something Core already described: Core picked the
// loader, so Core knows the loader, and the node has nothing to report.
// Importing a backup archive inverts that. The archive describes itself, the
// description is inside a tar the node is already unpacking, and Core has no
// copy - so the node reads it and reports it here.
//
// An archive with no description reports nothing at all, which is the ordinary
// case for anything written before manifests existed and for an archive
// assembled by hand. The files are installed either way; what is lost is the
// automatic loader and mod configuration, which the operator can still set.
type SetupResultService struct {
	store  store.Store
	redis  *redis.Client
	leader LeaderChecker
}

func NewSetupResultService(st store.Store, rdb *redis.Client, leader LeaderChecker) *SetupResultService {
	return &SetupResultService{store: st, redis: rdb, leader: leader}
}

// Start consumes setup reports until ctx is cancelled.
func (s *SetupResultService) Start(ctx context.Context) {
	if s.redis == nil {
		return
	}
	go s.consume(ctx)
}

type setupReport struct {
	ServerUUID string          `json:"serverUuid"`
	SubServer  string          `json:"subServer"`
	Manifest   json.RawMessage `json:"manifest"`
	Timestamp  int64           `json:"timestamp"`
}

func (s *SetupResultService) consume(ctx context.Context) {
	pubsub := s.redis.PSubscribe(ctx, queue.SetupResultsPattern)
	defer pubsub.Close()
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// Pub/Sub broadcasts to every subscriber, so without the leader
			// gate each Core replica would apply the same report.
			if s.leader != nil && !s.leader.IsLeader() {
				continue
			}
			var rep setupReport
			if err := json.Unmarshal([]byte(msg.Payload), &rep); err != nil {
				log.Printf("setup result: decode failed: %v", err)
				continue
			}
			s.apply(msg.Channel, rep)
		}
	}
}

// apply writes one report, after checking that the node that sent it is the
// node that hosts the server it is talking about.
//
// Pub/Sub carries no sender identity, so the channel name is the only
// attribution there is. Redis already refuses a cross-node publish through the
// node's ACL; reaching a mismatch here means an ACL that was never applied or a
// credential with wider reach than a node's, which is exactly what a second,
// independent check is for. Without it any node - a tenant-owned BYON machine
// included - could rewrite another tenant's loader and mod list.
func (s *SetupResultService) apply(channel string, rep setupReport) {
	token, ok := queue.NodeTokenFromSetupChannel(channel)
	if !ok {
		log.Printf("setup result: dropping a message on unattributable channel %q", channel)
		return
	}
	if rep.ServerUUID == "" || len(rep.Manifest) == 0 {
		return
	}
	srv, err := s.store.GetServerByUUID(rep.ServerUUID)
	if err != nil || srv == nil {
		log.Printf("setup result: dropping a message from node %q: server %q could not be loaded: %v",
			token, rep.ServerUUID, err)
		return
	}
	node, err := s.store.GetNodeByID(srv.NodeID)
	if err != nil || node == nil {
		log.Printf("setup result: dropping a message from node %q: node %d could not be loaded: %v",
			token, srv.NodeID, err)
		return
	}
	if node.Token != token {
		log.Printf("setup result: DROPPED a report from node %q about server %q, which another node hosts",
			token, rep.ServerUUID)
		return
	}

	m, ok := DecodeBackupManifest(string(rep.Manifest))
	if !ok {
		log.Printf("setup result: server %q reported a description that could not be read", rep.ServerUUID)
		return
	}
	ApplyImportedManifest(s.store, srv, rep.SubServer, m)
}

// importManifestStore is the slice of the store an import writes through. Narrow
// on purpose: it is the whole list of things applying an archive's description
// is allowed to change.
type importManifestStore interface {
	UpsertSubServerInstall(in models.SubServerInstall) error
	UpdateServerLoaderMetadata(id int, installerType, minecraftVersion, buildNumber string) error
	ReplaceServerMods(serverID int, subServerName string, mods []models.ServerMod) error
}

// ApplyImportedManifest writes an imported archive's description onto a server.
//
// Exported so the same rules apply wherever an import lands, and tested against
// them directly - the Pub/Sub loop above is plumbing, this is the part with
// decisions in it.
func ApplyImportedManifest(st importManifestStore, srv *models.Server, subServer string, m *BackupManifest) {
	if m == nil || srv == nil {
		return
	}
	if subServer == "" {
		// The node reports the directory it installed into. An empty name is a
		// report we cannot place, and guessing a default would write one
		// archive's loader onto a sub-server it says nothing about.
		log.Printf("setup result: server %q reported a description for no sub-server", srv.UUID)
		return
	}

	if in := manifestInstallForImport(m); in != nil {
		rec := *in
		// Everything that identifies the SOURCE installation is replaced by the
		// target's own. The archive is being imported into a new server, and an
		// id read out of it would address a row on a different platform.
		rec.ServerID = srv.ID
		rec.SubServerName = subServer
		// PackID and PackBuildID are primary keys in the source instance's
		// database. On the platform that wrote the archive they mean a specific
		// pack; here they mean whatever happens to hold that id, which is
		// somebody else's pack or nothing at all. Modrinth ids are kept: those
		// identify the same project everywhere.
		if rec.PackID != 0 || rec.PackBuildID != 0 {
			log.Printf("setup result: server %q: dropping the source platform's pack reference (%d/%d) from an imported description",
				srv.UUID, rec.PackID, rec.PackBuildID)
			rec.PackID = 0
			rec.PackBuildID = 0
		}
		if err := st.UpsertSubServerInstall(rec); err != nil {
			log.Printf("setup result: server %q: recording the install: %v", srv.UUID, err)
		} else if srv.ActiveSubServer == "" || srv.ActiveSubServer == subServer {
			// The server's own columns describe whichever sub-server is active.
			// Writing them from a description of an INACTIVE one would relabel
			// the running server as something it is not.
			if err := st.UpdateServerLoaderMetadata(srv.ID, rec.InstallerType, rec.McVersion, rec.BuildVersion); err != nil {
				log.Printf("setup result: server %q: recording loader and version: %v", srv.UUID, err)
			}
		}
	}

	// An entry with an EMPTY list is meaningful and is applied: it says the
	// archive had no mods, so the sub-server must end up with none. Only the
	// absence of an entry means "this archive says nothing", which leaves the
	// rows alone.
	if mods, ok := manifestModsForImport(m); ok {
		if err := st.ReplaceServerMods(srv.ID, subServer, ManifestModRows(mods)); err != nil {
			log.Printf("setup result: server %q/%s: recording mods: %v", srv.UUID, subServer, err)
		}
	}
}

// manifestInstallForImport picks the ONE install record an import applies.
//
// An import creates one sub-server, so a description of several cannot be
// applied without choosing, and choosing wrongly writes the wrong loader. The
// archive's own scope names the one it covers; a whole-server archive with
// exactly one record is unambiguous anyway; anything else is left to the
// operator, which is the same position an archive with no description leaves
// them in.
func manifestInstallForImport(m *BackupManifest) *models.SubServerInstall {
	if m.Scope != "" {
		for i := range m.Installs {
			if m.Installs[i].SubServerName == m.Scope {
				return &m.Installs[i]
			}
		}
		return nil
	}
	if len(m.Installs) == 1 {
		return &m.Installs[0]
	}
	return nil
}

// manifestModsForImport picks the mod list an import applies, by the same rule.
// ok is false when the archive says nothing about mods for this import.
func manifestModsForImport(m *BackupManifest) ([]BackupManifestMod, bool) {
	if m.Scope != "" {
		for _, e := range m.Mods {
			if e.SubServer == m.Scope {
				return e.Mods, true
			}
		}
		return nil, false
	}
	if len(m.Mods) == 1 {
		return m.Mods[0].Mods, true
	}
	return nil, false
}
