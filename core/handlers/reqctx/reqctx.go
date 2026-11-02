// Package reqctx is the request identity the auth middleware leaves on a
// request, and the only way to read it back.
//
// The setter and the getter of every typed key live here together. A key
// type declared in two packages is two different keys: the middleware would
// store under one and the handlers would read the other, and an API key
// request would come through with no key on it - unscoped, with its owner's
// full session rights. The string keys "userID", "isAdmin" and "username"
// are still plain strings set by the middleware directly.
package reqctx

import (
	"context"
	"net/http"

	"dylaris-core/models"
)

// contextKey is typed, unlike the string keys beside it, so it cannot collide
// with a request-scoped value set elsewhere.
type contextKey string

// tokenPurposeKey carries the JWT's Purpose claim to handlers that need to tell
// a fresh password login apart from a long-lived session.
const tokenPurposeKey contextKey = "tokenPurpose"

// sessionClaimsKey carries the caller's session claims to handlers that hand
// out a copy of the session (SessionToken), so the copy is the SAME session.
const sessionClaimsKey contextKey = "sessionClaims"

type apiKeyCtxKey struct{}

// apiKeyCtx is what the key middleware injects: the key, plus whether this
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

// WithAPIKey records the key that authenticated the request and whether its
// server scope was satisfied.
func WithAPIKey(ctx context.Context, key *models.APIKey, serverAllowed bool) context.Context {
	return context.WithValue(ctx, apiKeyCtxKey{}, &apiKeyCtx{key: key, serverAllowed: serverAllowed})
}

// APIKeyFromContext returns the key that authenticated this request, if any.
func APIKeyFromContext(r *http.Request) *models.APIKey {
	c, _ := r.Context().Value(apiKeyCtxKey{}).(*apiKeyCtx)
	if c == nil {
		return nil
	}
	return c.key
}

// APIKeyServerAllowed reports whether the key's SERVER capabilities count for
// this request. False on an owner-shaped route and on any request that did not
// come through the key middleware.
func APIKeyServerAllowed(r *http.Request) bool {
	c, _ := r.Context().Value(apiKeyCtxKey{}).(*apiKeyCtx)
	return c != nil && c.serverAllowed
}

// WithTokenPurpose records the Purpose claim of the JWT that authenticated
// the request.
func WithTokenPurpose(ctx context.Context, purpose string) context.Context {
	return context.WithValue(ctx, tokenPurposeKey, purpose)
}

// TokenPurpose returns the purpose of the JWT that authenticated this request
// ("" for a normal session).
func TokenPurpose(r *http.Request) string {
	p, _ := r.Context().Value(tokenPurposeKey).(string)
	return p
}

// WithSessionClaims records the session's claims. The value is the
// middleware's own *handlers.Claims; it is held as any because this package
// sits below the one that defines the type.
func WithSessionClaims(ctx context.Context, claims any) context.Context {
	return context.WithValue(ctx, sessionClaimsKey, claims)
}

// SessionClaims returns what WithSessionClaims stored, or nil. Callers assert
// it to *handlers.Claims.
func SessionClaims(r *http.Request) any {
	return r.Context().Value(sessionClaimsKey)
}

func IsAdmin(r *http.Request) bool {
	isAdmin, ok := r.Context().Value("isAdmin").(bool)
	return ok && isAdmin
}

// OperatorOverride reports whether an operator's bypass of a tenant-facing
// guard applies to this request: an admin in a panel session, never an API key.
// The panel asks an operator to confirm these overrides; a key has nobody to
// ask, so an automation script holding an operator's key started suspended
// customers' servers and acted inside installs and moves without a word.
func OperatorOverride(r *http.Request) bool {
	return IsAdmin(r) && APIKeyFromContext(r) == nil
}

// ActingOnSelf reports whether the request is aimed at the caller's own account.
func ActingOnSelf(r *http.Request, targetID string) bool {
	actorID, _ := r.Context().Value("userID").(string)
	return actorID != "" && actorID == targetID
}

// ByonCallerID returns the authenticated user's id from the request context.
func ByonCallerID(r *http.Request) string {
	id, _ := r.Context().Value("userID").(string)
	return id
}
