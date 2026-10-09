package main

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	pb "dylaris-proto/node"
)

// withServerHeadroom makes every budget belong to server "srv" with left bytes
// below its disk limit.
func withServerHeadroom(t *testing.T, left int64) {
	t.Helper()
	prev := serverDiskHeadroom
	t.Cleanup(func() { serverDiskHeadroom = prev })
	serverDiskHeadroom = func(string) (string, int64, bool) { return "srv", left, true }
}

func TestServerOfDir(t *testing.T) {
	a, b := filepath.FromSlash("/data/a"), filepath.FromSlash("/data/b")
	const id = "90909090-9090-9090-9090-909090909090"
	for _, tc := range []struct{ dir, uuid, serverDir string }{
		{filepath.Join(b, id), id, filepath.Join(b, id)},
		{filepath.Join(a, id, "survival", ".technic-stage-1"), id, filepath.Join(a, id)},
		{a, "", ""},
		{filepath.FromSlash("/data"), "", ""},
		{filepath.FromSlash("/elsewhere/x"), "", ""},
		{filepath.FromSlash("/data/ab/" + id), "", ""},
	} {
		uuid, dir := serverOfDir([]string{a, b}, tc.dir)
		if uuid != tc.uuid || dir != tc.serverDir {
			t.Errorf("serverOfDir(%q) = %q, %q; want %q, %q", tc.dir, uuid, dir, tc.uuid, tc.serverDir)
		}
	}
}

// Copies and installs were bounded by the node's free space alone.
func TestABudgetTakesTheSmallerOfNodeAndServer(t *testing.T) {
	withDiskBudget(t, 1<<30, 1000)

	withServerHeadroom(t, 10<<10)
	if b := newWriteBudget("x"); b.left != 10<<10 || b.err != errServerDiskLimit {
		t.Errorf("server smaller: left %d err %v", b.left, b.err)
	}
	withServerHeadroom(t, 2<<30)
	if b := newWriteBudget("x"); b.left != 1<<30 || b.err != errUnpackBudget {
		t.Errorf("node smaller: left %d err %v", b.left, b.err)
	}
	withServerHeadroom(t, -5)
	if b := newWriteBudget("x"); b.left >= 0 {
		t.Errorf("a server over its limit got %d bytes", b.left)
	}
}

// Two copies into one server each measured the same headroom.
func TestWritesIntoOneServerShareItsHeadroom(t *testing.T) {
	withDiskBudget(t, 1<<30, 1000)
	prev := serverDiskHeadroom
	t.Cleanup(func() { serverDiskHeadroom = prev })
	serverDiskHeadroom = func(dir string) (string, int64, bool) { return dir, 10 << 10, true }

	a := newWriteBudget("one")
	a.spend(6 << 10)
	if b := newWriteBudget("one"); b.left != 4<<10 {
		t.Errorf("the second write into the server got %d bytes", b.left)
	}
	if b := newWriteBudget("two"); b.left != 10<<10 {
		t.Errorf("another server got %d bytes", b.left)
	}
	a.release()
	if b := newWriteBudget("one"); b.left != 10<<10 {
		t.Errorf("after release the server got %d bytes", b.left)
	}
}

func TestACopyStopsAtTheServersDiskLimit(t *testing.T) {
	h, uuid, root := seedNodeOwned(t)
	if err := os.WriteFile(filepath.Join(root, "survival", "world.dat"), make([]byte, 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	withDiskBudget(t, 1<<30, 1000)
	withServerHeadroom(t, 16<<10)

	resp := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival/world.dat", DstPath: "survival/copy.dat"})
	if resp.GetError() == nil || !strings.Contains(resp.GetError().GetMessage(), "disk limit") {
		t.Errorf("a file past the server's limit: %v", resp.GetError())
	}
	if st, err := os.Stat(filepath.Join(root, "survival", "copy.dat")); err == nil && st.Size() > 16<<10 {
		t.Errorf("the copy wrote %d bytes past the limit", st.Size())
	}
	if resp := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival", DstPath: "creative"}); resp.GetError() == nil {
		t.Error("a folder past the server's limit was copied")
	}
	if resp := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival/server.properties", DstPath: "survival/b.properties"}); resp.GetError() != nil {
		t.Errorf("an ordinary copy was refused: %v", resp.GetError())
	}
}

func TestAnUploadZipStopsAtTheServersDiskLimit(t *testing.T) {
	withDiskBudget(t, 1<<30, 1000)
	withServerHeadroom(t, 16<<10)
	dest := t.TempDir()
	zipWith(t, filepath.Join(dest, ".upload.zip"), map[string]string{"world/region.mca": strings.Repeat("x", 64<<10)})
	if err := installFromUploadZip(dest, "direct"); err == nil || !strings.Contains(err.Error(), errServerDiskLimit.Error()) {
		t.Fatalf("err = %v", err)
	}
}

func TestAnImportDownloadStopsAtTheServersDiskLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 64<<10)))
	}))
	defer srv.Close()
	prev := guardedDownloadClient
	t.Cleanup(func() { guardedDownloadClient = prev })
	guardedDownloadClient = srv.Client()
	withDiskBudget(t, 1<<30, 1000)

	withServerHeadroom(t, 0)
	if err := downloadImportGuarded(srv.URL+"/server.jar", filepath.Join(t.TempDir(), "server.jar")); !errors.Is(err, errServerDiskLimit) {
		t.Errorf("a full server: err = %v", err)
	}
	withServerHeadroom(t, 16<<10)
	dest := t.TempDir()
	if err := installFromURL(dest, srv.URL+"/server.jar", downloadImportGuarded); err == nil {
		t.Error("a body past the server's limit was downloaded")
	}
	if st, err := os.Stat(filepath.Join(dest, "server.jar")); err == nil && st.Size() > 16<<10 {
		t.Errorf("server.jar is %d bytes", st.Size())
	}
}

