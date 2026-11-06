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
	"errors"
	"fmt"
	"io/fs"
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
	UUID            string   `json:"uuid"`
	ActiveSubServer string   `json:"activeSubServer"`
	TargetDir       string   `json:"targetDir"`
	Remove          []string `json:"remove"`
	// RemoveFrom names each removal's own directory. An older Core sends only
	// Remove, every name taken from TargetDir.
	RemoveFrom []versionUpdateRemoval `json:"removeFrom"`
	Install    []versionUpdateFile    `json:"install"`
}

// versionUpdateRemoval is one old jar and the directory under the sub-server
// it is in.
type versionUpdateRemoval struct {
	Dir      string `json:"dir"`
	FileName string `json:"fileName"`
}

// versionMoveInstallers is the installServer types a version move can target:
// the loaders that are also an install source. Not the proxies: a move sends
// the target MINECRAFT version, and Velocity, Waterfall and BungeeCord are
// versioned by their own release, so the install failed after the proxy had
// already been stopped and every player behind it dropped.
var versionMoveInstallers = map[string]bool{
	"paper": true, "fabric": true, "forge": true, "neoforge": true,
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

	statusKey := fmt.Sprintf("dylaris:server:%s:status", pl.UUID)
	// Every abort before the stop leaves the server as it was, so it reports
	// what Docker shows rather than the busy hold's "installing": Core landed
	// that as "stopped" for a server that went on running. Written before the
	// abandon signal, so Core reads the real status first. dm is nil only in
	// tests that never reach Docker.
	abort := func(reason string) {
		if dm != nil {
			rdb.Set(ctx, statusKey, statusAfterFailedStop(dm, pl.UUID), 30*time.Second)
		}
		abandonVersionUpdate(ctx, rdb, pl.UUID, reason)
	}

	if !commandsInFlight.enter("version_update:" + pl.UUID) {
		log.Printf("update_server_version: an update of %s is already running on this node, ignoring the duplicate", pl.UUID)
		return
	}
	defer commandsInFlight.leave("version_update:" + pl.UUID)
	// The copy's key too, so neither runs beside the other: a copy is what
	// makes the move reversible, and copyDir walking the tree while the move
	// swapped its jars captured a mix of both. Whichever comes second is
	// refused before it touches anything.
	if !commandsInFlight.enter("copy_sub:" + pl.UUID) {
		log.Printf("update_server_version: a copy of %s is running on this node, refusing the move", pl.UUID)
		// The copy's own hold reports its status; this one has written none.
		abandonVersionUpdate(ctx, rdb, pl.UUID, "a copy of this server was still being made")
		return
	}
	defer commandsInFlight.leave("copy_sub:" + pl.UUID)

	// Hold the reconciler off for the whole move, exactly like reinstall: the
	// container is about to be stopped while its desired_state is still online.
	releaseBusy := holdBusyStatus(rdb, pl.UUID, "installing", busyStatusTTL)
	defer releaseBusy()
	rdb.Set(ctx, fmt.Sprintf("dylaris:server:%s:install-start", pl.UUID), "1", 30*time.Second)
	// Before any abort below, which all report "left exactly as it was".
	recoverStrandedStash(serverPath, subName)

	log.Printf("update_server_version: moving %s/%s (%d removals, %d installs)", pl.UUID, subName, len(pl.Remove), len(pl.Install))

	// Refused before anything is fetched or stopped. Core accepts every Modrinth
	// loader tag, and spigot, bukkit, purpur and quilt are tags the installer has
	// no source for: the move used to swap the plugins and clean the jars, then
	// fail with "unknown installer type" on a server left with no jar.
	if !versionMoveInstallers[cmd.Installer.Type] {
		log.Printf("update_server_version: %s cannot be installed by this node, the move of %s is refused", cmd.Installer.Type, pl.UUID)
		abort(fmt.Sprintf("%q is not server software this node can install", cmd.Installer.Type))
		return
	}
	// A blank NeoForge build is resolved here, not by InstallServer after the
	// stop: a Minecraft version upstream has no build for took the server down
	// before it was refused.
	if cmd.Installer.Type == "neoforge" && cmd.Installer.Loader == "" && cmd.Installer.Version != "" {
		build, err := resolveNeoForgeVersion(cmd.Installer.Version)
		if err != nil {
			log.Printf("update_server_version: %v; the move of %s is refused", err, pl.UUID)
			abort(err.Error())
			return
		}
		cmd.Installer.Loader = build
	}

	// Same traversal + symlink boundary install_mod goes through: the mods
	// directory is bind-mounted into the tenant's own container, so a symlink
	// planted there would otherwise redirect every write and delete below.
	if _, err := resolveWithinDir(serverPath, filepath.Join(subName, targetDir)); err != nil {
		log.Printf("update_server_version: %v", err)
		abort(err.Error())
		return
	}
	// Opened once, with no link followed, and worked in from here on: the
	// downloads take long enough for the tenant to swap the directory for a
	// link, and every path-based call below would have followed it (pinDir).
	modsDir, releaseMods, err := pinDir(serverPath, filepath.ToSlash(filepath.Join(subName, targetDir)), true)
	if err != nil {
		log.Printf("update_server_version: open the mods directory: %v", err)
		abort("the mods directory could not be created")
		return
	}
	defer releaseMods()

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
		abort(err.Error())
		return
	}
	defer func() {
		for _, sj := range staged {
			os.Remove(sj.tmp) // a no-op once renamed into place
		}
	}()
	log.Printf("update_server_version: staged %d new jars for %s/%s", len(staged), pl.UUID, subName)

	// Past this point the server is stopped and its files change. Nothing is
	// deleted until the new software is installed:
	// the removed mods and the old server jars are moved aside, and a failure
	// puts them back, so the server is left on the version it ran.
	//
	// Through gracefulStop and its error, like reinstall: a bare docker stop
	// skipped save-all and, when it failed, the jars were swapped under the
	// running JVM. Nothing has been touched yet, so abandoning is exact.
	if err := gracefulStop(rdb, pl.UUID, dm); err != nil {
		log.Printf("update_server_version: the move of %s/%s is refused: the server could not be stopped: %v", pl.UUID, subName, err)
		abort("the server could not be stopped: " + err.Error())
		return
	}
	// The move ends by starting the server, like setup and reinstall.
	clearRunBlockers(ctx, rdb, pl.UUID)
	time.Sleep(3 * time.Second)

	stash, err := stashServerJars(serverPath, subName)
	fail := func(what string, cause error) {
		log.Printf("update_server_version: %s failed for %s/%s: %v; putting the previous version back", what, pl.UUID, subName, cause)
		if stash != nil {
			if rerr := stash.restore(); rerr != nil {
				log.Printf("update_server_version: could not put everything back for %s/%s: %v", pl.UUID, subName, rerr)
			}
		}
		// The disk is back on the old version, so Core's optimistic writes are
		// now the wrong ones - and the reason reaches the owner from there.
		abandonVersionUpdate(ctx, rdb, pl.UUID, fmt.Sprintf("%s failed: %v", what, cause))
		rdb.Set(ctx, statusKey, "stopped", 30*time.Second)
	}
	if err != nil {
		fail("setting the old server software aside", err)
		return
	}

	if name, err := takeVersionMoveRemovals(stash, serverPath, subName, targetDir, modsDir, pl); err != nil {
		fail("setting "+name+" aside", err)
		return
	}
	modsRel := filepath.ToSlash(filepath.Join(subName, targetDir))

	for _, sj := range staged {
		// Only a name this move created is removed again on a failure: one that
		// was already there and not on the removal list is not ours to delete.
		// Recorded before the rename, so a node that dies right after it still
		// leaves the name for the redelivery to take back.
		if _, lerr := os.Lstat(sj.final); os.IsNotExist(lerr) {
			stash.add(modsRel + "/" + filepath.Base(sj.final))
		}
		if err := os.Rename(sj.tmp, sj.final); err != nil {
			// A rename failing after every download succeeded is a filesystem
			// fault, not a content problem. Say which jar, so the gap in the
			// mods directory has a name.
			log.Printf("update_server_version: could not put %s in place: %v", sj.final, err)
			continue
		}
	}

	subServerDir := filepath.Join(serverPath, subName)
	installerCfg := cmd.Installer
	installerCfg.JavaImage = cmd.Config.Docker.Image
	installerCfg.ServerUUID = pl.UUID
	_, err = InstallServer(serverPath, subName, installerCfg)
	// Same as setup and reinstall: a delete that landed during the downloads
	// removed the directory the install just recreated.
	if discardIfDeleted(pl.UUID, serverPath) {
		stash.root.Close()
		return
	}
	if err != nil {
		fail("the install", err)
		return
	}

	startCmd, err := buildStartCommand(subServerDir, cmd.Config.Docker.RAM, cmd.Config.Docker.ExtraJvmFlags, cmd.Config.Docker.Image)
	if err != nil {
		fail("building the start command", err)
		return
	}
	cmd.Config.Docker.Command = startCmd

	// Started only while the platform still means to run it, as setup and
	// reinstall do: a cutoff that lands during the downloads writes only the
	// desired state, because the status reads "installing".
	provisionFloodgateKey(ctx, rdb, serverPath, pl.UUID, subName)
	recreate, status := dm.RecreateWithCommand, "starting"
	if desired, derr := rdb.Get(ctx, fmt.Sprintf("dylaris:server:%s:desired_state", pl.UUID)).Result(); derr == nil && desired == "stopped" {
		recreate, status = dm.RecreateStopped, "stopped"
	}
	// The new software is in place from here on, so a Docker fault in the
	// recreate keeps it, as reinstall does: putting the old files back under a
	// container that may already carry the new command would match neither.
	stash.discard()
	if err := recreate(cmd.Config); err != nil {
		log.Printf("update_server_version: failed to restart %s: %v", pl.UUID, err)
		rdb.Set(ctx, statusKey, "stopped", 30*time.Second)
		return
	}
	rdb.Set(ctx, statusKey, status, 30*time.Second)
	log.Printf("update_server_version: %s/%s moved (status %s)", pl.UUID, subName, status)
}

