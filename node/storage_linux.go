package main

import "syscall"

// getDiskSpace returns (total, free) bytes for the filesystem at path.
func getDiskSpace(path string) (uint64, uint64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bavail * uint64(stat.Bsize)
	return total, free
}

// getInodes returns (total, free) inodes for the filesystem at path. A server
// can exhaust them with empty files long before its disk limit, and then no
// server on the filesystem can create a file - a world save included.
func getInodes(path string) (uint64, uint64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	return stat.Files, stat.Ffree
}
