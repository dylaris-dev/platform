package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
)

// migrationCancelFlagTTL bounds the cancel flag so a stale request can never
// linger into a future move. It is only a backstop: the orchestrator deletes
// the flag when the migration ends, so the TTL matters solely when it never
// gets there (the Core died mid-run).
//
// It deliberately does NOT track how long a migration takes - a pre-cutover
// transfer runs up to migrationR2PhaseTimeout, far past this - because every
// pre-cutover wait re-reads the flag on each poll tick. A cancel is therefore
// observed within one poll interval of being set, long before this expires.
const migrationCancelFlagTTL = 10 * time.Minute

// isCancellableMigrationPhase reports whether an orchestration phase is still
// pre-cutover and in-flight, so a cancel would actually take effect. An empty
// phase (lock held but no orchestration record yet) is the just-started window
// and is cancellable. The post-cutover phase "finalizing" and every terminal
// phase (done/failed/.../cancelled/none) are NOT cancellable - after cutover a
// cancel is a no-op.
func isCancellableMigrationPhase(phase string) bool {
	switch phase {
	case "", "starting", "migrating":
		return true
	}
	return false
}

// migrationPhase returns the orchestration phase for a server, or "" if the
// record is absent/unreadable.
func (h *ServerHandler) migrationPhase(ctx context.Context, uuid string) string {
	if h.state.Redis == nil {
		return ""
	}
	raw, err := h.state.Redis.Get(ctx, fmt.Sprintf("dylaris:migration:%s:orchestration", uuid)).Result()
	if err != nil || raw == "" {
		return ""
	}
	var st struct {
		Phase string `json:"phase"`
	}
	if json.Unmarshal([]byte(raw), &st) != nil {
		return ""
	}
	return st.Phase
}

// CancelMigration POST /api/admin/servers/{id}/migration/cancel
// Admin oversight (PANEL servers.write, gateway-gated). Requests cancellation of
// an in-flight migration by setting a Redis flag the leader-elected orchestrator
// checks in its pre-cutover poll loops and its pre-cutover guard; the migration
// then rolls back to the source node, which is still authoritative before
// cutover. After the node_id flip the flag is a no-op and the move completes, so
// a cancel is only honored pre-cutover. Returns 409 when no migration is in
// progress or it has already finished.
func (h *ServerHandler) CancelMigration(w http.ResponseWriter, r *http.Request) {
	if h.state.Redis == nil {
		sendJSONError(w, "Migration state unavailable", 503)
		return
	}
	vars := mux.Vars(r)
	serverID, err := strconv.Atoi(vars["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return
	}
	// A customer's own transfer is theirs to cancel, not staff's.
	srv, err := h.state.Store.GetServerByID(serverID)
	if err != nil || srv == nil || foreignToCaller(h.state, r, srv.NodeID) {
		sendJSONError(w, "Server not found", 404)
		return
	}
	ctx := context.Background()
	// A migration is in progress iff the per-server migration lock is held.
	lockHeld, err := h.state.Redis.Exists(ctx, fmt.Sprintf("dylaris:server:%s:migration", srv.UUID)).Result()
	if err != nil {
		sendJSONError(w, "Failed to read migration state", 500)
		return
	}
	if lockHeld == 0 {
		sendJSONError(w, "No migration is in progress for this server", 409)
		return
	}
	if phase := h.migrationPhase(ctx, srv.UUID); !isCancellableMigrationPhase(phase) {
		sendJSONError(w, "Migration can no longer be cancelled (phase: "+phase+")", 409)
		return
	}
	username, _ := r.Context().Value("username").(string)
	if username == "" {
		username = "admin"
	}
	if err := h.state.Redis.Set(ctx, fmt.Sprintf("dylaris:server:%s:migration:cancel", srv.UUID), username, migrationCancelFlagTTL).Err(); err != nil {
		sendJSONError(w, "Failed to request cancellation", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Migration cancellation requested. The server rolls back to its current node if the migration has not yet cut over.",
	})
}
