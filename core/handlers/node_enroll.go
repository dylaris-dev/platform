package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// NodeEnrollHandler mints + manages per-user node enrollment tokens (BYON). A
// tenant mints a token in the panel, then runs their node with it as
// NODE_ENROLL_TOKEN; on first heartbeat the node is bound to that user (wired in
// the discovery binding chunk). All endpoints require feature_byon_enabled.
type NodeEnrollHandler struct {
	state *AppState
}

func NewNodeEnrollHandler(state *AppState) *NodeEnrollHandler {
	return &NodeEnrollHandler{state: state}
}

// maxNodeEnrollTokenExpiryDays caps a tenant-supplied expiresDays so a leaked
// or mistyped huge value cannot mint an effectively-eternal enroll token.
const maxNodeEnrollTokenExpiryDays = 30

// MintToken POST /api/nodes/enroll-token — generate a new enroll token for the
// calling user. The plaintext is returned ONCE; only its hash is stored.
func (h *NodeEnrollHandler) MintToken(w http.ResponseWriter, r *http.Request) {
	if !byonActive(h.state, r) {
		sendJSONError(w, "BYON is not enabled", http.StatusForbidden)
		return
	}
	userID := byonCallerID(r)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	// May they at all, before how many. The cap below is a ceiling and skips
	// itself at zero, which is exactly the value an account that bought nothing
	// carries; without this a fresh registration could mint enroll tokens
	// forever. See entitlement_gate.go.
	if !h.state.requireEntitlement(r, w, userID, services.EntitlementByon) {
		return
	}
	var req struct {
		Label       string `json:"label"`
		ExpiresDays int    `json:"expiresDays"`
		// The BYON node key (node-<hex>) minted for the same machine. Redeeming
		// the token binds that key to the node it creates, which is what lets the
		// machine's kit run the Link beside the node. Optional: an operator
		// minting a token by hand has no key to name.
		WarpKeyNodeID string `json:"warpKeyNodeId"`
	}
	// io.EOF is fine: every field is optional and an empty body means "mint one
	// with the defaults". Anything else is a malformed body, and swallowing it
	// meant a caller who sent `{"expiresDays": 30}` with a typo elsewhere in the
	// JSON got a silent 7-day token back and no way to tell.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		sendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	// Checked here, where a refusal can still be acted on: at the far end the
	// binding is best-effort and the gRPC layer flattens every error anyway.
	// Every refusal is a 400, someone else's key included, so the answer does
	// not confirm that key exists.
	warpKeyNodeID := strings.TrimSpace(req.WarpKeyNodeID)
	if warpKeyNodeID != "" {
		key, _, msg := ownNodeKey(h.state.Store, warpKeyNodeID, userID)
		if key == nil {
			sendJSONError(w, msg, http.StatusBadRequest)
			return
		}
		if key.BoundNodeID != 0 {
			sendJSONError(w, "This node key already belongs to a machine", http.StatusBadRequest)
			return
		}
	}

	// Enforce the tenant's node cap here, counting redeemable tokens as pending
	// nodes - the same rule MintNodeWarpKey applies to unrevoked warp keys, and
	// MintLinkKit to link kits. This was the one tenant-facing mint endpoint of
	// the three with no cap at all.
	//
	// The limit was never bypassable: Handshake.Enroll checks NodeLimitReached
	// before creating the node. But it refused at the far end of the flow, and
	// the gRPC layer flattens every enrollment error to "enrollment failed", so a
	// tenant over their plan set up a machine, watched it fail to pair, and had
	// nothing telling them why. Refusing at mint time says it in the place where
	// it can still be acted on - which is exactly what the warp sibling's
	// "Revoke an unused key or remove a machine first" does.
	//
	// It also bounds the table: minting was an uncapped, unrate-limited write
	// available to any authenticated tenant.
	//
	// Counted through services.NodeSlots, which sees BOTH kinds of pending
	// identity. This gate used to count only its own enroll tokens and the warp
	// sibling only its own keys, so neither could see what the other had handed
	// out.
	//
	// The question is what the tenant holds AFTER this mint, not whether they
	// are already at the cap. A machine needs both halves, so on max_nodes = 1
	// the warp key the panel mints a moment earlier filled the allowance and this
	// gate refused the token belonging to the same machine - see NodeSlots.
	if lim, lerr := services.EffectiveLimits(h.state.Store, userID); lerr == nil && lim.MaxNodes != nil {
		slots, serr := services.CountNodeSlots(h.state.Store, userID)
		if serr == nil && services.Exceeds(lim.MaxNodes, slots.UsedWithEnrollToken()) {
			sendJSONError(w, fmt.Sprintf(
				"Node limit reached (%d). Remove a machine, or revoke an unused enroll token or node key, before adding another.", *lim.MaxNodes),
				http.StatusForbidden)
			return
		}
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		sendJSONError(w, "Failed to generate token", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(b)

	// Default a 7-day expiry when none is given, so a leaked token is not valid
	// forever. Single-use (consumed on enroll) already limits reuse; 7 days is
	// enough to set a node up. Cap a tenant-supplied value too: an unbounded
	// expiresDays would let a mistyped or malicious huge number mint an
	// effectively-eternal enroll token.
	days := req.ExpiresDays
	if days <= 0 {
		days = 7
	} else if days > maxNodeEnrollTokenExpiryDays {
		days = maxNodeEnrollTokenExpiryDays
	}
	t := time.Now().AddDate(0, 0, days)
	exp := &t
	if err := h.state.Store.CreateNodeEnrollToken(userID, token, strings.TrimSpace(req.Label), exp, warpKeyNodeID); err != nil {
		sendJSONError(w, "Failed to create token", http.StatusInternalServerError)
		return
	}
	// The fingerprint is only meaningful pinning material when TLS is actually
	// on; showing it while GRPC_TLS_ENABLED=false would suggest a pin the
	// control channel never enforces.
	fingerprint := ""
	if h.state.GRPCTLSEnabled {
		fingerprint = h.state.GRPCTLSFingerprint
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":            true,
		"token":              token,
		"grpcTlsFingerprint": fingerprint,
		"note":               "Shown once. Start your node with NODE_ENROLL_TOKEN set to this value. When GRPC_TLS_ENABLED, also set GRPC_TLS_FINGERPRINT.",
	})
}

// ListTokens GET /api/nodes/enroll-token — the caller's tokens (metadata only).
func (h *NodeEnrollHandler) ListTokens(w http.ResponseWriter, r *http.Request) {
	if !byonActive(h.state, r) {
		sendJSONError(w, "BYON is not enabled", http.StatusForbidden)
		return
	}
	userID := byonCallerID(r)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	tokens, err := h.state.Store.ListNodeEnrollTokens(userID)
	if err != nil {
		sendJSONError(w, "Failed to load tokens", http.StatusInternalServerError)
		return
	}
	if tokens == nil {
		tokens = []store.NodeEnrollToken{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "tokens": tokens})
}

// RevokeToken DELETE /api/nodes/enroll-token/{id} — revoke one of the caller's
// tokens (scoped to owner).
func (h *NodeEnrollHandler) RevokeToken(w http.ResponseWriter, r *http.Request) {
	if !byonActive(h.state, r) {
		sendJSONError(w, "BYON is not enabled", http.StatusForbidden)
		return
	}
	userID := byonCallerID(r)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	id := mux.Vars(r)["id"]
	if err := h.state.Store.DeleteNodeEnrollToken(id, userID); err != nil {
		sendJSONError(w, "Failed to revoke token", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
