package main

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func seqMode() string { return "sequential" }

// The failure this pins, reproduced end to end on the testbed before the fix:
// Valkey runs in-memory only, so a host reboot erases the port ledger while the
// containers holding those ports survive. The node then believed the whole
// range was free and handed a live container's port to the next server. Docker
// refused the bind, the reconciler started the container anyway with NO
// published port, and Core still reported it online - a server nobody can
// reach.
func TestAdoptExistingBindings_RecoversTheLedgerAfterARedisWipe(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	// A wiped Redis: nothing to load.
	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	if got := pm.GetPort("alpha"); got != 0 {
		t.Fatalf("fresh manager knows port %d for alpha, want 0", got)
	}

	// The host still has alpha's container bound to 25600.
	pm.AdoptExistingBindings(map[string]int{"alpha": 25600})

	if got := pm.GetPort("alpha"); got != 25600 {
		t.Fatalf("GetPort(alpha) = %d, want 25600", got)
	}
	if v, err := mr.Get("dylaris:node:node-1:port:alpha"); err != nil || v != "25600" {
		t.Fatalf("ledger not restored in Redis: %q (err %v)", v, err)
	}

	// The next server must NOT be handed the port alpha is holding.
	got, err := pm.AllocatePort("beta")
	if err != nil {
		t.Fatalf("AllocatePort(beta): %v", err)
	}
	if got == 25600 {
		t.Fatal("beta was allocated 25600, the port alpha's live container holds")
	}
	if got != 25601 {
		t.Fatalf("beta got %d, want the next free port 25601", got)
	}
}

func TestAdoptExistingBindings_IsANoOpWhenTheLedgerAlreadyAgrees(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Set("dylaris:node:node-1:port:alpha", "25604")

	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	pm.AdoptExistingBindings(map[string]int{"alpha": 25604})

	if got := pm.GetPort("alpha"); got != 25604 {
		t.Fatalf("GetPort(alpha) = %d, want 25604 (adopt must not move a port)", got)
	}
}

// Two containers configured for the same host port is corruption, not a state
// to smooth over: only one of them can hold it. The second must be refused and
// must not steal the port from the first.
func TestAdoptExistingBindings_RefusesADuplicatePort(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	pm.AdoptExistingBindings(map[string]int{"alpha": 25600})
	pm.AdoptExistingBindings(map[string]int{"beta": 25600})

	if got := pm.GetPort("alpha"); got != 25600 {
		t.Fatalf("alpha lost its port: GetPort = %d", got)
	}
	if got := pm.GetPort("beta"); got != 0 {
		t.Fatalf("beta was given the duplicate port %d", got)
	}
}

// A binding outside the node's own range still belongs to that server; it just
// can never be handed out by AllocatePort.
func TestAdoptExistingBindings_KeepsOutOfRangeBindings(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	pm.AdoptExistingBindings(map[string]int{"legacy": 25000})

	if got := pm.GetPort("legacy"); got != 25000 {
		t.Fatalf("GetPort(legacy) = %d, want 25000", got)
	}
}

// A ledger entry Redis kept for a server that no longer holds the port (a
// release that failed during a Redis blip) used to beat the container that
// really binds it: the live server was left out of the ledger and its next
// recreate failed or moved its address. The containers win, in any order.
func TestAdoptContainerBindings_TheLiveContainerBeatsAStaleEntry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bindings map[string]int
		want     map[string]int
	}{
		{"stale owner without a container", map[string]int{"live": 25600}, map[string]int{"live": 25600, "stale": 0}},
		{"stale owner moved to another port", map[string]int{"live": 25600, "stale": 25601}, map[string]int{"live": 25600, "stale": 25601}},
		{"two containers on one port stay refused", map[string]int{"live": 25600, "stale": 25600}, map[string]int{"stale": 25600, "live": 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			mr.Set("dylaris:node:node-1:port:stale", "25600")
			mr.HSet("dylaris:node:node-1:ports", "stale", "25600")

			pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
			pm.AdoptContainerBindings(tc.bindings)

			for uuid, want := range tc.want {
				if got := pm.GetPort(uuid); got != want {
					t.Errorf("GetPort(%s) = %d, want %d", uuid, got, want)
				}
			}
			if tc.want["stale"] == 0 && mr.Exists("dylaris:node:node-1:port:stale") {
				t.Error("the stale entry survived in Redis")
			}
		})
	}
}
