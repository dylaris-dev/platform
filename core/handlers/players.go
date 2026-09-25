package handlers

// Player management as its own surface, gated on players.read / players.manage.
//
// Those two capabilities were in the catalog and in four presets, and enforced
// by nothing: grep found no route, no RequireCap and no tab bit. The Players
// page ran on rcon.exec plus files.read instead, which meant delegating "ban
// someone" required handing out the whole filesystem AND every RCON command
// there is - verified live that a member with rcon.exec and no power capability
// is refused POST /power and can then run `save-off` and `stop` through RCON.
//
// So the two things the page needs get their own doors:
//
//   GET  .../players/lists   players.read    - the three JSON files MC keeps
//   GET  .../players/online  players.read    - RCON `list`
//   POST .../players/action  players.manage  - one command from a fixed set
//
// The action route takes an ACTION, never a command, and builds the command
// here. That is the whole point: a moderator gets exactly these eight verbs
// against one named player, not a shell into the server. It mirrors
// /servers/{id}/power, which takes an action for the same reason.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
)

type PlayersHandler struct {
	state *AppState
	rcon  *RconHandler
}

func NewPlayersHandler(state *AppState) *PlayersHandler {
	return &PlayersHandler{state: state, rcon: NewRconHandler(state)}
}

// playerListFiles maps a response field to the file MC keeps that list in,
// relative to the active sub-server directory.
var playerListFiles = []struct {
	key      string
	filename string
}{
	{"bans", "banned-players.json"},
	{"whitelist", "whitelist.json"},
	{"ops", "ops.json"},
}

type playerListsResponse struct {
	Success   bool            `json:"success"`
	Bans      json.RawMessage `json:"bans"`
	Whitelist json.RawMessage `json:"whitelist"`
	Ops       json.RawMessage `json:"ops"`
	// Unavailable names each list that could NOT be read, and why. An empty
	// list and an unreadable one are different facts and the panel has to be
	// able to tell them apart - rendering "could not read" as "nobody is
	// banned" is the exact bug this endpoint was built to stop repeating.
	Unavailable map[string]string `json:"unavailable,omitempty"`
}

// emptyJSONArray is what a list reads as when MC has not written the file yet.
var emptyJSONArray = json.RawMessage("[]")

// GetLists GET /api/servers/{id}/players/lists - bans, whitelist and operators
// straight out of the active sub-server's JSON files.
func (h *PlayersHandler) GetLists(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil || srv == nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	out := playerListsResponse{
		Success: true, Bans: emptyJSONArray, Whitelist: emptyJSONArray, Ops: emptyJSONArray,
	}
	if srv.ActiveSubServer == "" {
		// Nothing installed yet: three empty lists is the truth, not a failure.
		json.NewEncoder(w).Encode(out)
		return
	}
	targets := map[string]*json.RawMessage{
		"bans": &out.Bans, "whitelist": &out.Whitelist, "ops": &out.Ops,
	}
	for _, f := range playerListFiles {
		raw, ferr := h.readPlayerList(srv.NodeID, srv.UUID, srv.ActiveSubServer, f.filename)
		if ferr != nil {
			if out.Unavailable == nil {
				out.Unavailable = map[string]string{}
			}
			out.Unavailable[f.key] = ferr.Error()
			continue
		}
		*targets[f.key] = raw
	}
	json.NewEncoder(w).Encode(out)
}

// readPlayerList reads one list file off the node and hands back its JSON
// unchanged. A file MC has not created yet is an empty list, not an error -
// a server nobody has banned on has no banned-players.json.
func (h *PlayersHandler) readPlayerList(nodeID int, serverUUID, subServer, filename string) (json.RawMessage, error) {
	content, found, err := h.rcon.readNodeFileString(nodeID, serverUUID, subServer+"/"+filename)
	if err != nil {
		return nil, fmt.Errorf("could not be read from the node: %w", err)
	}
	if !found || strings.TrimSpace(content) == "" {
		return emptyJSONArray, nil
	}
	// Passed through as-is so MC owns the entry shape (uuid, name, reason,
	// created, source, expires, level, ...) and this never has to track it.
	// Validated as an ARRAY first: relaying a half-written file would put a
	// parse error in the browser instead of here, where it can be named.
	var probe []json.RawMessage
	if jerr := json.Unmarshal([]byte(content), &probe); jerr != nil {
		return nil, fmt.Errorf("is not a valid JSON list - the server may be mid-write")
	}
	return json.RawMessage(content), nil
}

