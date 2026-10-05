//go:build unix

package main

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedBy reports whether a stat result belongs to uid.
//
// In its own file because syscall.Stat_t does not exist on Windows, and the node
// has to COMPILE there even though it only ever runs on Linux - a developer
// builds it on their laptop. See the non-unix twin for what that build gets.
func ownedBy(fi os.FileInfo, uid int) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid
}

// multiplyLinked reports a regular file with more than one name.
func multiplyLinked(d fs.DirEntry) bool {
	if !d.Type().IsRegular() {
		return false
	}
	fi, err := d.Info()
	if err != nil {
		return false
	}
	return multiplyLinkedInfo(fi)
}

func multiplyLinkedInfo(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.Mode().IsRegular() && st.Nlink > 1
}
