package guard

import (
	"context"
	"net/http"

	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/redis/go-redis/v9"
)

// RefuseIfMoving answers 409 while the server is being moved to another node.
// Power, resources and restore always refused then; setup, switching, copies
// and deletes did not, and one of them landing mid-move wrote into the copy the
// source was about to delete, or left the world on the target with no server
// row. Operators may still act on a move that is stuck.
func RefuseIfMoving(w http.ResponseWriter, r *http.Request, srv *models.Server) bool {
	if srv.Status == "migrating" && !reqctx.OperatorOverride(r) {
		httpx.SendJSONError(w, "Server is being moved to another node. Try again when the move has finished.", http.StatusConflict)
		return true
	}
	return false
}

// RefuseIfServerBusy is RefuseIfMoving plus the node's own interlock: the node
// holds node_busy while it installs, updates, copies or restores this server,
// and drops it the moment it finishes or dies, so it cannot strand a server the
// way a stale status could. Two of those jobs on one server at once - a setup
// beside a restore - wrote the same directory from both ends.
func RefuseIfServerBusy(w http.ResponseWriter, r *http.Request, rdb *redis.Client, srv *models.Server) bool {
	if RefuseIfMoving(w, r, srv) {
		return true
	}
	if rdb != nil {
		if busy, err := rdb.Get(context.Background(), services.NodeBusyKey(srv.UUID)).Result(); err == nil && busy != "" {
			httpx.SendJSONError(w, "The node is still working on this server ("+busy+"). Try again when it has finished.", http.StatusConflict)
			return true
		}
	}
	return false
}
