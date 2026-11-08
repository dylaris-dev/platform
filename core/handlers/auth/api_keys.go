package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"dylaris-core/authz"
	"dylaris-core/handlers/audit"
	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/reauth"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/demo"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// API key surface. Owner manages keys per-account. Each key has a
// scope (server UUIDs + permission names). The external RCON middleware
// validates Authorization: Bearer <key> against this store.

type APIKeysHandler struct {
	d Deps
	// rateLimiter is a per-key sliding-window counter, in-memory. Acceptable
	// for V1 — Multi-Core deployments share the lease but not the limiter;
	// the only real risk is "2x burst" if a script flaps between Cores. A
	// distributed Redis-backed limiter is wired in when that matters.
	rateLimiter *apiKeyRateLimiter
	// ipLimiter throttles per source IP BEFORE the key lookup so invalid-key
	// guessing can't hammer the DB hash lookup unbounded.
	ipLimiter *httpx.IPRateLimiter
}

func NewAPIKeysHandler(d Deps) *APIKeysHandler {
	return &APIKeysHandler{d: d, rateLimiter: newAPIKeyRateLimiter(), ipLimiter: httpx.NewIPRateLimiter()}
}

type createAPIKeyRequest struct {
	Name        string   `json:"name"`
	Servers     []string `json:"servers"`
	Permissions []string `json:"permissions"`
	RatePerMin  int      `json:"ratePerMin,omitempty"`
	// 0 or absent = never expires.
	ExpiresInDays int `json:"expiresInDays,omitempty"`
	// A key outlives the session that minted it: it authenticates by its own
	// hash, so the password change that kills every session leaves it working.
	// See requireReauth.
	reauth.ReauthFields
}

type createAPIKeyResponse struct {
	Success   bool           `json:"success"`
	APIKey    *models.APIKey `json:"apiKey"`
	Plaintext string         `json:"plaintext"`
	Message   string         `json:"message"`
}

