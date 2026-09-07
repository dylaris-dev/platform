package services

import (
	"encoding/json"
	"time"

	"dylaris-core/models"
)

// BackupManifestSchema is the version of the manifest FORMAT.
//
// Readers must accept a manifest whose schema they do not know by ignoring it
// rather than failing: an archive is restored by whatever Dylaris happens to be
// running, which may be older than the one that wrote it.
const BackupManifestSchema = 1

// BackupManifest is what an archive contains, described.
//
// A backup captures files and says nothing about the database that describes
// them, so a restore used to roll the files back and leave every row where it
// was. Measured both ways: back up, install a mod, restore - the jar is gone and
// the row stays, so the panel lists a mod that is not there. Install a mod, back
// up, uninstall it, restore - the jar is back and the row is gone, so the mod
// runs and nothing in the panel knows about it.
//
// It travels INSIDE the archive as well as on the run row, and that is the whole
// design rather than a redundancy. A snapshot held only in the source instance's
// database is exactly the part that cannot follow a downloaded archive into a
// different Dylaris; the copy on the run row is what a same-instance restore
// reads, so it costs no fetch. Both are written from the same bytes at the same
// moment and can therefore not disagree.
type BackupManifest struct {
	Schema    int       `json:"schema"`
	CreatedAt time.Time `json:"createdAt"`
	// Source is the release the writing Dylaris was running, for a reader that
	// has to explain what it is looking at. Never used for a decision.
	Source string `json:"source,omitempty"`
	// Scope is the sub-server this archive covers, or "" for the whole server.
	Scope string `json:"scope"`
	// Installs is how each covered sub-server was installed. Same records the
	// run row has carried since install_snapshot existed.
	Installs []models.SubServerInstall `json:"installs"`
	// Mods is one entry per covered sub-server, INCLUDING sub-servers with no
	// mods. An absent entry means "this archive says nothing", an entry with an
	// empty list means "there were none" - and the second is what makes the
	// backup-then-install-then-restore case remove the row it has to remove.
	Mods []BackupManifestMods `json:"mods"`
}

// BackupManifestMods is the mod list of one sub-server.
type BackupManifestMods struct {
	SubServer string              `json:"subServer"`
	Mods      []BackupManifestMod `json:"mods"`
}

// BackupManifestMod is one installed mod, in portable form.
//
// Deliberately NOT models.ServerMod. That carries the row id, the server id, the
// installing USER's id and the in-flight install id - all of them meaningful
// only inside the instance that wrote them, and the user id identifies a person
// on the source platform to whoever imports the archive. What is kept is exactly
// what recreating the row needs.
type BackupManifestMod struct {
	ModrinthProjectID   string `json:"modrinthProjectId"`
	ModrinthProjectSlug string `json:"modrinthProjectSlug,omitempty"`
	ModrinthVersionID   string `json:"modrinthVersionId"`
	Title               string `json:"title,omitempty"`
	FileName            string `json:"fileName"`
	TargetDir           string `json:"targetDir,omitempty"`
	SHA512              string `json:"sha512,omitempty"`
}

// ManifestModRows turns a manifest's portable mod descriptions into rows.
//
// One function rather than one per caller: a restore and an import write the
// same rows for the same reason, and the fields a row must NOT get from an
// archive - the server id, the installing user, the in-flight install id - are
// the ones a second copy of this loop would eventually start setting.
func ManifestModRows(mods []BackupManifestMod) []models.ServerMod {
	rows := make([]models.ServerMod, 0, len(mods))
	for _, mod := range mods {
		rows = append(rows, models.ServerMod{
			ModrinthProjectID:   mod.ModrinthProjectID,
			ModrinthProjectSlug: mod.ModrinthProjectSlug,
			ModrinthVersionID:   mod.ModrinthVersionID,
			Title:               mod.Title,
			FileName:            mod.FileName,
			TargetDir:           mod.TargetDir,
			SHA512:              mod.SHA512,
		})
	}
	return rows
}

