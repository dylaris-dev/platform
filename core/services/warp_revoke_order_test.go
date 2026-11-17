package services

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"dylaris-core/store"
)

// queueInOrder is queued without the sort: these tests are about order.
func queueInOrder(t *testing.T, mr *miniredis.Miniredis, leaderID string) []string {
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
	return out
}

// A revoke pushed remove_peer and only then deleted the row, so anything that
// read the row in between could queue a fresh add_peer behind the remove - and
// with the row gone, nothing ever removed it again. The row goes first now.
func TestDisconnectKeyPeers_DeletesTheRowBeforeTellingTheLeader(t *testing.T) {
	svc, fs, mr := enrollTestService(t)
	p := storePeer(7, "pubA", "10.0.99.2")
	p.AssignedLeader = "leader-01"
	fs.InsertWarpPeer(p)
	var queuedAtDelete []string
	fs.onDelete = func(string) { queuedAtDelete = queueInOrder(t, mr, "leader-01") }

	if n := svc.DisconnectKeyPeers(context.Background(), 7); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if len(queuedAtDelete) != 0 {
		t.Errorf("remove_peer was queued before the row went: %v", queuedAtDelete)
	}
	if got := queueInOrder(t, mr, "leader-01"); len(got) != 1 || got[0] != "remove_peer:pubA" {
		t.Errorf("queue = %v, want one remove_peer", got)
	}
}

// The readers that re-push a peer: the client's idempotent re-enroll and the
// home reconciler. Each read the row, then a revoke ran, then each queued its
// add_peer behind the revoke's remove_peer - leaving a revoked machine on the
// leader until it restarted. The last word for that peer must be a remove.
func TestAddPeer_RevokedInBetween_EndsRemoved(t *testing.T) {
	for _, tc := range []struct {
		name string
		home string // the peer's stored home
		// arm places the revoke right after the reader saw the row.
		arm func(fs *fakeWarpStore, revoke func(string))
		run func(t *testing.T, svc *WarpService)
	}{
		{
			name: "idempotent re-enroll",
			home: "leader-01",
			arm:  func(fs *fakeWarpStore, revoke func(string)) { fs.onGet = revoke },
			run: func(t *testing.T, svc *WarpService) {
				key := store.WarpAPIKey{ID: 7, Policy: "general", MaxConns: 5, OnNewConn: "block"}
				_, _ = svc.Enroll(context.Background(), key, "pubA", nil)
			},
		},
		{
			// A home that no longer exists, so the peer is moved and re-added. The
			// real store's assignment write does not fail on a row that is gone.
			name: "home reconciler",
			home: "leader-gone",
			arm:  func(fs *fakeWarpStore, revoke func(string)) { fs.onSetLeader = revoke },
			run: func(t *testing.T, svc *WarpService) {
				if err := svc.reconcileHomes(context.Background(), time.Now()); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, fs, mr := twoLeaderService(t)
			setAlive(t, mr, "leader-01", "leader-02")
			p := storePeer(7, "pubA", "10.0.99.2")
			p.AssignedLeader = tc.home
			fs.InsertWarpPeer(p)
			revoked := false
			tc.arm(fs, func(string) {
				if !revoked {
					revoked = true
					svc.DisconnectKeyPeers(context.Background(), 7)
				}
			})

			tc.run(t, svc)

			if !revoked {
				t.Fatal("the revoke never ran; the test did not reach the reader")
			}
			for _, l := range []string{"leader-01", "leader-02"} {
				q := queueInOrder(t, mr, l)
				last := ""
				for _, c := range q {
					if c == "add_peer:pubA" || c == "remove_peer:pubA" {
						last = c
					}
				}
				if last == "add_peer:pubA" {
					t.Errorf("%s ends with the revoked peer added: %v", l, q)
				}
			}
		})
	}
}

// A dead leader's row is kept on purpose, and every remove in its region is
// still pushed to it; nothing pops that queue, so it grew for as long as the row
// existed. A returning leader is resynced in full on boot, so the oldest
// commands are the ones that can go.
func TestLeaderQueueIsBounded(t *testing.T) {
	svc, _, mr := enrollTestService(t)
	for i := 0; i < warpLeaderQueueMax+3; i++ {
		svc.pushToLeader(context.Background(), "gone", removePeerCmd("pub"+strconv.Itoa(i)))
	}
	q := queueInOrder(t, mr, "gone")
	if len(q) != warpLeaderQueueMax {
		t.Fatalf("queue length = %d, want %d", len(q), warpLeaderQueueMax)
	}
	if want := "remove_peer:pub" + strconv.Itoa(warpLeaderQueueMax+2); q[len(q)-1] != want {
		t.Errorf("newest = %q, want %q: the trim must drop the OLDEST", q[len(q)-1], want)
	}
}
