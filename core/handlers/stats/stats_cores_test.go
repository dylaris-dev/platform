package stats

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

type coresFakeStore struct {
	store.Store
	servers []models.Server
}

func (f *coresFakeStore) GetServerByID(id int) (*models.Server, error) {
	for _, s := range f.servers {
		if s.ID == id {
			return &s, nil
		}
	}
	return nil, errors.New("not found")
}
func (f *coresFakeStore) GetNodeByID(id int) (*models.Node, error) {
	return &models.Node{ID: id, Token: "tok"}, nil
}
func (f *coresFakeStore) ListServerPinningByNode(int) ([]models.Server, error) { return f.servers, nil }

// The per-core load is the HOST's, so it is the server's own only while no
// other server can run on those cores. A member must not be shown a
// neighbour's load as theirs, nor the core ids their cpuset is redacted to hide.
func TestServerCoresShowOnlyWhatIsTheReadersOwn(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Set("dylaris:node:tok:cpuload", `{"cores":{"0":10,"1":20,"2":30,"3":40,"4":50}}`)

	own := models.Server{ID: 1, NodeID: 7, CPUPinningMode: "manual", Cpuset: "0-1"}
	neighbour := models.Server{ID: 2, NodeID: 7, CPUPinningMode: "auto", Cpuset: "2-3"}
	overlapping := models.Server{ID: 2, NodeID: 7, CPUPinningMode: "auto", Cpuset: "1-2"}

	type reply struct {
		Cores []struct {
			Index  int     `json:"index"`
			ID     *int    `json:"id"`
			Load   float64 `json:"load"`
			Shared bool    `json:"shared"`
		} `json:"cores"`
		Exclusive bool `json:"exclusive"`
	}
	get := func(servers []models.Server, admin bool) reply {
		h := NewStatsHandler(&handlers.AppState{Store: &coresFakeStore{servers: servers}, Redis: rdb})
		req := httptest.NewRequest("GET", "/api/servers/1/stats/cores", nil)
		req = mux.SetURLVars(req, map[string]string{"id": "1"})
		req = req.WithContext(context.WithValue(req.Context(), "isAdmin", admin))
		rec := httptest.NewRecorder()
		h.GetCores(rec, req)
		var out reply
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		return out
	}

	got := get([]models.Server{own, neighbour}, false)
	if !got.Exclusive || len(got.Cores) != 2 || got.Cores[0].Load != 10 || got.Cores[1].Load != 20 {
		t.Fatalf("member, exclusive cores: %+v, want cores 0 and 1 at 10 and 20", got)
	}
	if got.Cores[0].ID != nil || got.Cores[0].Index != 1 {
		t.Fatalf("member was given core id %v; they get a position only", got.Cores[0].ID)
	}

	got = get([]models.Server{own, overlapping}, false)
	if got.Exclusive || len(got.Cores) != 0 {
		t.Fatalf("member, a neighbour on core 1: %+v, want nothing shown", got)
	}

	got = get([]models.Server{own, overlapping}, true)
	if len(got.Cores) != 2 || got.Cores[1].ID == nil || *got.Cores[1].ID != 1 || !got.Cores[1].Shared || got.Cores[0].Shared {
		t.Fatalf("admin, a neighbour on core 1: %+v, want both cores with core 1 marked shared", got)
	}

	// A core missing from the published load leaves no gap in the positions.
	mr.Set("dylaris:node:tok:cpuload", `{"cores":{"1":20}}`)
	got = get([]models.Server{own, neighbour}, false)
	if len(got.Cores) != 1 || got.Cores[0].Index != 1 {
		t.Fatalf("with core 0 unreported: %+v, want one core at position 1", got)
	}
}
