package migration

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const testSecret = "test-cluster-secret-0123456789"

// ─── Token ──────────────────────────────────────────────────────────────

func TestToken_RoundTrip(t *testing.T) {
	tok, err := MintToken(testSecret, "server-abc", "node-src", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	c, err := VerifyToken(testSecret, tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.ServerUUID != "server-abc" || c.SourceNodeID != "node-src" {
		t.Fatalf("claims mismatch: %+v", c)
	}
}

func TestToken_Expired(t *testing.T) {
	// Negative TTL → ExpiresAt already in the past.
	tok, err := MintToken(testSecret, "s", "n", -time.Second)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := VerifyToken(testSecret, tok); err != ErrTokenExpired {
		t.Fatalf("want ErrTokenExpired, got %v", err)
	}
}

func TestToken_TamperedPayload(t *testing.T) {
	tok, err := MintToken(testSecret, "server-abc", "node-src", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	dot := strings.IndexByte(tok, '.')
	if dot < 0 {
		t.Fatal("token missing separator")
	}
	// Flip a character in the payload; signature no longer matches.
	payload := []byte(tok[:dot])
	if payload[0] == 'A' {
		payload[0] = 'B'
	} else {
		payload[0] = 'A'
	}
	tampered := string(payload) + tok[dot:]
	if _, err := VerifyToken(testSecret, tampered); err != ErrTokenInvalid {
		t.Fatalf("want ErrTokenInvalid, got %v", err)
	}
}

func TestToken_WrongSecret(t *testing.T) {
	tok, err := MintToken(testSecret, "s", "n", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := VerifyToken("other-secret", tok); err != ErrTokenInvalid {
		t.Fatalf("want ErrTokenInvalid, got %v", err)
	}
}

// ─── Full transport round-trip ───────────────────────────────────────────

func TestTransport_RoundTrip(t *testing.T) {
	src := t.TempDir()
	// A few nested files, an empty dir, and a binary blob.
	writeFile(t, filepath.Join(src, "server.properties"), []byte("level-name=world\nmotd=hi\n"))
	writeFile(t, filepath.Join(src, "world", "level.dat"), []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0x10, 0x00, 0x7f})
	writeFile(t, filepath.Join(src, "config", "mod", "settings.toml"), []byte("[a]\nb = 1\n"))
	if err := os.MkdirAll(filepath.Join(src, "logs-empty"), 0755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}

	stage := t.TempDir()
	zipPath := filepath.Join(stage, "server.zip")
	sum, size, err := Archive(src, zipPath)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if size <= 0 {
		t.Fatalf("archive size = %d", size)
	}

	const serverUUID = "srv-123"
	const sourceNodeID = "node-A"

	// Serve the staged zip behind the same token check + ServeFile logic the
	// node uses, so the test exercises the real verify path end-to-end.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims, verr := VerifyToken(testSecret, tok)
		if verr != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if claims.ServerUUID != serverUUID {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.ServeFile(w, r, zipPath)
	}))
	defer srv.Close()

	tok, err := MintToken(testSecret, serverUUID, sourceNodeID, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	dest := t.TempDir()
	pulledZip := filepath.Join(dest, "pulled.zip")

	// Correct sha → passes.
	if err := Pull(context.Background(), srv.URL+"/migration", tok, sum, pulledZip, 2, size); err != nil {
		t.Fatalf("pull (good sha): %v", err)
	}

	// Wrong sha → fails after retries.
	wrong := strings.Repeat("0", 64)
	if err := Pull(context.Background(), srv.URL+"/migration", tok, wrong, pulledZip, 2, size); err == nil {
		t.Fatal("pull (wrong sha): expected failure, got nil")
	}

	// Extract and assert the tree byte-matches the original.
	out := t.TempDir()
	if err := Extract(pulledZip, out, math.MaxInt64); err != nil {
		t.Fatalf("extract: %v", err)
	}
	assertTreesEqual(t, src, out)
}

func TestTransport_BadTokenRejected(t *testing.T) {
	stage := t.TempDir()
	zipPath := filepath.Join(stage, "s.zip")
	srcDir := t.TempDir()
	writeFile(t, filepath.Join(srcDir, "f.txt"), []byte("x"))
	sum, _, err := Archive(srcDir, zipPath)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, verr := VerifyToken(testSecret, tok); verr != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.ServeFile(w, r, zipPath)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.zip")
	// Forged token → server returns 401 → Pull never matches the hash.
	if err := Pull(context.Background(), srv.URL+"/migration", "garbage.token", sum, dest, 1, 0); err == nil {
		t.Fatal("expected pull to fail with a bad token")
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// assertTreesEqual walks want and got, comparing the set of relative paths and
// the byte contents of every regular file.
func assertTreesEqual(t *testing.T, want, got string) {
	t.Helper()
	wantFiles := snapshot(t, want)
	gotFiles := snapshot(t, got)

	if len(wantFiles) != len(gotFiles) {
		t.Fatalf("entry count: want %d, got %d\nwant=%v\ngot=%v",
			len(wantFiles), len(gotFiles), keys(wantFiles), keys(gotFiles))
	}
	for rel, wantData := range wantFiles {
		gotData, ok := gotFiles[rel]
		if !ok {
			t.Fatalf("missing entry %q in extracted tree", rel)
		}
		if wantData == nil { // directory marker
			if gotData != nil {
				t.Fatalf("%q: want dir, got file", rel)
			}
			continue
		}
		if string(wantData) != string(gotData) {
			t.Fatalf("%q: content mismatch", rel)
		}
	}
}

// snapshot returns rel-path → contents (nil contents marks a directory).
func snapshot(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			out[rel] = nil
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func keys(m map[string][]byte) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
