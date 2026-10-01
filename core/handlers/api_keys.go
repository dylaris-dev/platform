package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dylaris-core/authz"
	"dylaris-core/models"

	"github.com/gorilla/mux"
)

// API key surface. Owner manages keys per-account. Each key has a
// scope (server UUIDs + permission names). The external RCON middleware
// validates Authorization: Bearer <key> against this store.

type APIKeysHandler struct {
	state *AppState
	// rateLimiter is a per-key sliding-window counter, in-memory. Acceptable
	// for V1 — Multi-Core deployments share the lease but not the limiter;
	// the only real risk is "2x burst" if a script flaps between Cores. A
	// distributed Redis-backed limiter is wired in when that matters.
	rateLimiter *apiKeyRateLimiter
	// ipLimiter throttles per source IP BEFORE the key lookup so invalid-key
	// guessing can't hammer the DB hash lookup unbounded.
	ipLimiter *IPRateLimiter
}

func NewAPIKeysHandler(state *AppState) *APIKeysHandler {
	return &APIKeysHandler{state: state, rateLimiter: newAPIKeyRateLimiter(), ipLimiter: NewIPRateLimiter()}
}

type createAPIKeyRequest struct {
	Name        string   `json:"name"`
	Servers     []string `json:"servers"`
	Permissions []string `json:"permissions"`
	RatePerMin  int      `json:"ratePerMin,omitempty"`
	// A key outlives the session that minted it: it authenticates by its own
	// hash, so the password change that kills every session leaves it working.
	// See requireReauth.
	reauthFields
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
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req createAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		sendJSONError(w, "Name required", http.StatusBadRequest)
		return
	}
	if len(req.Permissions) == 0 {
		sendJSONError(w, "At least one permission required", http.StatusBadRequest)
		return
	}

	for _, p := range req.Permissions {
		if !authz.ValidKeyCap(p) {
			sendJSONError(w, "Unknown permission: "+p, http.StatusBadRequest)
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
		if !h.userKeysAllowed(r) {
			sendJSONError(w, "API keys are not enabled for users on this platform", http.StatusForbidden)
			return
		}
		if allowed := h.allowedUserKeyCaps(r); allowed != nil {
			for _, p := range req.Permissions {
				if !allowed[p] {
					sendJSONError(w, "This platform does not allow that permission on a user key: "+p, http.StatusForbidden)
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
			sendJSONError(w, "Server capabilities require at least one server in scope", http.StatusBadRequest)
			return
		}
		for _, serverUUID := range req.Servers {
			srv, err := h.state.Store.GetServerByUUID(serverUUID)
			if err != nil {
				sendJSONError(w, "Server not found: "+serverUUID, http.StatusBadRequest)
				return
			}
			res, rerr := h.state.Authz.Resolve(identity, srv.ID)
			// Resolve answers "no capabilities", not an error, for a server the
			// caller cannot reach, so the error alone let any server's uuid into
			// a key's scope. Holding something on the server is the floor for naming it.
			if rerr != nil || !res.HasAnyServerCap() {
				sendJSONError(w, "No access to server: "+serverUUID, http.StatusForbidden)
				return
			}
			for _, capID := range serverCaps {
				if !res.HasCap(capID) {
					sendJSONError(w, "No "+capID+" access to server: "+serverUUID, http.StatusForbidden)
					return
				}
			}
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
	if rerr := requireReauth(h.state, userID, req.Password, req.Code); rerr != nil {
		writeReauthError(w, rerr)
		return
	}

	plaintext, err := generatePlaintextKey()
	if err != nil {
		sendJSONError(w, "Failed to mint key", http.StatusInternalServerError)
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
		sendJSONError(w, fmt.Sprintf("ratePerMin can be at most %d", maxKeyRatePerMin), http.StatusBadRequest)
		return
	}
	k := &models.APIKey{
		UserID:  userID,
		Name:    req.Name,
		KeyHash: HashAPIKey(plaintext),
		Scope: models.APIKeyScope{
			Servers:     req.Servers,
			Permissions: req.Permissions,
		},
		RatePerMin: rate,
	}
	id, err := h.state.Store.CreateAPIKey(k)
	if err != nil {
		sendJSONError(w, "Failed to save key", http.StatusInternalServerError)
		return
	}
	k.ID = id
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
}

// List GET /api/me/api-keys - the calling user's own API keys, plus what they
// are allowed to mint.
func (h *APIKeysHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	keys, err := h.state.Store.ListAPIKeysByUser(userID)
	if err != nil {
		sendJSONError(w, "Failed to list keys", http.StatusInternalServerError)
		return
	}
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	opts := apiKeyOptions{Enabled: isAdmin || h.userKeysAllowed(r)}
	if !isAdmin && h.state != nil && h.state.FeatureFlags != nil {
		opts.AllowedCaps = h.state.FeatureFlags.UserAPIKeyAllowedCaps(r.Context())
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
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	keyID, _ := strconv.Atoi(mux.Vars(r)["id"])
	if err := h.state.Store.RevokeAPIKey(keyID, userID); err != nil {
		sendJSONError(w, "Key not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func generatePlaintextKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// generateLinkIdentity mints a stable, unique link identity (the warp key's
// node_id). The Link derives its tunnel token deterministically from this id +
// the cluster secret, so the id must be unguessable. 16 random bytes suffice.
func generateLinkIdentity() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "link-" + hex.EncodeToString(b), nil
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

type apiKeyCtxKey struct{}

// apiKeyCtx is what the middleware injects: the key, plus whether this
// request's server scope was satisfied.
//
// serverAllowed travels with the key because a handler that has to resolve a
// capability itself - the power route, whose capability is named in the request
// body - must resolve it exactly as the middleware would have. Hardcoding true
// there would work today (a server route refuses before reaching the handler
// when the allowlist misses) and would silently grant every SERVER capability
// the day such a handler is wired to an owner-shaped route.
type apiKeyCtx struct {
	key           *models.APIKey
	serverAllowed bool
}

// APIKeyFromContext returns the key that authenticated this request, if any.
func APIKeyFromContext(r *http.Request) *models.APIKey {
	c, _ := r.Context().Value(apiKeyCtxKey{}).(*apiKeyCtx)
	if c == nil {
		return nil
	}
	return c.key
}

// apiKeyServerAllowed reports whether the key's SERVER capabilities count for
// this request. False on an owner-shaped route and on any request that did not
// come through the key middleware.
func apiKeyServerAllowed(r *http.Request) bool {
	c, _ := r.Context().Value(apiKeyCtxKey{}).(*apiKeyCtx)
	return c != nil && c.serverAllowed
}

// APIKeyCallerID is the realm an owner-scoped key route may act on: the key's
// creator, and nobody else. Every owner-scoped handler must scope its query
// with this.
//
// This is the binding authz/apikey.go warns about: a key's OWNER capabilities
// are not tied to a realm by the resolver, so without scoping here, a key
// carrying one could act on another owner's data.
func APIKeyCallerID(r *http.Request) string {
	if k := APIKeyFromContext(r); k != nil {
		return k.UserID
	}
	return ""
}

// APIKeyAllowedServers is the key's server allowlist. A listing route must
// filter with it: owner scoping alone would return every server the owner has,
// which is wider than the key was minted for.
func APIKeyAllowedServers(r *http.Request) []string {
	if k := APIKeyFromContext(r); k != nil {
		return k.Scope.Servers
	}
	return nil
}

// userKeysAllowed reports whether a NON-ADMIN may hold a key on this platform.
// Nil flags mean "not configured", which reads as the default (off) rather than
// as permission - a missing dependency must never open a gate.
func (h *APIKeysHandler) userKeysAllowed(r *http.Request) bool {
	if h.state == nil || h.state.FeatureFlags == nil {
		return false
	}
	return h.state.FeatureFlags.UserAPIKeysEnabled(r.Context())
}

// allowedUserKeyCaps is the operator's capability whitelist for user keys, or
// nil when they have not set one. Nil is "no extra restriction", NOT "none":
// see UserAPIKeyAllowedCaps.
func (h *APIKeysHandler) allowedUserKeyCaps(r *http.Request) map[string]bool {
	if h.state == nil || h.state.FeatureFlags == nil {
		return nil
	}
	list := h.state.FeatureFlags.UserAPIKeyAllowedCaps(r.Context())
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
				sendJSONError(w, "Missing Authorization", http.StatusUnauthorized)
				return
			}
			plaintext := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
			plaintext = strings.TrimPrefix(plaintext, "dyl_")
			if plaintext == "" {
				sendJSONError(w, "Invalid key", http.StatusUnauthorized)
				return
			}
			// Throttle by source IP before the DB lookup so invalid-key brute
			// force can't hammer GetAPIKeyByHash unbounded (the per-key limiter
			// below never gets a chance to run for keys that don't exist).
			if !h.ipLimiter.allow(clientIP(r), 120) {
				w.Header().Set("Retry-After", "60")
				sendJSONError(w, "Too many requests", http.StatusTooManyRequests)
				return
			}
			key, err := h.state.Store.GetAPIKeyByHash(HashAPIKey(plaintext))
			if err != nil {
				sendJSONError(w, "Invalid key", http.StatusUnauthorized)
				return
			}
			if key.RevokedAt != nil {
				sendJSONError(w, "Key revoked", http.StatusUnauthorized)
				return
			}
			if key.ExpiresAt != nil && time.Now().After(*key.ExpiresAt) {
				sendJSONError(w, "Key expired", http.StatusUnauthorized)
				return
			}

			uuidVar := mux.Vars(r)["uuid"]

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
					sendJSONError(w, "Route misconfigured: server-scoped key route without a server", http.StatusInternalServerError)
					return
				}
				serverAllowed = key.Scope.AllowsServer(uuidVar)
				if !serverAllowed {
					sendJSONError(w, "Key not scoped to this server", http.StatusForbidden)
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
					sendJSONError(w, "Key lacks required permission", http.StatusForbidden)
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
				sendJSONError(w, "Rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			// Best-effort last-used stamp, throttled to ~once/min/key so a
			// hammered key can't spawn an unbounded goroutine fan-out.
			if h.rateLimiter.shouldTouch(key.ID) {
				go h.state.Store.TouchAPIKey(key.ID)
			}
			// Inject the key so an owner-scoped handler can bind its query to
			// the key's realm and allowlist. Without this the handler has no way
			// to tell whose data it may return.
			r = r.WithContext(context.WithValue(r.Context(), apiKeyCtxKey{}, &apiKeyCtx{key: key, serverAllowed: serverAllowed}))

			// The demo account is read-only, and AuthMiddleware is where that is
			// enforced - which a key request never passes through.
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if demo, derr := isDemoAccountChecked(h.state, key.UserID); derr != nil || demo {
					sendJSONError(w, "This account is read-only", http.StatusForbidden)
					return
				}
			}

			// A server action taken with a key is on the server's audit trail,
			// as the same action taken in the panel is. The panel records it in
			// RequireCap, which these routes never pass through: a console
			// command, an RCON call or a backup trigger by key left no record.
			if c, known := authz.Get(requiredPerm); known && shape == keyRouteServer && c.Scope == authz.ScopeServer &&
				c.Verb != authz.VerbRead && r.Method != http.MethodGet && r.Method != http.MethodHead {
				sw := &keyStatusWriter{ResponseWriter: w}
				next(sw, r)
				if sw.status >= 200 && sw.status < 300 {
					if srv, serr := h.state.Store.GetServerByUUID(uuidVar); serr == nil && srv != nil {
						LogServerAudit(h.state, r, srv.ID, requiredPerm, key.UserID, "", map[string]interface{}{
							"method": r.Method,
							"path":   r.URL.Path,
							"apiKey": key.ID,
						})
					}
				}
				return
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
	if h.state == nil || h.state.Authz == nil || h.state.Store == nil {
		return true
	}
	owner, err := h.state.Store.GetUserByID(key.UserID)
	if err != nil || owner == nil {
		sendJSONError(w, "Key owner is no longer valid", http.StatusUnauthorized)
		return false
	}

	// Operator gate at USE time, not only at mint. Enforcing it only when a key
	// is created would leave every key minted before the switch was turned off
	// working afterwards, which is not what an operator who turned it off means.
	// The owner is loaded here anyway, so this costs nothing extra.
	if !owner.IsAdmin && !h.userKeysAllowed(r) {
		sendJSONError(w, "API keys are not enabled for users on this platform", http.StatusForbidden)
		return false
	}

	if requiredPerm == "" {
		return true // nothing to re-check; the gate above was the point
	}

	// The operator's list of what a user key may carry, applied now as well
	// as at mint: a capability taken off the list kept working on every key
	// already minted with it, and so did the keys of a demoted admin, whose
	// capabilities were never checked against the list at all.
	if !owner.IsAdmin {
		if allowed := h.allowedUserKeyCaps(r); allowed != nil && !allowed[requiredPerm] {
			sendJSONError(w, "This platform no longer allows that permission on a user key", http.StatusForbidden)
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
		srv, serr := h.state.Store.GetServerByUUID(serverUUID)
		if serr != nil || srv == nil {
			sendJSONError(w, "Server not found", http.StatusNotFound)
			return false
		}
		serverID = srv.ID
	}

	res, err := h.state.Authz.Resolve(authz.Identity{
		UserID:   owner.ID,
		Username: owner.Username,
		IsAdmin:  owner.IsAdmin,
	}, serverID)
	if err != nil {
		sendJSONError(w, "Could not verify key authorization", http.StatusServiceUnavailable)
		return false
	}
	if !res.HasCap(requiredPerm) {
		sendJSONError(w, "The key's owner no longer has this access", http.StatusForbidden)
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

// generateNodeWarpIdentity mints the warp identity for a BYON node's key.
//
// The "node-" prefix is what separates the two kinds of tenant warp key, and the
// separation is load-bearing in both directions: LinkBoot refuses a key whose id
// is not "link-…" so a node key can never mint a link credential, and
// CountLinkKitsByOwner counts only "link-%" so a node key never eats the tenant's
// route-only allowance. Do not change either prefix without changing both.
func generateNodeWarpIdentity() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "node-" + hex.EncodeToString(b), nil
}

// maxKeyRatePerMin bounds the per-key request rate a key may be minted with.
const maxKeyRatePerMin = 600

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

// revokeAllAPIKeys revokes every live key of an account and returns how many.
// It runs on each way an account is RECOVERED - a reset by mail, an operator
// setting the password or resetting the second factor - because each of them
// means the account was out of its owner's hands, and a key minted in that
// time is a way back in that no session check sees. A voluntary password
// change in the profile does not call it: the owner is signed in and holds
// their keys.
func revokeAllAPIKeys(state *AppState, userID, why string) int {
	if state == nil || state.Store == nil {
		return 0
	}
	keys, err := state.Store.ListAPIKeysByUser(userID)
	if err != nil {
		log.Printf("%s: list API keys of %s: %v", why, userID, err)
		return 0
	}
	revoked := 0
	for _, k := range keys {
		if k.RevokedAt != nil {
			continue
		}
		if rerr := state.Store.RevokeAPIKey(k.ID, userID); rerr != nil {
			log.Printf("%s: revoke API key %d of %s: %v", why, k.ID, userID, rerr)
			continue
		}
		revoked++
	}
	return revoked
}
