package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A route registered without auth middleware and without a rate limiter can be
// called by anyone, as often as they like. Most of Core's session-less routes
// are fine that way - a health probe, a settings read, an in-memory list - and
// the ones that are not are limited: the solder mirror, /api/share/{token},
// the auth and store routes all carry an IPRateLimiter.
//
// /api/beam/download was the sibling that did not. It is the only session-less
// route that makes Core fetch a whole file from an external host and stream it
// out, so one anonymous request cost the full binary inbound plus the same
// again outbound, with nothing bounding how many could run at once.
//
// This test does not try to judge which handlers are expensive - it cannot.
// It freezes the set instead, so a new anonymous route with no limiter has to
// be argued for here rather than appearing by accident.

// Path literal as written at the registration, mapped to why it needs no limit.
var anonymousUnlimitedRoutes = map[string]string{
	"/healthz":                      "liveness probe; no DB, no external call",
	"/status":                       "token presence check, no work",
	"/system/capabilities":          "static capability catalog",
	"/system/core-info":             "static build/region info",
	"/setup/status":                 "one COUNT(); the wizard cannot be reached otherwise",
	"/maintenance":                  "public banner state; deliberately never blocked",
	"/versions/software":            "in-memory map of software names, no upstream call",
	"/auth/registration-status":     "one settings read",
	"/auth/security-questions/pool": "one settings read; the pool is public by design",
	"/tools/beam":                   "emits a Location header and nothing else",
	// "/node/connect" was listed here as "node bootstrap; the enroll token /
	// node secret is the credential". It checked neither - a bare node token,
	// which GET /api/nodes publishes, was enough to flip nodes.status. It was
	// also dead: nothing had called it since the node moved to gRPC. Route and
	// handler are removed; a justification in a freeze-list is only as good as
	// the code it describes.

	// The tab proxy is reached only with an unguessable token, which is the
	// credential. Bounding it per IP would throttle the legitimate embedded
	// session, which issues many requests by design.

	// The Solder API answers the Technic Launcher, which carries no session,
	// and sits outside all middleware on purpose. These four are metadata
	// queries; the sibling that serves the actual files, /solder/mirror/, is
	// the one that carries a limiter.
	"/api":                                   "solder: the retired shared root, explaining where it moved",
	"/api/":                                  "solder: the retired shared root, explaining where it moved",
	"/u/{handle}/api":                        "solder: API banner, the no-slash spelling",
	"/u/{handle}/api/":                       "solder: API banner",
	"/u/{handle}/api/modpack":                "solder: published pack list",
	"/u/{handle}/api/modpack/{slug}":         "solder: pack metadata",
	"/u/{handle}/api/modpack/{slug}/{build}": "solder: build metadata",

	// A limiter here would be WRONG, not missing. The key is 256 bits of
	// crypto/rand, so there is nothing to brute-force, and an IP bucket would
	// lock out Technic launchers that share one NAT address. /solder/mirror/ is
	// limited because it serves bytes, not because it checks a secret.
	"/u/{handle}/api/verify/{key}": "solder: API key check",
}

var handleFuncRe = regexp.MustCompile(`HandleFunc\("([^"]*)"`)

// callArgs returns the text between HandleFunc's parentheses, matching them so
// a registration that wraps its handler over several lines stays intact.
//
// The first version of this took everything up to the next HandleFunc instead,
// and /healthz came out looking guarded: the api.Use(MaintenanceMuxMiddleware)
// block sits between it and the next registration, dozens of lines away. A
// matcher that reaches past the call it is describing reports on the wrong
// code - the same trap as asserting on an identifier instead of a call.
func callArgs(src string, openParen int) string {
	depth := 0
	for i := openParen; i < len(src); i++ {
		switch src[i] {
		case '"':
			for i++; i < len(src) && src[i] != '"'; i++ {
				if src[i] == '\\' {
					i++
				}
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return src[openParen : i+1]
			}
		}
	}
	return src[openParen:]
}

