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
// against one named player (plus switching the whitelist on or off), not a shell into the server. It mirrors
// /servers/{id}/power, which takes an action for the same reason.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/authz"
	"dylaris-core/models"

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
	{"known", "usercache.json"},
}

type playerListsResponse struct {
	Success   bool            `json:"success"`
	Bans      json.RawMessage `json:"bans"`
	Whitelist json.RawMessage `json:"whitelist"`
	Ops       json.RawMessage `json:"ops"`
	// Known is usercache.json: everyone the server has resolved a profile for,
	// which in practice is everyone who ever joined. Capped at maxKnownPlayers.
	Known json.RawMessage `json:"known"`
	// The two server.properties switches behind the whitelist. nil means the
	// file could not be read (named in Unavailable as "properties"); a missing
	// file or key is MC's default, false. whitelist.json having entries says
	// nothing about whether it is enforced, and that is exactly what an owner
	// misread live: Dave was on the list while white-list was still false.
	WhitelistEnabled  *bool `json:"whitelistEnabled,omitempty"`
	WhitelistEnforced *bool `json:"whitelistEnforced,omitempty"`
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
		Success: true, Bans: emptyJSONArray, Whitelist: emptyJSONArray, Ops: emptyJSONArray, Known: emptyJSONArray,
	}
	if srv.ActiveSubServer == "" {
		// Nothing installed yet: empty lists are the truth, not a failure.
		json.NewEncoder(w).Encode(out)
		return
	}
	unavailable := func(key string, err error) {
		if out.Unavailable == nil {
			out.Unavailable = map[string]string{}
		}
		out.Unavailable[key] = err.Error()
	}
	targets := map[string]*json.RawMessage{
		"bans": &out.Bans, "whitelist": &out.Whitelist, "ops": &out.Ops, "known": &out.Known,
	}
	hideKnown := h.demoStranger(r, srv)
	for _, f := range playerListFiles {
		if f.key == "known" && hideKnown {
			continue
		}
		raw, ferr := h.readPlayerList(srv.NodeID, srv.UUID, srv.ActiveSubServer, f.filename)
		if ferr != nil {
			unavailable(f.key, ferr)
			continue
		}
		*targets[f.key] = raw
	}
	out.Known = knownPlayers(out.Known, maxKnownPlayers)

	props, found, perr := h.rcon.readNodeFileString(srv.NodeID, srv.UUID, srv.ActiveSubServer+"/"+serverPropertiesName)
	if perr != nil {
		unavailable("properties", fmt.Errorf("could not be read from the node: %w", perr))
	} else {
		if !found {
			props = ""
		}
		enabled, enforced := whitelistFlags(props)
		out.WhitelistEnabled, out.WhitelistEnforced = &enabled, &enforced
	}
	json.NewEncoder(w).Encode(out)
}

// maxKnownPlayers caps usercache.json in the response. MC itself keeps at most
// 1000 profiles and writes them most recently used first, so the cap only bites
// on a file something else has grown, and then keeps the recent ones.
const maxKnownPlayers = 1000

// knownPlayer is the part of a usercache.json entry the panel shows. Re-encoded
// rather than passed through: expiresOn says when each player was last seen,
// which nobody asked this endpoint to publish.
type knownPlayer struct {
	Name string `json:"name"`
	UUID string `json:"uuid,omitempty"`
}

// knownPlayers reduces usercache.json (already validated as an array by
// readPlayerList) to name+uuid, keeping the first n entries.
func knownPlayers(raw json.RawMessage, n int) json.RawMessage {
	var entries []knownPlayer
	if json.Unmarshal(raw, &entries) != nil {
		return emptyJSONArray
	}
	if len(entries) > n {
		entries = entries[:n]
	}
	b, err := json.Marshal(entries)
	if err != nil || len(entries) == 0 {
		return emptyJSONArray
	}
	return b
}

// demoStranger reports whether the caller reads this server only because it is
// a public demo, which any signed-in account may. Such a caller gets no
// usercache: the file browser already hides usercache.json from them, and this
// list is the same player identities. Owners, admins and real members hold
// files.read or players.manage, neither of which the demo grants. Fails closed.
func (h *PlayersHandler) demoStranger(r *http.Request, srv *models.Server) bool {
	if !isDemoServer(h.state, srv.UUID) {
		return false
	}
	if h.state.Authz == nil {
		return true
	}
	res, err := h.state.Authz.Resolve(authz.IdentityFromContext(r.Context()), srv.ID)
	if err != nil {
		return true
	}
	return !res.HasCap("players.manage") && !res.HasCap("files.read")
}

