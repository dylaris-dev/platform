package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	pb "dylaris-proto/node"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// RCON handlers. Two surfaces:
//   * Panel-internal: POST /api/servers/{id}/rcon (auth via session JWT)
//   * External:       POST /api/external/rcon/{uuid}/exec (auth via API key)
//
// Both funnel into a single helper that resolves the server, loads the
// stored RCON config, then dispatches the command via gRPC to the node.
// Permission gates differ — internal uses the existing server-access matrix
// (power-class action); external uses the API key's scope.

type RconHandler struct {
	state *AppState
	// applyProps writes the RCON keys into the active sub-server's
	// server.properties on the node. A field (not a direct method call) so the
	// SetConfig unit tests can inject a fake instead of a live node/gRPC
	// registry. Defaults to the real applyRconToServerProperties.
	applyProps func(nodeID int, serverUUID, activeSubServer string, enabled bool, port int, password string) error
}

func NewRconHandler(state *AppState) *RconHandler {
	h := &RconHandler{state: state}
	h.applyProps = h.applyRconToServerProperties
	return h
}

const (
	// rconExecTimeout is how long we wait for the node to come back with a
	// reply. MC servers typically reply instantly but a frozen server can
	// hang the TCP socket — we'd rather report timeout to the user than
	// stall a request thread.
	rconExecTimeout   = 5 * time.Second
	rconMaxCommandLen = 1024
	defaultRconPort   = 25575
)

type rconRequest struct {
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeoutMs,omitempty"`
}

type rconResponse struct {
	Success    bool   `json:"success"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"durationMs"`

	// status is the HTTP status writeRconResponse sends. Unexported, so it is
	// never part of the body and the wire shape is unchanged.
	//
	// It exists because every one of these endpoints answered 200 and put the
	// failure in the body, so nothing outside the panel could tell a ban that
	// happened from one that never reached the server. Measured on production:
	// a ban refused with "rcon not enabled" answered 200, and the server audit
	// trail - which records a successful request - then recorded the ban.
	status int
}

