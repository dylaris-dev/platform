package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"dylaris-core/authz"
	"dylaris-core/models"

	"github.com/gorilla/mux"
)

// The /api/external/* surface: the routes an API key may call.
//
// It is a SEPARATE surface from the panel's own routes on purpose. The panel's
// routes churn with the UI and address servers by their sequential numeric id;
// this one is a contract an integrator builds against and addresses servers by
// UUID, which is not guessable. Keeping them apart lets the internal routes
// stay free to change.
//
// Almost nothing here is new logic. Each route is an adapter that turns the
// {uuid} into the {id} + identity context the existing panel handler already
// expects, then calls that handler unchanged. That matters for more than
// brevity: the panel handlers carry guards an external re-implementation would
// silently lose - the billing-suspension block on start/restart, the disk-full
// block, the pending_setup block, the power audit entry. Duplicating a handler
// to "keep the external surface simple" is how those get dropped.
//
// Authorization has already happened by the time these run. APIKeyServerRoute /
// APIKeyOwnerRoute validated the key, checked the server allowlist, resolved
// the required capability against the key, and re-checked that the key owner
// still holds it. See api_keys.go.

// keyOwnerContext replaces the request identity with the KEY OWNER's.
//
// It is the key owner and never the key itself because a panel handler that
// resolves authority from the context needs a real principal. The narrower
// authority of the key was enforced by the middleware before this ran - except
// where the capability depends on the request body, which is what
// APIKeyPowerGate below is for.
func (h *APIKeysHandler) keyOwnerContext(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	key := APIKeyFromContext(r)
	if key == nil {
		// Only reachable by registering an adapter without the key middleware in
		// front of it. Same reasoning as the misconfigured server route in
		// apiKeyMiddleware: fail loudly rather than run a panel handler with no
		// principal at all.
		sendJSONError(w, "Route misconfigured: external adapter without key auth", http.StatusInternalServerError)
		return nil, false
	}
	owner, err := h.state.Store.GetUserByID(key.UserID)
	if err != nil || owner == nil {
		sendJSONError(w, "Key owner is no longer valid", http.StatusUnauthorized)
		return nil, false
	}
	ctx := context.WithValue(r.Context(), "userID", owner.ID)
	ctx = context.WithValue(ctx, "username", owner.Username)
	ctx = context.WithValue(ctx, "isAdmin", owner.IsAdmin)
	return r.WithContext(ctx), true
}

// ExternalServerRoute adapts a {uuid}-addressed key route onto the panel
// handler that already implements it: it resolves the server and hands that
// handler the {id} and identity it expects.
func (h *APIKeysHandler) ExternalServerRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		srv, err := h.state.Store.GetServerByUUID(mux.Vars(r)["uuid"])
		if err != nil || srv == nil {
			sendJSONError(w, "Server not found", http.StatusNotFound)
			return
		}
		r, ok := h.keyOwnerContext(w, r)
		if !ok {
			return
		}
		vars := mux.Vars(r)
		vars["id"] = strconv.Itoa(srv.ID)
		next(w, mux.SetURLVars(r, vars))
	}
}

// ExternalOwnerRoute adapts an owner-shaped key route onto a panel handler that
// acts on "the realm of the caller", so that realm becomes the key owner's.
func (h *APIKeysHandler) ExternalOwnerRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r, ok := h.keyOwnerContext(w, r)
		if !ok {
			return
		}
		next(w, r)
	}
}

// ExternalJobInServer refuses a {jobId} that does not belong to the {uuid} in
// the same path.
//
// Without it the {uuid} on the backup-job routes would be decorative. The panel
// handlers behind them resolve their server from the JOB ROW, not from the path
// (see resolveJobWithAccess: "the /backup-jobs/{jobId} family resolves its
// server from the job row rather than a path {id}/{uuid}"), and they authorize
// against the key OWNER - who normally holds backups.* on all of their servers.
// So a key allowlisted for server A could name A in the path, pass the
// allowlist check, and hand in a job belonging to server B. The allowlist is
// the whole boundary of a key; a route that lets a body or path parameter step
// outside it does not have one.
//
// A mismatch answers 404 rather than 403: a job on another server is
// indistinguishable from a job id that does not exist, and saying which it was
// would confirm the existence of a job the caller may not see.
func (h *APIKeysHandler) ExternalJobInServer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jobID, err := strconv.Atoi(mux.Vars(r)["jobId"])
		if err != nil {
			sendJSONError(w, "Invalid job ID", http.StatusBadRequest)
			return
		}
		job, err := h.state.Store.GetBackupJob(jobID)
		if err != nil || job == nil {
			sendJSONError(w, "Job not found", http.StatusNotFound)
			return
		}
		srv, err := h.state.Store.GetServerByUUID(mux.Vars(r)["uuid"])
		if err != nil || srv == nil {
			sendJSONError(w, "Server not found", http.StatusNotFound)
			return
		}
		if job.ServerID != srv.ID {
			sendJSONError(w, "Job not found", http.StatusNotFound)
			return
		}
		next(w, r)
	}
}

