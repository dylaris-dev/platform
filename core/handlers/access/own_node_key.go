package access

import (
	"net/http"
	"strings"

	"dylaris-core/store"
)

// OwnNodeKey resolves one of the caller's live BYON node keys by its identity, or
// the status and message to refuse with. "Not yours" reads exactly like "does
// not exist", so the answer never confirms that someone else's identity is real.
func OwnNodeKey(st store.Store, nodeID, userID string) (*store.WarpAPIKey, int, string) {
	if !strings.HasPrefix(nodeID, "node-") {
		return nil, http.StatusBadRequest, "Not a node key"
	}
	k, err := st.GetWarpAPIKeyByNodeID(nodeID)
	if err != nil || k.OwnerID == "" || k.OwnerID != userID {
		return nil, http.StatusNotFound, "Node key not found"
	}
	if k.RevokedAt != nil {
		return nil, http.StatusConflict, "This node key has been revoked"
	}
	return k, 0, ""
}
