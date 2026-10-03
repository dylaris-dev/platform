package handlers

import (
	"context"
	"crypto/sha256"
	"database/sql" // Import Models
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"dylaris-core/store"
	"dylaris-pkg/validate"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

type AuthHandler struct {
	state  *AppState
	jwtKey []byte
}

func NewAuthHandler(state *AppState, jwtSecret string) *AuthHandler {
	return &AuthHandler{state: state, jwtKey: []byte(jwtSecret)}
}

// IssueToken signs a standard 24h session JWT for the given user. Extracted
// from the login handler so other handlers (the setup wizard, future
// admin-impersonation flows) can mint tokens without duplicating the signing
// + claim shape.
// pwdHash is the caller's stored password hash, which binds the session to it
// (see passwordFingerprint). Callers that genuinely have no hash to bind to -
// the demo account short-circuit - pass "", and such a session is simply not
// password-bound; there is nothing for a reset to invalidate.
// sessionTTL is how long a session lives, in ONE place: the JWT expiry and the
// cookie Max-Age are the same window by construction. Two numbers here would
// drift, and the failure is quiet in both directions - a cookie that outlives
// its token is a browser resending a dead credential, and one that dies first
// logs out a session the server still considers valid.
const sessionTTL = 24 * time.Hour

func (h *AuthHandler) IssueToken(username string, isAdmin bool, pwdHash string) (string, error) {
	expirationTime := time.Now().Add(sessionTTL)
	claims := &Claims{
		Username:         username,
		IsAdmin:          isAdmin,
		PwdFp:            passwordFingerprint(pwdHash),
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(expirationTime)},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(h.jwtKey)
}

// tabProxyTicketTTL is the lifetime of a tab-proxy-scoped ticket (Claims.Purpose
// == "tab_proxy"). Short-lived because it lives in a cookie for the duration of
// one dashboard-tab iframe session, not a general 24h session token.
const tabProxyTicketTTL = 5 * time.Minute

// IssueTabProxyTicket signs a short-lived, tab-proxy-scoped JWT. The caller
// (ProxyHandler.MintTicket, on the panel's own origin) has already run this
// request through AuthMiddleware plus the tabs.read capability for this exact
// server/tab, so the ticket just carries that already-verified identity +
// scope forward: username/isAdmin, the server+tab it is valid for, and
// whether the underlying session is read-only (demo account). It is signed
// with the same key as a normal session token but is rejected by
// AuthMiddleware everywhere else (Purpose == "tab_proxy" is 403'd, see
// below) - it can never stand in for the real session token.
func (h *AuthHandler) IssueTabProxyTicket(username string, isAdmin bool, serverID, tabID int, readOnly bool) (string, error) {
	expiresAt := time.Now().Add(tabProxyTicketTTL)
	claims := &Claims{
		Username:         username,
		IsAdmin:          isAdmin,
		Purpose:          "tab_proxy",
		ServerID:         serverID,
		TabID:            tabID,
		ReadOnly:         readOnly,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(expiresAt)},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(h.jwtKey)
}

// ParseTabProxyTicket validates a tab-proxy ticket string: signature, not
// expired, and Purpose == "tab_proxy". It deliberately does NOT check the
// server/tab-id claims against a request URL - the caller (tab_proxy.go)
// does that, since only it knows which {id}/{tabId} the request is for.
func (h *AuthHandler) ParseTabProxyTicket(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		return h.jwtKey, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !token.Valid {
		return nil, errors.New("invalid token")
	}
	if claims.Purpose != "tab_proxy" {
		return nil, errors.New("wrong token purpose")
	}
	return claims, nil
}

// DemoStatus GET /api/auth/demo-login — public, rate-limited. Answers only
// whether a demo account exists, minting nothing.
//
// It exists so a caller can decide whether to OFFER the demo before triggering
// it. Without it the only way to find out was to POST, which mints a session as
// a side effect of asking a question - so anything wanting to render a "try the
// demo" button either created sessions nobody used or advertised a button that
// 404s. The answer leaks nothing the button itself would not.
func (h *AuthHandler) DemoStatus(w http.ResponseWriter, r *http.Request) {
	available := false
	if h.state.StoreEnabled && h.state.Store != nil {
		if uuid, _ := h.state.Store.GetSetting(demoAccountUUIDSetting); uuid != "" {
			if u, err := h.state.Store.GetUserByID(uuid); err == nil && u != nil {
				available = true
			}
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "available": available})
}

// DemoLogin POST /api/auth/demo-login — public, rate-limited.
// Issues a normal session for the designated read-only demo account, so the
// website/panel can offer a one-click "View demo" with no credentials. The
// account is forced GET-only by AuthMiddleware, so the session can only view.
// Returns 404 when no demo account is configured (feature off).
func (h *AuthHandler) DemoLogin(w http.ResponseWriter, r *http.Request) {
	if !h.state.StoreEnabled {
		sendJSONError(w, "Demo is not enabled", http.StatusNotFound)
		return
	}
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", http.StatusServiceUnavailable)
		return
	}
	uuid, _ := h.state.Store.GetSetting(demoAccountUUIDSetting)
	if uuid == "" {
		sendJSONError(w, "Demo is not enabled", http.StatusNotFound)
		return
	}
	u, err := h.state.Store.GetUserByID(uuid)
	if err != nil || u == nil {
		sendJSONError(w, "Demo account unavailable", http.StatusNotFound)
		return
	}
	// Checked again here and not only when the account was designated: it can
	// be given an admin flag or a staff role afterwards, and every public demo
	// session would then hold it.
	if caps, ok := strictPanelCaps(h.state, u.ID); u.IsAdmin || !ok || len(caps) > 0 {
		log.Printf("demo login: the demo account %s holds staff rights, refusing", u.Username)
		sendJSONError(w, "Demo account unavailable", http.StatusNotFound)
		return
	}
	token, err := h.IssueToken(u.Username, false, u.Password)
	if err != nil {
		sendJSONError(w, "Failed to issue token", http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, r, token, int(sessionTTL.Seconds()))
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "token": token, "username": u.Username})
}

