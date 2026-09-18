package handlers

import (
	"net/http"
	"sync"
	"time"
)

// IPRateLimiter is a per-IP sliding-window counter for public, unauthenticated
// endpoints (login, registration, password reset, first-admin setup) and the
// pre-validation gate on external API keys. In-memory and per-Core: enough to
// blunt brute-force and credential-stuffing. A distributed Redis-backed limiter
// can replace it if Multi-Core fairness ever matters.
type IPRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
}

func NewIPRateLimiter() *IPRateLimiter {
	return &IPRateLimiter{buckets: make(map[string]*rateBucket)}
}

// CredentialBodyLimit bounds a request body on the UNAUTHENTICATED surface.
//
// 64 KiB is roughly a thousand times what these payloads need - a username, a
// password, a token, a TOTP code, at most a handful of security questions - and
// still far below anything that can hurt.
const CredentialBodyLimit = 64 << 10

// LimitBody caps how many bytes a handler can read from the request body.
//
// The IP rate limiter above bounds how MANY requests an anonymous caller may
// send; it says nothing about how BIG one may be. Every public handler decodes
// with json.NewDecoder(r.Body).Decode(&req), which allocates whatever a string
// field contains, so one request carrying a multi-gigabyte value was enough to
// exhaust Core - no credential, no second request needed.
//
// A body over the cap makes the handler's existing Decode fail, which its
// existing "Invalid JSON" 400 already covers, so no handler changes.
//
// Deliberately NOT applied as blanket middleware on /api: the upload handlers
// set their own, much larger MaxBytesReader, and an outer wrapper would win
// (the inner one only wraps the already-capped reader) and silently break every
// upload at the smaller limit. Wrapping the routes that take credentials keeps
// the two from interacting.
// Deliberately NOT using capBody below. capBody answers an over-limit request
// itself with a 413, and this wrapper's contract is the opposite: it stays
// silent and lets the handler's own Decode fail into its existing 400, which is
// what TestLimitBody_OversizedJSONBecomesA400 pins. The 100-continue hang
// capBody exists for needs a client that sends a body larger than the cap, and
// these caps are credential-sized, so the case is a caller doing something
// deliberate rather than a legitimate upload being refused.
func LimitBody(max int64, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next(w, r)
	}
}

// capBody bounds the request body, answering an over-limit request from the
// declared Content-Length alone when there is one. It reports whether the
// caller should continue; false means the response is already written.
//
// The Content-Length check is not an optimization, it is what keeps an
// over-limit upload from HANGING the client. A client that sends
// "Expect: 100-continue" (curl does for any body over 1KB, as do several HTTP
// libraries; browsers do not, so the panel never hit this) waits for the
// server's go-ahead. Go emits that 100 Continue the moment the handler first
// reads the body - so with only a MaxBytesReader the sequence is: read a little,
// send 100, client streams megabytes, the reader trips its limit, the handler
// answers 413 - and the client, still blocked writing into a socket nobody is
// draining, never gets to read it. Measured against the ticket attachment
// upload: 60 seconds and no reply, versus 50 milliseconds for the same request
// with Expect disabled.
//
// Refusing before the first read means Go sends the 413 INSTEAD of the 100
// Continue, which is exactly what Expect is for. MaxBytesReader stays for the
// cases Content-Length cannot cover: chunked bodies, and a header that lies.
// capBodyLimit is capBody for a cap on the platform limit convention: nil is no
// limit and skips the check entirely, anything else - including 0, which refuses
// any body with content - is passed through.
func capBodyLimit(w http.ResponseWriter, r *http.Request, max *int64) bool {
	if max == nil {
		return true
	}
	return capBody(w, r, *max)
}

func capBody(w http.ResponseWriter, r *http.Request, max int64) bool {
	if r.ContentLength > max {
		sendJSONError(w, "Request body is too large", http.StatusRequestEntityTooLarge)
		return false
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, max)
	}
	return true
}

// Map-size bounds. maxBuckets is the ceiling that triggers eviction;
// bucketEvictTarget is how far down eviction brings it, leaving headroom so the
// sweep does not run on every call once busy.
const (
	maxBuckets        = 10000
	bucketEvictTarget = 9000
)

// allow reports whether the IP is still under its per-minute budget, rolling
// the window when the bucket's reset has passed.
//
// The map is bounded at maxBuckets. Expired buckets are dropped first; if a
// flood of DISTINCT live IPs keeps it over the ceiling with nothing expired,
// arbitrary buckets are evicted down to the target. An earlier version swept
// only expired entries and claimed that bounded the map - it did not, because
// under exactly that flood nothing is expired. Map iteration is randomized, so
// the eviction drops an arbitrary subset; an evicted IP simply gets a fresh
// window, which is the safe direction to fail under memory pressure. The XFF
// handling in clientIP is what stops a single client minting buckets by forging
// the header, so reaching this ceiling now takes genuinely many source IPs.
func (l *IPRateLimiter) allow(ip string, perMin int) bool {
	if perMin <= 0 {
		perMin = 60
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.buckets) >= maxBuckets {
		for k, b := range l.buckets {
			if now.After(b.reset) {
				delete(l.buckets, k)
			}
		}
		for k := range l.buckets {
			if len(l.buckets) <= bucketEvictTarget {
				break
			}
			delete(l.buckets, k)
		}
	}
	b, ok := l.buckets[ip]
	if !ok || now.After(b.reset) {
		l.buckets[ip] = &rateBucket{count: 1, reset: now.Add(time.Minute)}
		return true
	}
	if b.count >= perMin {
		return false
	}
	b.count++
	return true
}

// Limit wraps a public handler with a fixed per-minute budget keyed on the
// client IP, answering 429 + Retry-After when the budget is exhausted.
//
// The bucket is keyed by IP ONLY, so every route sharing one IPRateLimiter
// shares one counter per client and perMin is that route's ceiling on the shared
// count, not a budget of its own. Callers rely on this in both directions: the
// auth routes deliberately share an instance so an attacker rotating between
// them gets one budget, while the pack mirror, share links and beam download
// each construct their own so an expensive route cannot drain the login budget.
// Adding a route to an existing limiter therefore changes the effective limit of
// every other route on it.
func (l *IPRateLimiter) Limit(perMin int, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if ip == "" {
			ip = "unknown"
		}
		if !l.allow(ip, perMin) {
			w.Header().Set("Retry-After", "60")
			sendJSONError(w, "Too many requests, slow down", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}