// Create POST /api/me/api-keys - mints a key and returns its plaintext exactly
// once. A caller cannot hand a key more access than they hold themselves:
// every requested capability is checked against their own access to each
// server in scope, so an over-broad scope is refused with 403 rather than
// quietly narrowed.
func (h *APIKeysHandler) Create(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		httpx.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req createAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if msg := apiKeyNameProblem(req.Name); msg != "" {
		httpx.SendJSONError(w, msg, http.StatusBadRequest)
		return
	}
	if len(req.Permissions) == 0 {
		httpx.SendJSONError(w, "At least one permission required", http.StatusBadRequest)
		return
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > maxKeyExpiryDays {
		httpx.SendJSONError(w, fmt.Sprintf("expiresInDays must be between 1 and %d, or 0 for never", maxKeyExpiryDays), http.StatusBadRequest)
		return
	}

	for _, p := range req.Permissions {
		if !authz.ValidKeyCap(p) {
			httpx.SendJSONError(w, "Unknown permission: "+p, http.StatusBadRequest)
			return
		}
	}
	// Delegation subset check: a non-admin may only mint a key carrying SERVER
	// caps they themselves hold on each targeted server (owner short-circuit or an
	// explicit grant), so a key can never exceed its creator. OWNER caps act on the
	// creator's own realm (always held) and PANEL caps are already rejected above,
	// so only SERVER caps need a per-server check. Admin holds everything, so admin
	// keys stay unrestricted.
	isAdmin := r.Context().Value("isAdmin").(bool)
	if !isAdmin {
		// Operator gate, checked before the delegation maths below. An operator
		// who has not turned user keys on gets none minted, whatever the caller
		// is otherwise entitled to.
		// A gateway-only key of a route-only customer is exempt from the gate and
		// the whitelist; the same call decides it again on every request.
		viaEntitlement := h.gatewayKeyEntitled(r, userID, req.Permissions)
		// Nothing in a gateway-only key reads a server scope, so a list would
		// only suggest a restriction that does not exist.
		if viaEntitlement && len(req.Servers) > 0 {
			httpx.SendJSONError(w, "Gateway keys cannot be limited to servers.", http.StatusBadRequest)
			return
		}
		if !viaEntitlement && !h.userKeysAllowed(r) {
			httpx.SendJSONError(w, "API keys are not enabled for users on this platform", http.StatusForbidden)
			return
		}
		if allowed := h.allowedUserKeyCaps(r); allowed != nil && !viaEntitlement {
			for _, p := range req.Permissions {
				if !allowed[p] {
					httpx.SendJSONError(w, "This platform does not allow that permission on a user key: "+p, http.StatusForbidden)
					return
				}
			}
		}
		username := r.Context().Value("username").(string)
		identity := authz.Identity{UserID: userID, Username: username, IsAdmin: false}
		var serverCaps []string
		for _, p := range req.Permissions {
			if c, ok := authz.Get(p); ok && c.Scope == authz.ScopeServer {
				serverCaps = append(serverCaps, p)
			}
		}
		// A key carrying SERVER caps but scoped to no servers can never use them
		// (the middleware's per-request server-scope gate denies it), which would
		// also mean the per-server subset loop below applies no gate at all. Reject
		// it at creation rather than mint a key whose only safeguard is the use-time
		// middleware (defense in depth).
		if len(serverCaps) > 0 && len(req.Servers) == 0 {
			httpx.SendJSONError(w, "Server capabilities require at least one server in scope", http.StatusBadRequest)
			return
		}
		for _, serverUUID := range req.Servers {
			srv, err := h.d.Store.GetServerByUUID(serverUUID)
			if err != nil {
				httpx.SendJSONError(w, "Server not found: "+serverUUID, http.StatusBadRequest)
				return
			}
			res, rerr := h.d.Authz.Resolve(identity, srv.ID)
			// Resolve answers "no capabilities", not an error, for a server the
			// caller cannot reach, so the error alone let any server's uuid into
			// a key's scope. Holding something on the server is the floor for naming it.
			if rerr != nil || !res.HasAnyServerCap() {
				httpx.SendJSONError(w, "No access to server: "+serverUUID, http.StatusForbidden)
				return
			}
			for _, capID := range serverCaps {
				if !res.HasCap(capID) {
					httpx.SendJSONError(w, "No "+capID+" access to server: "+serverUUID, http.StatusForbidden)
					return
				}
			}
		}
	}

	// The name is what the audit log shows for everything the key does, so two
	// live keys of one account must not share it.
	existing, err := h.d.Store.ListAPIKeysByUser(userID)
	if err != nil {
		httpx.SendJSONError(w, "Failed to check existing keys", http.StatusInternalServerError)
		return
	}
	for _, k := range existing {
		live := k.RevokedAt == nil && (k.ExpiresAt == nil || time.Now().Before(*k.ExpiresAt))
		if live && strings.EqualFold(k.Name, req.Name) {
			httpx.SendJSONError(w, "You already have a key with this name", http.StatusConflict)
			return
		}
	}

	// Re-authenticate immediately before minting, and after every cheap check.
	// What is created here survives the password change that kills every session,
	// so the question is not what this session may do but whether the account
	// holder is the one asking.
	//
	// Last rather than first on purpose: nothing above this line writes anything,
	// and a bcrypt comparison in front of the request validation would let a
	// malformed body buy CPU time. Nothing is persisted before it either way.
	if rerr := reauth.RequireReauth(h.d.Store, ReauthCodeVerifier(h.d), userID, req.Password, req.Code); rerr != nil {
		reauth.WriteReauthError(w, rerr)
		return
	}

	plaintext, err := services.GeneratePlaintextKey()
	if err != nil {
		httpx.SendJSONError(w, "Failed to mint key", http.StatusInternalServerError)
		return
	}
	rate := req.RatePerMin
	if rate <= 0 {
		rate = 60
	}
	// Any positive number used to be taken, so a key could carry a limit of a
	// million a minute - the per-key limit switched off, leaving only the
	// per-IP one, which a caller with several addresses does not meet.
	if rate > maxKeyRatePerMin {
		httpx.SendJSONError(w, fmt.Sprintf("ratePerMin can be at most %d", maxKeyRatePerMin), http.StatusBadRequest)
		return
	}
	k := &models.APIKey{
		UserID:  userID,
		Name:    req.Name,
		KeyHash: services.HashAPIKey(plaintext),
		Scope: models.APIKeyScope{
			Servers:     req.Servers,
			Permissions: req.Permissions,
		},
		RatePerMin: rate,
	}
	if req.ExpiresInDays > 0 {
		exp := time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		k.ExpiresAt = &exp
	}
	id, err := h.d.Store.CreateAPIKey(k)
	if err != nil {
		httpx.SendJSONError(w, "Failed to save key", http.StatusInternalServerError)
		return
	}
	k.ID = id
	// A key counts like an invited member: the servers it can reach have
	// auditing on from the moment it exists, so the owner sees it switched on
	// before the key is first used.
	for _, u := range req.Servers {
		if srv, serr := h.d.Store.GetServerByUUID(u); serr == nil && srv != nil {
			audit.EnableServerAuditIfNeeded(h.d.Store, srv.ID)
		}
	}
	audit.LogIdentity(h.d.Store, r, audit.AuditEventAPIKeyCreated, userID, userID, map[string]interface{}{
		"keyId":       id,
		"name":        k.Name,
		"servers":     req.Servers,
		"permissions": req.Permissions,
		"ratePerMin":  rate,
		"expiresAt":   k.ExpiresAt,
	})
	json.NewEncoder(w).Encode(createAPIKeyResponse{
		Success:   true,
		APIKey:    k,
		Plaintext: "dyl_" + plaintext,
		Message:   "Store this key now — it will not be shown again.",
	})
}

// apiKeyOptions tells the panel what this caller may actually mint, so the page
// does not offer choices the operator gate would refuse at Create.
//
// AllowedCaps is nil when the operator has set no whitelist, which means "no
// extra restriction" - the same distinction UserAPIKeyAllowedCaps draws. An
// empty JSON array would read as "nothing allowed" on the panel side, so it
// must stay null.
type apiKeyOptions struct {
	Enabled     bool     `json:"enabled"`
	AllowedCaps []string `json:"allowedCaps"`
	// KeyCaps is every capability a key may carry at all (authz.KeyCaps).
	KeyCaps []string `json:"keyCaps"`
}

// List GET /api/me/api-keys - the calling user's own API keys, plus what they
// are allowed to mint.
func (h *APIKeysHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		httpx.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	keys, err := h.d.Store.ListAPIKeysByUser(userID)
	if err != nil {
		httpx.SendJSONError(w, "Failed to list keys", http.StatusInternalServerError)
		return
	}
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	opts := apiKeyOptions{Enabled: isAdmin || h.userKeysAllowed(r), KeyCaps: authz.KeyCaps()}
	if !isAdmin && h.d.FeatureFlags != nil {
		opts.AllowedCaps = h.d.FeatureFlags.UserAPIKeyAllowedCaps(r.Context())
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"keys":    keys,
		"options": opts,
	})
}

