package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

type consoleHistoryFakeStore struct{ store.Store }

func (consoleHistoryFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-uuid"}, nil
}

type consoleHistoryResp struct {
	Lines []string `json:"lines"`
	IDs   []string `json:"ids"`
	More  bool     `json:"more"`
}

// seedConsole writes lines 1..n with IDs 1-0..n-0, so a line's number is its ID.
func seedConsole(t *testing.T, rdb *redis.Client, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if err := rdb.XAdd(context.Background(), &redis.XAddArgs{
			Stream: "dylaris:server:srv-uuid:logs",
			ID:     fmt.Sprintf("%d-0", i),
			Values: map[string]interface{}{"line": fmt.Sprintf("line %d", i)},
		}).Err(); err != nil {
			t.Fatal(err)
		}
	}
}

func getConsoleHistory(t *testing.T, h *ConsoleHandler, query string) (int, consoleHistoryResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/servers/7/console/history"+query, nil)
	req = mux.SetURLVars(req, map[string]string{"id": "7"})
	rw := httptest.NewRecorder()
	h.GetHistory(rw, req)
	var out consoleHistoryResp
	if rw.Code == http.StatusOK {
		if err := json.Unmarshal(rw.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rw.Body.String())
		}
	}
	return rw.Code, out
}

func TestConsoleHistoryPaging(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	seedConsole(t, rdb, 1500)
	h := NewConsoleHandler(&AppState{Store: consoleHistoryFakeStore{}, Redis: rdb})

	tests := []struct {
		name                string
		query               string
		wantFirst, wantLast int // line numbers; 0 means no lines
		wantLen             int
		wantMore            bool
	}{
		{"default is the newest 1000", "", 501, 1500, 1000, true},
		{"count caps at 1000", "?count=5000", 501, 1500, 1000, true},
		{"count picks the newest n", "?count=3", 1498, 1500, 3, true},
		{"before is exclusive", "?before=501-0&count=3", 498, 500, 3, true},
		{"before pages to the start", "?before=501-0", 1, 500, 500, false},
		{"exactly count left is not more", "?before=4-0&count=3", 1, 3, 3, false},
		{"before a trimmed id still pages", "?before=0-5&count=3", 0, 0, 0, false},
		{"before between ids", "?before=10-5&count=2", 9, 10, 2, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, got := getConsoleHistory(t, h, tc.query)
			if code != http.StatusOK {
				t.Fatalf("status %d", code)
			}
			if len(got.Lines) != tc.wantLen || len(got.IDs) != tc.wantLen {
				t.Fatalf("got %d lines / %d ids, want %d", len(got.Lines), len(got.IDs), tc.wantLen)
			}
			if got.More != tc.wantMore {
				t.Errorf("more = %v, want %v", got.More, tc.wantMore)
			}
			if tc.wantLen == 0 {
				return
			}
			if want := fmt.Sprintf("line %d", tc.wantFirst); got.Lines[0] != want {
				t.Errorf("first line %q, want %q", got.Lines[0], want)
			}
			if want := fmt.Sprintf("line %d", tc.wantLast); got.Lines[len(got.Lines)-1] != want {
				t.Errorf("last line %q, want %q", got.Lines[len(got.Lines)-1], want)
			}
			if want := fmt.Sprintf("%d-0", tc.wantFirst); got.IDs[0] != want {
				t.Errorf("first id %q, want %q", got.IDs[0], want)
			}
		})
	}
}

func TestConsoleHistoryRejectsBadParams(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	h := NewConsoleHandler(&AppState{Store: consoleHistoryFakeStore{}, Redis: rdb})

	for _, q := range []string{
		"?before=abc", "?before=-", "?before=%2B", "?before=(5-0", "?before=5",
		"?count=0", "?count=-1", "?count=x",
	} {
		if code, _ := getConsoleHistory(t, h, q); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, code)
		}
	}
}