// ... Structs LoginRequest, Claims, UpdateRequest etc. kept as-is ...
// Claims.Purpose distinguishes normal sessions ("" / "session") from
// short-lived special-purpose tokens. "2fa_setup" is a token issued at
// login when the policy demands 2FA and the user hasn't configured it
// yet — accepted only by /auth/2fa/setup and /auth/2fa/verify so the
// user can finish enrollment, nothing else.
type Claims struct {
	Username string `json:"username"`
	IsAdmin  bool   `json:"isAdmin"`
	Purpose  string `json:"purpose,omitempty"`
	// ServerID, TabID and ReadOnly are only set when Purpose == "tab_proxy":
	// the server + tab this ticket authorizes, and whether the underlying
	// session was read-only (demo account) at mint time.
	ServerID int  `json:"serverId,omitempty"`
	TabID    int  `json:"tabId,omitempty"`
	ReadOnly bool `json:"readOnly,omitempty"`
	// PwdFp marks the password hash this session was issued against, so
	// changing the password ends every session that predates it. See
	// passwordFingerprint and the check in AuthMiddleware.
	PwdFp string `json:"pfp,omitempty"`
	jwt.RegisteredClaims
}

// passwordFingerprint reduces a bcrypt hash to a short, non-reversible marker.
//
// A session is a stateless JWT: nothing on the server can retract one, so an
// account whose password was reset BECAUSE it was compromised kept the
// attacker's session alive for the rest of its 24 hours. Measured on a live
// stack: an admin reset the password through the break-glass route, the old
// password stopped working, the new one worked - and the session token minted
// before the reset still answered 200.
//
// Deriving the marker from the stored hash rather than adding a
// password_changed_at column is what makes it cover every write path at once:
// the self-service profile change writes through UpdateUser, the admin reset
// and the emailed reset write through UpdateUserPassword, and a fourth path
// added later is covered without anyone remembering to stamp a column - which
// is precisely how this codebase loses guards.
//
// The row is already loaded on every request (below, to decide whether the
// account still exists), so this costs one SHA-256 over a 60-byte string, not
// a second query. Not the hash itself: the token is handed to the client, and
// a bcrypt hash inside it would be an offline cracking target.
func passwordFingerprint(hash string) string {
	if hash == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(hash))
	return hex.EncodeToString(sum[:8])
}

// tokenPurposeKey carries the JWT's Purpose claim to handlers that need to tell
// a fresh password login apart from a long-lived session. Typed, unlike the
// string keys beside it, so it cannot collide with a request-scoped value set
// elsewhere.
type contextKey string

const tokenPurposeKey contextKey = "tokenPurpose"

// TokenPurpose returns the purpose of the JWT that authenticated this request
// ("" for a normal session).
func TokenPurpose(r *http.Request) string {
	p, _ := r.Context().Value(tokenPurposeKey).(string)
	return p
}

// purposes whitelisted for setup-token JWTs. Anything else gets 403'd
// by AuthMiddleware so the bearer of a setup token can't access regular
// endpoints just because they have a valid signature.
var setupTokenAllowedPaths = map[string]bool{
	"/api/auth/2fa/setup":  true,
	"/api/auth/2fa/verify": true,
	// Used by the forced-setup page to load the user's username.
	"/api/auth/profile": true,
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TOTPCode string `json:"totpCode,omitempty"` // 6-digit TOTP or 8-char backup code
}
type UpdateRequest struct {
	OldPassword       string  `json:"oldPassword"`
	NewUsername       *string `json:"newUsername,omitempty"`
	NewPassword       *string `json:"newPassword,omitempty"`
	MinecraftUsername *string `json:"minecraftUsername,omitempty"`
	Email             *string `json:"email,omitempty"`
	// Note: 2FA is NOT toggled via this endpoint — use /auth/2fa/setup + /auth/2fa/verify
	// (or /auth/2fa/disable) so the user must prove possession of the secret.
}