// backupManifestStore is the slice of the store the builder reads.
type backupManifestStore interface {
	GetSubServerInstall(serverID int, subServer string) (*models.SubServerInstall, error)
	ListSubServerInstalls(serverID int) ([]models.SubServerInstall, error)
	ListServerMods(serverID int, subServerName string) ([]models.ServerMod, error)
	ListServerModSubServers(serverID int) ([]string, error)
}

// BuildBackupManifest describes what a run is about to archive.
//
// scope is the job's sub-server, or "" for a whole-server job - the same
// distinction the archive walk makes, so the manifest covers exactly what the
// tar does.
//
// Best-effort per part: a store failure drops that part rather than failing the
// backup. An archive with an incomplete description is worth more than no
// archive, and the restore side already treats an absent part as "says nothing".
func BuildBackupManifest(st backupManifestStore, serverID int, scope, release string) *BackupManifest {
	m := &BackupManifest{
		Schema:    BackupManifestSchema,
		CreatedAt: time.Now().UTC(),
		Source:    release,
		Scope:     scope,
		Installs:  []models.SubServerInstall{},
		Mods:      []BackupManifestMods{},
	}

	subs := manifestScopeSubServers(st, serverID, scope)
	for _, sub := range subs {
		if rec, err := st.GetSubServerInstall(serverID, sub); err == nil && rec != nil {
			m.Installs = append(m.Installs, *rec)
		}
		rows, err := st.ListServerMods(serverID, sub)
		if err != nil {
			continue
		}
		mods := make([]BackupManifestMod, 0, len(rows))
		for _, r := range rows {
			// An install the node never confirmed is not a state to restore
			// into: replaying it would assert a jar is present that the failed
			// install never wrote, and the restore has just replaced the
			// directory it would have written into.
			if r.Status == models.ServerModFailed || r.Status == models.ServerModInstalling {
				continue
			}
			mods = append(mods, BackupManifestMod{
				ModrinthProjectID:   r.ModrinthProjectID,
				ModrinthProjectSlug: r.ModrinthProjectSlug,
				ModrinthVersionID:   r.ModrinthVersionID,
				Title:               r.Title,
				FileName:            r.FileName,
				TargetDir:           r.TargetDir,
				SHA512:              r.SHA512,
			})
		}
		m.Mods = append(m.Mods, BackupManifestMods{SubServer: sub, Mods: mods})
	}
	return m
}

// manifestScopeSubServers lists the sub-servers a run covers.
//
// A scoped job covers exactly its own. A whole-server job covers every
// sub-server that either has an install record OR has mod rows - the union,
// because the two are written by different paths and a sub-server that predates
// install records still has mods, while a freshly created one has a record and
// no mods yet. Taking only one source would silently leave the other's
// sub-servers undescribed, and an undescribed sub-server is one whose rows a
// restore leaves untouched.
func manifestScopeSubServers(st backupManifestStore, serverID int, scope string) []string {
	if scope != "" {
		return []string{scope}
	}
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	if installs, err := st.ListSubServerInstalls(serverID); err == nil {
		for _, in := range installs {
			add(in.SubServerName)
		}
	}
	if names, err := st.ListServerModSubServers(serverID); err == nil {
		for _, n := range names {
			add(n)
		}
	}
	return out
}

// EncodeBackupManifest renders the manifest for the archive and the run row.
// Both get the SAME bytes, which is what keeps them from drifting apart.
func EncodeBackupManifest(m *BackupManifest) []byte {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// DecodeBackupManifest reads a manifest back. A blank or unreadable one is
// (nil, false) rather than an error: every archive written before manifests
// existed has none, and that is an ordinary case the restore must handle by
// leaving the rows alone.
func DecodeBackupManifest(raw string) (*BackupManifest, bool) {
	if raw == "" {
		return nil, false
	}
	var m BackupManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, false
	}
	return &m, true
}
