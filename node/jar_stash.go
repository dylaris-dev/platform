package main

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// jarStash is CleanServerJars done as a move instead of a delete, so a failed
// reinstall or version move can put the working server back.
//
// Both used to delete every root jar and only then download the new software.
// A PaperMC 404 for a version without a build yet, a Forge installer error or
// an upstream 5xx then left a server with no jar at all, and every start
// crash-looped. installServerJar's .part + checksum design could not help: the
// jar it was protecting was already gone.
//
// Everything goes through an os.Root on the server directory: the sub-server
// directory is the tenant's, and a link planted there must not carry a rename
// out of it.
type jarStash struct {
	root  *os.Root
	dir   string   // the stash, relative to root
	sub   string   // the sub-server directory, relative to root
	moved []string // what was taken, relative to root
	added []string // what the caller put in place, removed again by restore
	// complete: everything was taken, so an install may have run and what
	// now sits in the loader launch dirs is its output, not the old server's.
	complete bool
	// keep is what discard puts back (stashBeforeSetup): taken only for the
	// rollback, not something the install was asked to clear.
	keep []string
}

// softwareKey is the commandsInFlight key every command that stashes a
// server's jars holds (reinstall, update_server_version). Their own keys did
// not exclude each other, and both work on the one stash dir per sub-server:
// one's recovery put the other's live stash back over its install.
func softwareKey(uuid string) string { return "software:" + uuid }

// stashServerJars moves aside what CleanServerJars removes from sub: the root
// jars and the generated directories, plus the Forge/NeoForge launch dirs a
// loader install clears (clearLoaderLaunchDirs), which hold the unix_args.txt
// the old server starts from. wiped names further directories of sub the
// install is about to wipe (stashBeforeSetup); they are taken first, so a
// launch dir inside one goes with it. A stash a crashed earlier attempt left
// behind is cleared first. On an error the stash holds what was taken so
// far and the caller restores it.
func stashServerJars(serverPath, sub string, wiped ...string) (*jarStash, error) {
	root, err := os.OpenRoot(serverPath)
	if err != nil {
		return nil, err
	}
	s := &jarStash{root: root, dir: "." + sub + ".oldjars", sub: sub}
	recoverStash(root, s.dir)
	dropStash(root, s.dir)
	if err := root.Mkdir(s.dir, 0o700); err != nil {
		root.Close()
		return nil, err
	}
	dirs := append(append([]string{}, wiped...), "versions", "cache", ".fabric", ".gradle")
	for _, d := range append(dirs, loaderLaunchDirs...) {
		if st, err := root.Lstat(path.Join(sub, d)); err == nil && st.IsDir() {
			if err := s.take(path.Join(sub, d)); err != nil {
				return s, err
			}
		}
	}
	entries, err := fs.ReadDir(root.FS(), sub)
	if err != nil {
		return s, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".jar") {
			if err := s.take(path.Join(sub, e.Name())); err != nil {
				return s, err
			}
		}
	}
	s.complete = true
	// Written down for recoverStash: a node that dies from here on may have
	// run the install, and only a complete stash may clear what it wrote.
	if f, err := root.Create(path.Join(s.dir, stashCompleteMark)); err == nil {
		f.Close()
	}
	return s, nil
}

// stashCompleteMark is the file in the stash that says everything was taken.
const stashCompleteMark = "complete"

// stashAddedList is the file in the stash that lists what the attempt put in
// place (add), so an attempt that dies before restore can still be undone.
const stashAddedList = "added.list"

// recoverStash undoes an attempt that never finished (a node restart
// mid-install, or a restore that failed): what it put in place goes, and what
// its stash holds goes back. The stash held the only copy of the old software,
// and deleting it first meant a redelivered attempt that failed too left no
// jar at all.
//
// The stash wins over what is in place under the same name. A stash only
// survives an attempt that did not finish (discard and restore drop it with
// one rename), so anything at a name it took was written by that attempt: the
// new server jar, or a new mod that the redelivery cannot tell from one that
// was already there. In-place-wins kept those and deleted the old copies.
func recoverStash(root *os.Root, dir string) {
	if st, err := root.Lstat(dir); err != nil || !st.IsDir() {
		return
	}
	// A complete stash may have had an install run over it, whose launch dir
	// and root jars sit beside the names the stash took back: cleared first,
	// as restore does, or resolveLaunch could start the half-installed loader.
	// A partial one ran no install; what is in place is the old server.
	if _, err := root.Lstat(path.Join(dir, stashCompleteMark)); err == nil {
		sub := strings.TrimSuffix(strings.TrimPrefix(dir, "."), ".oldjars")
		CleanServerJars(filepath.Join(root.Name(), filepath.FromSlash(sub)))
		for _, d := range loaderLaunchDirs {
			root.RemoveAll(path.Join(sub, d))
		}
		// Once only: from here files leave the stash, and a second clear would
		// delete what was already put back.
		root.Remove(path.Join(dir, stashCompleteMark))
	}
	if b, err := root.ReadFile(path.Join(dir, stashAddedList)); err == nil {
		for _, rel := range strings.Split(string(b), "\n") {
			if rel = strings.TrimSpace(rel); rel != "" {
				root.Remove(rel)
			}
		}
	}
	fs.WalkDir(root.FS(), dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, dir+"/")
		// Everything taken sits under the sub-server's name; a file at the
		// top of the stash is the stash's own (stashAddedList).
		if !strings.Contains(rel, "/") {
			return nil
		}
		if _, err := root.Lstat(rel); !errors.Is(err, fs.ErrNotExist) {
			root.Remove(rel)
		}
		root.MkdirAll(path.Dir(rel), 0o755)
		root.Rename(p, rel)
		return nil
	})
}

