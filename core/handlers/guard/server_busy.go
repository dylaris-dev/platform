package guard

import (
	"context"
	"net/http"
	"strings"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	pb "dylaris-proto/node"

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
//
// A copy still waiting in the node's stream holds no node_busy yet, so it is
// read from copy_pending as well (RefuseIfCopyQueued); reg is how that is
// resolved once the copy may have landed.
func RefuseIfServerBusy(w http.ResponseWriter, r *http.Request, rdb *redis.Client, reg *nodegrpc.Registry, srv *models.Server) bool {
	if RefuseIfMoving(w, r, srv) {
		return true
	}
	if rdb != nil {
		if busy, err := rdb.Get(context.Background(), services.NodeBusyKey(srv.UUID)).Result(); err == nil && busy != "" {
			httpx.SendJSONError(w, "The node is still working on this server ("+busy+"). Try again when it has finished.", http.StatusConflict)
			return true
		}
	}
	return RefuseIfCopyQueued(w, r, rdb, reg, srv)
}

// listRootNames is ListServerRootNames, replaceable in tests.
var listRootNames = ListServerRootNames

// RefuseIfCopyQueued answers 409 copy_running while a copy Core queued has not
// landed on the node. Core never learns of a copy's success, so a set key is
// checked against the server root: the node builds the copy under a staging
// name and renames it in as its last step, so the target being there means it
// is done, and the key is dropped. A failed copy is cleared by the status
// watcher; the TTL is the backstop for a node too old to report one.
func RefuseIfCopyQueued(w http.ResponseWriter, r *http.Request, rdb *redis.Client, reg *nodegrpc.Registry, srv *models.Server) bool {
	if rdb == nil {
		return false
	}
	ctx := r.Context()
	pending, _ := rdb.Get(ctx, services.CopyPendingKey(srv.UUID)).Result()
	if pending == "" {
		return false
	}
	entries, err := listRootNames(reg, srv.NodeID, srv.UUID)
	if err != nil {
		httpx.SendJSONError(w, "The node did not answer, so it is not known whether a copy of this server has finished: "+err.Error(), http.StatusBadGateway)
		return true
	}
	if SubServerNameTaken(entries, pending) {
		rdb.Del(ctx, services.CopyPendingKey(srv.UUID))
		return false
	}
	httpx.SendJSONErrorCode(w, "A copy of this server is still being made. Try again once it has finished.", "copy_running", http.StatusConflict)
	return true
}

// SubServerNameTaken reports whether name is already an entry in a listing of
// the server root, ignoring case as the panel does.
func SubServerNameTaken(entries []*pb.FileInfo, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, e := range entries {
		if e != nil && strings.EqualFold(strings.TrimSpace(e.Name), name) {
			return true
		}
	}
	return false
}
