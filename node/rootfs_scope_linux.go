package main

import (
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
	sub, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", dir.Fd()))
	if err != nil {
		return nil, "", err
	}
	return sub, leaf, nil
}

// renameNoFollow renames oldRel to newRel inside rootDir, reaching both
// parents without following a link. renameat itself does not follow either
// last component.
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
	if err := unix.Renameat(int(od.Fd()), ol, int(nd.Fd()), nl); err != nil {
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
