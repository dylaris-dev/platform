package main

import (
	"os"
	"path/filepath"
	"testing"
)

// X11: dirSize could not report failure. A root it could not read (an NFS
// ESTALE or EIO) measured 0 bytes, so duDiskUsage published Total=0 and the
// stopped-server sweep released a disk_full hold on a server still over its
// limit. A root that does not exist is still 0: nothing is stored there.
func TestDirSizeReportsAnUnreadableRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path string
		ok   func(int64) bool
	}{
		{"readable tree", dir, func(n int64) bool { return n >= 1000 }},
		{"missing root", filepath.Join(dir, "gone"), func(n int64) bool { return n == 0 }},
		// A NUL byte fails every syscall with EINVAL, the portable stand-in for
		// a root the kernel refuses to stat.
		{"unreadable root", dir + "\x00x", func(n int64) bool { return n == -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dirSize(tc.path); !tc.ok(got) {
				t.Fatalf("dirSize(%q) = %d", tc.path, got)
			}
		})
	}
}
