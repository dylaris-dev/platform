package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/services"
	"dylaris-core/store"
	pb "dylaris-proto/node"
)

// What counts as a sub-server in a listing of a server's root. The count is the
// whole cap, so a file counted as a directory would make the limit bite early
// and a directory missed would let it be passed.
func TestCountSubServerDirs(t *testing.T) {
	cases := []struct {
		name    string
		entries []*pb.FileInfo
		want    int
	}{
		{"nothing", nil, 0},
		{
			"directories count",
			[]*pb.FileInfo{{Name: "main", IsDir: true}, {Name: "creative", IsDir: true}},
			2,
		},
		{
			// .active_server lives next to the sub-servers and is not one.
			"files do not count",
			[]*pb.FileInfo{{Name: "main", IsDir: true}, {Name: ".active_server"}, {Name: "notes.txt"}},
			1,
		},
		{
			"a hidden directory is not a sub-server",
			[]*pb.FileInfo{{Name: "main", IsDir: true}, {Name: ".cache", IsDir: true}},
			1,
		},
		{
			"blank and nil entries are ignored rather than counted",
			[]*pb.FileInfo{nil, {Name: "   ", IsDir: true}, {Name: "main", IsDir: true}},
			1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := countSubServerDirs(c.entries); got != c.want {
				t.Errorf("countSubServerDirs = %d, want %d", got, c.want)
			}
		})
	}
}

// The decision on counts alone, including the two ends of the platform's limit
// convention: nil is no cap, 0 is none at all.
func TestRefuseIfOverSubServerLimit(t *testing.T) {
	cases := []struct {
		name       string
		limit      *int64
		have       int
		wantRefuse bool
	}{
		{"no cap lets anything through", nil, 99, false},
		{"under the cap", services.LimitPtr(3), 2, false},
		{"AT the cap refuses the next one", services.LimitPtr(3), 3, true},
		{"over the cap refuses", services.LimitPtr(3), 7, true},
		{"a cap of none refuses the first", services.LimitPtr(0), 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			got := refuseIfOverSubServerLimit(rec, c.limit, c.have)
			if got != c.wantRefuse {
				t.Fatalf("refused = %v, want %v", got, c.wantRefuse)
			}
			if !c.wantRefuse {
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			var out struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if out.Message == "" {
				t.Error("a refusal with no message tells the operator nothing")
			}
		})
	}
}

// A cap that cannot be evaluated must not quietly pass. The count comes from
// the node, and the version this replaced read a Redis cache instead - a cache
// a stopped server does not write, so the cap silently lapsed exactly when
// sub-servers get added.
func TestSubServerLimitRefusesWhenTheNodeCannotBeAsked(t *testing.T) {
	st := &AppState{Store: &subServerLimitStore{limit: "3"}}
	rec := httptest.NewRecorder()
	// No GRPCRegistry on the state, so the listing cannot be made.
	if !refuseIfSubServerLimitReached(rec, st, 7, "srv-uuid") {
		t.Fatal("an unanswerable count was treated as room to spare")
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 - the node is what did not answer", rec.Code)
	}
}

// With no cap configured the node is not asked at all: an unlimited platform
// must not fail an install because a node was briefly busy.
func TestSubServerLimitDoesNotAskTheNodeWhenThereIsNoCap(t *testing.T) {
	st := &AppState{Store: &subServerLimitStore{limit: "unlimited"}}
	rec := httptest.NewRecorder()
	if refuseIfSubServerLimitReached(rec, st, 7, "srv-uuid") {
		t.Fatalf("refused although no cap is set: %s", rec.Body.String())
	}
}

// subServerLimitStore answers only the one setting this cap reads.
type subServerLimitStore struct {
	store.Store
	limit string
}

func (f *subServerLimitStore) GetSetting(key string) (string, error) {
	if key == SettingMaxSubServers {
		return f.limit, nil
	}
	return "", nil
}
