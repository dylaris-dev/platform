package gateway

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"

	"dylaris-core/handlers/reqctx"

	"github.com/gorilla/mux"
)

// GetRouteFloodgateKey GET /api/external/gateway/routes/{domain}/floodgate-key - the Floodgate key of one of the caller's addresses, a credential that needs gateway.write (a gateway.read key is refused) and is never cached (Cache-Control: no-store), as {success, domain, key} with key the standard base64 of the 16 bytes Floodgate reads as key.pem; 404 not_found when the address is not the caller's or has no key yet (give it Bedrock first). The key stays the same while the address exists; removing the address and adding it again gives a new one.
func (h *ExternalGatewayHandler) GetRouteFloodgateKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	owner := reqctx.ByonCallerID(r)
	if owner == "" {
		writeExternalError(w, http.StatusUnauthorized, "Unauthorized", "unauthorized")
		return
	}
	domain := mux.Vars(r)["domain"]
	// isAdmin is false whatever the key owner is: a key never carries admin
	// powers (see the top of external.go), and an admin skips the owner checks.
	key, status, msg := h.gw.linkRouteFloodgateKey(owner, false, domain)
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		writeExternalError(w, http.StatusNotFound, msg, "not_found")
		return
	default:
		writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
		return
	}
	log.Printf("external gateway: floodgate key of %s downloaded by %s", domain, owner)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "domain": domain, "key": base64.StdEncoding.EncodeToString(key)})
}
