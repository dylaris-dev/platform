package console

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
)

// playersCapStore is schedCapStore with RCON switched off, so a request that
// passes every check in Action ends at ExecRcon's 409 instead of a node.
type playersCapStore struct{ schedCapStore }

func (*playersCapStore) GetServerRconConfig(int) (bool, int, string, error) {
	return false, 0, "", nil
}

// `op` hands out in-game operator rights - /stop, /op, /execute - so a
// players.manage holder without rcon.exec got every console power back through
// one allowlisted verb. op and deop now also need rcon.exec.
func TestPlayerOpNeedsRcon(t *testing.T) {
	moderator := []string{"players.read", "players.manage"}
	cases := []struct {
		name   string
		action string
		caps   []string
		want   int
	}{
		{"op without rcon.exec", "op", moderator, http.StatusForbidden},
		{"deop without rcon.exec", "deop", moderator, http.StatusForbidden},
		{"op is matched case-insensitively", " OP ", moderator, http.StatusForbidden},
		{"op with rcon.exec", "op", append(append([]string{}, moderator...), "rcon.exec"), http.StatusConflict},
		{"kick stays players.manage alone", "kick", moderator, http.StatusConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &playersCapStore{schedCapStore{friendCaps: c.caps}}
			h := NewPlayersHandler(Deps{Store: fs, Authz: authz.NewResolver(fs)})
			rec := httptest.NewRecorder()
			h.Action(rec, schedCapReq(http.MethodPost, "/api/servers/1/players/action", "friend-1",
				playerActionRequest{Action: c.action, Player: "Notch"}))
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// The JSON bodies of these handlers were decoded unbounded; one of them,
// /scheduled-tasks/validate, is open to any signed-in account.
func TestConsoleJSONBodiesAreBounded(t *testing.T) {
	// Valid requests padded past the limit by a field nobody reads, so an
	// unbounded decode succeeds and only the bound answers 400.
	pad := `,"pad":"` + strings.Repeat("a", 128<<10) + `"}`
	fs := &playersCapStore{schedCapStore{settings: map[string]string{},
		task: &models.ScheduledTask{ID: 7, ServerID: 1, Name: "t", TaskType: "restart", ScheduleCron: "0 4 * * *"}}}
	sched := schedCapHandler(&fs.schedCapStore)
	players := NewPlayersHandler(Deps{Store: fs, Authz: authz.NewResolver(fs)})
	for name, c := range map[string]struct {
		call func(http.ResponseWriter, *http.Request)
		body string
	}{
		"validate":       {sched.ValidateCron, `{"scheduleCron":"* * * * *"`},
		"create":         {sched.Create, `{"name":"t","taskType":"restart","scheduleCron":"* * * * *"`},
		"update":         {sched.Update, `{"name":"t2"`},
		"players action": {players.Action, `{"action":"kick","player":"Notch"`},
	} {
		t.Run(name, func(t *testing.T) {
			r := schedCapReq(http.MethodPost, "/x", "owner-1", nil)
			r.Body = io.NopCloser(strings.NewReader(c.body + pad))
			call := c.call
			rec := httptest.NewRecorder()
			call(rec, r)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d for a body over the limit: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
