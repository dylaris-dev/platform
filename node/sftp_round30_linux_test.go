package main

import (
	"os"
	"testing"

	"dylaris-pkg/fileperms"

	"github.com/pkg/sftp"
)

// The path form a client sends ("/name/...", cleaned by pkg/sftp itself), on
// the platform the node runs on.
func TestSFTPNoTruncWriteWithAClientPath(t *testing.T) {
	fs, target := permittedFS(t, fileperms.Full())
	r := sftp.NewRequest("Put", "/myserver/survival/server.jar")
	r.Flags = 0x02 | 0x08 // write | creat, no trunc
	w, err := fs.Filewrite(r)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteAt([]byte("J"), 0)
	w.(interface{ Close() error }).Close()
	if got, _ := os.ReadFile(target); string(got) != "Jar" {
		t.Fatalf("file = %q, want Jar", got)
	}
}
