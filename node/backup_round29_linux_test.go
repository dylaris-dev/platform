package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// One mkfifo in a server held a backup worker forever (opening a pipe with no
// writer blocks); a socket failed every backup of its server.
func TestABackupSkipsPipesAndSockets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "world.dat"), []byte("w"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(root, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	done := make(chan error, 1)
	var buf bytes.Buffer
	go func() {
		_, err := writeServerArchive(&buf, root, root, nil, nil, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("backup failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the backup hung on the pipe")
	}
	got := readArchive(t, buf.Bytes())
	if _, ok := got["world.dat"]; !ok || len(got) != 1 {
		t.Fatalf("archive = %v, want only world.dat", got)
	}
}

// A file linked under many names, by hard link or symlink, was archived in
// full once per name: free for the tenant, terabytes for the platform.
func TestABackupStoresALinkedFileOnce(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "big.dat")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"h1", "h2"} {
		if err := os.Link(big, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("big.dat", filepath.Join(root, "s1")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := writeServerArchive(&buf, root, root, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	gr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gr)
	full, links := 0, 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			full++
		case tar.TypeLink:
			links++
		}
	}
	if full != 1 || links != 3 {
		t.Fatalf("full copies %d, links %d; want 1 and 3", full, links)
	}
}