// GetOnline GET /api/servers/{id}/players/online - RCON `list`, so the roster
// needs players.read rather than the right to run any command.
func (h *PlayersHandler) GetOnline(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil || srv == nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	resp := h.rcon.execAgainstServer(r.Context(), srv.ID, srv.UUID, srv.NodeID, rconRequest{Command: "list"})
	writeRconResponse(w, resp)
}

type playerActionRequest struct {
	Action  string `json:"action"`
	Player  string `json:"player"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Action POST /api/servers/{id}/players/action - one of a fixed set of player
// commands. The caller never supplies a command string.
func (h *PlayersHandler) Action(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil || srv == nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req playerActionRequest
	if derr := json.NewDecoder(r.Body).Decode(&req); derr != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	cmd, cerr := buildPlayerCommand(req.Action, req.Player, req.Reason, req.Message)
	if cerr != nil {
		sendJSONError(w, cerr.Error(), http.StatusBadRequest)
		return
	}
	resp := h.rcon.execAgainstServer(r.Context(), srv.ID, srv.UUID, srv.NodeID, rconRequest{Command: cmd})
	writeRconResponse(w, resp)
}

// playerActionVerbs is the allowlist: action -> the MC command it becomes.
// Adding an entry here is the ONLY way to widen what players.manage can do.
var playerActionVerbs = map[string]string{
	"kick":             "kick",
	"ban":              "ban",
	"unban":            "pardon",
	"op":               "op",
	"deop":             "deop",
	"whitelist_add":    "whitelist add",
	"whitelist_remove": "whitelist remove",
	"tell":             "tell",
}

// maxPlayerFreeText caps a ban reason or a whispered message. MC truncates
// long input on its own; this is about keeping the command line sane.
const maxPlayerFreeText = 200

// buildPlayerCommand turns an action plus a player into the exact RCON command,
// or refuses. Pulled out as a pure function because it is the security boundary
// of this endpoint: everything a players.manage holder can make the server do
// is decided here.
//
// The player name is checked rather than escaped - RCON has no quoting, so a
// name with a space in it silently changes what the command means ("ban Foo
// Bar" bans Foo for reason "Bar"), and a control character could break the
// command line outright. Free text is allowed spaces, since a ban reason is
// prose, but never control characters.
func buildPlayerCommand(action, player, reason, message string) (string, error) {
	verb, ok := playerActionVerbs[strings.TrimSpace(strings.ToLower(action))]
	if !ok {
		return "", fmt.Errorf("unknown player action")
	}
	player = strings.TrimSpace(player)
	if !isPlayerName(player) {
		return "", fmt.Errorf("invalid player name")
	}
	switch verb {
	case "tell":
		msg := sanitizePlayerFreeText(message)
		if msg == "" {
			return "", fmt.Errorf("message required")
		}
		return "tell " + player + " " + msg, nil
	case "kick", "ban":
		if why := sanitizePlayerFreeText(reason); why != "" {
			return verb + " " + player + " " + why, nil
		}
		return verb + " " + player, nil
	default:
		return verb + " " + player, nil
	}
}

// isPlayerName accepts a Java username (3-16 of [A-Za-z0-9_]) plus the leading
// marker Geyser/Floodgate gives a Bedrock player, and nothing else. Deliberately
// not a general "no spaces" rule: this string is concatenated into a command
// line, so the narrow set is the one worth allowing.
func isPlayerName(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	body := s
	switch s[0] {
	case '.', '*':
		body = s[1:]
	}
	if len(body) < 1 {
		return false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		default:
			return false
		}
	}
	return true
}

// sanitizePlayerFreeText keeps a reason or message printable and on one line.
func sanitizePlayerFreeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > maxPlayerFreeText {
		out = strings.TrimSpace(out[:maxPlayerFreeText])
	}
	return out
}
