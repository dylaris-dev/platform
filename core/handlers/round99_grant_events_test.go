package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
)

// countServersChanged runs fn with a subscriber on the system-events channel
// and returns how many servers.changed frames it published.
func countServersChanged(t *testing.T, state *AppState, fn func()) int {
	t.Helper()
	rdb := newServerPowerRedis(t)
	// Only Events gets Redis: state.Redis stays nil so the beam stamp, which
	// needs more of the store than these fakes answer, stays out of the way.
	state.Events = services.NewSystemEventsPublisher(rdb)
	ctx := context.Background()
	sub := rdb.Subscribe(ctx, services.SystemEventsChannel)
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	ch := sub.Channel()
	fn()
	n := 0
	for {
		select {
		case msg := <-ch:
			var ev services.SystemEvent
			if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
				t.Fatalf("bad frame %q: %v", msg.Payload, err)
			}
			if ev.Type == "servers.changed" {
				n++
			}
		case <-time.After(200 * time.Millisecond):
			return n
		}
	}
}

// An invitee's panel refetches its server list only on servers.changed, so a
// grant that publishes nothing left them without the server until a reload.
// The SSE stream forwards a payload-less servers.changed to every session
// (system_events_scope_test), which is what makes it reach the invitee.
func TestGrantChangesPublishServersChanged(t *testing.T) {
	serverID := 42
	tests := []struct {
		name   string
		actor  string
		revoke bool
		body   map[string]interface{}
		want   int
	}{
		{"assign on a server", ownerA, false, map[string]interface{}{"username": "friend", "serverId": serverID, "grantCaps": []string{"overview.read"}}, 1},
		{"assign account-wide", ownerA, false, map[string]interface{}{"username": "friend", "grantCaps": []string{"overview.read"}}, 1},
		{"revoke on a server", ownerA, true, map[string]interface{}{"username": "friend", "serverId": serverID}, 1},
		{"refused assign publishes nothing", actorB, false, map[string]interface{}{"username": "friend", "serverId": serverID, "grantCaps": []string{"overview.read"}}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := &grantFakeStore{target: &models.User{ID: friendC, Username: "friend"}, server: serverOwnedBy(ownerA)}
			state := grantState(fs)
			h := NewServerRolesHandler(state)
			rec := httptest.NewRecorder()
			got := countServersChanged(t, state, func() {
				if tc.revoke {
					h.RevokeGrant(rec, grantReq("DELETE", tc.actor, false, tc.body))
				} else {
					h.AssignGrant(rec, grantReq("POST", tc.actor, false, tc.body))
				}
			})
			if tc.want > 0 && rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if got != tc.want {
				t.Fatalf("servers.changed published %d times, want %d", got, tc.want)
			}
		})
	}
}
