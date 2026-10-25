package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

// sessionTTL is how long a session lives without being used, in ONE place: the
// JWT expiry and the cookie Max-Age are the same window by construction. Two
// numbers here would drift, and the failure is quiet in both directions - a
// cookie that outlives its token is a browser resending a dead credential, and
// one that dies first logs out a session the server still considers valid.
//
// A panel session that is used slides forward (slideSession), so it ends 30
// days after its LAST use, never mid-work; sessionMaxAge caps that at 90 days
// after the password was typed. It used to be a flat 24 hours with no way to
// extend it, which signed everyone out once a day.
const (
	sessionTTL        = 30 * 24 * time.Hour
	sessionMaxAge     = 90 * 24 * time.Hour
	sessionSlideAfter = time.Hour
)

// legacySessionTTL is what tokens minted before sliding sessions carried. They
// have no login time, so it is read back off their expiry.
const legacySessionTTL = 24 * time.Hour

// newSessionClaims is a fresh session: a new id, logged in now.
func newSessionClaims(username string, isAdmin bool, pwdHash string) *Claims {
	now := time.Now()
	return &Claims{
		Username: username,
		IsAdmin:  isAdmin,
		PwdFp:    passwordFingerprint(pwdHash),
		LoginAt:  now.Unix(),
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        newSessionID(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(sessionExpiry(now, now)),
		},
	}
}

// sessionClaimsKey carries the caller's session claims to handlers that hand
// out a copy of the session (SessionToken), so the copy is the SAME session.
const sessionClaimsKey contextKey = "sessionClaims"

// withSessionID gives a token minted before sessions had an id one derived from
// the token itself, so sliding keeps it and signing out can name it: a fresh
// random id on each slide would let a copy outlive the sign-out.
func withSessionID(c *Claims, token string) {
	if c.ID == "" {
		sum := sha256.Sum256([]byte(token))
		c.ID = "legacy-" + hex.EncodeToString(sum[:16])
	}
}

// copyOfSession signs another token of the caller's session: same id, same
// login time, so the cap and a sign-out apply to it too.
func (h *AuthHandler) copyOfSession(c *Claims, username string, isAdmin bool, pwdHash string) (string, error) {
	next := newSessionClaims(username, isAdmin, pwdHash)
	if c != nil && c.ID != "" {
		loginAt := loginTime(c)
		next.ID = c.ID
		next.LoginAt = loginAt.Unix()
		next.ExpiresAt = jwt.NewNumericDate(sessionExpiry(time.Now(), loginAt))
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, next).SignedString(h.jwtKey)
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sessionExpiry is the sliding window, cut off at the absolute cap.
func sessionExpiry(now, loginAt time.Time) time.Time {
	exp := now.Add(sessionTTL)
	if limit := loginAt.Add(sessionMaxAge); exp.After(limit) {
		return limit
	}
	return exp
}

// loginTime is when the password behind this session was typed.
func loginTime(c *Claims) time.Time {
	if c.LoginAt > 0 {
		return time.Unix(c.LoginAt, 0)
	}
	if c.ExpiresAt != nil {
		return c.ExpiresAt.Time.Add(-legacySessionTTL)
	}
	return time.Now()
}

// slideSession re-issues a cookie session that is in use, at most once per
// sessionSlideAfter, keeping its id and login time. Two tabs sliding at once
// both get a valid token; nothing is rotated or detected as reuse, so there is
// no race to lose. Every rule that ends a session early still applies to the
// new token: it carries the same password fingerprint, and the row is re-read
// on every request.
func (h *AuthHandler) slideSession(w http.ResponseWriter, r *http.Request, c *Claims) {
	now := time.Now()
	if c.IssuedAt != nil && now.Sub(c.IssuedAt.Time) < sessionSlideAfter {
		return
	}
	loginAt := loginTime(c)
	exp := sessionExpiry(now, loginAt)
	if c.ExpiresAt != nil && !exp.After(c.ExpiresAt.Time) {
		return // at the cap: nothing to extend
	}
	next := *c
	next.LoginAt = loginAt.Unix()
	next.IssuedAt = jwt.NewNumericDate(now)
	next.ExpiresAt = jwt.NewNumericDate(exp)
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, &next).SignedString(h.jwtKey)
	if err != nil {
		log.Printf("auth: extending the session of %s: %v", c.Username, err)
		return
	}
	setSessionCookie(w, r, tok, int(time.Until(exp).Seconds()))
}

func revokedSessionKey(id string) string { return "dylaris:auth:revoked:" + id }

// revokeSession ends one session for good: the id stays on the list until no
// copy of it can still be alive, which with sliding is the absolute cap, not
// the token's own expiry.
//
// Postgres holds the list; Redis is its fast path. Redis alone runs without
// persistence here, so a restart of it brought every individually signed-out
// session back to life for the rest of its lifetime.
func revokeSession(ctx context.Context, st any, rdb *redis.Client, c *Claims) error {
	if c.ID == "" {
		return nil
	}
	until := loginTime(c).Add(sessionMaxAge)
	ttl := time.Until(until)
	if ttl <= 0 {
		return nil
	}
	if rs, ok := st.(revokedSessionStore); ok {
		if err := rs.RevokeSession(c.ID, until); err != nil {
			return fmt.Errorf("record the sign-out: %w", err)
		}
	}
	if rdb == nil {
		return nil
	}
	return rdb.Set(ctx, revokedSessionKey(c.ID), "1", ttl).Err()
}

// revokedSessionStore is the durable half of the sign-out list. An interface
// here rather than a store.Store method so a test fake that does not implement
// it simply has no durable list.
type revokedSessionStore interface {
	RevokeSession(id string, expiresAt time.Time) error
	IsSessionRevoked(id string) (bool, error)
}

// sessionRevoked asks Redis first and Postgres when Redis does not say
// "revoked" (a miss after a Redis restart, or Redis down). It fails OPEN only
// when both cannot answer: an outage of both must not sign the whole platform
// out. "Sign out everywhere" (the session epoch) does not depend on this.
func sessionRevoked(ctx context.Context, st any, rdb *redis.Client, id string) bool {
	if id == "" {
		return false
	}
	if rdb != nil {
		// Bounded: this runs on every request, and go-redis would otherwise
		// spend seconds on dial and retries against a Redis that is gone.
		rctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		n, err := rdb.Exists(rctx, revokedSessionKey(id)).Result()
		cancel()
		if err == nil && n > 0 {
			return true
		}
		if err != nil {
			log.Printf("auth: session revocation check in Redis failed, asking Postgres: %v", err)
		}
	}
	rs, ok := st.(revokedSessionStore)
	if !ok {
		return false
	}
	revoked, err := rs.IsSessionRevoked(id)
	if err != nil {
		log.Printf("auth: session revocation check failed, letting it through: %v", err)
		return false
	}
	return revoked
}