// Revoke DELETE /api/me/api-keys/{id} - revokes one of the caller's own keys.
// Another user's key answers 404, because the delete is scoped by user id in
// the same statement and a wrong owner is indistinguishable from a wrong id.
func (h *APIKeysHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		httpx.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	keyID, _ := strconv.Atoi(mux.Vars(r)["id"])
	if err := h.d.Store.RevokeAPIKey(keyID, userID); err != nil {
		httpx.SendJSONError(w, "Key not found", http.StatusNotFound)
		return
	}
	meta := map[string]interface{}{"keyId": keyID}
	if keys, lerr := h.d.Store.ListAPIKeysByUser(userID); lerr == nil {
		for _, k := range keys {
			if k.ID == keyID {
				meta["name"] = k.Name
			}
		}
	}
	audit.LogIdentity(h.d.Store, r, audit.AuditEventAPIKeyRevoked, userID, userID, meta)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiKeyRollGrace is how long the old hash of a rolled key keeps working, so a
// provider can swap the key in their config without downtime.
const apiKeyRollGrace = 24 * time.Hour

type rollAPIKeyRequest struct {
	reauth.ReauthFields
}

// Roll POST /api/me/api-keys/{id}/roll - replaces the secret of one of the
// caller's own keys. The new key is returned once; the old one keeps working for
// 24 h, and a second roll inside that window drops it. Another user's key
// answers 404, like Revoke. Re-authenticates like Create: a roll mints a
// credential that outlives the session.
func (h *APIKeysHandler) Roll(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		httpx.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	keyID, _ := strconv.Atoi(mux.Vars(r)["id"])
	var req rollAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	keys, err := h.d.Store.ListAPIKeysByUser(userID)
	if err != nil {
		httpx.SendJSONError(w, "Failed to load keys", http.StatusInternalServerError)
		return
	}
	var key *models.APIKey
	for i := range keys {
		if keys[i].ID == keyID {
			key = &keys[i]
		}
	}
	if key == nil || key.RevokedAt != nil || (key.ExpiresAt != nil && time.Now().After(*key.ExpiresAt)) {
		httpx.SendJSONError(w, "Key not found", http.StatusNotFound)
		return
	}
	if rerr := reauth.RequireReauth(h.d.Store, ReauthCodeVerifier(h.d), userID, req.Password, req.Code); rerr != nil {
		reauth.WriteReauthError(w, rerr)
		return
	}
	h.rollKey(w, r, key, userID)
}

// RollSelf POST /api/external/api-key/roll - a key replaces its own secret. It
// needs no capability: any valid key may roll itself, and it can only ever touch
// the row it authenticated as (APIKeyOwnerRoute with an empty capability, the
// narrowest shape that accepts every key). The old hash being usable during the
// grace must not let a leaked old key take the account over, so only the
// CURRENT key may roll.
func (h *APIKeysHandler) RollSelf(w http.ResponseWriter, r *http.Request) {
	key := reqctx.APIKeyFromContext(r)
	if key == nil {
		httpx.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if key.ViaPrevious {
		httpx.SendJSONError(w, "Roll with the current key, not the previous one", http.StatusForbidden)
		return
	}
	h.rollKey(w, r, key, key.UserID)
}

func (h *APIKeysHandler) rollKey(w http.ResponseWriter, r *http.Request, key *models.APIKey, userID string) {
	plaintext, err := services.GeneratePlaintextKey()
	if err != nil {
		httpx.SendJSONError(w, "Failed to mint key", http.StatusInternalServerError)
		return
	}
	until := time.Now().Add(apiKeyRollGrace)
	if err := h.d.Store.RollAPIKey(r.Context(), key.ID, userID, key.KeyHash, services.HashAPIKey(plaintext), apiKeyRollGrace); err != nil {
		if errors.Is(err, store.ErrAPIKeyRollConflict) {
			httpx.SendJSONError(w, "The key was rolled meanwhile; use the newest key.", http.StatusConflict)
			return
		}
		httpx.SendJSONError(w, "Failed to roll key", http.StatusInternalServerError)
		return
	}
	audit.LogIdentity(h.d.Store, r, audit.AuditEventAPIKeyRolled, userID, userID, map[string]interface{}{
		"keyId": key.ID,
		"name":  key.Name,
	})
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":            true,
		"key":                "dyl_" + plaintext,
		"id":                 key.ID,
		"previousValidUntil": until.UTC().Format(time.RFC3339),
	})
}

