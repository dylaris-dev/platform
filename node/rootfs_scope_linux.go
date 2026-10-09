package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// openParentNoFollow opens the directory that holds rel's last component, one
// component at a time and refusing every link on the way, and returns it with
// that last component. mkParents creates missing directories as it goes.
//
// Each step is a single openat with O_NOFOLLOW on a directory the previous
// step already holds open, so there is no window in which a component can be
// swapped for a link between a check and its use.
func openParentNoFollow(rootDir, rel string, mkParents bool) (*os.File, string, error) {
	rel = path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))[1:]
	if rel == "" {
		rel = "."
	}
	dir, leaf := path.Split(rel)
	cur, err := os.Open(rootDir)
	if err != nil {
		return nil, "", err
	}
	for _, c := range strings.Split(strings.TrimSuffix(dir, "/"), "/") {
		if c == "" || c == "." {
			continue
		}
		fd, oerr := unix.Openat(int(cur.Fd()), c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(oerr, unix.ENOENT) && mkParents {
			if merr := unix.Mkdirat(int(cur.Fd()), c, 0o755); merr != nil && !errors.Is(merr, unix.EEXIST) {
				cur.Close()
				return nil, "", merr
			}
			if mcUser() != 0 {
				_ = unix.Fchownat(int(cur.Fd()), c, mcUser(), mcUser(), unix.AT_SYMLINK_NOFOLLOW)
			}
			fd, oerr = unix.Openat(int(cur.Fd()), c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		cur.Close()
		if oerr != nil {
			if errors.Is(oerr, unix.ELOOP) || errors.Is(oerr, unix.ENOTDIR) {
				return nil, "", fmt.Errorf("%w: %q is a link, and a write does not follow links", os.ErrPermission, c)
			}
			return nil, "", &os.PathError{Op: "open", Path: c, Err: oerr}
		}
		cur = os.NewFile(uintptr(fd), c)
	}
	return cur, leaf, nil
}

// writeScope returns a Root at the directory holding rel - reached without
// following any link - and rel's last component, for a write to happen there.
//
// os.Root confines an operation to its directory but follows a link that stays
// inside it. The tenant's server can create links in its own sub-server
// directories, so "survival/x -> ../.node_config.json" stayed inside the server
// root, and a save, create or upload to survival/x wrote the node's own file -
// the one the container is rebuilt from. Every name check passed, because they
// read the path's text. A write now cannot pass through a link at all; reads
// still follow them.
func writeScope(rootDir, rel string, mkParents bool) (*os.Root, string, error) {
	dir, leaf, err := openParentNoFollow(rootDir, rel, mkParents)
	if err != nil {
		return nil, "", err
	}
	defer dir.Close()
	// The Root below is rootDir itself when rel has no parent, and there a link
	// at the leaf can name .node_config.json without leaving the Root. The
	// tenant cannot create one there (the server root is root's), only move one
	// in by a rename, which renameNoFollow refuses; this is the second half.
	if !strings.Contains(path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))[1:], "/") && isLinkAt(dir, leaf) {
		return nil, "", fmt.Errorf("%w: %q is a link, and a write does not follow links", os.ErrPermission, leaf)
	}
	sub, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", dir.Fd()))
	if err != nil {
		return nil, "", err
	}
	return sub, leaf, nil
}

func isLinkAt(dir *os.File, name string) bool {
	var st unix.Stat_t
	return unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK
}

// renameNoFollow renames oldRel to newRel inside rootDir, reaching both
// parents without following a link. renameat itself does not follow either
// last component.
//
// It does not move a link. A link the tenant's server made in a sub-server
// directory, "survival/z -> .node_config.json", is harmless there - its Root
// is survival/ - but renamed to the top of the server it names the node's own
// file, and the next save to it wrote that file. The check before renameat
// can lose to a swap in the tenant's directory, and a check after it leaves
// the link under its new name for the moment a parallel save needs - measured
// winnable in milliseconds. So it lands first under a name every write path
// refuses (isProtectedFile), is judged there, and only then takes its name.
func renameNoFollow(rootDir, oldRel, newRel string) error {
	od, ol, err := openParentNoFollow(rootDir, oldRel, false)
	if err != nil {
		return err
	}
	defer od.Close()
	nd, nl, err := openParentNoFollow(rootDir, newRel, false)
	if err != nil {
		return err
	}
	defer nd.Close()
	errLink := fmt.Errorf("%w: %q is a link, and links are not moved", os.ErrPermission, path.Base(oldRel))
	if isLinkAt(od, ol) {
		return errLink
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	tmp := ".pending-delete-mv-" + hex.EncodeToString(b)
	if err := unix.Renameat(int(od.Fd()), ol, int(nd.Fd()), tmp); err != nil {
		return &os.LinkError{Op: "rename", Old: oldRel, New: newRel, Err: err}
	}
	if isLinkAt(nd, tmp) {
		if unix.Renameat(int(nd.Fd()), tmp, int(od.Fd()), ol) != nil {
			_ = unix.Unlinkat(int(nd.Fd()), tmp, 0)
		}
		return errLink
	}
	if err := unix.Renameat(int(nd.Fd()), tmp, int(nd.Fd()), nl); err != nil {
		_ = unix.Renameat(int(nd.Fd()), tmp, int(od.Fd()), ol)
		return &os.LinkError{Op: "rename", Old: oldRel, New: newRel, Err: err}
	}
	return nil
}

// pinDir opens the directory rel inside rootDir without following a link,
// creating it when mk is set, and returns a path that names THAT directory
// for as long as release has not been called: /proc/self/fd/<n>. Path-based
// code (installers that download, rename and remove by name) then works inside
// it; a component the tenant swaps for a link afterwards is never resolved
// again. Only leaf operations that do not follow links belong behind it -
// Remove, Rename, OpenFile with O_EXCL.
func pinDir(rootDir, rel string, mk bool) (string, func(), error) {
	dir, _, err := openParentNoFollow(rootDir, path.Join(rel, "_"), mk)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("/proc/self/fd/%d", dir.Fd()), func() { dir.Close() }, nil
}

// openNoFollow opens path for reading and refuses a link at its last
// component. For an archive the tenant uploaded into their own directory: a
// link planted under its name pointed the installer, running as root, at any
// file on the host - another tenant's upload included - and unpacked it into
// this server.
//
// Only a regular file is opened. A named pipe is something the tenant's server
// can create, and opening one with no writer blocks forever: eight such
// installs held every command consumer on the node. O_NONBLOCK makes the open
// of a pipe return at once so it can be refused; reads of a regular file
// ignore the flag.
func openNoFollow(p string) (*os.File, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path.Base(p))
	}
	// Go leaves a flag the caller passed in place, and a FUSE storage path
	// can hand it on to its daemon: back to blocking reads.
	if err := unix.SetNonblock(int(f.Fd()), false); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// openRegularIn is openNoFollow through an os.Root, for the reads a walk does
// after it judged the entry. The walk refuses a pipe when it lists it, but the
// open is a second call, and a server that swaps a file for a pipe between the
// two held a backup or a copy forever. Measured winnable for the link case
// (rootfs.go), so the pipe case is too.
func openRegularIn(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path.Base(name))
	}
	if err := unix.SetNonblock(int(f.Fd()), false); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// openDirIn opens name in root as a directory without blocking: a FIFO in its
// place fails instead of hanging the open.
func openDirIn(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
}
