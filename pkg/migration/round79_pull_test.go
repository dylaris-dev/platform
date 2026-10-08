package migration

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The pull address comes from the source node. A 302 from it must not make the
// target fetch somewhere else with its own network position.
func TestPullFollowsNoRedirect(t *testing.T) {
	var reached atomic.Int32
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.Write([]byte("internal"))
	}))
	defer inner.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL, http.StatusFound)
	}))
	defer src.Close()

	err := Pull(context.Background(), src.URL, "tok", strings.Repeat("0", 64), filepath.Join(t.TempDir(), "o.zip"), 0, 1<<20)
	if err == nil || reached.Load() != 0 {
		t.Fatalf("err = %v, redirect target reached %d times", err, reached.Load())
	}
}

// A source that sends its headers and then trickles nothing held a command
// slot on the target for good.
func TestPullGivesUpOnAStalledBody(t *testing.T) {
	prev := pullStallTimeout
	pullStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { pullStallTimeout = prev })

	release := make(chan struct{})
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer src.Close()
	defer close(release)

	done := make(chan error, 1)
	go func() {
		done <- Pull(context.Background(), src.URL, "tok", strings.Repeat("0", 64), filepath.Join(t.TempDir(), "o.zip"), 0, 1<<20)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled download succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled download held the pull")
	}
}

// Node to node goes direct; a pre-signed URL goes through the node's proxy, as
// every other public download on the node does (installer_http.go).
func TestPullProxyRouting(t *testing.T) {
	if pullClient.Transport.(*http.Transport).Proxy != nil {
		t.Error("a node-to-node pull would go through the egress proxy")
	}
	if presignedClient.Transport.(*http.Transport).Proxy == nil {
		t.Error("a pre-signed download ignores HTTP(S)_PROXY")
	}
}

// The first stall is before any byte: headers sent, body never starts.
func TestPullGivesUpWhenTheBodyNeverStarts(t *testing.T) {
	prev := pullStallTimeout
	pullStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { pullStallTimeout = prev })

	release := make(chan struct{})
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer src.Close()
	defer close(release)

	done := make(chan error, 1)
	go func() {
		done <- Pull(context.Background(), src.URL, "tok", strings.Repeat("0", 64), filepath.Join(t.TempDir(), "o.zip"), 0, 1<<20)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a body that never started was accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a body that never started held the pull")
	}
}

// Steady progress keeps a slow download alive past the stall timeout.
func TestPullKeepsASlowButMovingDownload(t *testing.T) {
	prev := pullStallTimeout
	pullStallTimeout = 300 * time.Millisecond
	t.Cleanup(func() { pullStallTimeout = prev })

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 8; i++ {
			w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer src.Close()

	sum := sha256.Sum256([]byte("xxxxxxxx"))
	err := Pull(context.Background(), src.URL, "tok", hex.EncodeToString(sum[:]), filepath.Join(t.TempDir(), "o.zip"), 0, 1<<20)
	if err != nil {
		t.Fatalf("a moving download was cut off: %v", err)
	}
}

// Empty entries cost an inode each and pass any byte budget.
func TestExtractRefusesTooManyEntries(t *testing.T) {
	prev := MaxArchiveEntries
	MaxArchiveEntries = 2
	t.Cleanup(func() { MaxArchiveEntries = prev })

	dir := t.TempDir()
	zp := filepath.Join(dir, "a.zip")
	f, _ := os.Create(zp)
	zw := zip.NewWriter(f)
	for _, n := range []string{"a", "b", "c"} {
		w, _ := zw.Create(n)
		w.Write([]byte(n))
	}
	zw.Close()
	f.Close()

	dest := filepath.Join(dir, "out")
	if err := Extract(zp, dest, 1<<20); err == nil {
		t.Fatal("an archive over the entry limit was extracted")
	}
	if _, err := os.Stat(filepath.Join(dest, "a")); err == nil {
		t.Error("entries were written before the refusal")
	}
}
