package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"

	"dylaris-pkg/fileperms"
	pb "dylaris-proto/node"
)

// R25-06: a running copy or SFTP handle has its bytes on the disk already, so
// the free space a new budget measures has them out. Subtracting them again
// refused every new write on the filesystem while one large upload was open.
func TestWrittenBytesAreNotSubtractedTwice(t *testing.T) {
	free := int64(100)
	prev := restoreDiskBudget
	restoreDiskBudget = func(string) int64 { return free }
	t.Cleanup(func() { restoreDiskBudget = prev; writesOn("").inflight.Store(0); writesOn("").onDisk.Store(0) })

	sm, _ := newMovableServer(t)
	refs := []sftpServerRef{{UUID: movingUUID, Name: "s", Perms: fileperms.Full()}}
	w, err := newVirtualFS(refs, sm, nil, "erin").Filewrite(&sftp.Request{Method: "Put", Filepath: "s/survival/big.bin", Flags: 0x02 | 0x08 | 0x10})
	if err != nil {
		t.Fatal(err)
	}
	defer w.(io.Closer).Close()

	for _, tc := range []struct {
		name  string
		write func() // writes 40 bytes that reach the disk
	}{
		{"copy", func() {
			b := nodeWriteBudget("x")
			if _, err := io.Copy(io.Discard, b.reader(bytes.NewReader(make([]byte, 40)))); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(b.release)
		}},
		{"sftp", func() {
			if _, err := w.WriteAt(make([]byte, 40), 0); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.write()
			free -= 40 // what the disk now reports
			if b := nodeWriteBudget("x"); b.left != free {
				t.Errorf("a new budget sees %d, want the %d the disk has free", b.left, free)
			}
		})
	}

	// A reservation is not on the disk yet and still counts.
	r := nodeWriteBudget("x")
	r.spend(30)
	defer r.release()
	if b := nodeWriteBudget("x"); b.left != free-30 {
		t.Errorf("a new budget sees %d, want %d less the reservation", b.left, free)
	}

	// A write that adds nothing cannot fill the disk.
	full := nodeWriteBudget("x")
	full.spend(free)
	defer full.release()
	if b := nodeWriteBudget("x"); !b.spend(0) {
		t.Error("a write adding no bytes was refused")
	}
}

// R25-07: a copy refused for space left its truncated destination behind,
// counted against the server; an overwrite lost the original too.
func TestARefusedCopyLeavesNothingBehind(t *testing.T) {
	for _, tc := range []struct {
		name, src, dst string
		overwrite      bool
		want           string // what dst holds after, "" for nothing
	}{
		{"new file", "survival/world.dat", "survival/copy.dat", false, ""},
		{"overwrite", "survival/world.dat", "survival/server.properties", true, "motd=hi"},
		{"folder", "survival", "creative", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, uuid, root := seedNodeOwned(t)
			if err := os.WriteFile(filepath.Join(root, "survival", "world.dat"), make([]byte, 64<<10), 0o644); err != nil {
				t.Fatal(err)
			}
			withDiskBudget(t, 16<<10, 1000)
			if resp := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: tc.src, DstPath: tc.dst, Overwrite: tc.overwrite}); resp.GetError() == nil {
				t.Fatal("a copy larger than the free space succeeded")
			}
			got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(tc.dst)))
			switch {
			case tc.want == "" && !os.IsNotExist(err):
				t.Errorf("the failed copy left %s behind (%v)", tc.dst, err)
			case tc.want != "" && string(got) != tc.want:
				t.Errorf("%s holds %d bytes, want the original", tc.dst, len(got))
			}
			if es, _ := os.ReadDir(filepath.Join(root, "survival")); len(es) != 3 {
				t.Errorf("survival holds %d entries, want the 3 seeded: a temp was left", len(es))
			}
		})
	}
}