// rewriteTransport sends every request to target, so a pack can name an
// approved host and still be served by the test.
type rewriteTransport struct{ target *url.URL }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

// The mods a .mrpack lists are downloaded, not unpacked, and only the
// manifest-wide 4 GB cap bounded them.
func TestModpackFilesStopAtTheServersDiskLimit(t *testing.T) {
	mod := []byte(strings.Repeat("m", 64<<10))
	sum := sha512.Sum512(mod)
	idx, _ := json.Marshal(mrpackIndex{
		FormatVersion: 1, Game: "minecraft",
		Files: []mrpackFile{{
			Path: "mods/big.jar", FileSize: int64(len(mod)),
			Hashes:    map[string]string{"sha512": hex.EncodeToString(sum[:])},
			Downloads: []string{"https://cdn.modrinth.com/data/x/big.jar"},
		}},
		Dependencies: map[string]string{"minecraft": "1.21.1"},
	})
	packDir := t.TempDir()
	zipWith(t, filepath.Join(packDir, "p.mrpack"), map[string]string{"modrinth.index.json": string(idx)})
	pack, err := os.ReadFile(filepath.Join(packDir, "p.mrpack"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".mrpack") {
			w.Write(pack)
			return
		}
		w.Write(mod)
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	prev := guardedDownloadClient
	t.Cleanup(func() { guardedDownloadClient = prev })
	guardedDownloadClient = &http.Client{Transport: rewriteTransport{target}}

	withDiskBudget(t, 1<<30, 1000)
	withServerHeadroom(t, 16<<10)
	err = installModpack(t.TempDir(), InstallerConfig{URL: "https://cdn.modrinth.com/data/x/p.mrpack"})
	if !errors.Is(err, errServerDiskLimit) {
		t.Fatalf("err = %v", err)
	}
}

// The real measurement: the cached limit less what the server holds now.
func TestServerDiskHeadroomMeasuresTheServer(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	prevSM, prevQ := globalStorageMgr, globalQuotaSet
	t.Cleanup(func() { globalStorageMgr, globalQuotaSet = prevSM, prevQ })
	globalStorageMgr, globalQuotaSet = NewStorageManager(t.TempDir(), rdb), nil

	const uuid = "90909090-9090-9090-9090-909090909090"
	serverDir := globalStorageMgr.GetServerDir(uuid)
	if err := os.MkdirAll(filepath.Join(serverDir, "survival"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "survival", "world.dat"), make([]byte, 300<<10), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, ok := serverDiskHeadroom(filepath.Join(serverDir, "survival")); ok {
		t.Error("a server with no cached limit was bounded")
	}
	recordDiskLimit(context.Background(), rdb, uuid, 1)
	got, left, ok := serverDiskHeadroom(filepath.Join(serverDir, "survival"))
	if want := int64(1<<20) - dirSize(serverDir); !ok || got != uuid || left != want {
		t.Errorf("headroom = %q %d %v, want %q %d", got, left, ok, uuid, want)
	}
	if _, _, ok := serverDiskHeadroom(t.TempDir()); ok {
		t.Error("a directory outside the storage paths was bounded")
	}
}

// The path every caller hands over must resolve to its server: on Linux a
// folder copy and a Technic stage are named /proc/self/fd/N, and a stub that
// ignored the path kept the tests green while neither was bounded.
func TestAFolderCopyIsChargedToItsServer(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	sm := NewStorageManager(t.TempDir(), rdb)
	prevSM, prevQ := globalStorageMgr, globalQuotaSet
	t.Cleanup(func() { globalStorageMgr, globalQuotaSet = prevSM, prevQ })
	globalStorageMgr, globalQuotaSet = sm, nil
	withDiskBudget(t, 1<<40, 1<<20)

	h := NewStreamHandler(sm)
	const uuid = "90909090-9090-9090-9090-909090909091"
	root := sm.GetServerDir(uuid)
	if err := os.MkdirAll(filepath.Join(root, "survival"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "survival", "world.dat"), make([]byte, 400<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	recordDiskLimit(context.Background(), rdb, uuid, 1)

	if resp := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival", DstPath: "a"}); resp.GetError() != nil {
		t.Fatalf("a copy within the limit was refused: %v", resp.GetError())
	}
	resp := h.handleCopy("c", uuid, &pb.CopyFileReq{SrcPath: "survival", DstPath: "b"})
	if resp.GetError() == nil || !strings.Contains(resp.GetError().GetMessage(), "disk limit") {
		t.Errorf("a folder copy past the server's limit: %v", resp.GetError())
	}
}

func TestATechnicInstallIsChargedToItsServer(t *testing.T) {
	pack := zipBytes(t, map[string]string{"server.jar": "jar", "mods/big.jar": strings.Repeat("x", 64<<10)})
	base := serveFiles(t, map[string][]byte{"/pack.zip": pack})
	stubLoaders(t)
	dir := t.TempDir()
	prev := serverDiskHeadroom
	t.Cleanup(func() { serverDiskHeadroom = prev })
	serverDiskHeadroom = func(d string) (string, int64, bool) { return "srv", 16 << 10, d == dir }

	err := installTechnic(dir, InstallerConfig{Variant: "server", URL: base + "/pack.zip"})
	if err == nil || !strings.Contains(err.Error(), "disk limit") {
		t.Fatalf("err = %v", err)
	}
	if exists(filepath.Join(dir, "mods", "big.jar")) {
		t.Error("a pack past the server's limit was installed")
	}
}
