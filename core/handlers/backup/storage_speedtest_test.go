package backup

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"

	"github.com/gorilla/mux"
)

type speedTestFakeStore struct {
	ownStorageFakeStore
	nodes   map[int]*models.Node
	servers []models.Server
}

func (f *speedTestFakeStore) ListNodesByOwner(owner string) ([]models.Node, error) {
	var out []models.Node
	for _, n := range f.nodes {
		if n.OwnerID != nil && *n.OwnerID == owner {
			out = append(out, *n)
		}
	}
	return out, nil
}

func (f *speedTestFakeStore) ListServersByOwner(string) ([]models.Server, error) {
	return f.servers, nil
}

func (f *speedTestFakeStore) GetNodeByID(id int) (*models.Node, error) { return f.nodes[id], nil }

func ptr(s string) *string { return &s }

func speedTestStore() *speedTestFakeStore {
	return &speedTestFakeStore{
		ownStorageFakeStore: ownStorageFakeStore{byID: map[int]*models.BackupStorage{
			5: {ID: 5, Name: "mine", Provider: "s3", OwnerID: ptr("u1")},
		}},
		nodes: map[int]*models.Node{
			1: {ID: 1, Name: "platform-a"},
			2: {ID: 2, Name: "platform-b"},
			3: {ID: 3, Name: "my-home", OwnerID: ptr("u1")},
			4: {ID: 4, Name: "someone-else", OwnerID: ptr("u2")},
		},
		// u1 has servers on node 1 (twice) and on their own node 3.
		servers: []models.Server{{NodeID: 1}, {NodeID: 1}, {NodeID: 3}},
	}
}

// A tenant tests from where their backups leave: their own machine and the
// nodes their servers run on - never a stranger's machine or an unrelated node.
func TestTenantSpeedTestNodes(t *testing.T) {
	h := &BackupHandler{d: testDeps(Deps{Store: speedTestStore()})}
	nodes, err := h.tenantTestNodes(asUser(httptest.NewRequest("GET", "/", nil), "u1"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]bool{}
	for _, n := range nodes {
		if got[n.ID] {
			t.Fatalf("node %d listed twice", n.ID)
		}
		got[n.ID] = true
	}
	if len(got) != 2 || !got[1] || !got[3] {
		t.Fatalf("nodes = %v, want 1 and 3", got)
	}
}

func TestSpeedTestRefusesNodesOutsideTheCallersScope(t *testing.T) {
	st := speedTestStore()
	st.byID[6] = &models.BackupStorage{ID: 6, Name: "platform", Provider: "s3"}
	h := &BackupHandler{d: testDeps(Deps{Store: st})}
	call := func(handler http.HandlerFunc, storageID string, body string) int {
		r := httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
		r = mux.SetURLVars(asUser(r, "u1"), map[string]string{"id": storageID})
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Code
	}
	tests := []struct {
		name    string
		handler http.HandlerFunc
		storage string
		body    string
		want    int
	}{
		{"tenant: a stranger's machine", h.SpeedTestOwnStorage, "5", `{"nodeId":4,"sizeMb":64}`, http.StatusNotFound},
		{"tenant: a node without their servers", h.SpeedTestOwnStorage, "5", `{"nodeId":2,"sizeMb":64}`, http.StatusNotFound},
		{"operator: a customer's machine", h.SpeedTestStorage, "6", `{"nodeId":3,"sizeMb":64}`, http.StatusNotFound},
		{"size not offered", h.SpeedTestOwnStorage, "5", `{"nodeId":1,"sizeMb":1000}`, http.StatusBadRequest},
		// Allowed, but no gRPC registry in the test: offline, not a test run.
		{"tenant: their server's node", h.SpeedTestOwnStorage, "5", `{"nodeId":1,"sizeMb":64}`, http.StatusConflict},
		{"operator: a platform node", h.SpeedTestStorage, "6", `{"nodeId":2,"sizeMb":256}`, http.StatusConflict},
	}
	for _, tt := range tests {
		if got := call(tt.handler, tt.storage, tt.body); got != tt.want {
			t.Errorf("%s: status %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestSpeedTestMbit(t *testing.T) {
	for _, c := range []struct{ bytes, ms, want int64 }{
		{64 << 20, 5000, 107}, // 64 MiB in 5 s
		{512 << 20, 4294, 1000},
		{10, 0, 0},
	} {
		if got := speedTestMbit(c.bytes, c.ms); got != c.want {
			t.Errorf("speedTestMbit(%d, %d) = %d, want %d", c.bytes, c.ms, got, c.want)
		}
	}
}

func TestUploadCapValidation(t *testing.T) {
	v := func(n int64) *int64 { return &n }
	for _, c := range []struct {
		cap *int64
		ok  bool
	}{{nil, true}, {v(1), true}, {v(maxUploadCapMbit), true}, {v(0), false}, {v(-5), false}, {v(maxUploadCapMbit + 1), false}} {
		err := validateBackupStorageEndpoint(models.BackupStorage{Provider: "shared", UploadCapMbit: c.cap})
		if (err == nil) != c.ok {
			t.Errorf("cap %v: err = %v, want ok=%v", c.cap, err, c.ok)
		}
	}
}
