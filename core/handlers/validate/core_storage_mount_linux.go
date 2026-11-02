package validate

import "syscall"

// PathIsOnContainerRootFS reports whether path sits on the SAME filesystem
// device as "/", and whether that question could be answered at all.
//
// Core ships as a Docker image, so "/" is the container's own writable overlay
// layer. Anything that shares its device number is therefore NOT backed by a
// volume or bind mount and does not survive a container recreation. Every real
// mount - a named volume, a host bind mount, an NFS/SMB share mounted on the
// host and passed through - is a separate filesystem with its own device
// number, so the comparison is a reliable positive signal for "this is
// ephemeral".
//
// It cannot see the reverse case: a path on a different device is definitely
// mounted, but says nothing about whether that mount is the one the operator
// intended.
func PathIsOnContainerRootFS(path string) (onRoot bool, determinable bool) {
	var st, root syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false, false
	}
	if err := syscall.Stat("/", &root); err != nil {
		return false, false
	}
	return st.Dev == root.Dev, true
}
