package main

import (
	"io/fs"
	"syscall"
)

// fileIdentity is what makes two names one file.
type fileIdentity struct{ dev, ino uint64 }

func identityOf(info fs.FileInfo) (fileIdentity, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, false
	}
	return fileIdentity{uint64(st.Dev), st.Ino}, true
}
