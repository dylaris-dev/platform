package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

func TestConsoleResumeID(t *testing.T) {
	tests := []struct {
		name, header, query string
		want                string
		wantOK              bool
	}{
		{"nothing is live only", "", "", "$", true},
		{"header resumes", "12-3", "", "12-3", true},
		{"after resumes", "", "?after=12-3", "12-3", true},
		{"header beats a stale after", "20-0", "?after=12-3", "20-0", true},
		{"malformed header is ignored", "x", "", "$", true},
		{"malformed header falls back to after", "x", "?after=5-0", "5-0", true},
		{"malformed after is refused", "", "?after=$", "", false},
		{"after must be a stream id", "", "?after=12", "", false},
		{"after cannot name another key", "", "?after=1-0%20dylaris:x", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/servers/7/console/stream"+tt.query, nil)
			if tt.header != "" {
				r.Header.Set("Last-Event-ID", tt.header)
			}
			got, ok := consoleResumeID(r)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestCompareStreamIDs(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1-0", "1-0", 0},
		{"2-0", "10-0", -1},
		{"10-0", "9-99", 1},
		{"5-2", "5-10", -1},
		{"05-1", "5-1", 0},
	}
	for _, tt := range tests {
		if got := compareStreamIDs(tt.a, tt.b); got != tt.want {
			t.Errorf("compareStreamIDs(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// streamConsoleFor runs the stream handler until the request deadline and
// returns everything it wrote.
func streamConsoleFor(t *testing.T, h *ConsoleHandler, query, lastEventID string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/servers/7/console/stream"+query, nil).WithContext(ctx)
	req = mux.SetURLVars(req, map[string]string{"id": "7"})
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	rw := httptest.NewRecorder()
	h.StreamConsole(rw, req)
	return rw.Code, rw.Body.String()
}

func TestStreamConsoleResume(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	seedConsole(t, rdb, 10)
	// Retention trimmed lines 1-3.
	if err := rdb.XTrimMaxLen(context.Background(), "dylaris:server:srv-uuid:logs", 7).Err(); err != nil {
		t.Fatal(err)
	}
	h := NewConsoleHandler(&AppState{Store: consoleHistoryFakeStore{}, Redis: rdb})

	tests := []struct {
		name, query, header string
		wantCode            int
		wantFirst           string // first replayed id, "" for none
		wantGap             bool
	}{
		{"fresh stream replays nothing", "", "", http.StatusOK, "", false},
		{"after replays the lines after it", "?after=7-0", "", http.StatusOK, "id: 8-0\n", false},
		{"header replays the lines after it", "", "8-0", http.StatusOK, "id: 9-0\n", false},
		{"oldest retained line is no gap", "?after=4-0", "", http.StatusOK, "id: 5-0\n", false},
		{"trimmed position replays from the oldest with a gap", "?after=2-0", "", http.StatusOK, "id: 4-0\n", true},
		{"malformed after", "?after=abc", "", http.StatusBadRequest, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := streamConsoleFor(t, h, tt.query, tt.header)
			if code != tt.wantCode {
				t.Fatalf("code %d, want %d (%s)", code, tt.wantCode, body)
			}
			if code != http.StatusOK {
				return
			}
			first := strings.Index(body, "id: ")
			switch {
			case tt.wantFirst == "" && first >= 0:
				t.Fatalf("replayed lines on a fresh stream: %q", body)
			case tt.wantFirst != "" && (first < 0 || !strings.HasPrefix(body[first:], tt.wantFirst)):
				t.Fatalf("first replayed line wrong, want %q in %q", tt.wantFirst, body)
			}
			if tt.wantFirst != "" && !strings.Contains(body, "id: 10-0\ndata: line 10\n\n") {
				t.Fatalf("replay did not reach the newest line: %q", body)
			}
			gap := strings.Contains(body, "event: gap\n")
			if gap != tt.wantGap {
				t.Fatalf("gap = %v, want %v: %q", gap, tt.wantGap, body)
			}
			if gap && strings.Index(body, "event: gap") > first {
				t.Fatalf("gap must come before the replay: %q", body)
			}
		})
	}
}

func TestConsoleHistoryTrimmed(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	h := NewConsoleHandler(&AppState{Store: consoleHistoryFakeStore{}, Redis: rdb})

	seedConsole(t, rdb, 3)
	if _, body := getConsoleHistoryRaw(t, h, ""); !strings.Contains(body, `"trimmed":false`) {
		t.Fatalf("short stream should say untrimmed: %s", body)
	}
	for i := 4; i <= consoleRetainedLines; i++ {
		mr.XAdd("dylaris:server:srv-uuid:logs", "*", []string{"line", "x"})
	}
	if _, body := getConsoleHistoryRaw(t, h, "?before=1001-0"); !strings.Contains(body, `"trimmed":true`) {
		t.Fatalf("full stream should say trimmed: %s", body)
	}
	if _, body := getConsoleHistoryRaw(t, h, ""); strings.Contains(body, `"trimmed"`) {
		t.Fatalf("a page with more must not answer trimmed: %s", body)
	}
}

func getConsoleHistoryRaw(t *testing.T, h *ConsoleHandler, query string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/servers/7/console/history"+query, nil)
	req = mux.SetURLVars(req, map[string]string{"id": "7"})
	rw := httptest.NewRecorder()
	h.GetHistory(rw, req)
	return rw.Code, rw.Body.String()
}