func (h *AuthHandler) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		// SSE auth ticket: EventSource cannot set an Authorization header, so
		// the panel mints a short-lived random ticket (POST /api/sse-ticket)
		// and carries it in the URL instead of the session JWT — the JWT must
		// never appear in URLs (access logs / Referer). Confined to GET. A
		// valid ticket resolves to a username in Redis, and we re-derive the
		// full identity (incl. isAdmin) from the DB so nothing here is
		// client-supplied. The ticket TTL is refreshed on every accepted
		// request, giving a sliding window so native EventSource reconnects
		// keep working with the same ?ticket=.
		if authHeader == "" && r.Method == http.MethodGet && h.state.Redis != nil && sseTicketPaths.MatchString(r.URL.Path) {
			if ticket := r.URL.Query().Get("ticket"); ticket != "" {
				key := "sse:ticket:" + ticket
				if stored, err := h.state.Redis.Get(r.Context(), key).Result(); err == nil && stored != "" {
					username, ticketFp, legacy := parseSSETicket(stored)
					w.Header().Set("Referrer-Policy", "no-referrer")
					w.Header().Set("Cache-Control", "no-store")
					// Sliding window: keep long-lived streams alive.
					if !legacy {
						h.state.Redis.Expire(r.Context(), key, sseTicketTTL)
					}

					// FAIL CLOSED, same rule as the Bearer path below.
					//
					// This used to swallow the lookup error and continue with
					// isAdmin=false and NO userID in the context - the exact
					// shape the Bearer path was fixed out of. It survived here
					// because the branch is GET-only, which looks like it bounds
					// the damage, and it does not: ListLinkRoutes is a GET that
					// reads userID with a bare .(string), so a nil value panics
					// the request rather than filtering to nothing.
					//
					// The reachable case needs no outage. The ticket outlives the
					// account, and its TTL is refreshed on every accepted request
					// (the sliding window just above), so a deleted user's
					// EventSource keeps its own ticket alive indefinitely. The
					// ticket is dropped rather than left to slide; a rename lands
					// here too, since the ticket stores the name, and re-minting
					// after a 401 is what the panel already does.
					isAdmin := false
					userID := ""
					if h.state.Store != nil {
						user, uerr := h.state.Store.GetUserByUsername(username)
						if errors.Is(uerr, sql.ErrNoRows) || (uerr == nil && user == nil) {
							h.state.Redis.Del(r.Context(), key)
							sendJSONError(w, "Account no longer exists", http.StatusUnauthorized)
							return
						}
						if uerr != nil {
							log.Printf("auth: could not resolve SSE ticket holder %q: %v", username, uerr)
							sendJSONError(w, "Could not verify account", http.StatusServiceUnavailable)
							return
						}
						// The same rule the session token follows: a ticket minted
						// under an older password is over.
						if ticketFp != "" && ticketFp != passwordFingerprint(user.Password) {
							h.state.Redis.Del(r.Context(), key)
							sendJSONError(w, "Your password changed - please sign in again", http.StatusUnauthorized)
							return
						}
						isAdmin = user.IsAdmin
						userID = user.ID
					}
					ctx := context.WithValue(r.Context(), "username", username)
					ctx = context.WithValue(ctx, "isAdmin", isAdmin)
					if userID != "" {
						ctx = context.WithValue(ctx, "userID", userID)
					}
					next(w, r.WithContext(ctx))
					return
				}
				// Invalid/expired ticket: fall through to the logic below,
				// which 401s (no Authorization header, no ?token=).
			}
		}
		// Fallback: EventSource (SSE) cannot send custom headers, so accept a
		// ?token= query param. Confine it to GET (SSE + downloads) — a mutating
		// verb never legitimately authenticates via the URL — and when used,
		// stop the token from leaking onward: no-referrer keeps it out of the
		// Referer header if the page navigates away, no-store keeps it out of
		// shared caches/proxies.
		if authHeader == "" {
			if qToken := r.URL.Query().Get("token"); qToken != "" && r.Method == http.MethodGet {
				authHeader = "Bearer " + qToken
				w.Header().Set("Referrer-Policy", "no-referrer")
				w.Header().Set("Cache-Control", "no-store")
			}
		}
		// The session cookie: same JWT, different transport. Last, so an
		// explicit Authorization header always wins - ambient authority must
		// never override something the caller stated.
		//
		// The CSRF gate applies ONLY here. A request that named its own
		// credential cannot be triggered by a page somebody else wrote.
		cookieAuth := false
		if authHeader == "" {
			if tok := sessionTokenFromCookie(r); tok != "" {
				if !requireSameOriginForCookieAuth(r, h.state.FrontendURL) {
					sendJSONError(w, "Cross-origin request refused", http.StatusForbidden)
					return
				}
				authHeader = "Bearer " + tok
				cookieAuth = true
			}
		}
		if authHeader == "" {
			sendJSONError(w, "Missing Authorization Header", http.StatusUnauthorized)
			return
		}
		tokenString := strings.TrimPrefix(authHeader, "Bearer ")
		claims := &Claims{}
		token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
			return h.jwtKey, nil
		}, jwt.WithValidMethods([]string{"HS256"}))
		if err != nil || !token.Valid {
			// A dead cookie is removed rather than left to be resent on every
			// request for the next 24 hours: the browser keeps sending it, and
			// each rejection looks to the panel like a server fault rather than
			// a signed-out state.
			if cookieAuth {
				clearSessionCookie(w, r)
			}
			sendJSONError(w, "Invalid Token", http.StatusUnauthorized)
			return
		}

		// Setup tokens are scoped to a tiny allowlist. Any other endpoint
		// must reject them with 403 - bearer doesn't have a full session yet.
		if claims.Purpose == "2fa_setup" {
			if !setupTokenAllowedPaths[r.URL.Path] {
				sendJSONError(w, "Token is restricted to 2FA setup - finish enrollment first", http.StatusForbidden)
				return
			}
		}

		// tab_proxy tickets are validated exclusively by the tab-proxy
		// handler's own cookie parsing (root router, not behind
		// AuthMiddleware) - they must never authenticate a normal /api call,
		// even though they share the session signing key.
		if claims.Purpose == "tab_proxy" {
			sendJSONError(w, "Token is restricted to the tab proxy", http.StatusForbidden)
			return
		}

		// Everything above is a DENYLIST, and it named the two token types that
		// existed when it was written. A third one already did not appear in it:
		// the BEAM TICKET.
		//
		// A beam ticket is HS256-signed with this exact key (BEAM_JWT_SECRET is
		// wired to JWT_SECRET in both compose files, deliberately - see the
		// README), carries a "username" claim under the same JSON name a session
		// uses, and carries no purpose at all. It therefore fell through to the
		// session path below, and since authorization is then re-read from the
		// user ROW, the bearer got that user's full session for the ticket's 30
		// minutes.
		//
		// Who holds one: the ticket travels to the beam desktop app, through the
		// gateway beam-relay (a separate repo and deployment), and to the NODE -
		// which for BYON is the customer's own machine. So a BYON operator who
		// receives an admin's beam ticket could replay it here as that admin.
		//
		// The rule is an ALLOWLIST by construction rather than a third name on
		// the list: every token this middleware should accept comes from
		// IssueToken / the login handler, and none of them sets an issuer. Any
		// future token type minted with this key the way the beam ticket was -
		// with its own issuer, for its own audience - is refused here without
		// anyone having to remember to extend a list.
		if claims.Issuer != "" {
			sendJSONError(w, "Token was not issued for the panel API", http.StatusForbidden)
			return
		}

		ctx := context.WithValue(r.Context(), "username", claims.Username)
		ctx = context.WithValue(ctx, "isAdmin", claims.IsAdmin)
		// Handlers that re-authenticate need to know WHICH kind of token this is.
		// A 2fa_setup token is minted only by a successful password login, lives
		// 15 minutes and reaches three paths, so the password is already proven
		// for its bearer; a normal session token is long-lived and stealable and
		// proves nothing about who is holding it right now.
		ctx = context.WithValue(ctx, tokenPurposeKey, claims.Purpose)

		// Resolve userID from DB for invite checks.
		//
		// FAIL CLOSED on a failed lookup. Everything below used to sit inside
		// `if err == nil`, so an error silently produced a session that was
		// authenticated but had NO userID and had skipped the demo read-only
		// gate entirely. Two consequences: handlers that scope by owner
		// (gateway link routes) asserted a missing value and panicked, and the
		// demo gate fixed one level down in 2b0b6d5 never ran at all.
		//
		// A missing row means the account was deleted while its token was still
		// valid, which is a 401 - the token names someone who no longer exists.
		// Any other error is infrastructure, and must not read as "your login is
		// bad": that would log everyone out during a database blip and hide the
		// outage.
		if h.state.Store != nil {
			user, err := h.state.Store.GetUserByUsername(claims.Username)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && user == nil) {
				sendJSONError(w, "Account no longer exists", http.StatusUnauthorized)
				return
			}
			if err != nil {
				log.Printf("auth: could not resolve %q: %v", claims.Username, err)
				sendJSONError(w, "Could not verify account", http.StatusServiceUnavailable)
				return
			}

			// A password change ends the sessions that predate it. Same rule as
			// the row-over-claim one below, applied to the credential itself:
			// the claim says which password this session was issued against,
			// the row says which one is current, and until now nothing
			// compared them - so resetting a password because the account was
			// compromised left the attacker's session alive for the rest of
			// its 24 hours.
			//
			// An EMPTY claim is allowed through on purpose. Tokens minted
			// before this shipped carry none, and refusing them would log the
			// whole platform out on deploy; they age out within 24 hours on
			// their own. The demo-account session is unbound for the same
			// reason and has nothing to invalidate.
			if claims.PwdFp != "" && claims.PwdFp != passwordFingerprint(user.Password) {
				sendJSONError(w, "Your password changed - please sign in again", http.StatusUnauthorized)
				return
			}

			ctx = context.WithValue(ctx, "userID", user.ID)

			// Authorization comes from the ROW, never from the claim.
			//
			// The claim was signed at login and the session lasts 24 hours, so
			// PUT /admin/users/{id}/role took a full day to take effect on the
			// person it was aimed at. SetUserRole writes users.is_admin
			// immediately, but a demoted admin's token still said isAdmin:true,
			// and that is what landed in the context - which authz.Resolve
			// short-circuits on to grant EVERY capability. The demotion did not
			// merely lag: the window was long enough, and carried users.write
			// and panelroles.write, for the demoted account to simply promote
			// itself back.
			//
			// The row is right here already; it is fetched a few lines up to
			// decide whether the account still exists at all. The SSE-ticket
			// branch near the top of this function has always re-derived
			// identity from the database for exactly this reason ("so nothing
			// here is client-supplied") - this is the same rule applied to the
			// path that carries every other request.
			//
			// Promotions land immediately too, which is the harmless direction.
			// One deliberate gap remains: IsAdminToken, which the maintenance
			// gate calls BEFORE any of this, still reads the claim because it
			// runs without a database. A just-demoted admin can therefore still
			// pass a maintenance window for the rest of their token's life - and
			// then arrives here, gets a non-admin identity, and is refused by
			// every admin route.
			ctx = context.WithValue(ctx, "isAdmin", user.IsAdmin)

			// The demo account is read-only: reject every mutating verb so a
			// public demo session can only ever view, never change anything.
			// One central gate covers all write endpoints (power, files, RCON,
			// profile, server-create, ...) without per-handler checks.
			if !user.IsAdmin &&
				r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
				// Refuse when the answer is UNKNOWN, not just when it is
				// "yes". The lookup's error used to be discarded, and its
				// zero value said "not the demo account" - so a failed
				// settings read silently lifted the read-only gate.
				demo, derr := isDemoAccountChecked(h.state, user.ID)
				if derr != nil {
					sendJSONError(w, "Could not verify account restrictions", http.StatusServiceUnavailable)
					return
				}
				if demo {
					sendJSONError(w, "The demo account is read-only", http.StatusForbidden)
					return
				}
			}
		}

		next(w, r.WithContext(ctx))
	}
}