// --- API key middleware ---

// keyRouteShape says what kind of route the middleware is guarding. It is
// declared at the route, never inferred.
//
// It used to be inferred from whether the path happened to carry a {uuid}:
//
//	serverAllowed := uuidVar == "" || key.Scope.AllowsServer(uuidVar)
//	func ownerStillHolds(...) { if serverUUID == "" { return true } }
//
// Both guards therefore switched themselves OFF for any route without one. That
// was invisible while the single key route was server-scoped, and would have
// become a hole the moment a list or owner-scoped route was added: every SERVER
// capability on the key would have counted, for every server, regardless of the
// key's allowlist, and the owner re-check would have been skipped entirely.
// Declaring the shape makes the two cases different code paths instead of an
// accident of path syntax.
type keyRouteShape int

const (
	// keyRouteServer addresses one server by UUID. The path MUST carry {uuid};
	// the key's allowlist gates it and the owner re-check runs against it.
	keyRouteServer keyRouteShape = iota
	// keyRouteOwner acts on the caller's own realm and names no server. SERVER
	// capabilities never count here - there is no server to have scoped them to
	// - so the handler must scope its own query with APIKeyCallerID and, where
	// it lists servers, APIKeyAllowedServers.
	keyRouteOwner
)

// APIKeyCallerID is the realm an owner-scoped key route may act on: the key's
// creator, and nobody else. Every owner-scoped handler must scope its query
// with this.
//
// This is the binding authz/apikey.go warns about: a key's OWNER capabilities
// are not tied to a realm by the resolver, so without scoping here, a key
// carrying one could act on another owner's data.
func APIKeyCallerID(r *http.Request) string {
	if k := reqctx.APIKeyFromContext(r); k != nil {
		return k.UserID
	}
	return ""
}

