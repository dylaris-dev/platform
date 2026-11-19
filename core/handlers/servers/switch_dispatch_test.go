package servers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// switchDispatchStore records what a sub-server switch writes.
type switchDispatchStore struct {
	*installFakeStore
	activeWrites []string
}

func (f *switchDispatchStore) ListSubServerInstalls(int) ([]models.SubServerInstall, error) {
	return []models.SubServerInstall{{SubServerName: "main"}, {SubServerName: "lobby"}}, nil
}
func (f *switchDispatchStore) UpdateServerActiveSubServer(_ int, name string) error {
	f.activeWrites = append(f.activeWrites, name)
	return nil
}
func (f *switchDispatchStore) GetServerRconConfig(int) (bool, int, string, error) {
	return false, 0, "", nil
}
func (f *switchDispatchStore) GetVoicePort(int) (*store.VoicePort, error) { return nil, nil }

// C30/C38: a switch whose command never reached the node answered "queued"
// with Core's active_sub_server already moved, so Core and the node's
// .active_server disagreed from then on. Nothing is left changed and the
// caller is told.
func TestSwitchSubServerUndoesWhenTheDispatchFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nodeErr error
		queue   func(t *testing.T) *services.QueueService
	}{
		{"the node row cannot be read", errors.New("db down"), nil},
		{"the command cannot be queued", nil, func(t *testing.T) *services.QueueService {
			dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
			t.Cleanup(func() { dead.Close() })
			return services.NewQueueService(dead)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fs, rdb := newInstallTest(t)
			fs.server.Status = "stopped"
			fs.server.DesiredState = "stopped"
			fs.nodeErr = tc.nodeErr
			ss := &switchDispatchStore{installFakeStore: fs}
			h.d.Store = ss
			if tc.queue != nil {
				h.d.Queue = tc.queue(t)
			}
			rec := switchTo(h, "lobby")
			if rec.Code < 500 && rec.Code != http.StatusNotFound {
				t.Fatalf("status %d, want a failure: %s", rec.Code, rec.Body.String())
			}
			if n := len(ss.activeWrites); n != 0 && ss.activeWrites[n-1] != "main" {
				t.Fatalf("active sub-server writes %v, want it left on main", ss.activeWrites)
			}
			if n := len(fs.updateDesiredStateCalls); n != 0 && fs.updateDesiredStateCalls[n-1].state != "stopped" {
				t.Fatalf("desired state writes %v, want it left stopped", fs.updateDesiredStateCalls)
			}
			if got, _ := rdb.Get(context.Background(), services.DesiredStateKey("srv-uuid")).Result(); got == "online" {
				t.Fatal("published desired state left online for a switch that never happened")
			}
			if len(fs.setRconNeedsRestartCalls) != 0 {
				t.Fatalf("rcon needs-restart written %v for a switch that never happened", fs.setRconNeedsRestartCalls)
			}
		})
	}
}

// C102: a switch after a crash loop left reconcile_failed and stopped_by_server
// in place. The status watcher, running before the node cleared them, read them
// as unlanded and set the intent back to stopped under the running switch.
func TestSwitchSubServerClearsTheNodeHolds(t *testing.T) {
	h, fs, rdb := newInstallTest(t)
	fs.server.Status = "offline"
	fs.server.DesiredState = "stopped"
	ss := &switchDispatchStore{installFakeStore: fs}
	h.d.Store = ss
	ctx := context.Background()
	holds := []string{"dylaris:server:srv-uuid:reconcile_failed", "dylaris:server:srv-uuid:stopped_by_server"}
	for _, k := range holds {
		rdb.Set(ctx, k, "1", 0)
	}

	if rec := switchTo(h, "lobby"); rec.Code >= 300 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	for _, k := range holds {
		if n, _ := rdb.Exists(ctx, k).Result(); n != 0 {
			t.Errorf("%s survived the switch", k)
		}
	}
	if got, _ := rdb.Get(ctx, services.DesiredStateKey("srv-uuid")).Result(); got != "online" {
		t.Errorf("published desired state = %q, want online", got)
	}
	// R30-01: what the status watcher puts back if the node refuses it.
	var undo services.SwitchUndo
	raw, _ := rdb.Get(ctx, services.SwitchUndoKey("srv-uuid")).Result()
	if json.Unmarshal([]byte(raw), &undo) != nil || undo.Target != "lobby" || undo.Previous != "main" {
		t.Errorf("switch undo record = %q, want target lobby, previous main", raw)
	}
}
