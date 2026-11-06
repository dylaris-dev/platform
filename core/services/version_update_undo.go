package services

// Putting a version move back when the node refused to make it.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"dylaris-core/models"
)

// A version move writes the database optimistically and then dispatches the
// command, the same ordering a reinstall uses. That is right while the node
// always goes through with it - and it does not.
//
// The node stages and verifies every jar BEFORE it removes anything or even
// stops the server, so a download it cannot complete aborts the move with the
// machine untouched. The database has already been rewritten by then, and what
// it now claims is not cosmetic: minecraft_version is what the next mod install
// and the next cross-version availability check resolve against. Left wrong, it
// quietly installs jars for a Minecraft version the server is not running.
//
// So the pre-move state is parked in Redis at dispatch, the node writes a
// give-up signal on the abort paths, and the status watcher pairs the two and
// puts the columns back. Only a disk that is on the old version signals: a
// pre-commit abort, or a failure after the stop whose files the node put back.
// A server that really has moved must not be reverted.
//
// A lost signal leaves exactly the old behaviour, so this can only improve on it.

// VersionUpdateUndoKey holds the pre-move state, written by Core at dispatch.
func VersionUpdateUndoKey(serverUUID string) string {
	return "dylaris:server:" + serverUUID + ":version_update_undo"
}

// VersionUpdateFailedKey is the node's give-up signal. It is written by the
// NODE, which is why it lives under the per-server prefix the node's Redis ACL
// already covers (`~dylaris:server:<uuid>:*`).
func VersionUpdateFailedKey(serverUUID string) string {
	return "dylaris:server:" + serverUUID + ":version_update_failed"
}

// versionUpdateFailedPattern is what the watcher scans for.
const versionUpdateFailedPattern = "dylaris:server:*:version_update_failed"

// VersionUpdateUndo is the state a version move is about to overwrite. Only the
// columns a move touches: this is a compensating write for one command, not a
// snapshot of the server.
type VersionUpdateUndo struct {
	InstallerType    string                 `json:"installerType"`
	MinecraftVersion string                 `json:"minecraftVersion"`
	BuildNumber      string                 `json:"buildNumber"`
	SubServerName    string                 `json:"subServerName"`
	Mods             []VersionUpdateUndoMod `json:"mods"`
	// GameImage is the Java image the move overwrote; empty in a record from
	// an older Core, which never captured it.
	GameImage string `json:"gameImage,omitempty"`
	// Install is the sub-server's install record before the move, nil when it
	// had none. InstallRecorded tells that nil apart from a record written by
	// an older Core, which did not capture it and must not delete one.
	Install         *models.SubServerInstall `json:"install,omitempty"`
	InstallRecorded bool                     `json:"installRecorded,omitempty"`
}

// VersionUpdateUndoMod is one installed mod as it was before the move.
type VersionUpdateUndoMod struct {
	ModrinthProjectID   string `json:"modrinthProjectId"`
	ModrinthProjectSlug string `json:"modrinthProjectSlug"`
	ModrinthVersionID   string `json:"modrinthVersionId"`
	Title               string `json:"title"`
	FileName            string `json:"fileName"`
	SHA512              string `json:"sha512"`
	TargetDir           string `json:"targetDir"`
}