// APIKeyAllowedServers is the key's server allowlist. A listing route must
// filter with it: owner scoping alone would return every server the owner has,
// which is wider than the key was minted for.
func APIKeyAllowedServers(r *http.Request) []string {
	if k := reqctx.APIKeyFromContext(r); k != nil {
		return k.Scope.Servers
	}
	return nil
}

// userKeysAllowed reports whether a NON-ADMIN may hold a key on this platform.
// Nil flags mean "not configured", which reads as the default (off) rather than
// as permission - a missing dependency must never open a gate.
func (h *APIKeysHandler) userKeysAllowed(r *http.Request) bool {
	if h.d.FeatureFlags == nil {
		return false
	}
	return h.d.FeatureFlags.UserAPIKeysEnabled(r.Context())
}

// gatewayKeyEntitled reports whether a NON-ADMIN owner may hold a key with these
// permissions without the operator's user-key switch: every permission is a
// gateway one (or usage.read), at least one is gateway.*, route-only is on, and
// the owner's entitlement carries RouteOnly. Mint and ownerStillHolds both call
// this, so they cannot drift; any error answers false (fail closed), and the
// entitlement is re-read on every request so a lapse stops the key.
func (h *APIKeysHandler) gatewayKeyEntitled(r *http.Request, ownerID string, perms []string) bool {
	hasGateway := false
	for _, p := range perms {
		switch p {
		case "gateway.read", "gateway.write":
			hasGateway = true
		case "usage.read":
		default:
			return false
		}
	}
	// Without a store plane (self-hosted) there is nothing to be entitled by:
	// EffectiveEntitlement would read that as unlimited, so these keys follow
	// the ordinary user-key gate there.
	if !hasGateway || !h.d.StoreEnabled || h.d.FeatureFlags == nil || h.d.Store == nil || !h.d.FeatureFlags.IsRouteOnlyEnabled(r.Context()) {
		return false
	}
	ent, err := services.EffectiveEntitlement(h.d.Store, ownerID, time.Now(), h.d.StoreEnabled, false)
	return err == nil && ent.RouteOnly
}

// allowedUserKeyCaps is the operator's capability whitelist for user keys, or
// nil when they have not set one. Nil is "no extra restriction", NOT "none":
// see UserAPIKeyAllowedCaps.
func (h *APIKeysHandler) allowedUserKeyCaps(r *http.Request) map[string]bool {
	if h.d.FeatureFlags == nil {
		return nil
	}
	list := h.d.FeatureFlags.UserAPIKeyAllowedCaps(r.Context())
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]bool, len(list))
	for _, c := range list {
		out[c] = true
	}
	return out
}

// APIKeyServerRoute guards a route that addresses one server by {uuid}.
func (h *APIKeysHandler) APIKeyServerRoute(requiredPerm string) func(http.HandlerFunc) http.HandlerFunc {
	return h.apiKeyMiddleware(keyRouteServer, requiredPerm)
}

// APIKeyOwnerRoute guards a route that acts on the caller's own realm and names
// no server.
func (h *APIKeysHandler) APIKeyOwnerRoute(requiredPerm string) func(http.HandlerFunc) http.HandlerFunc {
	return h.apiKeyMiddleware(keyRouteOwner, requiredPerm)
}

