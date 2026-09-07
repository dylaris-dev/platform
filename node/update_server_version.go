package main

// Move a server to another Minecraft version and carry its mods along, as ONE
// command.
//
// It is one command on purpose. The queue consumer runs with Concurrency = 8,
// so separate "swap the mods" and "reinstall" commands have no ordering between
// them: a reinstall ends by starting the container, and a mod download still in
// flight would land in a server that is already running. Sequencing it here is
// the only way the whole move lands together.
//
// The reinstall step keeps the world and the mods directory - CleanServerJars
// only removes root jars plus versions/cache/.fabric - which is what makes
// carrying mods across a version change possible at all.
//
// update_server_version payload (from core):
//
//	uuid, activeSubServer, targetDir, remove[], install[]{fileName,downloadUrl,sha512}
//	plus the usual docker + installer blocks
//
// Every field that becomes part of a path is re-checked here, same as
// install_mod: this arrives over a queue, and the node is the last thing
// between it and the filesystem.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"dylaris-pkg/validate"

	"github.com/redis/go-redis/v9"
)

type versionUpdateFile struct {
	FileName    string `json:"fileName"`
	DownloadURL string `json:"downloadUrl"`
	SHA512      string `json:"sha512"`
}

type versionUpdatePayload struct {
	UUID            string              `json:"uuid"`
	ActiveSubServer string              `json:"activeSubServer"`
	TargetDir       string              `json:"targetDir"`
	Remove          []string            `json:"remove"`
	Install         []versionUpdateFile `json:"install"`
}

// runUpdateServerVersion performs the whole move: stop, drop the named jars,
// fetch the new ones, reinstall the server software at the new version, restart.
func runUpdateServerVersion(ctx context.Context, rdb *redis.Client, dm *DockerManager, storage *StorageManager, cmd NodeCommand, payload string) {
	var pl versionUpdatePayload
	if err := json.Unmarshal([]byte(payload), &struct {
		Config *versionUpdatePayload `json:"config"`
	}{Config: &pl}); err != nil {
		log.Printf("update_server_version: decode failed: %v", err)
		return
	}
	if pl.UUID == "" {
		log.Printf("update_server_version: no server uuid in payload")
		return
	}
	subName := pl.ActiveSubServer
	if subName == "" {
		subName = "server"
	}
	if !validActiveSubServer(subName) {
		log.Printf("update_server_version: invalid active sub-server %q", subName)
		return
	}
	targetDir := pl.TargetDir
	if targetDir == "" {
		targetDir = "mods"
	}
	if !validSubDir(targetDir) {
		log.Printf("update_server_version: invalid target dir %q", targetDir)
		return
	}
	serverPath := storage.GetServerDir(pl.UUID)
	if serverPath == "" {
		log.Printf("update_server_version: storage lookup for %s returned empty path", pl.UUID)
		return
	}

	if !commandsInFlight.enter("version_update:" + pl.UUID) {
		log.Printf("update_server_version: an update of %s is already running on this node, ignoring the duplicate", pl.UUID)
		return
	}
	defer commandsInFlight.leave("version_update:" + pl.UUID)

	statusKey := fmt.Sprintf("dylaris:server:%s:status", pl.UUID)
	// Hold the reconciler off for the whole move, exactly like reinstall: the
	// container is about to be stopped while its desired_state is still online.
	releaseBusy := holdBusyStatus(rdb, pl.UUID, "installing", busyStatusTTL)
	defer releaseBusy()
	rdb.Del(ctx, fmt.Sprintf("dylaris:server:%s:reconcile_failed", pl.UUID))
	rdb.Set(ctx, fmt.Sprintf("dylaris:server:%s:install-start", pl.UUID), "1", 30*time.Second)

	log.Printf("update_server_version: moving %s/%s (%d removals, %d installs)", pl.UUID, subName, len(pl.Remove), len(pl.Install))

	// Same traversal + symlink boundary install_mod goes through: the mods
	// directory is bind-mounted into the tenant's own container, so a symlink
	// planted there would otherwise redirect every write and delete below.
	modsDir, err := resolveWithinDir(serverPath, filepath.Join(subName, targetDir))
	if err != nil {
		log.Printf("update_server_version: %v", err)
		abandonVersionUpdate(ctx, rdb, pl.UUID, err.Error())
		return
	}
	if err := os.MkdirAll(modsDir, 0o755); err != nil {
		log.Printf("update_server_version: mkdir %s: %v", modsDir, err)
		abandonVersionUpdate(ctx, rdb, pl.UUID, "the mods directory could not be created")
		return
	}

	// EVERY new jar is fetched and verified BEFORE anything is removed and
	// before the server is even stopped.
	//
	// The order used to be the other way round - remove, then download, skipping
	// past any download that failed, then clean, reinstall and start. One failed
	// fetch meant the old jar was already gone, the new one never arrived, and
	// the server came up on the new Minecraft version without that mod while the
	// panel reported success. The only trace was a line in this log.
	//
	// Which mods travel and which are dropped is a decision the tenant already
	// made in the panel, on a list Core resolved. A jar that Core said was
	// available and then would not download is not that decision, so it aborts
	// instead of quietly enacting a different one. Aborting here costs nothing:
	// nothing has been deleted and the server has not even been stopped.
	staged, err := stageInstalls(modsDir, pl.Install, downloadAndVerify)
	if err != nil {
		log.Printf("update_server_version: %v; the move is aborted and %s/%s is left exactly as it was",
			err, pl.UUID, subName)
		// Core wrote the new version and the new mod rows before dispatching
		// this. Nothing on disk moved, so those writes now describe a move that
		// did not happen - tell Core to put them back.
		abandonVersionUpdate(ctx, rdb, pl.UUID, err.Error())
		return
	}
	defer func() {
		for _, sj := range staged {
			os.Remove(sj.tmp) // a no-op once renamed into place
		}
	}()
	log.Printf("update_server_version: staged %d new jars for %s/%s", len(staged), pl.UUID, subName)

	// Past this point the move is committed: everything it needs is on disk.
	dm.PowerAction(pl.UUID, "stop")
	time.Sleep(3 * time.Second)

	for _, name := range pl.Remove {
		if !validate.IsPlainFileName(name) {
			log.Printf("update_server_version: skipping invalid removal name %q", name)
			continue
		}
		p := filepath.Join(modsDir, name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("update_server_version: rm %s: %v", p, err)
		}
	}

	for _, sj := range staged {
		if err := os.Rename(sj.tmp, sj.final); err != nil {
			// A rename failing after every download succeeded is a filesystem
			// fault, not a content problem. Say which jar, so the gap in the
			// mods directory has a name.
			log.Printf("update_server_version: could not put %s in place: %v", sj.final, err)
		}
	}

	subServerDir := filepath.Join(serverPath, subName)
	if err := CleanServerJars(subServerDir); err != nil {
		log.Printf("update_server_version: clean failed for %s/%s: %v", pl.UUID, subName, err)
	}

	installerCfg := cmd.Installer
	installerCfg.JavaImage = cmd.Config.Docker.Image
	installerCfg.ServerUUID = pl.UUID
	if _, err := InstallServer(serverPath, subName, installerCfg); err != nil {
		log.Printf("update_server_version: install failed for %s/%s: %v", pl.UUID, subName, err)
		rdb.Set(ctx, statusKey, "stopped", 30*time.Second)
		return
	}

	startCmd, err := buildStartCommand(subServerDir, cmd.Config.Docker.RAM, cmd.Config.Docker.ExtraJvmFlags, cmd.Config.Docker.Image)
	if err != nil {
		log.Printf("update_server_version: buildStartCommand failed for %s/%s: %v", pl.UUID, subName, err)
		rdb.Set(ctx, statusKey, "stopped", 30*time.Second)
		return
	}
	cmd.Config.Docker.Command = startCmd
	if err := dm.RecreateWithCommand(cmd.Config); err != nil {
		log.Printf("update_server_version: failed to restart %s: %v", pl.UUID, err)
		rdb.Set(ctx, statusKey, "stopped", 30*time.Second)
		return
	}
	log.Printf("update_server_version: %s/%s moved and running", pl.UUID, subName)
}