// IsAdminToken reports whether the request carries a valid token whose claims
// mark an admin. Used by middleware that runs before AuthMiddleware (the
// maintenance gate) to honor the admin bypass without a full auth pass.
func (h *AuthHandler) IsAdminToken(r *http.Request) bool {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		if q := r.URL.Query().Get("token"); q != "" {
			authHeader = "Bearer " + q
		}
	}
	tokenString := strings.TrimPrefix(authHeader, "Bearer ")
	if tokenString == "" {
		return false
	}
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		return h.jwtKey, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !token.Valid {
		return false
	}
	return claims.IsAdmin
}

// invalidLoginMessage is returned for BOTH an unknown username and a wrong
// password, so a failed login says nothing about which of the two it was.
const invalidLoginMessage = "Invalid username or password"

// LoginHandler POST /api/auth/login - issues the session JWT. A wrong password
// and an unknown username answer identically, so the form cannot be turned
// into a username oracle, and a database outage answers 503 rather than 401 so
// it does not read as bad credentials.
func (h *AuthHandler) LoginHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", http.StatusServiceUnavailable)
		return
	}

	var req LoginRequest
	json.NewDecoder(r.Body).Decode(&req)

	// STORE REFACTOR: No more raw SQL queries!
	user, err := h.state.Store.GetUserByUsername(req.Username)

	if err == sql.ErrNoRows {
		// Same wording as the wrong-password branch below, deliberately: a
		// distinct "user not found" turns the login form into a username oracle.
		sendJSONError(w, invalidLoginMessage, http.StatusUnauthorized)
		return
	} else if err != nil {
		if dbUnavailable(err) {
			sendJSONError(w, "The database is currently unavailable — please try again in a moment.", http.StatusServiceUnavailable)
			return
		}
		sendJSONError(w, "Database Error", http.StatusInternalServerError)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
		sendJSONError(w, invalidLoginMessage, http.StatusUnauthorized)
		return
	}

	// Email-verify gate: if the server requires verification and
	// this user hasn't verified yet, block login with a distinctive flag so
	// the UI can offer a "resend verification email" button instead of a
	// generic auth error. Admins still get to log in even when unverified —
	// otherwise a misconfigured SMTP would lock everyone out.
	policy := LoadAuthPolicy(h.state)
	if policy.EmailVerifyRequired && !user.IsAdmin && user.EmailVerifiedAt == nil {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":              false,
			"requiresVerification": true,
			"email":                user.Email,
			"message":              "Please verify your email address before signing in.",
		})
		return
	}

	// 2FA enforcement gate: if the policy requires 2FA for this
	// user (admin or everyone) and they haven't enrolled, issue a *setup token*
	// — a short-lived JWT scoped exclusively to the 2FA setup endpoints —
	// instead of a normal session. Forces enrollment before they can use the
	// panel. Admins enforcing 2FA on themselves still works: they get the
	// setup token like anyone else.
	if !user.Is2FAEnabled {
		needs2FA := (user.IsAdmin && policy.Require2FAForAdmins) || policy.Require2FAForAllUsers
		if needs2FA {
			setupExp := time.Now().Add(15 * time.Minute)
			setupClaims := &Claims{
				Username:         user.Username,
				IsAdmin:          user.IsAdmin,
				Purpose:          "2fa_setup",
				RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(setupExp)},
			}
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, setupClaims)
			setupTokenString, _ := tok.SignedString(h.jwtKey)
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success":           false,
				"requires2FASetup":  true,
				"setupToken":        setupTokenString,
				"setupTokenExpires": setupExp.Unix(),
				"message":           "Two-factor authentication is required for this account. Finish enrollment to continue.",
			})
			return
		}
	}

	// 2FA gate: if the user has 2FA enabled, require a valid TOTP or backup code.
	if user.Is2FAEnabled {
		if req.TOTPCode == "" {
			// Signal to the frontend that a code is needed without leaking
			// whether the password was correct (it was — but we still
			// expose this fact since 2FA is the user's chosen second factor).
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success":     false,
				"requires2FA": true,
				"message":     "2FA code required",
			})
			return
		}
		ok, err := h.verifyTOTPOrBackup(user, req.TOTPCode)
		if err != nil {
			sendJSONError(w, "2FA verification failed", http.StatusInternalServerError)
			return
		}
		if !ok {
			sendJSONError(w, "Invalid 2FA code", http.StatusUnauthorized)
			return
		}
	}

	// The same window IssueToken uses; the cookie's Max-Age is set from it too.
	expirationTime := time.Now().Add(sessionTTL)
	claims := &Claims{
		Username: user.Username,
		IsAdmin:  user.IsAdmin,
		// Binds the session to the password it was issued against.
		PwdFp:            passwordFingerprint(user.Password),
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(expirationTime)},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, _ := token.SignedString(h.jwtKey)

	// Best-effort last-login stamp. Auto-delete reads this to distinguish
	// active vs dormant users — a stale value here is harmless.
	if err := h.state.Store.UpdateLastLoginAt(user.ID); err != nil {
		// Non-fatal — log and continue so a slow DB write never blocks login.
		log.Printf("login: UpdateLastLoginAt for userID=%s failed: %v", user.ID, err)
	}

	// Auto-rescue: if the user was already in pending_deletion
	// state, cancel the scheduled deletion silently. The fact that they
	// logged in is the strongest possible signal that the account is alive.
	if user.DeletionStatus == "pending_deletion" {
		if err := h.state.Store.CancelUserDeletion(user.ID); err == nil {
			LogIdentityAudit(h.state, r, AuditEventDeletionCancelledAtLogin, user.ID, user.ID, nil)
		}
	}

	// The session goes out as an HttpOnly cookie. It is still in the body too,
	// and that is not redundancy for its own sake: the Beam desktop client and
	// anything else driving this API programmatically read it from there, and
	// the panel is the only caller that has a cookie jar to put it in.
	//
	// What CHANGED is that the panel no longer stores what it reads. That is
	// where the win is - a token in localStorage is readable by any script that
	// manages to run on the page.
	setSessionCookie(w, r, tokenString, int(sessionTTL.Seconds()))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"token":   tokenString,
		"isAdmin": user.IsAdmin,
	})
}

