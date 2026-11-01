package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"strings"
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

	if _, err := downloadPresigned(context.Background(), transferClient(true, nil), srv.URL+"/obj"); err == nil {
		t.Fatal("a guarded transfer reached a loopback address")
	}
	if hits != 0 {
		t.Fatalf("the internal address received %d requests", hits)
	}
	// The platform's own storage may sit on a private address on purpose.
	body, err := downloadPresigned(context.Background(), transferClient(false, nil), srv.URL+"/obj")
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

func TestPrivateAllowed(t *testing.T) {
	one := parseAllowPrivate([]string{"192.168.1.10/32"})
	everything := parseAllowPrivate([]string{"0.0.0.0/0", "::/0"})
	for _, tc := range []struct {
		ip    string
		allow []netip.Prefix
		want  bool
	}{
		{"192.168.1.10", one, true},
		{"::ffff:192.168.1.10", one, true},
		{"192.168.1.11", one, false},
		{"10.0.0.1", one, false},
		{"192.168.1.10", nil, false},
		{"10.0.5.7", everything, true},
		{"fd00::1", everything, true},
		// Never, whatever is listed.
		{"127.0.0.1", everything, false},
		{"169.254.169.254", everything, false},
		{"0.0.0.0", everything, false},
		{"224.0.0.1", everything, false},
		{"::1", everything, false},
		{"fe80::1", everything, false},
		{"::", everything, false},
		{"ff02::1", everything, false},
		{"ff01::1", everything, false}, // interface-local multicast
		{"127.0.0.1", parseAllowPrivate([]string{"127.0.0.0/8"}), false},
		{"169.254.169.254", parseAllowPrivate([]string{"169.254.169.254/32"}), false},
	} {
		if got := privateAllowed(net.ParseIP(tc.ip), tc.allow); got != tc.want {
			t.Errorf("privateAllowed(%s, %v) = %v, want %v", tc.ip, tc.allow, got, tc.want)
		}
	}
	// A public address still passes the guard with or without a list; that half
	// is dialAllowed, unchanged.
	if !dialAllowed("8.8.8.8", "443") {
		t.Error("a public address was refused")
	}
}

// An entry Core sent that does not parse is dropped, never read as something
// broader; a list of only bad entries is the plain guarded client.
func TestParseAllowPrivateDropsBadEntries(t *testing.T) {
	got := parseAllowPrivate([]string{"192.168.1.10", " 10.0.5.9/24 ", "nope", "10.0.0.0/33"})
	if len(got) != 1 || got[0] != netip.MustParsePrefix("10.0.5.0/24") {
		t.Fatalf("parsed %v, want only 10.0.5.0/24", got)
	}
	if transferClient(true, []string{"garbage"}) != guardedObjectTransferClient {
		t.Error("a list of unparsable entries did not fall back to the plain guarded client")
	}
	a := transferClient(true, []string{"10.0.5.0/24"})
	if a == guardedObjectTransferClient || a != transferClient(true, []string{" 10.0.5.9/24"}) {
		t.Error("the same allow list did not reuse one cached client")
	}
	if transferClient(false, []string{"10.0.5.0/24"}) != objectTransferClient {
		t.Error("an unguarded transfer took a guarded client")
	}
}

// The guarded client built for a list refuses a private address outside it.
func TestAGuardedTransferRefusesAnUnlistedPrivateAddress(t *testing.T) {
	c := transferClient(true, []string{"192.168.77.1/32"})
	_, err := downloadPresigned(context.Background(), c, "http://192.168.77.2:1/obj")
	if err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("err = %v, want the guard's refusal", err)
	}
}

func TestAllowPrivateIsReadFromBothCommands(t *testing.T) {
	raw := []byte(`{"guardedTransfer":true,"allowPrivate":["192.168.1.10/32","10.0.5.0/24"]}`)
	want := []string{"192.168.1.10/32", "10.0.5.0/24"}
	var run BackupRunCommand
	var restore BackupRestoreCommand
	if err := json.Unmarshal(raw, &run); err != nil || !reflect.DeepEqual(run.AllowPrivate, want) {
		t.Fatalf("backup run: %v %v", err, run.AllowPrivate)
	}
	if err := json.Unmarshal(raw, &restore); err != nil || !reflect.DeepEqual(restore.AllowPrivate, want) {
		t.Fatalf("restore: %v %v", err, restore.AllowPrivate)
	}
}
