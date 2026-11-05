package main

import (
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
}

// stashServerJars moves aside what CleanServerJars removes from sub: the root
// jars and the generated directories. A stash a crashed earlier attempt left
// behind is cleared first. On an error the stash holds what was taken so
// far and the caller restores it.
func stashServerJars(serverPath, sub string) (*jarStash, error) {
	root, err := os.OpenRoot(serverPath)
	if err != nil {
		return nil, err
	}
	s := &jarStash{root: root, dir: "." + sub + ".oldjars", sub: sub}
	root.RemoveAll(s.dir)
	if err := root.Mkdir(s.dir, 0o700); err != nil {
		root.Close()
		return nil, err
	}
	for _, d := range []string{"versions", "cache", ".fabric", ".gradle"} {
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
	return s, nil
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

// add records rel as written by this attempt, for restore to remove.
func (s *jarStash) add(rel string) { s.added = append(s.added, rel) }

// restore removes what the failed install wrote in the stashed places and
// puts everything taken back. The stash is kept when anything could not be
// moved back, so nothing that was there is lost.
func (s *jarStash) restore() error {
	defer s.root.Close()
	for _, rel := range s.added {
		s.root.Remove(rel)
	}
	// Best effort: what it cannot clear, the renames below replace or report.
	CleanServerJars(filepath.Join(s.root.Name(), filepath.FromSlash(s.sub)))
	var first error
	for i := len(s.moved) - 1; i >= 0; i-- {
		rel := s.moved[i]
		if err := s.root.Rename(path.Join(s.dir, rel), rel); err != nil && first == nil {
			first = err
		}
	}
	if first == nil {
		s.root.RemoveAll(s.dir)
	}
	return first
}

// discard drops the stash once the new software is in place.
func (s *jarStash) discard() {
	s.root.RemoveAll(s.dir)
	s.root.Close()
}
