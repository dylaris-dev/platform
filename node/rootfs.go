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
	"slices"
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
	return openWriteScope(rootDir, reqPath, true)
}

// openWriteScope checks reqPath against rootDir like openJailed, and returns a
// Root at the directory that holds it, reached without following a link (see
// writeScope), with the name inside that Root. The caller closes the Root and
// writes through it.
func openWriteScope(rootDir, reqPath string, mkParents bool) (*os.Root, string, error) {
	abs, err := resolveWithinDir(rootDir, reqPath)
	if err != nil {
		return nil, "", err
	}
	name, err := rootName(rootDir, abs)
	if err != nil {
		return nil, "", err
	}
	return writeScope(rootDir, name, mkParents)
}

// rootEntryInfo decides whether a walked entry belongs in an archive or a copy
// and which FileInfo describes it. A link is followed by the Root, so one that
// leaves it, dangles, or names a directory is skipped; one that stays inside is
// taken as its target. Same rule the archive walkers always had, now judged at
// the moment of use.
//
// Only a regular file or a directory is taken. A named pipe, a socket or a
// device is something the tenant's server can create, and opening a pipe with
// no writer blocks forever: one mkfifo held a backup worker for good, the
// server stayed on save-off, and eight of them stopped every command on the
// node. A socket failed every backup of its server.
func rootEntryInfo(root *os.Root, name string, d fs.DirEntry) (fs.FileInfo, bool) {
	if d.Type()&fs.ModeSymlink == 0 {
		info, err := d.Info()
		if err != nil || !(info.Mode().IsRegular() || info.IsDir()) {
			return nil, false
		}
		return info, true
	}
	target, err := root.Stat(name)
	if err != nil || !target.Mode().IsRegular() {
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
	// A name that is a second hard link to a file - .node_config.json, made by
	// a tenant on a host without fs.protected_hardlinks - would hand THAT file
	// over, after a write through it had already replaced its contents.
	// ponytail: Lstat then Lchown is not atomic; closing it needs fchown on the
	// writer's own fd at every call site.
	if fi, err := root.Lstat(name); err == nil && multiplyLinkedInfo(fi) {
		log.Printf("mc-user: not handing %s over: more than one hard link", name)
		return
	}
	if err := root.Lchown(name, mcUser(), mcUser()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("mc-user: cannot hand %s to uid %d: %v", name, mcUser(), err)
	}
}

// copyFileIn copies srcName in src to dstName in dst, creating dst's parents.
func copyFileIn(src *os.Root, srcName string, dst *os.Root, dstName string, deny map[fileIdentity]bool) error {
	in, err := openTenantReadIn(src, srcName, deny)
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

// nodeOwnedFileMode and nodeOwnedDirMode keep the node's own files in a server
// directory away from the container, which mounts that directory at /data and
// runs as uid 1000: a plugin read .node_config.json (the start command and the
// JVM flags, hidden from members without server.settings.write) and every
// node-local backup straight off the mount. A container run as root
// (MC_RUN_AS=0) still reads them.
const (
	nodeOwnedFileMode os.FileMode = 0o600
	nodeOwnedDirMode  os.FileMode = 0o700
)

// restrictNodeOwnedFiles applies those modes to files written before they
// existed. os.WriteFile keeps an existing file's mode, so without this an old
// config stayed readable for good. Links are left alone: the node never makes
// one here, and the tenant cannot create entries in this directory.
func restrictNodeOwnedFiles(serverDir string) {
	for name, mode := range map[string]os.FileMode{
		".node_config.json": nodeOwnedFileMode,
		".dylaris.json":     nodeOwnedFileMode,
		".active_server":    nodeOwnedFileMode,
		backupDirName:       nodeOwnedDirMode,
	} {
		p := filepath.Join(serverDir, name)
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&fs.ModeSymlink != 0 || info.Mode().Perm() == mode {
			continue
		}
		if err := os.Chmod(p, mode); err != nil {
			log.Printf("restrict %s: %v", p, err)
		}
	}
}

// nodeOwnedIdentities collects the identities of the files the node keeps in
// the server directory root is opened at: its config, its metadata and, with
// withBackups, the node-local backup store and everything in it.
//
// A read is refused by identity and not only by name because a name can be a
// link: a plugin planting survival/x -> ../.node_config.json made a member with
// files.read read the container command and JVM flags (hidden from them since
// round 64) and download backups without backups.read, through every read path
// that follows a link inside the server directory.
func nodeOwnedIdentities(root *os.Root, withBackups bool) map[fileIdentity]bool {
	ids := map[fileIdentity]bool{}
	add := func(info fs.FileInfo) {
		if id, ok := identityOf(info); ok {
			ids[id] = true
		}
	}
	for _, n := range []string{".node_config.json", ".dylaris.json", ".active_server"} {
		if info, err := root.Lstat(n); err == nil {
			add(info)
		}
	}
	if withBackups {
		_ = fs.WalkDir(root.FS(), backupDirName, func(_ string, d fs.DirEntry, err error) error {
			if err != nil || d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if info, ierr := d.Info(); ierr == nil {
				add(info)
			}
			return nil
		})
	}
	return ids
}

// deniedIdentity reports whether info is one of deny's files.
func deniedIdentity(info fs.FileInfo, deny map[fileIdentity]bool) bool {
	id, ok := identityOf(info)
	return ok && deny[id]
}

// openTenantReadIn is openRegularIn for a read on a tenant's behalf: a file
// that turns out to be one of deny's is not found, however it was reached.
// Judged on the open file, so a link swapped after a check cannot slip past.
func openTenantReadIn(root *os.Root, name string, deny map[fileIdentity]bool) (*os.File, error) {
	f, err := openRegularIn(root, name)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || deniedIdentity(st, deny) {
		f.Close()
		return nil, fs.ErrNotExist
	}
	return f, nil
}

// copyDirForTenant copies a tenant's tree to dstName, writing through a root
// AT dstName, reached without following a link.
//
// Not through a root at its parent: for a destination in the server's top
// level that parent is the server directory, which holds .node_config.json,
// and a Root follows any link that stays inside it. A destination that is an
// existing sub-server is writable by its running container, so a plugin
// planting survival/x -> ../.node_config.json made a copy of a folder holding
// "x" into "survival" overwrite the node's config for this server. With the
// root at the destination, a link inside it can only reach the destination.
func copyDirForTenant(src *os.Root, srcName, rootDir, dstName string) error {
	pinned, release, err := pinDir(rootDir, dstName, true)
	if err != nil {
		return err
	}
	defer release()
	dst, err := os.OpenRoot(pinned)
	if err != nil {
		return err
	}
	defer dst.Close()
	return copyWalkIn(src, srcName, dst, ".", true)
}

// copyWalkIn copies the tree at srcName in src to dstName in dst. With
// forTenant set it is a DUPLICATION for a user: protected entries are skipped
// and everything written is handed to the container's uid (see copyDir). Without
// it, it is a verbatim MOVE of a whole server (see copyTree).
func copyWalkIn(src *os.Root, srcName string, dst *os.Root, dstName string, forTenant bool) error {
	var deny map[fileIdentity]bool
	if forTenant {
		deny = nodeOwnedIdentities(src, true)
	}
	return walkRoot(src, srcName, func(name string, info fs.FileInfo) error {
		rel := "."
		if name != srcName {
			rel = strings.TrimPrefix(name, strings.TrimSuffix(srcName, "/")+"/")
			if srcName == "." {
				rel = name
			}
		}
		if forTenant && rel != "." && (isProtectedFile(rel) || isProtectedFile(name) || deniedIdentity(info, deny)) {
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
		if err := copyFileIn(src, name, dst, target, deny); err != nil {
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

// errUnpackBudget ends an extraction that would fill the disk.
var errUnpackBudget = errors.New("the archive unpacks to more than the free space this node can give it")

// budgetReader fails once more than *left bytes have been read through it,
// across every reader sharing left. An archive's sizes are its own claim, so
// the count is of what actually comes out.
type budgetReader struct {
	r    io.Reader
	left *int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if int64(n) > *b.left {
		// Hand back only what fits: io.Copy writes the bytes of a read before
		// it looks at the error.
		n = int(max(*b.left, 0))
		*b.left = -1
		return n, errUnpackBudget
	}
	*b.left -= int64(n)
	return n, err
}

// writeEULA accepts the EULA for a sub-server. Through a scope that follows
// no link: eula.txt is a name the tenant can plant a link under, and a plain
// write followed it - to the node's own config in the server root, or
// further with a race.
func writeEULA(serverPath, subName string) error {
	root, leaf, err := writeScope(serverPath, path.Join(filepath.ToSlash(subName), "eula.txt"), false)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.WriteFile(leaf, []byte("eula=true\n"), 0o644); err != nil {
		return err
	}
	chownForMCIn(root, leaf)
	return nil
}

// readTenantDir lists name in root for a tenant, judged on the directory it
// opened: a Stat followed by a second lookup let a link swapped in between
// list the backup store's archive names.
func readTenantDir(root *os.Root, name string, deny map[fileIdentity]bool) ([]fs.DirEntry, error) {
	f, err := openDirIn(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if deniedIdentity(st, deny) {
		return nil, fs.ErrNotExist
	}
	entries, err := f.ReadDir(-1)
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
}