// dbUnavailable reports whether err looks like the database being
// unreachable or not yet ready, rather than a genuine query failure.
// Both the connection being down and the schema not being rebuilt yet
// (the schema-heal loop recreates it) are transient — the right
// user-facing response is "try again shortly", not a hard 500.
func dbUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, n := range []string{
		"connection refused", "dial tcp", "no such host",
		"bad connection", "connection reset", "broken pipe",
		"i/o timeout", "server closed the connection",
		"the database system is", // starting up / shutting down / in recovery
		"does not exist",         // schema not rebuilt yet
	} {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

// GetProfileHandler GET /api/auth/profile - the calling user's own row, with
// the password hash cleared before it is written out.
func (h *AuthHandler) GetProfileHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		return
	}
	username := r.Context().Value("username").(string)

	// STORE REFACTOR
	user, err := h.state.Store.GetUserByUsername(username)
	if err != nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}

	// Clear password before sending
	user.Password = ""
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "user": user})
}

// UpdateProfileHandler PUT /api/auth/profile - updates the calling user's own
// profile. A password change requires the current one (401). A username change
// is the expensive path: it honours the admin-set toggle (403), the rename
// cooldown (429) and uniqueness (409), and records a history row.
func (h *AuthHandler) UpdateProfileHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		return
	}
	username := r.Context().Value("username").(string)
	isAdmin, _ := r.Context().Value("isAdmin").(bool)

	var req UpdateRequest
	json.NewDecoder(r.Body).Decode(&req)

	user, err := h.state.Store.GetUserByUsername(username)
	if err != nil {
		sendJSONError(w, "User not found", 404)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.OldPassword)); err != nil {
		sendJSONError(w, "Invalid current password", 401)
		return
	}

	// The address, decided BEFORE anything is written. The rename below commits
	// on its own, so a refusal found after it would report failure for a save
	// that had half happened.
	//
	// This door used to write any address straight through UpdateUser: no
	// uniqueness check, the verified badge kept, no mail. The admin door has had
	// all three for a reason its own comment gives - the address is where a
	// password reset goes - and with registration answering "account created"
	// for an address that is already taken, an account could claim a stranger's
	// address and that stranger could then never register.
	emailChanged, newEmail := false, ""
	if req.Email != nil {
		newEmail = strings.ToLower(strings.TrimSpace(*req.Email))
		if !strings.EqualFold(strings.TrimSpace(user.Email), newEmail) {
			if newEmail == "" {
				// Removing the address would lock the account out at its next
				// sign-in with nowhere to send the confirmation it then needs.
				if LoadAuthPolicy(h.state).EmailVerifyRequired && !isAdmin {
					sendJSONError(w, "An email address is required on this platform", http.StatusBadRequest)
					return
				}
			} else {
				if !validate.IsEmail(newEmail) {
					sendJSONError(w, "Invalid email address", http.StatusBadRequest)
					return
				}
				// There is no unique index on users.email; this check is the only
				// thing between two accounts and one reset mailbox. Same answer
				// the admin door gives.
				if existing, eerr := h.state.Store.GetUserByEmail(newEmail); eerr == nil && existing != nil && existing.ID != user.ID {
					sendJSONError(w, "That email address is already in use", http.StatusConflict)
					return
				}
			}
			// One address change per verification-mail window. Each change
			// mails the new address, and switching between two strangers'
			// addresses in a loop sent them mail from this domain without end.
			if !verificationResendAllowed(user.EmailVerificationSentAt, time.Now(), resendVerificationCooldown) {
				sendJSONError(w, "Please wait a minute before changing your address again", http.StatusTooManyRequests)
				return
			}
			emailChanged = true
		}
	}

	// Username change: route through RenameUser with policy + cooldown + uniqueness guards.
	// RenameUser writes user_username_history and bumps last_username_change in one tx, so
	// we MUST NOT also write the username column in the generic UpdateUser call below.
	if req.NewUsername != nil && strings.TrimSpace(*req.NewUsername) != user.Username {
		newName := strings.TrimSpace(*req.NewUsername)
		if newName == "" {
			sendJSONError(w, "Username cannot be empty", http.StatusBadRequest)
			return
		}
		// The rename path historically skipped the charset regex the register/
		// setup paths apply. A username is interpolated into Redis keys (e.g. the
		// beam daily-upload counter dylaris:beam:daily:<username>:<date>), so a ':'
		// or space here lets a user collide/hijack another user's key namespace.
		if !validate.IsUsername(newName) {
			sendJSONError(w, "Invalid username: 3-32 characters, must start with a letter or digit, then letters, digits, '.', '_' or '-'", http.StatusBadRequest)
			return
		}
		allow, cooldownDays, _ := h.state.Store.GetUserAccountPolicy()
		if !allow && !isAdmin {
			sendJSONError(w, "Username changes are disabled by the admin", http.StatusForbidden)
			return
		}
		if !isAdmin && user.LastUsernameChange != nil {
			earliest := user.LastUsernameChange.Add(time.Duration(cooldownDays) * 24 * time.Hour)
			if time.Now().Before(earliest) {
				w.Header().Set("Retry-After", earliest.UTC().Format(time.RFC3339))
				sendJSONError(w, "Username cooldown active; try again on "+earliest.Format("2006-01-02"), http.StatusTooManyRequests)
				return
			}
		}
		// Case-INSENSITIVE; the unique index on LOWER(username) is the real guard.
		if taken, _ := h.state.Store.UsernameTaken(newName, user.ID); taken {
			sendJSONError(w, "Username already taken", http.StatusConflict)
			return
		}
		if err := h.state.Store.RenameUser(user.ID, newName, user.ID); err != nil {
			// The pre-check above is best-effort UX; the username UNIQUE
			// constraint is the real guard against a concurrent rename race.
			if errors.Is(err, store.ErrUsernameTaken) {
				sendJSONError(w, "Username already taken", http.StatusConflict)
				return
			}
			sendJSONError(w, "Rename failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		h.state.Events.Publish(r.Context(), "users.changed", map[string]interface{}{"userId": user.ID})
		user.Username = newName
	}

	// Update Fields in Struct (non-username fields)
	passwordChanged := false
	if req.NewPassword != nil && *req.NewPassword != "" {
		// Enforce the same length policy the register + reset paths apply; this
		// self-service field previously accepted any non-empty password.
		if min := LoadAuthPolicy(h.state).PasswordMinLength; len(*req.NewPassword) < min {
			sendJSONError(w, fmt.Sprintf("Password must be at least %d characters", min), http.StatusBadRequest)
			return
		}
		hashed, herr := bcrypt.GenerateFromPassword([]byte(*req.NewPassword), bcrypt.DefaultCost)
		if herr != nil {
			sendJSONError(w, "Update failed", 500)
			return
		}
		user.Password = string(hashed)
		passwordChanged = true
	}
	if emailChanged {
		user.Email = newEmail
	}
	if req.MinecraftUsername != nil {
		mc := strings.TrimSpace(*req.MinecraftUsername)
		if mc != "" && !validate.IsMinecraftUsername(mc) {
			sendJSONError(w, "Invalid Minecraft username: 3-16 characters, letters, digits or _", http.StatusBadRequest)
			return
		}
		user.MinecraftUsername = mc
	}

	// Each field the form owns gets its own write; the username went through
	// RenameUser above and the email goes through SetUserEmail below. This
	// used to be one whole-row save from the row read before the bcrypt check,
	// which wrote back is_admin, the 2FA state, the password and the
	// permissions as they were then: a demotion, a password reset or a 2FA
	// reset an admin made in that gap was undone, and a demoted admin looping
	// their own profile save could get is_admin back.
	if passwordChanged {
		if err := h.state.Store.UpdateUserPassword(user.ID, user.Password); err != nil {
			sendJSONError(w, "Update failed", 500)
			return
		}
	}
	if req.MinecraftUsername != nil {
		if err := h.state.Store.SetUserMinecraftUsername(user.ID, user.MinecraftUsername); err != nil {
			sendJSONError(w, "Update failed", 500)
			return
		}
	}

	// A new address is unproven, exactly as when an admin types one: the badge
	// goes, and a confirmation goes out when the policy asks for one.
	emailVerifySent := false
	if emailChanged {
		if err := h.state.Store.SetUserEmail(user.ID, newEmail); err != nil {
			sendJSONError(w, "Update failed", 500)
			return
		}
		LogIdentityAudit(h.state, r, AuditEventUserEmailChanged, user.ID, user.ID, nil)
		emailVerifySent = sendChangedEmailVerification(h.state, user.ID, newEmail, user.Username, "profile-email-change")
	}

	// A password change invalidates every session issued against the old one -
	// including the caller's own, which they are holding right now. Hand back a
	// replacement so changing your password in settings does not read as being
	// thrown out of the panel. Everyone ELSE holding a session for this account
	// still loses it, which is the point.
	out := map[string]string{"success": "true", "message": "Profile updated!"}
	if emailVerifySent {
		// Said here because it decides the next sign-in: with the policy on, the
		// account stays usable now and asks for the confirmation at next login.
		out["message"] = "Profile updated. We sent a link to your new address - confirm it before you next sign in."
	}
	if passwordChanged {
		if fresh, terr := h.IssueToken(user.Username, user.IsAdmin, user.Password); terr == nil {
			out["token"] = fresh
			// The cookie has to be replaced too, or the caller keeps sending
			// the one their own password change just invalidated and is thrown
			// out on the next request.
			setSessionCookie(w, r, fresh, int(sessionTTL.Seconds()))
		} else {
			log.Printf("profile: could not re-issue a session after a password change for %s: %v", user.Username, terr)
		}
	}
	json.NewEncoder(w).Encode(out)
}

// Logout POST /api/auth/logout - drops the session cookie.
//
// There was no such endpoint before, and there did not need to be: the session
// lived in localStorage, so signing out was the panel deleting its own copy.
// An HttpOnly cookie is the point and it is also the reason - the panel cannot
// delete what it cannot read, so the server has to be asked.
//
// Unauthenticated on purpose. It only ever CLEARS, so the worst a stranger can
// do by calling it is sign themselves out; requiring a valid session would mean
// an expired or corrupted one could never be cleared, which is exactly the state
// somebody trying to log out is most likely in.
//
// The body token, if a caller still holds one, is not invalidated here. Sessions
// are stateless JWTs - the mechanism that ends them early is the password
// fingerprint, and that is what a password change already uses.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w, r)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// SessionToken POST /api/auth/session-token - hands the caller a bearer copy of
// their own session, and ONLY inside the Beam desktop client.
//
// This exists because one consumer genuinely cannot use the cookie. Beam runs
// the panel in a webview and its NATIVE side calls Core directly, with no cookie
// jar and no browser; it needs a real bearer and it needs a long-lived one,
// because the app stays open.
//
// It is also, read plainly, a way to turn an HttpOnly cookie back into a
// readable token - which is the one thing HttpOnly is for. An XSS on the panel
// can already ACT with the cookie; what it cannot do is take the credential
// away and use it elsewhere, later, from another machine. An unrestricted
// endpoint here would hand that back.
//
// So it is restricted to the origin that has the problem. A page in a normal
// browser cannot claim to be the Beam webview: the browser sets Origin itself,
// and the request must ALSO have been addressed to wails.localhost. Everyone
// else keeps a session they can use and cannot copy.
//
// No new token type and no new lifetime: it re-issues the ordinary session for
// the authenticated user, so every rule that ends a session early - the password
// fingerprint, the deleted account, the row-derived authorization - applies to
// it unchanged.
func (h *AuthHandler) SessionToken(w http.ResponseWriter, r *http.Request) {
	if !isWailsOrigin(r) {
		// Deliberately not "forbidden for you": the endpoint should read as
		// absent from anywhere it does not apply, so nothing invites a caller
		// to go looking for a way in.
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	username, _ := r.Context().Value("username").(string)
	if username == "" || h.state.Store == nil {
		sendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	user, err := h.state.Store.GetUserByUsername(username)
	if err != nil || user == nil {
		sendJSONError(w, "Account no longer exists", http.StatusUnauthorized)
		return
	}
	token, err := h.IssueToken(user.Username, user.IsAdmin, user.Password)
	if err != nil {
		sendJSONError(w, "Failed to issue token", http.StatusInternalServerError)
		return
	}
	// Never cached, never referred onward: this response body IS a credential.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "token": token})
}

// StatusHandler GET /api/status - liveness for the login page. needsSetup is
// always false and the panel ignores it; the first-run wizard is gated by
// /api/setup instead.
func (h *AuthHandler) StatusHandler(w http.ResponseWriter, r *http.Request) {
	// Setup is removed. If the API is reachable, the system is "Ready".
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"needsSetup": false, // Always false, frontend ignores setup
		"message":    "Ready",
	})
}
