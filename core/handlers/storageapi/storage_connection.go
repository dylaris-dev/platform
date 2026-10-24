package storageapi

import (
	"encoding/json"
	"net/http"

	"dylaris-core/handlers"
)

// StorageConnectionHandler serves the current connection state of the two core
// storage backends.
type StorageConnectionHandler struct {
	state *handlers.AppState
}

func NewStorageConnectionHandler(state *handlers.AppState) *StorageConnectionHandler {
	return &StorageConnectionHandler{state: state}
}

// GetConnection GET /api/storage/connection
//
// This exists because the SSE channel has no replay and its hello frame carries
// no state: a panel that connects DURING an outage would otherwise wait for the
// recovery event without ever having been told there was something to recover
// from. Callers read this once on connect and then follow the
// storage.connection.changed events.
//
// The body is the same coarse state the event carries, for the same reason: the
// route is behind AuthMiddleware only, so every authenticated user can read it.
// No cause, no path, no bucket, no endpoint.
func (h *StorageConnectionHandler) GetConnection(w http.ResponseWriter, r *http.Request) {
	// Set explicitly rather than left to Go's sniffer, which labels a JSON body
	// text/plain. GetStatus in health.go omits it and is a standing wart; this
	// does not copy it.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.state.StorageStatus.Snapshot())
}