// abandonVersionUpdate tells Core the move did not happen, so it can undo the
// database writes it made before dispatching the command.
//
// ONLY when the disk is on the old version: an abort before anything was
// touched, or a failure after the stop whose files were put back. A server that
// really has moved must not be reverted, or the database would be wrong in the
// other direction. The TTL outlives several of the status watcher's
// five-second ticks without lingering long enough to land on a later,
// successful move.
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

// takeVersionMoveRemovals sets every old jar aside into the stash, each from
// its own directory, and names the one that failed. modsDir is the pinned
// targetDir.
func takeVersionMoveRemovals(stash *jarStash, serverPath, subName, targetDir, modsDir string, pl versionUpdatePayload) (string, error) {
	removals := pl.RemoveFrom
	if len(removals) == 0 {
		for _, name := range pl.Remove {
			removals = append(removals, versionUpdateRemoval{Dir: targetDir, FileName: name})
		}
	}
	for _, rm := range removals {
		if !validate.IsPlainFileName(rm.FileName) || !validSubDir(rm.Dir) {
			log.Printf("update_server_version: skipping invalid removal %q in %q", rm.FileName, rm.Dir)
			continue
		}
		dir := modsDir
		if rm.Dir != targetDir {
			// Pinned like modsDir, for the same reason: the tenant can swap the
			// directory for a link while the move runs.
			d, release, err := pinDir(serverPath, subName+"/"+rm.Dir, false)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return rm.FileName, err
			}
			defer release()
			dir = d
		}
		if err := stash.takeFile(filepath.Join(dir, rm.FileName), subName+"/"+rm.Dir+"/"+rm.FileName); err != nil {
			return rm.FileName, err
		}
	}
	return "", nil
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
		// A plain name in the pinned directory; the fetch opens it with
		// Remove + O_EXCL, which does not follow a link at the leaf.
		tmpFile := filepath.Join(modsDir, f.FileName+".part")
		staged = append(staged, stagedJar{tmp: tmpFile, final: filepath.Join(modsDir, f.FileName)})
		if err := fetch(f.DownloadURL, tmpFile, f.SHA512); err != nil {
			return fail(fmt.Errorf("%s could not be fetched: %w", f.FileName, err))
		}
	}
	return staged, nil
}

// stagedJar is one downloaded-and-verified jar waiting to be renamed into place.
type stagedJar struct{ tmp, final string }