func anonymousUnlimited(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	// Comment lines go first: a comment naming a middleware must not make a
	// route look guarded.
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	src := strings.Join(kept, "\n")

	locs := handleFuncRe.FindAllStringSubmatchIndex(src, -1)
	if len(locs) == 0 {
		t.Fatal("no route registrations found; the matcher is broken, not the code")
	}

	found := map[string]bool{}
	for _, loc := range locs {
		// loc[0] is the "H" of HandleFunc; the paren is just before the path.
		args := callArgs(src, strings.Index(src[loc[0]:], "(")+loc[0])
		path := src[loc[2]:loc[3]]
		if appliesCredentialOrLimit(args) {
			continue
		}
		found[path] = true
	}
	return found
}

// credentialWrappers are the wrappers that put a credential in front of a
// route. This used to be the substring "Middleware", which held only for as
// long as every such wrapper happened to be named that way: renaming the API
// key middleware to APIKeyServerRoute made a key-authed route read as
// anonymous here and in the generated reference, because neither matched on
// what the wrapper DOES, only on what it is called.
//
// A wrapper missing from this list makes its routes look unauthenticated, which
// fails loudly (this test), so the failure mode is safe. Add new ones here and
// in apidoc.go's switch, which classifies the same call for the reference.
var credentialWrappers = []string{
	"Middleware",        // AuthMiddleware, WarpAPIKeyMiddleware, ...
	"APIKeyServerRoute", // user API key, route addresses one server
	"APIKeyOwnerRoute",  // user API key, route acts on the caller's realm
}

func appliesCredentialOrLimit(args string) bool {
	if strings.Contains(args, ".Limit(") {
		return true
	}
	for _, w := range credentialWrappers {
		if strings.Contains(args, w) {
			return true
		}
	}
	return false
}

func TestAnonymousUnlimitedRouteSurfaceIsFrozen(t *testing.T) {
	found := anonymousUnlimited(t)

	var added, stale []string
	for path := range found {
		if _, ok := anonymousUnlimitedRoutes[path]; !ok {
			added = append(added, path)
		}
	}
	for path := range anonymousUnlimitedRoutes {
		if !found[path] {
			stale = append(stale, path)
		}
	}

	if len(added) > 0 {
		sort.Strings(added)
		t.Errorf("new route(s) answering with neither auth nor a rate limit:\n  %s\n"+
			"If the handler does real work - a DB scan, an external fetch, anything that streams -"+
			" wrap it in an IPRateLimiter. If it is genuinely cheap, add it to"+
			" anonymousUnlimitedRoutes with the reason.", strings.Join(added, "\n  "))
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("anonymousUnlimitedRoutes lists route(s) that are no longer registered"+
			" that way; drop them so the list keeps meaning something:\n  %s", strings.Join(stale, "\n  "))
	}
}

// Both spellings of the Solder API root have to stay registered. This is the URL
// an operator types into the Technic Platform, and only the trailing-slash form
// existed - so "/solder/api" fell through to the panel's HTML catch-all and
// Technic reported an invalid Solder URL for a Solder that was answering
// correctly one character away. Upstream TechnicSolder serves both.
//
// Checked here because this file already reads the registrations out of
// routes.go, and because the failure it guards against is a route DISAPPEARING,
// which no handler test can see.
func TestSolderAPIRootAnswersBothSpellings(t *testing.T) {
	found := anonymousUnlimited(t)
	for _, path := range []string{"/u/{handle}/api", "/u/{handle}/api/"} {
		if !found[path] {
			t.Errorf("the solder API root %q is not registered; a Technic Platform "+
				"entry using that spelling gets the panel's HTML instead of JSON", path)
		}
	}
}

// The specific regression: the one session-less route that pulls a file from
// an external host must stay limited.
func TestBeamDownloadStaysRateLimited(t *testing.T) {
	if anonymousUnlimited(t)["/beam/download"] {
		t.Error("/api/beam/download answers anonymously with no rate limit again: " +
			"each request makes Core fetch the whole Beam binary from its upstream " +
			"and stream it out, so an unbounded caller amplifies through Core")
	}
}
