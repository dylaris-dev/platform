package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type playersFakeStore struct {
	store.Store
	listed []models.Server
}

func (f *playersFakeStore) ListServersForUser(string, bool) ([]models.Server, error) {
	return f.listed, nil
}

// The edges count players for every server; a caller is told only about the
// servers their own list shows.
func TestGatewayPlayersOnlyForTheCallersServers(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	rdb.Set(ctx, "edge:registry:e1", `{"edge_id":"e1"}`, 0)
	rdb.XAdd(ctx, &redis.XAddArgs{Stream: "dylaris:edge:e1:stats", Values: map[string]any{
		"data": `{"players_by_server":{"uuid-mine":3,"uuid-theirs":7}}`,
	}})

	h := NewServerHandler(&AppState{
		Store: &playersFakeStore{listed: []models.Server{{ID: 5, UUID: "uuid-mine"}, {ID: 6, UUID: "uuid-empty"}}},
		Redis: rdb,
	})
	req := httptest.NewRequest("GET", "/api/servers/players", nil)
	req = req.WithContext(context.WithValue(req.Context(), "userID", "u1"))
	rec := httptest.NewRecorder()
	h.GetGatewayPlayers(rec, req)
	var out struct {
		Players map[string]int `json:"players"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if len(out.Players) != 1 || out.Players["5"] != 3 {
		t.Fatalf("got %v, want only server 5 with 3 (server 6 has nobody, the other server is not the caller's)", out.Players)
	}
}