// stashBeforeSetup is the stash for a setup over a sub-server that is already
// installed, the panel's only way to change a server's software. Core puts the
// old type and version back when the setup fails, and the wipe and the loader
// install had left nothing on disk to start them. libraries/ goes into the
// stash when the setup wipes it, since a Forge/NeoForge server cannot start
// from its launch dir without the libraries it names, and so do mods/,
// config/ and versions/ when they are wiped.
//
// The stash is the rollback, not a wipe: what the owner did not tick to clear
// goes back on success (discard), unless the install wrote the same name. Only
// the loader launch dirs are dropped either way: a Forge/NeoForge install
// clears them itself, and an args file left beside other software outranks its
// jar (resolveLaunch).
//
// nil for a first install, which has nothing to keep, and for an installer
// that installs files already in the directory (upload, upload-zip, backup)
// or from a library or URL import: taking their jars would remove the server
// being installed.
func stashBeforeSetup(serverPath, sub string, cfg InstallerConfig) (*jarStash, error) {
	if !setupSoftware[cfg.Type] {
		return nil, nil
	}
	if st, err := os.Lstat(filepath.Join(serverPath, sub)); err != nil || !st.IsDir() {
		return nil, nil
	}
	clears := map[string]bool{}
	for _, d := range loaderLaunchDirs {
		clears[path.Join(sub, d)] = true
	}
	var wiped []string
	jars := false
	for _, raw := range cfg.WipePaths {
		t := strings.TrimSpace(raw)
		// Every directory the wipe would delete: mods/ and config/ had no
		// copy, so a failed pack update came back without them.
		wiped = append(wiped, wipeTargets[t]...)
		jars = jars || t == WipeJars
		for _, d := range wipeTargets[t] {
			clears[path.Join(sub, d)] = true
		}
	}
	s, err := stashServerJars(serverPath, sub, wiped...)
	if err != nil {
		return s, err
	}
	for _, rel := range s.moved {
		rootJar := path.Dir(rel) == sub && strings.HasSuffix(strings.ToLower(rel), ".jar")
		if !clears[rel] && !(jars && rootJar) {
			s.keep = append(s.keep, rel)
		}
	}
	return s, nil
}

// setupSoftware is every setup installer that puts new server software over
// what is there: the plain installers and the packs, which download the pack
// and install its loader after the wipe.
var setupSoftware = func() map[string]bool {
	m := map[string]bool{"modpack": true, "technic": true}
	for t := range uploadSoftware {
		m[t] = true
	}
	return m
}()

// recoverStrandedStash is the recovery stashServerJars opens with, for a path
// that can fail before it gets there: a move redelivered after the node died
// mid-move aborted while staging, said the server was left as it was, and left
// the old software in the stash with no jar at the root.
func recoverStrandedStash(serverPath, sub string) {
	root, err := os.OpenRoot(serverPath)
	if err != nil {
		return
	}
	defer root.Close()
	dir := "." + sub + ".oldjars"
	recoverStash(root, dir)
	dropStash(root, dir)
}

// dropStash removes a stash. It is renamed away first, so a removal that fails
// part-way leaves debris under another name rather than a half stash that
// recoverStash would put back over a server that has moved on.
func dropStash(root *os.Root, dir string) {
	trash := dir + ".drop"
	root.RemoveAll(trash)
	if root.Rename(dir, trash) == nil {
		root.RemoveAll(trash)
		return
	}
	root.RemoveAll(dir)
}