// abandonVersionUpdate tells Core the move did not happen, so it can undo the
// database writes it made before dispatching the command.
//
// ONLY for the aborts that happen before anything on disk has been touched. Past
// the commit point the server really has moved, and asking Core to revert then
// would make the database wrong in the other direction. The TTL outlives several
// of the status watcher's five-second ticks without lingering long enough to
// land on a later, successful move.
func abandonVersionUpdate(ctx context.Context, rdb *redis.Client, serverUUID, reason string) {
	if rdb == nil {
		return
	}
	if reason == "" {
		reason = "the node could not carry out the move"
	}
	key := fmt.Sprintf("dylaris:server:%s:version_update_failed", serverUUID)
	rdb.Set(ctx, key, reason, 5*time.Minute)
}

// stageInstalls fetches and verifies every jar into a ".part" beside its final
// name, and returns what to rename once the move is committed.
//
// All or nothing, and separate from the caller so that is testable: the point of
// staging is that a fetch which fails costs nothing, and the way that stops being
// true is a caller that carries on with a partial set. Anything already written
// is removed before returning the error, so a failed attempt leaves no debris in
// a directory that is bind-mounted into the tenant's container.
//
// fetch is injected for the same reason - the rule worth pinning is what happens
// when one of them fails, and that must not need a Modrinth to test.
func stageInstalls(modsDir string, files []versionUpdateFile, fetch func(url, dst, sha512 string) error) ([]stagedJar, error) {
	staged := make([]stagedJar, 0, len(files))
	fail := func(err error) ([]stagedJar, error) {
		for _, sj := range staged {
			os.Remove(sj.tmp)
		}
		return nil, err
	}
	for _, f := range files {
		if !validate.IsPlainFileName(f.FileName) {
			return fail(fmt.Errorf("refusing invalid install name %q", f.FileName))
		}
		if err := validateModrinthURL(f.DownloadURL); err != nil {
			return fail(err)
		}
		tmpFile, err := resolveWithinDir(modsDir, f.FileName+".part")
		if err != nil {
			return fail(err)
		}
		staged = append(staged, stagedJar{tmp: tmpFile, final: filepath.Join(modsDir, f.FileName)})
		if err := fetch(f.DownloadURL, tmpFile, f.SHA512); err != nil {
			return fail(fmt.Errorf("%s could not be fetched: %w", f.FileName, err))
		}
	}
	return staged, nil
}

// stagedJar is one downloaded-and-verified jar waiting to be renamed into place.
type stagedJar struct{ tmp, final string }