// whitelistFlags reads white-list and enforce-whitelist out of server.properties
// content. MC parses booleans with Boolean.parseBoolean, so anything but a
// case-insensitive "true" is false, and so is a missing key.
func whitelistFlags(props string) (enabled, enforced bool) {
	for _, line := range strings.Split(strings.ReplaceAll(props, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "!") {
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq < 0 {
			continue
		}
		on := strings.EqualFold(strings.TrimSpace(trimmed[eq+1:]), "true")
		switch strings.TrimSpace(trimmed[:eq]) {
		case "white-list":
			enabled = on
		case "enforce-whitelist":
			enforced = on
		}
	}
	return enabled, enforced
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
	// Each call was an RCON round trip to the server, with nothing in between:
	// every open players tab, every poll, every read-only account. One answer
	// now serves everyone asking within a few seconds.
	cacheKey := fmt.Sprintf("dylaris:server:%s:players-online", srv.UUID)
	if h.state.Redis != nil {
		if cached, cerr := h.state.Redis.Get(r.Context(), cacheKey).Bytes(); cerr == nil {
			var resp rconResponse
			if json.Unmarshal(cached, &resp) == nil {
				writeRconResponse(w, resp)
				return
			}
		}
	}
	resp := h.rcon.execAgainstServer(r.Context(), srv.ID, srv.UUID, srv.NodeID, rconRequest{Command: "list"})
	if resp.Success && h.state.Redis != nil {
		if b, merr := json.Marshal(resp); merr == nil {
			h.state.Redis.Set(r.Context(), cacheKey, b, playersOnlineCacheTTL)
		}
	}
	writeRconResponse(w, resp)
}

// playersOnlineCacheTTL is how long one roster answers every caller.
const playersOnlineCacheTTL = 3 * time.Second

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
	if resp.Success {
		// The command reached the server, so it is audited either way; a
		// refusal ("That player does not exist") is recorded as one rather
		// than as a ban that happened.
		refused, reason := classifyPlayerCommandOutput(resp.Output)
		auditPlayerAction(h.state, r, srv.ID, req.Action, req.Player, refused)
		if refused {
			resp.Success = false
			resp.Error = reason
			resp.status = http.StatusUnprocessableEntity
		}
	}
	writeRconResponse(w, resp)
}

// playerCommandFailurePrefixes are vanilla's refusals, lowercased, from the
// en_us lang files: 1.13-1.21 (argument.player.unknown,
// argument.entity.notfound.player, command.unknown.command/argument) and 1.12
// (commands.generic.*, commands.<verb>.failed "Could not ..."). Servers send
// these in English whatever the client's language. The "Nothing changed ..." /
// "already whitelisted" replies are deliberately absent: the state the
// moderator asked for already holds.
var playerCommandFailurePrefixes = []string{
	"that player does not exist",
	"no player was found",
	"unknown or incomplete command",
	"unknown command",
	"incorrect argument for command",
	"invalid command syntax",
	"an unknown error occurred",
	"usage: ",
	"could not ",
	// EssentialsX replaces kick/ban/tell on most Paper servers and refuses
	// with "Error: Player not found." once the colour codes are gone.
	"error: ",
}

// classifyPlayerCommandOutput reports whether Minecraft refused a player
// command, plus the reply with formatting codes stripped. A prefix match, not a
// substring one: success replies echo free text ("Kicked X: <reason>",
// "You whisper to X: <message>"), and a reason quoting a refusal must not turn
// a kick that happened into a failure.
func classifyPlayerCommandOutput(output string) (bool, string) {
	clean := strings.TrimSpace(stripMinecraftFormatting(output))
	low := strings.ToLower(clean)
	for _, p := range playerCommandFailurePrefixes {
		if strings.HasPrefix(low, p) {
			return true, clean
		}
	}
	// 1.12: "Player '<name>' cannot be found".
	if strings.HasPrefix(low, "player '") && strings.Contains(low, "' cannot be found") {
		return true, clean
	}
	return false, clean
}

// stripMinecraftFormatting drops section-sign codes (a colour or style
// character after U+00A7).
func stripMinecraftFormatting(s string) string {
	var b strings.Builder
	skip := false
	for _, r := range s {
		switch {
		case skip:
			skip = false
		case r == '§':
			skip = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// auditPlayerAction records a player action in the server's own words.
// Without it RequireCap's recorder writes a bare "players.manage" row with the
// path, which cannot say who was banned or that the whitelist went off. The
// reason and whispered text stay out: free text typed by a moderator is not
// what the trail is for.
func auditPlayerAction(state *AppState, r *http.Request, serverID int, action, player string, refused bool) {
	action = strings.TrimSpace(strings.ToLower(action))
	meta := map[string]interface{}{"action": action}
	if refused {
		meta["refused"] = true
	}
	if p := strings.TrimSpace(player); p != "" && action != "whitelist_on" && action != "whitelist_off" {
		meta["player"] = p
	}
	actorID, _ := r.Context().Value("userID").(string)
	LogServerAudit(state, r, serverID, ServerAuditEventPlayerAction, actorID, "", meta)
}

// ServerAuditEventPlayerAction is one players.manage action (kick, ban, op,
// whitelist on/off, ...); metadata names the action and the player.
const ServerAuditEventPlayerAction = "player_action"

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
	// The two that take no player. `whitelist on|off` also persists white-list
	// in server.properties; enforce-whitelist has no RCON command at all.
	"whitelist_on":  "whitelist on",
	"whitelist_off": "whitelist off",
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
	if verb == "whitelist on" || verb == "whitelist off" {
		return verb, nil
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
