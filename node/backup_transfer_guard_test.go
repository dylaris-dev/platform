package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// A tenant's own bucket is presigned against the endpoint they typed. On a
// platform node that URL could name Redis, Postgres or the Hub next door, and
// the node made the request from inside. httptest listens on loopback: exactly
// such an address.
func TestATenantBucketTransferRefusesAnInternalAddress(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	if _, err := downloadPresigned(context.Background(), transferClient(true), srv.URL+"/obj"); err == nil {
		t.Fatal("a guarded transfer reached a loopback address")
	}
	if hits != 0 {
		t.Fatalf("the internal address received %d requests", hits)
	}
	// The platform's own storage may sit on a private address on purpose.
	body, err := downloadPresigned(context.Background(), transferClient(false), srv.URL+"/obj")
	if err != nil {
		t.Fatalf("an unguarded transfer was refused: %v", err)
	}
	body.Close()
}

// The flag arrives in the command JSON Core sends; a typo in the tag would
// leave every transfer unguarded without a single test failing elsewhere.
func TestTheGuardFlagIsReadFromBothCommands(t *testing.T) {
	var run BackupRunCommand
	var restore BackupRestoreCommand
	raw := []byte(`{"guardedTransfer":true}`)
	if err := json.Unmarshal(raw, &run); err != nil || !run.GuardedTransfer {
		t.Fatalf("backup run: %v %+v", err, run.GuardedTransfer)
	}
	if err := json.Unmarshal(raw, &restore); err != nil || !restore.GuardedTransfer {
		t.Fatalf("restore: %v %+v", err, restore.GuardedTransfer)
	}
}

// Core's own mirror is fetched without the address guard, because a
// self-hosted Core may sit on a private address. So it must not follow a
// redirect off that host: the tenant picks the path, and one open redirect
// there would point this node anywhere, the answer written into their server.
func TestTheMirrorDownloadStaysOnItsHost(t *testing.T) {
	internalHits := 0
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHits++
		w.Write([]byte("secret"))
	}))
	t.Cleanup(internal.Close)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, "/pack", http.StatusFound)
			return
		}
		if r.URL.Path == "/pack" {
			w.Write([]byte("pack"))
			return
		}
		http.Redirect(w, r, internal.URL+"/x", http.StatusFound)
	}))
	t.Cleanup(mirror.Close)
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if _, err := downloadBoundedInto(root, "a", mirror.URL+"/open-redirect", 1<<20); err == nil || internalHits != 0 {
		t.Fatalf("followed a redirect off the mirror: err=%v hits=%d", err, internalHits)
	}
	if n, err := downloadBoundedInto(root, "b", mirror.URL+"/hop", 1<<20); err != nil || n != 4 {
		t.Fatalf("a redirect on the mirror itself failed: n=%d err=%v", n, err)
	}
}