// apiKeyMiddleware validates Authorization: Bearer <dyl_...> and injects the
// resolved key into the request context.
func (h *APIKeysHandler) apiKeyMiddleware(shape keyRouteShape, requiredPerm string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				httpx.SendJSONError(w, "Missing Authorization", http.StatusUnauthorized)
				return
			}
			plaintext := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
			plaintext = strings.TrimPrefix(plaintext, "dyl_")
			if plaintext == "" {
				httpx.SendJSONError(w, "Invalid key", http.StatusUnauthorized)
				return
			}
			// Failed attempts are throttled by source IP before the DB lookup, so
			// invalid-key guessing can't hammer GetAPIKeyByHash unbounded. Only
			// failures are charged: counting valid requests too capped every key
			// at this number from one host, below the per-key rate it was minted
			// with.
			ip := httpx.ClientIP(r)
			if h.ipLimiter.Exhausted(ip, keyFailuresPerMin) {
				w.Header().Set("Retry-After", "60")
				httpx.SendJSONError(w, "Too many requests", http.StatusTooManyRequests)
				return
			}
			key, err := h.d.Store.GetAPIKeyByHash(services.HashAPIKey(plaintext))
			if err != nil {
				h.ipLimiter.Allow(ip, keyFailuresPerMin)
				httpx.SendJSONError(w, "Invalid key", http.StatusUnauthorized)
				return
			}

			// From here on the request belongs to a real key, and what it
			// comes to is on the record (auditKeyRequest). The audit slot lets
			// a handler that writes its own entry say so, so the request is not
			// recorded twice.
			sw := &keyStatusWriter{ResponseWriter: w}
			w = sw
			r = r.WithContext(authz.WithAuditSlot(r.Context()))
			uuidVar := mux.Vars(r)["uuid"]
			defer func() {
				// A refusal is a failed attempt whatever the key: a valid key
				// pointed at what it may not touch must be throttled like a
				// guess, or each refusal is a free DB round and an audit write.
				if keyRefusal(sw.status) {
					h.ipLimiter.Allow(ip, keyFailuresPerMin)
				}
				h.auditKeyRequest(r, key, shape, uuidVar, requiredPerm, sw.status)
			}()

			if key.RevokedAt != nil {
				httpx.SendJSONError(w, "Key revoked", http.StatusUnauthorized)
				return
			}
			if key.ExpiresAt != nil && time.Now().After(*key.ExpiresAt) {
				httpx.SendJSONError(w, "Key expired", http.StatusUnauthorized)
				return
			}

			// serverAllowed decides whether the key's SERVER capabilities count
			// for this request. On an owner route it is false unconditionally:
			// there is no server, so nothing could have scoped them.
			serverAllowed := false
			switch shape {
			case keyRouteServer:
				if uuidVar == "" {
					// A server route registered without {uuid} is a wiring
					// mistake. Failing loudly beats silently granting every
					// SERVER cap the key holds, which is what inferring the
					// shape from the path used to do.
					httpx.SendJSONError(w, "Route misconfigured: server-scoped key route without a server", http.StatusInternalServerError)
					return
				}
				serverAllowed = key.Scope.AllowsServer(uuidVar)
				if !serverAllowed {
					httpx.SendJSONError(w, "Key not scoped to this server", http.StatusForbidden)
					return
				}
			case keyRouteOwner:
				uuidVar = "" // owner routes never address a server
			}

			// Authorization through the SAME chokepoint as session auth: the key's caps
			// become a Resolution and the required cap is checked via HasCap. A key
			// holds exactly its minted caps - no admin/owner short-circuit, no panel
			// caps - so this generalizes enforcement to any SERVER/OWNER cap, not just
			// rcon.exec, while denying anything the key does not carry.
			if requiredPerm != "" {
				if !authz.ResolveAPIKey(key.Scope.Permissions, serverAllowed).HasCap(requiredPerm) {
					httpx.SendJSONError(w, "Key lacks required permission", http.StatusForbidden)
					return
				}
			}
			// Always, not only when a capability is declared: the operator gate
			// inside must apply to every key-authed request, and a route without
			// a required capability is still a route.
			if !h.ownerStillHolds(w, r, key, uuidVar, requiredPerm) {
				return
			}
			if !h.rateLimiter.allow(key.ID, key.RatePerMin) {
				w.Header().Set("Retry-After", "60")
				httpx.SendJSONError(w, "Rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			// Best-effort last-used stamp, throttled to ~once/min/key so a
			// hammered key can't spawn an unbounded goroutine fan-out.
			if h.rateLimiter.shouldTouch(key.ID) {
				go h.d.Store.TouchAPIKey(key.ID)
			}
			// Inject the key so an owner-scoped handler can bind its query to
			// the key's realm and allowlist. Without this the handler has no way
			// to tell whose data it may return.
			r = r.WithContext(reqctx.WithAPIKey(r.Context(), key, serverAllowed))

			// The demo account is read-only, and AuthMiddleware is where that is
			// enforced - which a key request never passes through.
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if demo, derr := demo.IsAccountChecked(h.d.Store, h.d.StoreEnabled, key.UserID); derr != nil || demo {
					httpx.SendJSONError(w, "This account is read-only", http.StatusForbidden)
					return
				}
			}
			next(w, r)
		}
	}
}

