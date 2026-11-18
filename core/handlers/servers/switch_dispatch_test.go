package servers

import (
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
			h, fs, _ := newInstallTest(t)
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
			if len(fs.setRconNeedsRestartCalls) != 0 {
				t.Fatalf("rcon needs-restart written %v for a switch that never happened", fs.setRconNeedsRestartCalls)
			}
		})
	}
}