// powerActionPeek is the one field APIKeyPowerGate needs out of the body.
type powerActionPeek struct {
	Action string `json:"action"`
}

// APIKeyPowerGate enforces the power.<action> capability of the KEY itself.
//
// Power is the one route whose capability is named in the request body
// (start|stop|restart|kill), so the middleware cannot declare one up front and
// ServerPowerHandler resolves it in-handler instead - against the OWNER's
// authority, taken from the context. That is the correct owner-side check and
// says nothing at all about what the KEY was minted for: without this gate an
// ADMIN's key minted for console.send could start servers, because its owner
// resolves as an admin.
//
// It has to read the body to learn the action, so it puts the bytes back before
// delegating.
func (h *APIKeysHandler) APIKeyPowerGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := APIKeyFromContext(r)
		if key == nil {
			sendJSONError(w, "Route misconfigured: power gate without key auth", http.StatusInternalServerError)
			return
		}
		// A power request is a two-field JSON object. Anything longer is not one,
		// and capping the read keeps a large body from being buffered here just
		// to peek at it.
		body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			sendJSONError(w, "Invalid body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		var peek powerActionPeek
		if err := json.Unmarshal(body, &peek); err != nil {
			sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		// The action itself is validated by ServerPowerHandler. An unknown one
		// resolves to a capability no key can hold, so it is refused here first,
		// which is the safe order.
		if !authz.ResolveAPIKey(key.Scope.Permissions, apiKeyServerAllowed(r)).HasCap("power." + peek.Action) {
			sendJSONError(w, "Key lacks required permission", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// ListExternalServers GET /api/external/servers - the servers this KEY may act
// on: the servers of its owner, narrowed to the allowlist of the key.
//
// Deliberately not gated on a capability, because the allowlist is a narrower
// boundary than any capability check could be here. overview.read is a SERVER
// capability and this route names no server, so ResolveAPIKey would not grant
// it (Stage 0's rule: SERVER caps never count where there is no server to have
// scoped them to). Requiring it would make the route permanently unusable;
// leaving it out bounds the response to exactly the UUIDs the key was already
// minted for.
//
// The owner is resolved with their REAL admin flag. Hard-coding false here read
// as a safeguard and was not one: the allowlist above already bounds the answer
// - an empty scope returns early and a non-empty one is intersected below, so
// this query can never widen past the UUIDs the key was minted for. What the
// hard-false actually did was hide servers the key demonstrably works on: an
// operator's support key scoped to a customer's server answered 200 on
// /external/servers/{uuid} (the middleware resolves the owner as the admin they
// are) while this listing called the same key empty. A listing that disagrees
// with the routes it is a directory for is worse than no listing.
func (h *APIKeysHandler) ListExternalServers(w http.ResponseWriter, r *http.Request) {
	ownerID := APIKeyCallerID(r)
	if ownerID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	allowed := APIKeyAllowedServers(r)
	if len(allowed) == 0 {
		// A key with no servers in scope can address none of them, so an empty
		// list is the honest answer - and it skips the query.
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "servers": []models.Server{}})
		return
	}
	inScope := make(map[string]bool, len(allowed))
	for _, u := range allowed {
		inScope[u] = true
	}
	owner, err := h.state.Store.GetUserByID(ownerID)
	if err != nil || owner == nil {
		sendJSONError(w, "Key owner is no longer valid", http.StatusUnauthorized)
		return
	}
	servers, err := h.state.Store.ListServersForUser(ownerID, owner.IsAdmin)
	if err != nil {
		sendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}
	out := make([]models.Server, 0, len(allowed))
	for _, s := range servers {
		if inScope[s.UUID] {
			out = append(out, s)
		}
	}
	redactNodeAddress(h.state, out, owner.IsAdmin, ownerID)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "servers": out})
}

// GetExternalServer GET /api/external/servers/{uuid} - the record of one
// server. The allowlist of the key and the live overview.read of its owner were
// both checked by the middleware, so this only has to load and answer.
func (h *APIKeysHandler) GetExternalServer(w http.ResponseWriter, r *http.Request) {
	srv, err := h.state.Store.GetServerByUUID(mux.Vars(r)["uuid"])
	if err != nil || srv == nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	ownerID := APIKeyCallerID(r)
	owner, oerr := h.state.Store.GetUserByID(ownerID)
	redactNodeAddressOne(h.state, srv, oerr == nil && owner != nil && owner.IsAdmin, ownerID)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "server": srv})
}
