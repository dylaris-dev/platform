package main

import (
	"errors"
	"io/fs"
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
}

// stashServerJars moves aside what CleanServerJars removes from sub: the root
// jars and the generated directories, plus the Forge/NeoForge launch dirs a
// loader install clears (clearLoaderLaunchDirs), which hold the unix_args.txt
// the old server starts from. A stash a crashed earlier attempt left
// behind is cleared first. On an error the stash holds what was taken so
// far and the caller restores it.
func stashServerJars(serverPath, sub string) (*jarStash, error) {
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
	for _, d := range append([]string{"versions", "cache", ".fabric", ".gradle"}, loaderLaunchDirs...) {
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
	return s, nil
}

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
	}
	var first error
	for i := len(s.moved) - 1; i >= 0; i-- {
		rel := s.moved[i]
		// A wipe of libraries/ during the install took a launch dir's parents.
		s.root.MkdirAll(path.Dir(rel), 0o755)
		if err := s.root.Rename(path.Join(s.dir, rel), rel); err != nil && first == nil {
			first = err
		}
	}
	if first == nil {
		dropStash(s.root, s.dir)
	}
	return first
}

// discard drops the stash once the new software is in place.
func (s *jarStash) discard() {
	dropStash(s.root, s.dir)
	s.root.Close()
}
