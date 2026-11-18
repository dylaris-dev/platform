package nodes

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
)

// Y10: a node that skipped a storage path at boot (its disk did not mount)
// lists none of that path's servers, and every one of them showed as
// "Missing from disk" with a "Remove from DB" button. An old node sends no
// storageUnavailable, and the missing list stays what it was.
func TestDiskAnalysisListsNoServerMissingWhileStorageIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name        string
		heartbeat   string
		wantMissing int
	}{
		{"old node, every path listed", `{"storage":[{"path":"/a","server_uuids":["on-a"]}]}`, 1},
		{"a path unavailable", `{"storage":[{"path":"/a","server_uuids":["on-a"]}],"storageUnavailable":["/b"]}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			mr.Set("dylaris:discovery:t7", tc.heartbeat)
			fs := &nodeDeleteStore{
				node:    &models.Node{ID: 7, Token: "t7"},
				servers: []models.Server{{ID: 1, UUID: "on-a"}, {ID: 2, UUID: "on-b"}},
			}
			h := &NodeHandler{d: testDeps(Deps{Store: fs, Redis: rdb})}
			rw := httptest.NewRecorder()
			h.GetDiskAnalysis(rw, handlertest.VisAdminRequest("GET", "/x", map[string]string{"id": "7"}))
			var got struct {
				Missing            []struct{ UUID string } `json:"missing"`
				StorageUnavailable []string                `json:"storageUnavailable"`
			}
			if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", rw.Body.String(), err)
			}
			if len(got.Missing) != tc.wantMissing {
				t.Fatalf("missing = %+v, want %d", got.Missing, tc.wantMissing)
			}
			if wantUnavail := tc.wantMissing == 0; (len(got.StorageUnavailable) > 0) != wantUnavail {
				t.Fatalf("storageUnavailable = %v", got.StorageUnavailable)
			}
		})
	}
}