// writeRconResponse is the single writer for all four RCON-backed endpoints.
// A failure with no status of its own is a bad gateway: the command left here
// and the answer did not come back.
func writeRconResponse(w http.ResponseWriter, resp rconResponse) {
	code := resp.status
	if code == 0 {
		if resp.Success {
			code = http.StatusOK
		} else {
			code = http.StatusBadGateway
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}

// ExecForUser POST /api/servers/{id}/rcon — panel-side entry. Gated by the
// existing "power"-class permission (owner / admin / member-with-power).
func (h *RconHandler) ExecForUser(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req rconRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	resp := h.execAgainstServer(r.Context(), srv.ID, srv.UUID, srv.NodeID, req)
	writeRconResponse(w, resp)
}

// ExecExternal POST /api/external/rcon/{uuid}/exec — automation entry.
// Auth + scope check happen in the middleware before we land here; once we
// hit this handler the key is already valid and scoped to the server UUID
// from the URL.
func (h *RconHandler) ExecExternal(w http.ResponseWriter, r *http.Request) {
	serverUUID := mux.Vars(r)["uuid"]
	srv, err := h.state.Store.GetServerByUUID(serverUUID)
	if err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req rconRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	resp := h.execAgainstServer(r.Context(), srv.ID, srv.UUID, srv.NodeID, req)
	writeRconResponse(w, resp)
}

func (h *RconHandler) execAgainstServer(ctx context.Context, serverID int, serverUUID string, nodeID int, req rconRequest) rconResponse {
	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		return rconResponse{Error: "command required", status: http.StatusBadRequest}
	}
	if len(req.Command) > rconMaxCommandLen {
		return rconResponse{Error: "command too long", status: http.StatusBadRequest}
	}

	enabled, port, password, err := h.state.Store.GetServerRconConfig(serverID)
	if err != nil {
		return rconResponse{Error: "failed to load rcon config", status: http.StatusInternalServerError}
	}
	if !enabled || password == "" {
		return rconResponse{Error: "rcon not enabled for this server", status: http.StatusConflict}
	}
	if port == 0 {
		port = defaultRconPort
	}
	if h.state.GRPCRegistry == nil {
		return rconResponse{Error: "node registry not available", status: http.StatusServiceUnavailable}
	}

	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 || timeoutMs > 10_000 {
		timeoutMs = int(rconExecTimeout / time.Millisecond)
	}

	msg := &pb.NodeMessage{
		RequestId:  uuid.NewString(),
		ServerUuid: serverUUID,
		Payload: &pb.NodeMessage_RconExecReq{
			RconExecReq: &pb.RconExecReq{
				Command:      req.Command,
				RconPassword: password,
				RconPort:     int32(port),
				TimeoutMs:    int32(timeoutMs),
			},
		},
	}
	respMsg, err := h.state.GRPCRegistry.SendRequest(nodeID, msg, time.Duration(timeoutMs+1000)*time.Millisecond)
	if err != nil {
		return rconResponse{Error: rconFailureMessage(serverUUID, nodeID, err.Error())}
	}
	rconResp := respMsg.GetRconExecResp()
	if rconResp == nil {
		if errMsg := respMsg.GetError(); errMsg != nil {
			return rconResponse{Error: rconFailureMessage(serverUUID, nodeID, errMsg.Message)}
		}
		return rconResponse{Error: "unexpected response from node", status: http.StatusBadGateway}
	}
	if rconResp.Error != "" {
		return rconResponse{
			Success:    rconResp.Ok,
			Output:     rconResp.Output,
			Error:      rconFailureMessage(serverUUID, nodeID, rconResp.Error),
			DurationMs: rconResp.DurationMs,
		}
	}
	return rconResponse{
		Success:    rconResp.Ok,
		Output:     rconResp.Output,
		DurationMs: rconResp.DurationMs,
	}
}

// rconFailureMessage logs what actually went wrong and returns what the caller
// is told. The two are not the same thing: the node's error is a dial error, so
// it carries the container's private overlay address, and RCON is handed out as
// its own capability - a friend granted rcon.exec holds neither network.read
// nor any other right to the topology. Measured on production: a delegate
// running one command read back
// "dial 10.20.13.16:25575: ... connect: connection refused".
//
// The one case worth naming is a refused connection, because it has an action
// attached and is what an operator hits right after enabling RCON: MC only
// opens the listener at JVM start. Everything else is one message plus a log
// line, the same trade the tab proxy makes.
func rconFailureMessage(serverUUID string, nodeID int, detail string) string {
	log.Printf("rcon: server %s on node %d failed: %s", serverUUID, nodeID, detail)
	if strings.Contains(detail, "connection refused") {
		return "The server is not accepting RCON connections. If RCON was just enabled, restart the server to apply it."
	}
	if strings.Contains(detail, "timeout") || strings.Contains(detail, "deadline exceeded") {
		return "The server did not answer the RCON command in time."
	}
	if strings.Contains(detail, "authentication") || strings.Contains(detail, "password") {
		return "The server refused the RCON password. Regenerate it in the RCON settings and restart the server."
	}
	return "The RCON command could not be delivered to the server."
}

// --- RCON config CRUD (Network sub-tab support) ---

type rconConfigRequest struct {
	Enabled    bool   `json:"enabled"`
	Port       int    `json:"port"`
	Password   string `json:"password,omitempty"`
	Regenerate bool   `json:"regenerate,omitempty"`
	// HideLogNoise drops Minecraft's per-RCON-connection thread lines from the
	// console. A POINTER so an old client that omits the field keeps the stored
	// value: a plain bool would default to false and silently turn the filter off
	// on every unrelated RCON config save.
	HideLogNoise *bool `json:"hideLogNoise,omitempty"`
}

type rconConfigResponse struct {
	Success   bool   `json:"success"`
	Enabled   bool   `json:"enabled"`
	Port      int    `json:"port"`
	HasSecret bool   `json:"hasSecret"`
	Password  string `json:"password,omitempty"` // populated only when regenerated
	Message   string `json:"message,omitempty"`
	// RestartRequired is true whenever this call actually rewrote
	// server.properties (srv.ActiveSubServer != ""). MC only opens/re-reads the
	// RCON listener at JVM start, so the change is inert until the server
	// restarts - this lets the panel render that state deterministically
	// instead of inferring it from a later connection-refused error. Set
	// regardless of enabled/disabled direction: disabling also rewrites the
	// file and is equally inert until restart. No auto-restart happens here.
	RestartRequired bool `json:"restartRequired"`
	// HideLogNoise mirrors servers.rcon_log_filter. Applies live - the
	// log-shipper re-reads it from Redis on a timer, no restart involved, which
	// is why it is deliberately NOT part of RestartRequired above.
	HideLogNoise bool `json:"hideLogNoise"`
}

// GetConfig GET /api/servers/{id}/rcon/config — returns enabled/port +
// whether a password is set. Password value is never returned.
func (h *RconHandler) GetConfig(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	if _, err := h.state.Store.GetServerByID(serverID); err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	enabled, port, password, err := h.state.Store.GetServerRconConfig(serverID)
	if err != nil {
		sendJSONError(w, "Failed to load rcon config", http.StatusInternalServerError)
		return
	}
	// Surface the persisted "restart required" flag so a panel reload restores
	// the banner and keeps the RCON-dependent Players tabs locked. Non-fatal:
	// the column is NOT NULL DEFAULT FALSE, so a read error means a full DB
	// outage that GetServerRconConfig above would already have caught; fall back
	// to false rather than blanking the whole config card.
	needsRestart, _ := h.state.Store.GetServerRconNeedsRestart(serverID)
	// Same non-fatal treatment: NOT NULL DEFAULT FALSE, so a read error means a
	// DB outage the config read above already surfaced.
	hideNoise, _ := h.state.Store.GetServerRconLogFilter(serverID)
	json.NewEncoder(w).Encode(rconConfigResponse{
		Success:         true,
		Enabled:         enabled,
		Port:            port,
		HasSecret:       password != "",
		RestartRequired: needsRestart,
		HideLogNoise:    hideNoise,
	})
}

// SetConfig PUT /api/servers/{id}/rcon/config — enable/disable, set port +
// password. If `regenerate`=true the panel asks the backend to mint a fresh
// random password instead of supplying one. Newly minted passwords are
// returned ONCE in the response so the user can copy them.
func (h *RconHandler) SetConfig(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req rconConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Port < 0 || req.Port > 65535 {
		sendJSONError(w, "Invalid port", http.StatusBadRequest)
		return
	}

	_, existingPort, existingPassword, _ := h.state.Store.GetServerRconConfig(serverID)
	password := existingPassword
	exposeNew := ""
	if req.Regenerate || (req.Enabled && password == "") {
		newPw, err := generateRconPassword(24)
		if err != nil {
			sendJSONError(w, "Failed to generate password", http.StatusInternalServerError)
			return
		}
		password = newPw
		exposeNew = newPw
	} else if req.Password != "" {
		password = req.Password
	}
	port := req.Port
	if port == 0 {
		port = existingPort
	}
	if port == 0 {
		port = defaultRconPort
	}

	// Enabling RCON is only meaningful once the server has an installed,
	// active sub-server whose server.properties we can write.
	if req.Enabled && srv.ActiveSubServer == "" {
		sendJSONError(w, "Install or start the server before enabling RCON", http.StatusConflict)
		return
	}

	// Write server.properties BEFORE flipping the DB flag so the DB and the file
	// never diverge: DB rcon_enabled is what the exec + Players gates read, but
	// MC only listens if enable-rcon lives in server.properties. On a node/file
	// failure we return an error and touch NEITHER, so a transient failure can't
	// leave the DB saying "enabled" while MC never opened the port. Skipped only
	// when disabling a not-yet-installed server (no file, nothing listening).
	if srv.ActiveSubServer != "" {
		if err := h.applyProps(srv.NodeID, srv.UUID, srv.ActiveSubServer, req.Enabled, port, password); err != nil {
			sendJSONError(w, fmt.Sprintf("Failed to apply RCON to server.properties: %v", err), http.StatusBadGateway)
			return
		}
	}

	if err := h.state.Store.SetServerRconConfig(serverID, req.Enabled, port, password); err != nil {
		sendJSONError(w, "Failed to save rcon config", http.StatusInternalServerError)
		return
	}
	// Persist whether this write left the running server stale: server.properties
	// was actually rewritten (ActiveSubServer != "") and MC only re-reads it at
	// start, so a restart is pending. Persisting it keeps the panel banner + the
	// RCON-dependent Players-tab lock across a reload; the flag clears when the
	// server (re)starts.
	needsRestart := srv.ActiveSubServer != ""
	if err := h.state.Store.SetServerRconNeedsRestart(serverID, needsRestart); err != nil {
		sendJSONError(w, "Failed to save rcon config", http.StatusInternalServerError)
		return
	}
	// Console filter. Independent of everything above: it never touches
	// server.properties and takes effect without a restart, so it is applied even
	// when the RCON write path did nothing.
	hideNoise, _ := h.state.Store.GetServerRconLogFilter(serverID)
	if req.HideLogNoise != nil && *req.HideLogNoise != hideNoise {
		hideNoise = *req.HideLogNoise
		if err := h.state.Store.SetServerRconLogFilter(serverID, hideNoise); err != nil {
			sendJSONError(w, "Failed to save console filter", http.StatusInternalServerError)
			return
		}
		// Publish immediately so a running server picks it up on its next poll
		// rather than waiting for the status watcher's republish tick.
		if h.state.Redis != nil {
			v := "false"
			if hideNoise {
				v = "true"
			}
			h.state.Redis.Set(r.Context(), fmt.Sprintf("dylaris:server:%s:log_filter_rcon", srv.UUID), v, 60*time.Second)
		}
	}

	json.NewEncoder(w).Encode(rconConfigResponse{
		Success:         true,
		Enabled:         req.Enabled,
		Port:            port,
		HasSecret:       password != "",
		Password:        exposeNew,
		Message:         "RCON config saved and written to server.properties. Restart the server to apply.",
		RestartRequired: needsRestart,
		HideLogNoise:    hideNoise,
	})
}

// generateRconPassword returns a hex-encoded random password of `nBytes`*2
// characters. Hex (not base64) so it survives copy-paste into terminals and
// server.properties without escaping.
func generateRconPassword(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HashAPIKey returns the canonical storage form of an API-key plaintext.
// Exported so API-key creation paths and external auth middleware agree.
func HashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