// ownerStillHolds re-checks the key's capability against what its OWNER holds
// RIGHT NOW, and reports whether the request may continue (it writes the
// response itself when it may not).
//
// Create's doc comment promises "a caller cannot hand a key more access than
// they hold themselves", and that was true at mint time and never again. The
// key's authority is the caps frozen into its scope row, so removing someone
// from a server took away their session's access and left every key they had
// minted working. Revoking a member is exactly when you most need the credential
// they created to stop working, and it was the one thing that did not.
//
// The owner is loaded rather than assumed non-admin because an admin's key is
// deliberately unrestricted (Create skips the delegation check for admins), and
// resolving them as an ordinary user would refuse keys that are supposed to
// work. Resolve's own admin and owner short-circuits then answer those cases
// without touching a grant table.
//
// Both route shapes are checked. A server route resolves against that server;
// an owner route resolves against serverID 0, the key holder's own realm. The
// owner-route case is not a formality: the operator gate above runs for every
// key-authed request, and an account that has lost its admin flag resolves
// differently than it did at mint time. Both lookups fail closed, matching
// AuthMiddleware: a vanished account or server is a 401, and a database fault
// is a 503 rather than a 403 that would read as a permissions problem.
func (h *APIKeysHandler) ownerStillHolds(w http.ResponseWriter, r *http.Request, key *models.APIKey, serverUUID, requiredPerm string) bool {
	if h.d.Authz == nil || h.d.Store == nil {
		return true
	}
	owner, err := h.d.Store.GetUserByID(key.UserID)
	// An anonymized account's keys are revoked with it (AnonymizeUser); this
	// covers a key that slipped past that, or an account anonymized before.
	if err != nil || owner == nil || owner.DeletionStatus == "anonymized" {
		httpx.SendJSONError(w, "Key owner is no longer valid", http.StatusUnauthorized)
		return false
	}
	// Operator gate at USE time, not only at mint. Enforcing it only when a key
	// is created would leave every key minted before the switch was turned off
	// working afterwards, which is not what an operator who turned it off means.
	// The owner is loaded here anyway, so this costs nothing extra.
	viaEntitlement := !owner.IsAdmin && h.gatewayKeyEntitled(r, owner.ID, key.Scope.Permissions)
	if !owner.IsAdmin && !viaEntitlement && !h.userKeysAllowed(r) {
		httpx.SendJSONError(w, "API keys are not enabled for users on this platform", http.StatusForbidden)
		return false
	}
	// A key in use is an account in use, as a login is: an automation-only
	// member must not be anonymized for not signing in. After the gate: a key
	// the platform refuses is not in use.
	if owner.DeletionStatus == "pending_deletion" {
		if err := h.d.Store.CancelUserDeletion(owner.ID); err == nil {
			audit.LogIdentityAs(h.d.Store, r, key, audit.AuditEventDeletionCancelledByAPIKey, owner.ID, owner.ID, nil)
		}
	}

	if requiredPerm == "" {
		return true // nothing to re-check; the gate above was the point
	}

	// The operator's list of what a user key may carry, applied now as well
	// as at mint: a capability taken off the list kept working on every key
	// already minted with it, and so did the keys of a demoted admin, whose
	// capabilities were never checked against the list at all.
	if !owner.IsAdmin && !viaEntitlement {
		if allowed := h.allowedUserKeyCaps(r); allowed != nil && !allowed[requiredPerm] {
			httpx.SendJSONError(w, "This platform no longer allows that permission on a user key", http.StatusForbidden)
			return false
		}
	}

	// serverID 0 means "the owner's own realm", which is exactly what an
	// owner-scoped route acts on. The empty-serverUUID case used to return true
	// here, so on such a route the check did not run at all and a key kept
	// working after its owner lost the access it was minted from. Now both
	// shapes resolve; only the scope differs.
	serverID := 0
	if serverUUID != "" {
		srv, serr := h.d.Store.GetServerByUUID(serverUUID)
		if serr != nil || srv == nil {
			httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
			return false
		}
		serverID = srv.ID
	}

	res, err := h.d.Authz.Resolve(authz.Identity{
		UserID:   owner.ID,
		Username: owner.Username,
		IsAdmin:  owner.IsAdmin,
	}, serverID)
	if err != nil {
		httpx.SendJSONError(w, "Could not verify key authorization", http.StatusServiceUnavailable)
		return false
	}
	if !res.HasCap(requiredPerm) {
		httpx.SendJSONError(w, "The key's owner no longer has this access", http.StatusForbidden)
		return false
	}
	return true
}

