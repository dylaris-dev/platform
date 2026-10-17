package services

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"dylaris-core/store"
)

// twoLeaderService is the region that flapped a client on 2026-10-17: two live
// leaders sharing the region key.
func twoLeaderService(t *testing.T) (*WarpService, *fakeWarpStore, *miniredis.Miniredis) {
	t.Helper()
	svc, fs, mr := enrollTestService(t)
	fs.leaders = []store.WarpLeader{
		{LeaderID: "leader-01", Region: "leader-01", Endpoint: "a.example.com:25599", Enabled: true},
		{LeaderID: "leader-02", Region: "leader-01", Endpoint: "b.example.com:25599", Enabled: true},
	}
	return svc, fs, mr
}

func setAlive(t *testing.T, mr *miniredis.Miniredis, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := mr.Set("dylaris:warp:"+id+":alive", "1"); err != nil {
			t.Fatal(err)
		}
	}
}

// queued returns "type:pubkey" for every command in a leader's queue.
func queued(t *testing.T, mr *miniredis.Miniredis, leaderID string) []string {
	t.Helper()
	vals, _ := mr.List("dylaris:warp:" + leaderID + ":queue")
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		var c struct{ Type, Pubkey string }
		if err := json.Unmarshal([]byte(v), &c); err != nil {
			t.Fatalf("unmarshal %q: %v", v, err)
		}
		out = append(out, c.Type+":"+c.Pubkey)
	}
	sort.Strings(out)
	return out
}

func TestEnroll_AddsPeerOnlyToItsHomeLeader(t *testing.T) {
	svc, fs, mr := twoLeaderService(t)
	setAlive(t, mr, "leader-01", "leader-02")
	key := store.WarpAPIKey{ID: 1, Policy: "general", MaxConns: 5, OnNewConn: "block"}
	if _, err := svc.Enroll(context.Background(), key, "pubA", nil); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	home := fs.peers["pubA"].AssignedLeader
	other := map[string]string{"leader-01": "leader-02", "leader-02": "leader-01"}[home]
	if home == "" || other == "" {
		t.Fatalf("peer not homed: %q", home)
	}
	if got := queued(t, mr, home); !slices.Equal(got, []string{"add_peer:pubA"}) {
		t.Errorf("home %s queue = %v", home, got)
	}
	if got := queued(t, mr, other); len(got) != 0 {
		t.Errorf("non-home %s was given the peer: %v", other, got)
	}
}

// A lost add_peer is re-sent on the client's next enroll; otherwise the peer
// would stay off its home until that leader restarted.
func TestEnroll_ReEnrollRepushesToHome(t *testing.T) {
	svc, fs, mr := twoLeaderService(t)
	setAlive(t, mr, "leader-01", "leader-02")
	p := storePeer(1, "pubA", "10.0.99.2")
	p.AssignedLeader = "leader-02"
	fs.InsertWarpPeer(p)
	key := store.WarpAPIKey{ID: 1, Policy: "general", MaxConns: 5, OnNewConn: "block"}
	r, err := svc.Enroll(context.Background(), key, "pubA", nil)
	if err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	if got := queued(t, mr, "leader-02"); !slices.Equal(got, []string{"add_peer:pubA"}) {
		t.Errorf("home queue = %v", got)
	}
	if got := queued(t, mr, "leader-01"); len(got) != 0 {
		t.Errorf("non-home was given the peer: %v", got)
	}
	if r.Endpoints[0] != "b.example.com:25599" {
		t.Errorf("endpoints = %v, want the home first", r.Endpoints)
	}
}

// The assignment keeps the client on its home while the home's alive key is
// missing: only reconcileHomes moves a client, after the grace.
func TestAssignment_KeepsHomeFirstWhileItsAliveKeyIsMissing(t *testing.T) {
	svc, fs, mr := twoLeaderService(t)
	setAlive(t, mr, "leader-01") // leader-02 is the home and looks dead
	p := storePeer(1, "pubA", "10.0.99.2")
	p.AssignedLeader = "leader-02"
	fs.InsertWarpPeer(p)
	r, err := svc.Assignment(context.Background(), store.WarpAPIKey{ID: 1}, "pubA")
	if err != nil {
		t.Fatalf("assignment: %v", err)
	}
	if !r.Assigned || r.Endpoints[0] != "b.example.com:25599" {
		t.Fatalf("assignment = %+v, want assigned with the home first", r)
	}
}

func TestReconcileHomes(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name     string
		home     string
		alive    []string
		disabled string
		passes   []time.Duration // reconcile runs at t0+each
		wantHome string
		want01   []string
		want02   []string
	}{
		{
			name: "unhomed peer is homed and taken off the other leader",
			home: "", alive: []string{"leader-01", "leader-02"},
			passes:   []time.Duration{0},
			wantHome: "leader-01", // freest-first falls back to id order
			want01:   []string{"add_peer:pub1"}, want02: []string{"remove_peer:pub1"},
		},
		{
			name: "live home is left alone",
			home: "leader-02", alive: []string{"leader-01", "leader-02"},
			passes:   []time.Duration{0},
			wantHome: "leader-02",
		},
		{
			name: "dead home within the grace keeps its peers",
			home: "leader-02", alive: []string{"leader-01"},
			passes:   []time.Duration{0, warpHomeDeadAfter - time.Second},
			wantHome: "leader-02",
		},
		{
			name: "dead home past the grace loses its peers",
			home: "leader-02", alive: []string{"leader-01"},
			passes:   []time.Duration{0, warpHomeDeadAfter},
			wantHome: "leader-01",
			want01:   []string{"add_peer:pub1"}, want02: []string{"remove_peer:pub1"},
		},
		{
			name: "a home that is not a leader row moves at once",
			home: "eu-edge-01", alive: []string{"leader-01", "leader-02"},
			passes:   []time.Duration{0},
			wantHome: "leader-01",
			want01:   []string{"add_peer:pub1"}, want02: []string{"remove_peer:pub1"},
		},
		{
			name: "a disabled home moves at once",
			home: "leader-02", alive: []string{"leader-01", "leader-02"}, disabled: "leader-02",
			passes:   []time.Duration{0},
			wantHome: "leader-01",
			want01:   []string{"add_peer:pub1"}, // a disabled row is not a target for commands
		},
		{
			name: "no live leader anywhere keeps the peer where it is",
			home: "leader-02", alive: nil,
			passes:   []time.Duration{0, time.Hour},
			wantHome: "leader-02",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, fs, mr := twoLeaderService(t)
			for i := range fs.leaders {
				if fs.leaders[i].LeaderID == c.disabled {
					fs.leaders[i].Enabled = false
				}
			}
			setAlive(t, mr, c.alive...)
			p := storePeer(1, "pub1", "10.0.99.2")
			p.AssignedLeader = c.home
			fs.InsertWarpPeer(p)
			for _, d := range c.passes {
				if err := svc.reconcileHomes(context.Background(), t0.Add(d)); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
			}
			if got := fs.peers["pub1"].AssignedLeader; got != c.wantHome {
				t.Errorf("home = %q, want %q", got, c.wantHome)
			}
			if got := queued(t, mr, "leader-01"); !slices.Equal(got, c.want01) {
				t.Errorf("leader-01 queue = %v, want %v", got, c.want01)
			}
			if got := queued(t, mr, "leader-02"); !slices.Equal(got, c.want02) {
				t.Errorf("leader-02 queue = %v, want %v", got, c.want02)
			}
		})
	}
}
