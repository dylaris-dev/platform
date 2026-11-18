package servers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// TestStripStorageServerUUIDs proves the co-tenant identifiers are removed from
// what /servers/{id}/storage-path returns, while everything the picker renders
// (path, capacity, server_count) survives. The input is the real heartbeat
// shape: node/storage.go's StorageInfo, one entry per configured path.
func TestStripStorageServerUUIDs(t *testing.T) {
	raw := `[
		{"path":"/mnt/ssd","total_bytes":100,"free_bytes":40,"used_bytes":60,"server_count":2,
		 "server_uuids":["aaaa-1111","bbbb-2222"],"quota_enforceable":true},
		{"path":"/mnt/hdd","total_bytes":900,"free_bytes":800,"used_bytes":100,"server_count":1,
		 "server_uuids":["cccc-3333"],"quota_enforceable":false}
	]`
	var decoded interface{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("fixture does not decode: %v", err)
	}

	out, err := json.Marshal(stripStorageServerUUIDs(decoded))
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	var entries []map[string]interface{}
	if err := json.Unmarshal(out, &entries); err != nil {
		t.Fatalf("result is not a list of objects: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 - stripping must not drop paths", len(entries))
	}
	for _, e := range entries {
		if _, present := e["server_uuids"]; present {
			t.Errorf("entry %v still carries server_uuids", e["path"])
		}
		// The fields the migrate picker actually reads must survive.
		for _, k := range []string{"path", "total_bytes", "free_bytes", "used_bytes", "server_count"} {
			if _, present := e[k]; !present {
				t.Errorf("entry %v lost %q", e["path"], k)
			}
		}
	}
}

// TestStripStorageServerUUIDsPassesThroughUnexpectedShapes: the payload is
// whatever the node sent, so a shape this function does not recognize must come
// back unchanged rather than becoming nil - the endpoint would otherwise report
// "no storage paths" for a node whose heartbeat format drifted.
func TestStripStorageServerUUIDsPassesThroughUnexpectedShapes(t *testing.T) {
	for _, in := range []interface{}{
		nil,
		"a string",
		map[string]interface{}{"path": "/mnt/ssd"},
		[]interface{}{"not an object", 42.0},
	} {
		got := stripStorageServerUUIDs(in)
		a, _ := json.Marshal(in)
		b, _ := json.Marshal(got)
		if string(a) != string(b) {
			t.Errorf("input %s came back as %s", a, b)
		}
	}
}

// L31: a storage move under a running install or a move between nodes pulled
// the tree out from under it. A move to another node staged its archive under
// the old path, and its abort then looked under the new one and left the
// full-size zip behind. UpdateServerResources refuses both; so does this.
//
// V11: a restore runs under status "restarting" and holds node_busy; the move
// went through and, refused on the node, dropped the restore's interlock.
func TestMigrateServerStorage_RefusedWhileInstallingOrMigrating(t *testing.T) {
	for _, tc := range []struct {
		status   string
		nodeBusy string
		refused  bool
	}{
		{"installing", "", true},
		{"migrating", "", true},
		{"stopped", "", false},
		{"restarting", "restarting", true},
	} {
		t.Run(tc.status, func(t *testing.T) {
			fake := &resourcesFakeStore{server: resourcesServer(tc.status)}
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			if tc.nodeBusy != "" {
				mr.Set(services.NodeBusyKey("srv-1"), tc.nodeBusy)
			}
			h := &ServerHandler{d: testDeps(Deps{Store: fake, Redis: rdb})}

			r := httptest.NewRequest(http.MethodPost, "/api/servers/1/migrate-storage", bytes.NewReader([]byte(`{"targetPath":"/mnt/b"}`)))
			r = mux.SetURLVars(r, map[string]string{"id": "1"})
			ctx := context.WithValue(r.Context(), "isAdmin", true)
			ctx = context.WithValue(ctx, "userID", "admin-1")
			rec := httptest.NewRecorder()
			h.MigrateServerStorage(rec, r.WithContext(ctx))

			if (rec.Code == http.StatusConflict) != tc.refused {
				t.Fatalf("status = %d, want refused %v: %s", rec.Code, tc.refused, rec.Body.String())
			}
		})
	}
}

// C31: the storage path of a platform node is the operator's choice. Create
// passes one on only for an admin or the node's own owner; the later move and
// the path list let any server.settings.write holder pick any disk.
func TestStoragePathChoiceIsForWhoeverRunsTheNode(t *testing.T) {
	tenant := "tenant-1"
	for _, tc := range []struct {
		name    string
		isAdmin bool
		owner   *string
		allowed bool
	}{
		{"admin on a platform node", true, nil, true},
		{"tenant on a platform node", false, nil, false},
		{"tenant on someone else's node", false, strPtr("other"), false},
		{"tenant on their own node", false, &tenant, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &storagePathStore{resourcesFakeStore{server: resourcesServer("stopped"), node: models.Node{OwnerID: tc.owner}}}
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			h := &ServerHandler{d: testDeps(Deps{Store: fake, Redis: rdb, Queue: services.NewQueueService(rdb)})}
			req := func(method, route, body string) *http.Request {
				r := httptest.NewRequest(method, "/api/servers/1/"+route, bytes.NewReader([]byte(body)))
				r = mux.SetURLVars(r, map[string]string{"id": "1"})
				ctx := context.WithValue(r.Context(), "isAdmin", tc.isAdmin)
				ctx = context.WithValue(ctx, "userID", tenant)
				return r.WithContext(ctx)
			}

			rec := httptest.NewRecorder()
			h.MigrateServerStorage(rec, req(http.MethodPost, "migrate-storage", `{"targetPath":"/mnt/b"}`))
			if (rec.Code == http.StatusOK) != tc.allowed || (!tc.allowed && rec.Code != http.StatusForbidden) {
				t.Fatalf("migrate: status %d, allowed %v: %s", rec.Code, tc.allowed, rec.Body.String())
			}
			rec = httptest.NewRecorder()
			h.GetServerStoragePath(rec, req(http.MethodGet, "storage-path", ""))
			if (rec.Code == http.StatusOK) != tc.allowed || (!tc.allowed && rec.Code != http.StatusForbidden) {
				t.Fatalf("path list: status %d, allowed %v: %s", rec.Code, tc.allowed, rec.Body.String())
			}
		})
	}
}

type storagePathStore struct{ resourcesFakeStore }

func (f *storagePathStore) GetUserBilling(string) (*store.UserBilling, error) { return nil, nil }

func strPtr(s string) *string { return &s }
