package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newQuotaSetOverPaths builds a QuotaSet over n temp dirs. The providers there
// are all unavailable (tmpfs/overlay is neither xfs nor ext4), which is fine:
// these tests are about ROUTING - which filesystem a server's quota operations
// are aimed at - not about the quota tools themselves, which need a real
// quota-enabled mount and belong to integration scope.
func newQuotaSetOverPaths(t *testing.T, n int) (*QuotaSet, *StorageManager, []string) {
	t.Helper()
	roots := make([]string, n)
	for i := range roots {
		roots[i] = t.TempDir()
	}
	sm := NewStorageManager(strings.Join(roots, ","), nil)
	if got := len(sm.Paths()); got != n {
		t.Fatalf("storage manager took %d paths, want %d", got, n)
	}
	return NewQuotaSet(sm), sm, roots
}

// The bug this replaces: one provider bound to paths[0] meant a server on any
// other path had no quota at all, silently.
func TestQuotaSetCoversEveryStoragePath(t *testing.T) {
	qs, _, roots := newQuotaSetOverPaths(t, 3)

	avail := qs.AvailabilityByPath()
	if len(avail) != len(roots) {
		t.Fatalf("AvailabilityByPath() covers %d paths, want %d", len(avail), len(roots))
	}
	for _, p := range roots {
		if _, ok := avail[p]; !ok {
			t.Errorf("path %q has no quota provider", p)
		}
	}
}

func TestQuotaSetRoutesToTheServersOwnPath(t *testing.T) {
	qs, _, roots := newQuotaSetOverPaths(t, 3)

	// A server directory on the LAST path: resolution must follow the data, not
	// default to the first path.
	const uuid = "11111111-2222-3333-4444-555555555555"
	if err := os.MkdirAll(filepath.Join(roots[2], uuid), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	qp := qs.forServer(uuid)
	if qp == nil {
		t.Fatal("forServer returned nil for a server that exists on a configured path")
	}
	if qp != qs.providers[roots[2]] {
		t.Errorf("forServer routed to the wrong path; want the provider for %q", roots[2])
	}
}

// Deletion must not depend on path resolution: it runs while the directory and
// the Redis path key are being torn down.
func TestQuotaSetRemoveQuotaIsPathIndependent(t *testing.T) {
	qs, _, roots := newQuotaSetOverPaths(t, 2)

	// No directory anywhere, so forServer cannot identify a path.
	qs.RemoveQuota("never-existed")

	// The contract is that it touched every provider rather than giving up.
	if len(qs.providers) != len(roots) {
		t.Fatalf("providers = %d, want %d", len(qs.providers), len(roots))
	}
}

// Every method must survive a nil set and an unresolvable server, because the
// delete path and a misconfigured storage path both produce exactly that.
func TestQuotaSetNilAndUnknownAreNoOps(t *testing.T) {
	var nilSet *QuotaSet
	if err := nilSet.AssignQuota("x"); err != nil {
		t.Errorf("nil AssignQuota returned %v, want nil", err)
	}
	if err := nilSet.SetLimit("x", 100); err != nil {
		t.Errorf("nil SetLimit returned %v, want nil", err)
	}
	if got := nilSet.GetDiskUsage("x"); got != nil {
		t.Errorf("nil GetDiskUsage returned %v, want nil", got)
	}
	if nilSet.IsAvailableFor("x") {
		t.Error("nil IsAvailableFor returned true")
	}
	if got := nilSet.AvailabilityByPath(); len(got) != 0 {
		t.Errorf("nil AvailabilityByPath returned %v, want empty", got)
	}
	nilSet.RemoveQuota("x")

	qs, _, _ := newQuotaSetOverPaths(t, 1)
	if err := qs.SetLimit("unknown-server", 100); err != nil {
		t.Errorf("SetLimit for an unresolvable server returned %v, want nil", err)
	}
}

// The disk poll interval was picked node-wide: one quota-capable path put
// every server on a du-only path (NFS/CIFS) on the 10 s quota cadence, which
// for them is a full du walk back to back.
func TestDiskPollIntervalIsPerServer(t *testing.T) {
	qs, _, roots := newQuotaSetOverPaths(t, 2)
	qs.providers[roots[0]].available = true
	quotaSrv := "11111111-2222-3333-4444-555555555555"
	duSrv := "66666666-7777-8888-9999-000000000000"
	for _, d := range []string{filepath.Join(roots[0], quotaSrv), filepath.Join(roots[1], duSrv)} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		uuid string
		want time.Duration
	}{
		{quotaSrv, diskInterval},
		{duSrv, diskFallbackInterval},
	}
	for _, c := range cases {
		if got := diskPollInterval(qs, c.uuid); got != c.want {
			t.Errorf("%s: interval %v, want %v", c.uuid, got, c.want)
		}
	}
	if got := diskPollInterval(nil, quotaSrv); got != diskFallbackInterval {
		t.Errorf("nil quota set: interval %v, want %v", got, diskFallbackInterval)
	}
}
