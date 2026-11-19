package main

import (
	"os"
	"strconv"
	"syscall"
)

// openLevelByFd opens name through its descriptor's /proc name, which stays
// short at any depth. That handle stays open with the Root: an entry's Info
// resolves through the Root's name. O_DIRECTORY so that a directory swapped
// for a FIFO fails instead of blocking the open.
func openLevelByFd(level *os.Root, name string) (*os.Root, func(), bool) {
	f, err := level.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, nil, false
	}
	r, err := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
	if err != nil {
		f.Close()
		return nil, nil, false
	}
	return r, func() { r.Close(); f.Close() }, true
}