// consumeVersionUpdateFailures lands the node's give-up signal for a version
// move: restore what the dispatch overwrote, and clear both keys.
//
// Returns true when anything panel-visible changed, so the caller fires one
// servers.changed for the tick.
func (s *StatusWatcherService) consumeVersionUpdateFailures(ctx context.Context) bool {
	var cursor uint64
	changed := false
	for {
		keys, next, err := s.redis.Scan(ctx, cursor, versionUpdateFailedPattern, 100).Result()
		if err != nil {
			return changed
		}
		for _, key := range keys {
			parts := strings.Split(key, ":")
			if len(parts) != 4 {
				continue
			}
			uuid := parts[2]
			reason, rerr := s.redis.Get(ctx, key).Result()
			if rerr != nil {
				continue
			}

			// Clear the signal FIRST. A restore that fails halfway must not be
			// retried every five seconds against a database it is already
			// partway through; the operator can re-run the move instead.
			s.redis.Del(ctx, key)

			raw, uerr := s.redis.Get(ctx, VersionUpdateUndoKey(uuid)).Result()
			s.redis.Del(ctx, VersionUpdateUndoKey(uuid))
			if uerr != nil {
				// Expired, or the move predates this mechanism. Say so rather
				// than leaving it looking handled: the columns are now wrong and
				// only a log line will ever mention it.
				log.Printf("Server %s: the version move was abandoned by the node (%s) and there is no undo record, so its Minecraft version and mod list still describe the move that did not happen", uuid, reason)
				continue
			}
			var undo VersionUpdateUndo
			if json.Unmarshal([]byte(raw), &undo) != nil {
				log.Printf("Server %s: the version move was abandoned by the node (%s) and its undo record could not be read", uuid, reason)
				continue
			}

			srv, serr := s.store.GetServerByUUID(uuid)
			if serr != nil || srv == nil {
				continue
			}
			log.Printf("Server %s: the node abandoned the version move (%s), putting Minecraft %s and %d mod rows back",
				uuid, reason, undo.MinecraftVersion, len(undo.Mods))

			if err := s.store.UpdateServerLoaderMetadata(srv.ID, undo.InstallerType, undo.MinecraftVersion, undo.BuildNumber); err != nil {
				log.Printf("Server %s: could not restore the Minecraft version: %v", uuid, err)
			}
			if undo.GameImage != "" {
				if err := s.store.UpdateServerRuntime(srv.ID, undo.GameImage, srv.ExtraJvmFlags); err != nil {
					log.Printf("Server %s: could not restore the Java image: %v", uuid, err)
				}
			}
			if undo.InstallRecorded {
				var err error
				if undo.Install != nil {
					rec := *undo.Install
					rec.ServerID, rec.SubServerName = srv.ID, undo.SubServerName
					err = s.store.UpsertSubServerInstall(rec)
				} else {
					err = s.store.DeleteSubServerInstall(srv.ID, undo.SubServerName)
				}
				if err != nil {
					log.Printf("Server %s: could not restore the install record: %v", uuid, err)
				}
			}
			s.restoreServerMods(srv.ID, undo)
			// A move refused before its stop left the server running, and a
			// newer node reports that ahead of this signal; "stopped" over it
			// turned logins away from a running server. An older node leaves
			// the busy hold's "installing", which still lands as stopped.
			if srv.Status != "online" {
				s.store.UpdateServerStatus(srv.ID, "stopped")
			}
			// The panel only showed a stopped server on its old version; the
			// reason sat in the node's log.
			notifyServerOwner(s.store, srv, "Version change failed",
				fmt.Sprintf("Moving %s to Minecraft %s did not finish: %s. The server was left on its previous version.",
					srv.Name, srv.MinecraftVersion, reason))
			changed = true
		}
		cursor = next
		if cursor == 0 {
			return changed
		}
	}
}

// restoreServerMods puts the mod inventory back.
//
// Upsert rather than a delete-and-insert: the rows conflict on
// (server_id, sub_server_name, modrinth_project_id), so writing each remembered
// mod restores both the ones whose version was rewritten and the ones that were
// deleted as unavailable. Row ids may differ afterwards, which nothing depends
// on - the project id is the identity.
//
// A mod with no Modrinth project id is skipped: it has no conflict key, so
// writing it would insert a duplicate on every restore rather than replace one.
func (s *StatusWatcherService) restoreServerMods(serverID int, undo VersionUpdateUndo) {
	for _, m := range undo.Mods {
		if strings.TrimSpace(m.ModrinthProjectID) == "" {
			continue
		}
		row := models.ServerMod{
			ServerID:            serverID,
			SubServerName:       undo.SubServerName,
			ModrinthProjectID:   m.ModrinthProjectID,
			ModrinthProjectSlug: m.ModrinthProjectSlug,
			ModrinthVersionID:   m.ModrinthVersionID,
			Title:               m.Title,
			FileName:            m.FileName,
			SHA512:              m.SHA512,
			TargetDir:           m.TargetDir,
		}
		if _, err := s.store.UpsertServerMod(&row); err != nil {
			log.Printf("Server %d: could not restore mod %s: %v", serverID, m.Title, err)
		}
	}
}