// take moves rel (relative to the server directory) into the stash. A name
// that is not there is not an error: there was nothing to keep.
func (s *jarStash) take(rel string) error {
	dst := path.Join(s.dir, rel)
	if err := s.root.MkdirAll(path.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := s.root.Rename(rel, dst); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	s.moved = append(s.moved, rel)
	return nil
}

// takeFile is take for a file whose source the caller already holds a pinned
// path to (pinDir); rel is the same file relative to the server directory,
// where restore puts it back.
func (s *jarStash) takeFile(src, rel string) error {
	dst := path.Join(s.dir, rel)
	if err := s.root.MkdirAll(path.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.Rename(src, filepath.Join(s.root.Name(), filepath.FromSlash(dst))); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	s.moved = append(s.moved, rel)
	return nil
}

// add records rel as written by this attempt, for restore to remove. It is
// also written down in the stash, for the recoverStash of a redelivery when
// this attempt dies first. Call it BEFORE putting rel in place.
func (s *jarStash) add(rel string) {
	s.added = append(s.added, rel)
	if f, err := s.root.OpenFile(path.Join(s.dir, stashAddedList), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
		f.WriteString(rel + "\n")
		f.Close()
	}
}

// restore removes what the failed install wrote in the stashed places and
// puts everything taken back. The stash is kept when anything could not be
// moved back, so nothing that was there is lost.
func (s *jarStash) restore() error {
	defer s.root.Close()
	for _, rel := range s.added {
		s.root.Remove(rel)
	}
	// Only a complete stash may clear anything: a partial one stopped before
	// the install ran, and what it did not reach (the server jar among them)
	// is the old server, with no copy in the stash.
	if s.complete {
		// Best effort: what it cannot clear, the renames below replace or report.
		CleanServerJars(filepath.Join(s.root.Name(), filepath.FromSlash(s.sub)))
		// A failed loader install leaves a partial launch dir that
		// resolveLaunch would pick over the old jar or args file.
		for _, d := range loaderLaunchDirs {
			s.root.RemoveAll(path.Join(s.sub, d))
		}
		// A stash kept after a failed rename below must not be cleared again
		// by recoverStash: what was already put back has no copy in it.
		s.root.Remove(path.Join(s.dir, stashCompleteMark))
	}
	var first error
	for i := len(s.moved) - 1; i >= 0; i-- {
		rel := s.moved[i]
		// A wipe of libraries/ during the install took a launch dir's parents.
		mkdirAllForMC(s.root, path.Dir(rel))
		// A directory taken whole (libraries/) may have been written again by
		// the install, and a rename does not replace a directory that is not
		// empty. Whatever sits there now came from the failed attempt.
		if s.complete {
			if st, err := s.root.Lstat(path.Join(s.dir, rel)); err == nil && st.IsDir() {
				s.root.RemoveAll(rel)
			}
		}
		if err := s.root.Rename(path.Join(s.dir, rel), rel); err != nil && first == nil {
			first = err
		}
	}
	if first == nil {
		dropStash(s.root, s.dir)
	}
	return first
}

// discard drops the stash once the new software is in place, after putting
// back what it holds only for the rollback (keep).
func (s *jarStash) discard() {
	if len(s.keep) > 0 {
		// Renamed away first: from here a node that dies must not have
		// recoverStash roll the finished install back.
		trash := s.dir + ".drop"
		s.root.RemoveAll(trash)
		if s.root.Rename(s.dir, trash) == nil {
			for _, rel := range s.keep {
				s.putBackMissing(path.Join(trash, rel), rel)
			}
		}
	}
	dropStash(s.root, s.dir)
	s.root.Close()
}

// putBackMissing moves src back to rel wherever the install wrote nothing
// under the same name: the new software wins a clash. A directory the install
// did not write goes back whole, with the owner and mode it had: this runs
// after the install's ownership walk (handInstalledTree), and a directory
// made here as root left Paper unable to write cache/ or versions/.
func (s *jarStash) putBackMissing(src, rel string) {
	fs.WalkDir(s.root.FS(), src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		dst := rel + strings.TrimPrefix(p, src)
		if _, err := s.root.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		err = mkdirAllForMC(s.root, path.Dir(dst))
		if err == nil {
			err = s.root.Rename(p, dst)
		}
		if err != nil {
			log.Printf("setup: could not keep %s: %v", dst, err)
			return nil
		}
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
}

// mkdirAllForMC is MkdirAll inside the server directory with every directory
// it creates handed to the container's uid, which a running server must be
// able to write into.
func mkdirAllForMC(root *os.Root, dir string) error {
	if dir == "." || dir == "/" {
		return nil
	}
	if _, err := root.Lstat(dir); err == nil {
		return nil
	}
	if err := mkdirAllForMC(root, path.Dir(dir)); err != nil {
		return err
	}
	if err := root.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	chownForMCIn(root, dir)
	return nil
}
