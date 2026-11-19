package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"dylaris-core/models"
)

// R35-01/R35-03: the node's .active_server is the truth. Core follows it once
// it has disagreed for activeSubSettle with no switch or setup in flight, so
// it never fights a command, and never acts on a heartbeat taken before the
// node's record of a landed command cleared.
func TestActiveSubStep(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	settled := t0.Add(activeSubSettle)
	seenAt := activeSubSighting{core: "modded", node: "main", since: t0}
	for _, c := range []struct {
		name       string
		core, node string
		pending    bool
		prev       *activeSubSighting
		now        time.Time
		wantAdopt  bool
		wantTrack  bool
	}{
		{"old node, no field", "modded", "", false, &seenAt, settled, false, false},
		{"agrees", "main", "main", false, &seenAt, settled, false, false},
		{"Core names none", "", "main", false, nil, settled, false, false},
		{"not a name", "modded", "../x", false, nil, settled, false, false},
		{"switch or setup pending", "modded", "main", true, &seenAt, settled, false, false},
		{"first sighting", "modded", "main", false, nil, t0, false, true},
		{"not settled yet", "modded", "main", false, &seenAt, settled.Add(-time.Second), false, true},
		{"settled", "modded", "main", false, &seenAt, settled, true, false},
		{"Core moved on since", "other", "main", false, &seenAt, settled, false, true},
		{"node moved on since", "modded", "third", false, &seenAt, settled, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var prev activeSubSighting
			if c.prev != nil {
				prev = *c.prev
			}
			adopt, next, track := activeSubStep(c.core, c.node, c.pending, prev, c.prev != nil, c.now)
			if adopt != c.wantAdopt || track != c.wantTrack {
				t.Fatalf("adopt=%v track=%v, want %v %v", adopt, track, c.wantAdopt, c.wantTrack)
			}
			if track && (next.core != c.core || next.node != c.node) {
				t.Errorf("tracked %+v for %s/%s", next, c.core, c.node)
			}
		})
	}
}

type adoptFakeStore struct {
	switchFailFakeStore
	servers []models.Server
	install *models.SubServerInstall
	adopted [][5]string
}

func (f *adoptFakeStore) ListNodes() ([]models.Node, error) {
	return []models.Node{{ID: 7, Token: "node-a"}}, nil
}
func (f *adoptFakeStore) ListAllServers() ([]models.Server, error) { return f.servers, nil }
func (f *adoptFakeStore) GetSubServerInstall(int, string) (*models.SubServerInstall, error) {
	return f.install, nil
}
func (f *adoptFakeStore) AdoptServerActiveSubServer(id int, from, to, it, mv, build string) (bool, error) {
	f.adopted = append(f.adopted, [5]string{from, to, it, mv, build})
	return true, nil
}

// End to end through the heartbeat: the settled mismatch is adopted with the
// install record's loader columns; a pending record or an older node is not.
func TestAdoptNodeActiveSubServers(t *testing.T) {
	const uuid = "35353535-0000-0000-0000-000000000001"
	for _, c := range []struct {
		name        string
		field       map[string]string
		record      string // "" none, else the key that is pending
		status      string // "" stopped
		noInstall   bool
		wantAdopted bool
	}{
		{"adopted", map[string]string{uuid: "main"}, "", "", false, true},
		{"old node", nil, "", "", false, false},
		{"switch pending", map[string]string{uuid: "main"}, SwitchUndoKey(uuid), "", false, false},
		{"setup pending", map[string]string{uuid: "main"}, setupBeforeKey(uuid), "", false, false},
		{"no install record", map[string]string{uuid: "main"}, "", "", true, false},
		// R36-01/R36-02: a version move writes neither record above.
		{"version move pending", map[string]string{uuid: "main"}, VersionUpdateUndoKey(uuid), "", false, false},
		{"installing", map[string]string{uuid: "main"}, "", "installing", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := &adoptFakeStore{
				servers: []models.Server{{ID: 1, UUID: uuid, NodeID: 7, ActiveSubServer: "modded", Status: "stopped"}},
				install: &models.SubServerInstall{InstallerType: "paper", McVersion: "1.21.4", BuildVersion: "100"},
			}
			if c.noInstall {
				fs.install = nil
			}
			if c.status != "" {
				fs.servers[0].Status = c.status
			}
			s := &StatusWatcherService{store: fs, redis: newQueueTestRedis(t)}
			ctx := context.Background()
			hb, _ := json.Marshal(NodeHeartbeat{ID: "node-a", ActiveSubServers: c.field})
			s.redis.Set(ctx, "dylaris:discovery:node-a", hb, time.Minute)
			if c.record != "" {
				s.redis.Set(ctx, c.record, `{}`, time.Minute)
			}
			t0 := time.Now()
			first := s.adoptNodeActiveSubServers(ctx, t0)
			got := s.adoptNodeActiveSubServers(ctx, t0.Add(activeSubSettle))
			if first || got != c.wantAdopted || (len(fs.adopted) == 1) != c.wantAdopted {
				t.Fatalf("first=%v adopted=%v writes=%v, want %v", first, got, fs.adopted, c.wantAdopted)
			}
			if c.wantAdopted && fs.adopted[0] != [5]string{"modded", "main", "paper", "1.21.4", "100"} {
				t.Errorf("adopted %v", fs.adopted[0])
			}
		})
	}
}

// R35-05: marking the previous sub-server deleted never brings back a record
// the node deleted, and keeps the TTL of one that is there.
func TestMarkSwitchPreviousDeleted(t *testing.T) {
	ctx := context.Background()
	rdb := newQueueTestRedis(t)
	MarkSwitchPreviousDeleted(ctx, rdb, "u-1", "main")
	if n, _ := rdb.Exists(ctx, SwitchUndoKey("u-1")).Result(); n != 0 {
		t.Fatal("a record was created")
	}
	rdb.Set(ctx, SwitchUndoKey("u-1"), `{"target":"modded","previous":"main"}`, time.Hour)
	MarkSwitchPreviousDeleted(ctx, rdb, "u-1", "main")
	var undo SwitchUndo
	raw, _ := rdb.Get(ctx, SwitchUndoKey("u-1")).Result()
	if json.Unmarshal([]byte(raw), &undo) != nil || !undo.PreviousDeleted {
		t.Errorf("record = %s, want previousDeleted", raw)
	}
	if ttl, _ := rdb.TTL(ctx, SwitchUndoKey("u-1")).Result(); ttl <= 0 {
		t.Errorf("TTL = %v, want kept", ttl)
	}
}
