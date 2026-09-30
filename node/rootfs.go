package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// The node's filesystem boundary for tenant trees.
//
// A server directory is written by its tenant WHILE the node works in it: the
// Minecraft container has it bind-mounted, and plugins run whatever code the
// tenant uploads. resolveWithinDir checks a path and hands back a STRING; the
// operation that follows opens that string again and follows every symlink on
// the way a second time. A tenant that swaps a name between a file and a link in
// that gap reaches whatever the link names, as root. Measured before this file
// existed: a name flipped in a loop, read 13156 times through the real check,
// returned the file OUTSIDE the directory 636 times - in under half a second.
//
// os.Root closes the gap by making the check and the operation one step: each
// component is opened relative to the directory before it, and a link that
// leaves the root is refused while the operation runs, not by a check that ran
// earlier. So resolveWithinDir stays as the early, readable refusal, and every
// operation on a tenant tree goes through a Root.
//
// A Root refuses absolute symlinks even when they point inside. That is
// narrower than the old rule and deliberate: a link written from inside the
// container is absolute in the CONTAINER's view (/data/...), which names
// nothing on the node anyway.

// rootName converts abs, which resolveWithinDir already placed under rootDir,
// into the slash-separated name a Root and its FS expect ("." for the root).
func rootName(rootDir, abs string) (string, error) {
	rel, err := filepath.Rel(filepath.Clean(rootDir), filepath.Clean(abs))
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("access denied: path traversal")
	}
	return filepath.ToSlash(rel), nil
}

// openJailed checks reqPath against rootDir and opens rootDir as a Root. The
// caller closes the Root and operates on the returned name through it.
func openJailed(rootDir, reqPath string) (*os.Root, string, error) {
	abs, err := resolveWithinDir(rootDir, reqPath)
	if err != nil {
		return nil, "", err
	}
	name, err := rootName(rootDir, abs)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, "", err
	}
	return root, name, nil
}

// openJailedForWrite is openJailed for an operation that may be the first
// write into a server: an import can upload before setup has made the server
// directory, and the old path-based writes created it on the way. rootDir is
// the node's own path (storage path plus server UUID), never the tenant's.
func openJailedForWrite(rootDir, reqPath string) (*os.Root, string, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, "", err
	}
	return openJailed(rootDir, reqPath)
}

// rootEntryInfo decides whether a walked entry belongs in an archive or a copy
// and which FileInfo describes it. A link is followed by the Root, so one that
// leaves it, dangles, or names a directory is skipped; one that stays inside is
// taken as its target. Same rule the archive walkers always had, now judged at
// the moment of use.
func rootEntryInfo(root *os.Root, name string, d fs.DirEntry) (fs.FileInfo, bool) {
	if d.Type()&fs.ModeSymlink == 0 {
		info, err := d.Info()
		if err != nil {
			return nil, false
		}
		return info, true
	}
	target, err := root.Stat(name)
	if err != nil || target.IsDir() {
		return nil, false
	}
	return target, true
}

// walkRoot walks start inside root and calls fn with each entry's name and the
// FileInfo rootEntryInfo chose. The directory reads go through the Root too, so
// a directory swapped for a link mid-walk is not listed from outside.
func walkRoot(root *os.Root, start string, fn func(name string, info fs.FileInfo) error) error {
	return fs.WalkDir(root.FS(), start, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, ok := rootEntryInfo(root, name, d)
		if !ok {
			return nil
		}
		return fn(name, info)
	})
}

// createIn opens name inside root for writing, creating its parent
// directories and truncating an existing file.
func createIn(root *os.Root, name string, perm os.FileMode) (*os.File, error) {
	if err := mkdirParentIn(root, name); err != nil {
		return nil, err
	}
	return root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
}

// mkdirParentIn creates the directory that will hold name.
func mkdirParentIn(root *os.Root, name string) error {
	if dir := path.Dir(filepath.ToSlash(name)); dir != "." {
		return root.MkdirAll(dir, 0o755)
	}
	return nil
}

// createTempIn is os.CreateTemp inside a Root: a new file in dir named
// prefix+random+suffix, created exclusively. It returns the open file and its
// name relative to the root.
func createTempIn(root *os.Root, dir, prefix, suffix string) (*os.File, string, error) {
	for range 10 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, "", err
		}
		name := path.Join(filepath.ToSlash(dir), prefix+hex.EncodeToString(b[:])+suffix)
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("create temp in %q: too many collisions", dir)
}

// chownForMCIn hands one freshly written name inside a Root to the container's
// uid, the way a file created by the node as root needs so a RUNNING server can
// modify it. Errors are logged, never returned.
func chownForMCIn(root *os.Root, name string) {
	if mcUser() == 0 {
		return
	}
	if err := root.Lchown(name, mcUser(), mcUser()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("mc-user: cannot hand %s to uid %d: %v", name, mcUser(), err)
	}
}

