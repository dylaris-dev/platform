package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
	"dylaris-pkg/queue"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type modResultFakeStore struct {
	store.Store
	applied bool
	writes  int
}

func (f *modResultFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, NodeID: 3}, nil
}

func (f *modResultFakeStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id, Token: "node-a"}, nil
}

func (f *modResultFakeStore) SetServerModStatus(serverID int, sub, projectID, installID, status, message string) (bool, error) {
	f.writes++
	return f.applied, nil
}

// The panel re-reads a server's mod list only on server_mods.changed, so a
// report that moved the row without publishing left the install spinning until
// a reload. Exactly one event per applied report, none for a late or foreign one.
func TestModInstallResultPublishesOnceWhenApplied(t *testing.T) {
	tests := []struct {
		name       string
		channel    string
		applied    bool
		wantEvents int
	}{
		{name: "applied report", channel: queue.ModResultsChannel("node-a"), applied: true, wantEvents: 1},
		{name: "late report about a superseded attempt", channel: queue.ModResultsChannel("node-a"), applied: false, wantEvents: 0},
		{name: "report from a node that does not host the server", channel: queue.ModResultsChannel("node-b"), applied: true, wantEvents: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
			sub := rdb.Subscribe(context.Background(), SystemEventsChannel)
			defer sub.Close()
			if _, err := sub.Receive(context.Background()); err != nil {
				t.Fatal(err)
			}

			fs := &modResultFakeStore{applied: tt.applied}
			svc := NewModInstallResultService(fs, rdb, nil)
			svc.apply(tt.channel, modInstallReport{
				InstallID: "attempt-1", ServerID: 7, SubServer: "survival", ProjectID: "spark",
				Status: models.ServerModFailed, Message: "download failed: upstream status 404",
			})

			var got []SystemEvent
			deadline := time.After(200 * time.Millisecond)
		collect:
			for {
				select {
				case msg := <-sub.Channel():
					var evt SystemEvent
					if err := json.Unmarshal([]byte(msg.Payload), &evt); err != nil {
						t.Fatal(err)
					}
					got = append(got, evt)
				case <-deadline:
					break collect
				}
			}
			if len(got) != tt.wantEvents {
				t.Fatalf("events = %d, want %d (%+v)", len(got), tt.wantEvents, got)
			}
			if tt.wantEvents == 1 {
				evt := got[0]
				if evt.Type != "server_mods.changed" || evt.Payload["serverId"] != float64(7) ||
					evt.Payload["projectId"] != "spark" || evt.Payload["status"] != models.ServerModFailed {
					t.Errorf("event = %+v", evt)
				}
			}
		})
	}
}
