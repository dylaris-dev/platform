package main

import (
	"context"
	"fmt"
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

// A PORT_RANGE over the node's own listeners (SFTP 25520, beam 25521,
// migration 25522, beam LAN 25523) or below 1024 handed those ports to game
// servers: Docker could not bind them, and the ledger gave the same port back
// on every retry. Every other path that publishes a port already skipped them.
func TestAllocatePort_SkipsPortsTheNodeCannotPublish(t *testing.T) {
	cases := []struct {
		name       string
		start, end int
		want       []int
	}{
		{"the node's own listeners", 25519, 25524, []int{25519, 25524}},
		{"privileged ports", 1022, 1025, []int{1024, 1025}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			pm := NewPortManager(rdb, "node-1", tc.start, tc.end, seqMode)
			for i, want := range tc.want {
				got, err := pm.AllocatePort(fmt.Sprintf("srv-%d", i))
				if err != nil || got != want {
					t.Fatalf("allocation %d = %d, %v; want %d", i, got, err, want)
				}
			}
			if got, err := pm.AllocatePort("one-more"); err == nil {
				t.Fatalf("allocated %d past the publishable ports", got)
			}
		})
	}
}

// J46: SetPort dropped the server's old port before the new one was stored, so
// a failed persist left a live container's port free for the next server.
func TestSetPort_AFailedPersistKeepsTheOldPort(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	if err := pm.SetPort("alpha", 25600); err != nil {
		t.Fatal(err)
	}

	mr.SetError("LOADING Redis is loading the dataset in memory")
	if err := pm.SetPort("alpha", 25650); err == nil {
		t.Fatal("SetPort succeeded against a failing Redis")
	}
	mr.SetError("")
	rdb.Ping(context.Background()) // drops the connection the failed MULTI left unread data on

	if got := pm.GetPort("alpha"); got != 25600 {
		t.Fatalf("alpha holds %d after the refused move, want its old 25600", got)
	}
	if v, _ := mr.Get("dylaris:node:node-1:port:alpha"); v != "25600" {
		t.Fatalf("alpha's key = %q, want 25600", v)
	}
	if got, err := pm.AllocatePort("beta"); err != nil || got == 25600 {
		t.Fatalf("beta was handed %d (err %v), the port alpha's container binds", got, err)
	}

	// A move that does get stored still releases the old port.
	if err := pm.SetPort("alpha", 25650); err != nil {
		t.Fatal(err)
	}
	if got := pm.GetPort("alpha"); got != 25650 {
		t.Fatalf("alpha holds %d, want 25650", got)
	}
	if v := mr.HGet("dylaris:node:node-1:ports", "alpha"); v != "25650" {
		t.Fatalf("alpha's hash entry = %q, want 25650", v)
	}
	if got, err := pm.AllocatePort("gamma"); err != nil || got != 25600 {
		t.Fatalf("gamma got %d (err %v), want the released 25600", got, err)
	}
}
