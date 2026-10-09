package main

import (
	"archive/tar"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	beampb "dylaris-proto/beam"
	pb "dylaris-proto/node"
)

func withDiskBudget(t *testing.T, bytes int64, entries int) {
	t.Helper()
	pb, pe := restoreDiskBudget, maxRestoreEntries
	t.Cleanup(func() { restoreDiskBudget, maxRestoreEntries = pb, pe })
	restoreDiskBudget = func(string) int64 { return bytes }
	maxRestoreEntries = entries
}

// A copy had no bound: a member duplicating a large world again and again
// filled the node's disk for every server on it.
func TestACopyStopsAtTheNodesFreeSpace(t *testing.T) {
	h, uuid, root := seedNodeOwned(t)
	if err := os.WriteFile(filepath.Join(root, "survival", "world.dat"), make([]byte, 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	withDiskBudget(t, 16<<10, 1000)

	if resp := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival/world.dat", DstPath: "survival/copy.dat"}); resp.GetError() == nil {
		t.Error("a file larger than the free space was copied")
	}
	if st, err := os.Stat(filepath.Join(root, "survival", "copy.dat")); err == nil && st.Size() > 16<<10 {
		t.Errorf("the copy wrote %d bytes past the budget", st.Size())
	}
	if resp := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival", DstPath: "creative"}); resp.GetError() == nil {
		t.Error("a folder larger than the free space was copied")
	}

	withDiskBudget(t, 1<<30, 1000)
	if resp := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival/server.properties", DstPath: "survival/b.properties"}); resp.GetError() != nil {
		t.Errorf("an ordinary copy was refused: %v", resp.GetError())
	}
}

// A folder of many small files used the node's inodes, not its bytes.
func TestACopyStopsAtTheEntryCount(t *testing.T) {
	for name, mk := range map[string]func(string) error{
		"files":   func(p string) error { return os.WriteFile(p, nil, 0o644) },
		"folders": func(p string) error { return os.Mkdir(p, 0o755) },
	} {
		t.Run(name, func(t *testing.T) {
			h, uuid, root := seedNodeOwned(t)
			os.MkdirAll(filepath.Join(root, "many"), 0o755)
			for i := range 8 {
				if err := mk(filepath.Join(root, "many", fmt.Sprintf("e%d", i))); err != nil {
					t.Fatal(err)
				}
			}
			withDiskBudget(t, 1<<30, 5)
			if resp := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "many", DstPath: "copy"}); resp.GetError() == nil {
				t.Error("a folder with more entries than allowed was copied")
			}
		})
	}
}

// Only a restore and a migration capped the entries an archive creates.
func TestExtractorsStopAtTheEntryCount(t *testing.T) {
	many := map[string]string{}
	for i := range 5 {
		many[fmt.Sprintf("f%d", i)] = ""
	}

	t.Run("upload zip", func(t *testing.T) {
		withDiskBudget(t, 1<<30, 3)
		dest := t.TempDir()
		zipWith(t, filepath.Join(dest, ".upload.zip"), many)
		if err := installFromUploadZip(dest, "direct"); err == nil {
			t.Fatal("unpacked more entries than allowed")
		}
	})
	t.Run("mrpack overrides", func(t *testing.T) {
		withDiskBudget(t, 1<<30, 3)
		dir := t.TempDir()
		over := map[string]string{}
		for n := range many {
			over["overrides/"+n] = ""
		}
		zipWith(t, filepath.Join(dir, "p.mrpack"), over)
		if err := extractOverrides(filepath.Join(dir, "p.mrpack"), filepath.Join(dir, "srv")); err == nil {
			t.Fatal("unpacked more overrides than allowed")
		}
	})
	t.Run("backup import", func(t *testing.T) {
		withDiskBudget(t, 1<<30, 3)
		dest := t.TempDir()
		var hdrs []tar.Header
		var bodies [][]byte
		for n := range many {
			hdrs = append(hdrs, tar.Header{Name: n, Typeflag: tar.TypeReg})
			bodies = append(bodies, nil)
		}
		writeImportArchive(t, dest, hdrs, bodies)
		if _, err := unpackBackupArchive(filepath.Join(dest, backupImportArchiveName), dest); err == nil {
			t.Fatal("unpacked more entries than allowed")
		}
	})
	t.Run("within the cap", func(t *testing.T) {
		withDiskBudget(t, 1<<30, 10)
		dest := t.TempDir()
		zipWith(t, filepath.Join(dest, ".upload.zip"), many)
		if err := installFromUploadZip(dest, "direct"); err != nil {
			t.Fatal(err)
		}
	})
}

