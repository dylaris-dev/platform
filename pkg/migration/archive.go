package migration

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Archive streams a zip of the srcDir tree into destZipPath and computes its
// sha256 over the same bytes as they are written. Nothing is buffered whole in
// memory: each entry is copied file→zip→disk. The hash used to be a second
// pass over the closed archive, a full re-read of a multi-GB file during which
// the zip no longer grew - and growth is what the node reports as progress. Relative paths and file modes are preserved; empty dirs
// are written as explicit directory entries so Extract recreates them.
//
// Returns the lowercase hex sha256 and the archive's byte size.
func Archive(srcDir, destZipPath string) (sha256hex string, size int64, err error) {
	srcDir = filepath.Clean(srcDir)

	out, err := os.Create(destZipPath)
	if err != nil {
		return "", 0, err
	}
	// Close errors matter for a zip (the central directory is flushed on
	// Close), so surface them if no earlier error already won.
	closed := false
	defer func() {
		if !closed {
			out.Close()
		}
	}()

	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(out, h)}
	zw := zip.NewWriter(cw)
	walkErr := filepath.Walk(srcDir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // skip the root itself
		}
		// zip uses forward slashes regardless of host OS.
		name := filepath.ToSlash(rel)

		if info.IsDir() {
			// Trailing slash marks a directory entry so empty dirs survive.
			hdr, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			hdr.Name = name + "/"
			_, err = zw.CreateHeader(hdr)
			return err
		}

		// Symlinks and other non-regular files are out of scope for a
		// server-directory move; skip them rather than dereferencing.
		if !info.Mode().IsRegular() {
			return nil
		}

		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(w, f)
		f.Close()
		return copyErr
	})
	if walkErr != nil {
		zw.Close()
		out.Close()
		closed = true
		return "", 0, walkErr
	}
	if err := zw.Close(); err != nil {
		out.Close()
		closed = true
		return "", 0, err
	}
	if err := out.Close(); err != nil {
		closed = true
		return "", 0, err
	}
	closed = true

	return hex.EncodeToString(h.Sum(nil)), cw.n, nil
}

// countingWriter counts what passes through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// MaxArchiveEntries bounds the entries Extract creates. A large modded server
// holds a few hundred thousand files; a variable for tests.
var MaxArchiveEntries = 2_000_000

// Extract unzips zipPath into destDir with zip-slip protection: any entry whose
// cleaned absolute path would escape destDir is rejected. Directories and file
// modes from the archive are recreated, without setuid, setgid or sticky bits.
//
// budget caps the bytes written. The archive comes from the source node, and a
// customer's node can send one that unpacks to far more than its compressed
// size; the target's disk is shared with every other server on it.
//
// onWrite, if not nil, is told the bytes of every file written.
func Extract(zipPath, destDir string, budget int64, onWrite func(int64)) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	// Bytes are not the only thing a shared filesystem runs out of: an archive
	// of empty entries costs one inode each and passes any byte budget.
	if len(zr.File) > MaxArchiveEntries {
		return fmt.Errorf("migration: archive has %d entries, more than the %d allowed", len(zr.File), MaxArchiveEntries)
	}

	destDir = filepath.Clean(destDir)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	// Resolve once so the containment check compares like-for-like paths.
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	// Directories already cleared by linkFreeUnder, so the check costs one Lstat
	// per distinct directory rather than one per entry per level.
	verified := map[string]bool{destAbs: true}
	// Inodes this extraction creates. Capping entries alone did not bound them:
	// MkdirAll makes one directory per missing path segment, so a single deep
	// entry costs thousands.
	created := 0

	for _, f := range zr.File {
		target := filepath.Join(destDir, f.Name)
		targetAbs, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		// Zip-slip guard: the resolved target must stay strictly within
		// destDir. The separator suffix prevents a sibling like "destfoo"
		// from passing a naive prefix check against "dest".
		if targetAbs != destAbs && !strings.HasPrefix(targetAbs, destAbs+string(os.PathSeparator)) {
			return fmt.Errorf("migration: entry %q escapes destination", f.Name)
		}
		missing, err := linkFreeUnder(destAbs, targetAbs, verified)
		if err != nil {
			return err
		}
		if created += missing; created > MaxArchiveEntries {
			return fmt.Errorf("migration: archive creates more than the %d files and directories allowed", MaxArchiveEntries)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, f.Mode().Perm()); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		n, err := extractOne(f, target, budget)
		if onWrite != nil {
			onWrite(n)
		}
		if err != nil {
			return err
		}
		budget -= n
	}
	return nil
}

// linkFreeUnder reports whether target - already known to be LEXICALLY inside
// destAbs - is reachable without following a symlink out of it.
//
// The zip-slip guard above cleans a string and never asks the filesystem, so it
// cannot see a link that is already sitting in the destination. Extract's
// destination is a server directory, and a server directory is bind-mounted
// into the tenant's own Minecraft container: on a move BACK to a node that still
// holds the old copy, "world" can be a link pointing anywhere, and then
// MkdirAll adopts it and every entry written underneath lands outside. Archive
// never puts a link INTO the zip (it skips non-regular files), so nothing here
// legitimately traverses one.
//
// verified is the caller's cache of directories already cleared. missing is how
// many segments of target, leaf included, do not exist yet: the inodes writing
// it will create.
func linkFreeUnder(destAbs, target string, verified map[string]bool) (missing int, err error) {
	rel, err := filepath.Rel(destAbs, target)
	if err != nil {
		return 0, err
	}
	cur := destAbs
	segs := strings.Split(filepath.ToSlash(rel), "/")
	for i, seg := range segs {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		if verified[cur] {
			continue
		}
		fi, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return len(segs) - i, nil // nothing from here down exists yet, so nothing to follow
		}
		if err != nil {
			return 0, err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return 0, fmt.Errorf("migration: %q in the destination is a symlink", rel)
		}
		// Only directories are cached: a file leaf is about to be replaced.
		if fi.IsDir() {
			verified[cur] = true
		}
	}
	return 0, nil
}

// ErrExtractBudget ends an extraction that would write more than its budget.
var ErrExtractBudget = errors.New("migration: archive unpacks to more than the space allowed for it")

func extractOne(f *zip.File, target string, budget int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode().Perm())
	if err != nil {
		return 0, err
	}
	// Counted as written: the sizes in the zip's headers are the sender's claim.
	limit := budget
	if limit < math.MaxInt64 {
		limit++ // one byte past the budget tells "exactly fits" from "too big"
	}
	n, err := io.Copy(out, io.LimitReader(rc, limit))
	if err == nil && n > budget {
		err = ErrExtractBudget
	}
	if err != nil {
		out.Close()
		return n, err
	}
	return n, out.Close()
}
