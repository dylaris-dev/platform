package main

import (
	"fmt"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// FS_IOC_FSGETXATTR / FS_IOC_FSSETXATTR and struct fsxattr from
// <linux/fs.h>, which x/sys does not carry. The ioctl numbers use the generic
// encoding, valid on amd64 and arm64 (the arches the node ships for). ext4
// and xfs both implement them; chattr -p and xfs_quota project -s set the
// project id through the same calls.
const (
	fsIocFsgetxattr    = 0x801c581f
	fsIocFssetxattr    = 0x401c5820
	fsXflagProjinherit = 0x00000200
)

type fsxattr struct {
	xflags     uint32
	extsize    uint32
	nextents   uint32
	projid     uint32
	cowextsize uint32
	pad        [8]byte
}

// tagProjectTree sets project id pid on root and everything under it, and
// project inheritance on every directory, so new files are charged to it too.
//
// The node-local backup folder at the top is left out: it is charged by
// setProjectTree on its own (QuotaProvider.TagBackupDir), to the server only
// while Core shares the server's quota with backups.
func tagProjectTree(root string, pid uint32) error {
	return setProjectTree(root, backupDirName, pid)
}

// setProjectTree sets project id pid on root and everything under it except
// the entry skip directly below root. pid 0 is no project: inheritance comes
// off as well, so new files stay uncharged.
func setProjectTree(root, skip string, pid uint32) error {
	return walkProjectTree(root, skip, func(fd int, dir bool) error {
		var fa fsxattr
		if err := fsxattrIoctl(fd, fsIocFsgetxattr, &fa); err != nil {
			return fmt.Errorf("get attributes: %w", err)
		}
		want := projectAttrs(fa, pid, dir)
		if want == fa {
			return nil
		}
		if err := fsxattrIoctl(fd, fsIocFssetxattr, &want); err != nil {
			return fmt.Errorf("set project %d: %w", pid, err)
		}
		return nil
	})
}

// projectAttrs is fa with project pid, inherited by a directory's new entries
// unless pid is 0.
func projectAttrs(fa fsxattr, pid uint32, dir bool) fsxattr {
	fa.projid = pid
	if dir {
		if pid != 0 {
			fa.xflags |= fsXflagProjinherit
		} else {
			fa.xflags &^= fsXflagProjinherit
		}
	}
	return fa
}

func fsxattrIoctl(fd int, req uintptr, fa *fsxattr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(fa))); errno != 0 {
		return errno
	}
	return nil
}

// walkProjectTree calls visit with an open fd for root and for every
// directory and regular file below it, on root's filesystem, except the
// entry named skip directly below root and everything in it.
//
// The tree belongs to a tenant whose server is running, and the walk runs as
// root. Every entry is opened relative to its parent's fd with O_NOFOLLOW and
// typed from the opened fd, never re-resolved by path: chattr -R and xfs_quota
// lstat()ed an entry and then opened it by name, and a directory swapped for
// a symlink in between had the walk tag whatever the link named - another
// tenant's tree or the storage root. Other entry types (fifos, devices,
// sockets) are never opened for reading.
//
// Like chattr -R, an entry that fails is skipped and the walk goes on; the
// first failure and the count come back as the error.
func walkProjectTree(root, skip string, visit func(fd int, dir bool) error) error {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", root, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return fmt.Errorf("stat %s: %w", root, err)
	}
	w := &treeWalk{visit: visit, dev: st.Dev, root: root, skip: skip}
	w.dir(fd, ".", 0)
	if w.failed > 0 {
		return fmt.Errorf("%d entries not tagged, first: %w", w.failed, w.first)
	}
	return nil
}

// maxProjectTreeDepth caps how deep the walk descends. Opening by parent fd
// means PATH_MAX never stops it, and each level holds two fds and its listing
// while the levels below run; a tenant can nest directories with renames alone
// (R25-08). A directory deeper than this is counted as failed, not entered.
var maxProjectTreeDepth = 4096

// treeWalk carries no path down the recursion: a full path per level kept
// every ancestor's string alive, memory growing with the square of the depth.
// An error names the entry and its depth below root instead.
type treeWalk struct {
	visit  func(fd int, dir bool) error
	dev    uint64
	root   string
	skip   string
	failed int
	first  error
}

func (w *treeWalk) fail(name string, depth int, err error) {
	if w.failed == 0 {
		w.first = fmt.Errorf("%s: %q at depth %d: %w", w.root, name, depth, err)
	}
	w.failed++
}

// dir visits the directory open at dfd and everything in it, then closes dfd.
func (w *treeWalk) dir(dfd int, name string, depth int) {
	defer unix.Close(dfd)
	names, err := readDirNames(dfd)
	if err != nil {
		w.fail(name, depth, err)
	}
	if err := w.visit(dfd, true); err != nil {
		w.fail(name, depth, err)
	}
	for _, n := range names {
		w.entry(dfd, n, depth+1)
	}
}

func (w *treeWalk) entry(dfd int, name string, depth int) {
	if depth == 1 && name == w.skip {
		return
	}
	// O_PATH opens nothing: a symlink yields the link itself and a device is
	// not handed to its driver. The real open below goes through this fd, so
	// it reaches the inode just typed, whatever happens to the name meanwhile.
	pfd, err := unix.Openat(dfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if err != unix.ENOENT {
			w.fail(name, depth, err)
		}
		return
	}
	defer unix.Close(pfd)
	var st unix.Stat_t
	if err := unix.Fstat(pfd, &st); err != nil {
		w.fail(name, depth, err)
		return
	}
	if st.Dev != w.dev {
		return
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		if depth > maxProjectTreeDepth {
			w.fail(name, depth, fmt.Errorf("deeper than %d levels, not entered", maxProjectTreeDepth))
			return
		}
		cfd, err := unix.Openat(pfd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			w.fail(name, depth, err)
			return
		}
		w.dir(cfd, name, depth)
	case unix.S_IFREG:
		// An ioctl needs a real open; /proc/self/fd reopens the pinned inode.
		cfd, err := unix.Open("/proc/self/fd/"+strconv.Itoa(pfd), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if err != nil {
			w.fail(name, depth, err)
			return
		}
		if err := w.visit(cfd, false); err != nil {
			w.fail(name, depth, err)
		}
		unix.Close(cfd)
	}
}

func readDirNames(dfd int) ([]string, error) {
	var names []string
	buf := make([]byte, 32<<10)
	for {
		n, err := unix.Getdents(dfd, buf)
		if err != nil {
			return names, err
		}
		if n <= 0 {
			return names, nil
		}
		_, _, names = unix.ParseDirent(buf[:n], -1, names)
	}
}