// --- Rate limiter ---

type rateBucket struct {
	count int
	reset time.Time
}

type apiKeyRateLimiter struct {
	mu        sync.Mutex
	buckets   map[int]*rateBucket
	lastTouch map[int]time.Time
}

func newAPIKeyRateLimiter() *apiKeyRateLimiter {
	return &apiKeyRateLimiter{buckets: make(map[int]*rateBucket), lastTouch: make(map[int]time.Time)}
}

// shouldTouch returns true at most once per minute per key, gating the
// last-used DB stamp so a hot key doesn't spawn a goroutine + UPDATE on
// every request.
func (l *apiKeyRateLimiter) shouldTouch(keyID int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if t, ok := l.lastTouch[keyID]; ok && now.Sub(t) < time.Minute {
		return false
	}
	l.lastTouch[keyID] = now
	return true
}

// allow returns false when the per-key budget is exhausted for the current
// 60-second window. Cheap sliding-window-counter: roll the bucket on each
// call when its reset has passed.
func (l *apiKeyRateLimiter) allow(keyID, perMin int) bool {
	if perMin <= 0 {
		perMin = 60
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[keyID]
	now := time.Now()
	if !ok || now.After(b.reset) {
		l.buckets[keyID] = &rateBucket{count: 1, reset: now.Add(time.Minute)}
		return true
	}
	if b.count >= perMin {
		return false
	}
	b.count++
	return true
}

// maxKeyRatePerMin bounds the per-key request rate a key may be minted with.
const maxKeyRatePerMin = 600

// maxKeyExpiryDays bounds the expiry a key may be minted with.
const maxKeyExpiryDays = 365

// apiKeyNameProblem says what is wrong with a key name, or "". The name is
// what the audit log shows beside the owner for everything the key does, so it
// is kept short and printable: no newline or control character can make an
// entry read as something else.
func apiKeyNameProblem(name string) string {
	if name == "" {
		return "Name required"
	}
	if utf8.RuneCountInString(name) > 64 {
		return "Name can be at most 64 characters"
	}
	for _, c := range name {
		if !unicode.IsPrint(c) {
			return "Name may only contain printable characters"
		}
		// The log shows "key: <name> (owner <user>)"; a parenthesis in the name
		// could fake the owner part.
		if c == '(' || c == ')' {
			return "Name may not contain parentheses"
		}
	}
	return ""
}

// keyStatusWriter remembers the status a key-authed handler answered with.
type keyStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *keyStatusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *keyStatusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *keyStatusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