// "import" fetches a URL the tenant names, and nothing capped the body.
func TestAnImportDownloadStopsAtTheNodesFreeSpace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 64<<10)))
	}))
	defer srv.Close()
	prev := guardedDownloadClient
	t.Cleanup(func() { guardedDownloadClient = prev })
	guardedDownloadClient = srv.Client()
	withDiskBudget(t, 16<<10, 1000)

	dest := t.TempDir()
	if err := installFromURL(dest, srv.URL+"/server.jar", downloadImportGuarded); err == nil {
		t.Fatal("a body larger than the free space was downloaded")
	}
	if st, err := os.Stat(filepath.Join(dest, "server.jar")); err == nil && st.Size() > 16<<10 {
		t.Fatalf("server.jar is %d bytes", st.Size())
	}
}

func TestABeamCopyStopsAtTheNodesFreeSpace(t *testing.T) {
	bs, uuid, ctx := newTestBeamServer(t)
	dir := bs.storageMgr.GetServerDir(uuid)
	os.MkdirAll(filepath.Join(dir, "survival"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "survival", "world.dat"), make([]byte, 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	withDiskBudget(t, 16<<10, 1000)
	if resp, err := bs.CopyFile(ctx, &beampb.BeamFileCopyReq{SrcPath: "survival/world.dat", DstPath: "survival/copy.dat"}); err == nil && resp.Success {
		t.Error("beam copied a file larger than the free space")
	}
}

// A node already in its reserve gave the import a cap of 0, which the download
// reads as no cap at all.
func TestAnImportIsRefusedWhenNoSpaceIsLeft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 64<<10)))
	}))
	defer srv.Close()
	prev := guardedDownloadClient
	t.Cleanup(func() { guardedDownloadClient = prev })
	guardedDownloadClient = srv.Client()
	withDiskBudget(t, 0, 1000)

	dest := t.TempDir()
	if err := installFromURL(dest, srv.URL+"/server.jar", downloadImportGuarded); err == nil {
		t.Fatal("downloaded with no space left")
	}
	if _, err := os.Stat(filepath.Join(dest, "server.jar")); err == nil {
		t.Fatal("server.jar was written")
	}
}

// One entry a/a/a/.../f makes every directory on its way; each costs an
// inode, and they were charged as one entry.
func TestAnArchiveEntryPaysForTheDirectoriesItMakes(t *testing.T) {
	withDiskBudget(t, 1<<30, 5)
	deep := "a/b/c/d/e/f/g/h/file"
	t.Run("zip", func(t *testing.T) {
		dest := t.TempDir()
		zipWith(t, filepath.Join(dest, ".upload.zip"), map[string]string{deep: "x"})
		if err := installFromUploadZip(dest, "direct"); err == nil {
			t.Fatal("a deep entry was charged as one")
		}
	})
	t.Run("backup import", func(t *testing.T) {
		dest := t.TempDir()
		writeImportArchive(t, dest, []tar.Header{{Name: deep, Typeflag: tar.TypeReg}}, [][]byte{[]byte("x")})
		if _, err := unpackBackupArchive(filepath.Join(dest, backupImportArchiveName), dest); err == nil {
			t.Fatal("a deep entry was charged as one")
		}
	})
	t.Run("mrpack", func(t *testing.T) {
		dir := t.TempDir()
		zipWith(t, filepath.Join(dir, "p.mrpack"), map[string]string{"overrides/" + deep: "x"})
		if err := extractOverrides(filepath.Join(dir, "p.mrpack"), filepath.Join(dir, "srv")); err == nil {
			t.Fatal("a deep entry was charged as one")
		}
	})
	t.Run("shallow fits", func(t *testing.T) {
		dest := t.TempDir()
		zipWith(t, filepath.Join(dest, ".upload.zip"), map[string]string{"a/file": "x", "a/other": "y"})
		if err := installFromUploadZip(dest, "direct"); err != nil {
			t.Fatal(err)
		}
		if n := inflightWrites.Load(); n != 0 {
			t.Errorf("a finished extraction left %d bytes reserved", n)
		}
	})
}

// Each budget measured the free space alone, so copies started together each
// saw all of it. What a running one has written counts against the next, and
// is given back when it ends.
func TestRunningWritesCountAgainstTheNext(t *testing.T) {
	withDiskBudget(t, 100<<10, 1000)
	if n := inflightWrites.Load(); n != 0 {
		t.Fatalf("inflight %d before the test", n)
	}
	a := newWriteBudget("x")
	a.spend(80 << 10)
	if b := newWriteBudget("x"); b.left != 20<<10 {
		t.Errorf("a second budget sees %d, want what the first left", b.left)
	}
	a.release()
	if b := newWriteBudget("x"); b.left != 100<<10 {
		t.Errorf("after release a budget sees %d", b.left)
	}

	h, uuid, _ := seedNodeOwned(t)
	h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival", DstPath: "creative"})
	h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival/server.properties", DstPath: "survival/c.properties"})
	if n := inflightWrites.Load(); n != 0 {
		t.Errorf("finished copies left %d bytes reserved", n)
	}
}