// copyFileIn copies srcName in src to dstName in dst, creating dst's parents.
func copyFileIn(src *os.Root, srcName string, dst *os.Root, dstName string) error {
	in, err := src.Open(srcName)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := createIn(dst, dstName, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyWalkIn copies the tree at srcName in src to dstName in dst. With
// forTenant set it is a DUPLICATION for a user: protected entries are skipped
// and everything written is handed to the container's uid (see copyDir). Without
// it, it is a verbatim MOVE of a whole server (see copyTree).
func copyWalkIn(src *os.Root, srcName string, dst *os.Root, dstName string, forTenant bool) error {
	return walkRoot(src, srcName, func(name string, info fs.FileInfo) error {
		rel := "."
		if name != srcName {
			rel = strings.TrimPrefix(name, strings.TrimSuffix(srcName, "/")+"/")
			if srcName == "." {
				rel = name
			}
		}
		if forTenant && rel != "." && isProtectedFile(rel) {
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := path.Join(dstName, rel)
		if info.IsDir() {
			if err := dst.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
			if forTenant {
				chownForMCIn(dst, target)
			}
			return nil
		}
		if err := copyFileIn(src, name, dst, target); err != nil {
			return err
		}
		if forTenant {
			chownForMCIn(dst, target)
		}
		return nil
	})
}

// --- installer / archive helpers -------------------------------------------

// openRootMk creates dir if missing and opens it as an os.Root. Used by the
// extract, download and copy paths, which write into a directory whose CONTENTS
// the tenant controls even though its path is node-chosen.
func openRootMk(dir string) (*os.Root, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}

// extractRel cleans an archive entry name into a slash path relative to the
// extraction root, rejecting the empty name and the root itself. Traversal and
// absolute names are folded away here; a symlink component that survives is
// refused by the Root when the entry is created, which is the real boundary.
func extractRel(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("empty entry name")
	}
	n := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(name)), "/")
	if n == "" || n == "." {
		return "", fmt.Errorf("entry names the destination directory itself")
	}
	return n, nil
}

// extractSkip decides whether an archive entry must be SKIPPED - because its
// name escapes destDir lexically or through a symlink already planted there -
// and otherwise returns its Root-relative name. The Root is still the runtime
// boundary at the moment of writing; this keeps one poisoned entry from failing
// the whole archive, the way the extractors always behaved. A link planted in
// the race window after this check is caught by the Root and fails the extract,
// which is safe.
func extractSkip(destDir, entry string) (name string, skip bool) {
	if _, err := resolveWithinDir(destDir, filepath.FromSlash(entry)); err != nil {
		return "", true
	}
	n, err := extractRel(entry)
	if err != nil {
		return "", true
	}
	return n, false
}

// writeFileInto creates name inside root (truncating, creating parents) and
// copies r into it, capped at max bytes when max > 0. The file is handed to the
// container's uid, since it lands in a running server's tree.
func writeFileInto(root *os.Root, name string, mode fs.FileMode, r io.Reader, max int64) error {
	out, err := createIn(root, name, mode)
	if err != nil {
		return err
	}
	var src io.Reader = r
	if max > 0 {
		src = io.LimitReader(r, max)
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	if cerr := out.Close(); cerr != nil {
		return cerr
	}
	chownForMCIn(root, name)
	return nil
}

// copyDirInto duplicates the tree src into dst FOR A USER: protected entries are
// dropped and everything written is handed to the container's uid. Both ends are
// confined to their parent directories, so a link swapped in for src or planted
// in the walk cannot move the copy outside them.
func copyDirInto(src, dst string) error { return copyTreeAt(src, dst, true) }

// copyTreeInto copies src to dst VERBATIM, protected entries included. Only a
// whole-server MOVE uses it; see the copyTree comment.
func copyTreeInto(src, dst string) error { return copyTreeAt(src, dst, false) }

func copyTreeAt(src, dst string, forTenant bool) error {
	srcRoot, err := os.OpenRoot(filepath.Dir(src))
	if err != nil {
		return err
	}
	defer srcRoot.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	dstRoot, err := os.OpenRoot(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer dstRoot.Close()
	return copyWalkIn(srcRoot, filepath.Base(src), dstRoot, filepath.Base(dst), forTenant)
}

// copyFileInto copies the single file src to dst, confined to their parents and
// handed to the container's uid.
func copyFileInto(src, dst string) error {
	srcRoot, err := os.OpenRoot(filepath.Dir(src))
	if err != nil {
		return err
	}
	defer srcRoot.Close()
	dstRoot, err := openRootMk(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer dstRoot.Close()
	if err := copyFileIn(srcRoot, filepath.Base(src), dstRoot, filepath.Base(dst)); err != nil {
		return err
	}
	chownForMCIn(dstRoot, filepath.Base(dst))
	return nil
}
