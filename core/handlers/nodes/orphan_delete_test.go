package nodes

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// L11: the orphan delete asked the node to delete path "." of the folder over
// gRPC, and every node refuses the server root as a protected file, so the
// admin always got "Delete failed". It goes through the whole-server delete
// command now, which every node already runs for a deleted server.
func TestDeletingAnOrphanQueuesTheWholeServerDelete(t *testing.T) {
	cases := []struct {
		name     string
		uuid     string
		wantCode int
		queued   bool
	}{
		{"a uuid folder", "b083ff0c-fc16-47c5-9700-7b16486583e7", http.StatusOK, true},
		{"a legacy name the node resolves", "test-server-1", http.StatusOK, true},
		// Shorter than any server id: the node resolves no directory for it and
		// would skip the delete, so success would be a lie.
		{"a name the node cannot resolve", "tmp", http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { rdb.Close() })
			node := &models.Node{ID: 7, Status: "online", Token: "node-tok"}
			d := visState(node, nil, sql.ErrNoRows)
			d.Redis = rdb
			d.Queue = services.NewQueueService(rdb)
			h := &NodeHandler{d: d}

			rw := httptest.NewRecorder()
			h.DeleteOrphanedFolder(rw, handlertest.VisAdminRequest("DELETE", "/x?uuid="+tc.uuid, map[string]string{"id": "7"}))
			if rw.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, tc.wantCode, rw.Body.String())
			}

			msgs, err := rdb.XRange(context.Background(), "dylaris:node:node-tok:cmds", "-", "+").Result()
			if err != nil && tc.queued {
				t.Fatalf("read command stream: %v", err)
			}
			if !tc.queued {
				if len(msgs) != 0 {
					t.Fatalf("queued %d command(s) for a folder the node cannot resolve", len(msgs))
				}
				return
			}
			if len(msgs) != 1 {
				t.Fatalf("queued %d commands, want 1", len(msgs))
			}
			var cmd struct {
				Action string `json:"action"`
				Config struct {
					UUID string `json:"uuid"`
				} `json:"config"`
			}
			if err := json.Unmarshal([]byte(msgs[0].Values["data"].(string)), &cmd); err != nil {
				t.Fatal(err)
			}
			if cmd.Action != "delete" || cmd.Config.UUID != tc.uuid {
				t.Fatalf("queued %+v, want delete of %s", cmd, tc.uuid)
			}
		})
	}
}
