package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The failure this exists for.
//
// A machine still on an older deploy file has no Link service beside the node.
// Removing the one container it has would take the only way into every server on
// that host with it: in gateway routing the Link is the sole ingress and an MC
// container publishes no port. A cleanup step must never be the thing that locks
// players out, so it only ever runs when a replacement is already serving.
func TestKeepSoleLink(t *testing.T) {
	for _, tc := range []struct {
		name   string
		links  int
		listOK bool
		keep   bool
	}{
		// The count INCLUDES the container about to be removed.
		{"the only Link on the host", 1, true, true},
		{"a replacement is already running", 2, true, false},
		{"several others are running", 5, true, false},

		// Cannot tell. A wrong keep costs a stale container; a wrong removal
		// costs every player on the host, so the doubt resolves towards keeping.
		{"the container list failed", 1, false, true},
		{"the list failed and reported a count anyway", 9, false, true},

		// Should not happen - the caller has just inspected one - but a zero must
		// not read as "nothing to protect, remove away".
		{"counted zero despite holding one", 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepSoleLink(tc.links, tc.listOK); got != tc.keep {
				t.Errorf("keepSoleLink(%d, %v) = %v, want %v", tc.links, tc.listOK, got, tc.keep)
			}
		})
	}
}

// The two files hold a working tunnel token and a discovery proof for a Link
// this node no longer runs. Leaving them is leaving a live credential in a file
// with no owner. They sit under a customer storage path, so the function must
// name exactly those two and touch nothing else.
func TestRemoveStaleLinkCreds(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, ".node_secret")
	for _, name := range []string{".link_secret", ".link_discovery_proof", ".node_secret"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removeStaleLinkCreds(dir)

	for _, name := range []string{".link_secret", ".link_discovery_proof"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived: a live tunnel token is still on disk with nothing that owns it", name)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf(".node_secret was removed as well: this runs against a customer storage path and must name only its own two files (%v)", err)
	}
}

// A machine that never had them is the normal case, and the storage path may not
// exist yet at all. Neither may panic or complain.
func TestRemoveStaleLinkCredsIsQuietWhenThereIsNothingToRemove(t *testing.T) {
	removeStaleLinkCreds(t.TempDir())
	removeStaleLinkCreds(filepath.Join(t.TempDir(), "not-created-yet"))
}
