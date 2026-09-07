package authz

import (
	"fmt"

	"github.com/gorilla/mux"
)

// ExemptRoutes is the allowlist of routes that legitimately carry NO capability
// because they are auth-exempt or public. Keyed by the mux path template
// (route.GetPathTemplate()). Reconciled in Phase 4 Task 22 against the real
// router (see TestEveryRouteIsClassified) so every route is either here, in
// requiredCaps, or in InHandlerAuthzRoutes before the strict coverage flip
// (Task 23).
//
// Two sub-blocks:
//   - PUBLIC: no session auth at all (unauthenticated, or a non-session
//     credential like an API key / shared secret / warp key / share token).
//   - AUTHED-EXEMPT: sits behind AuthMiddleware (a valid session is required)
//     but is intentionally NOT capability-gated because it is the caller's
//     own data/self-service action, a non-privileged helper read, or the
//     in-handler filter/ACL (owner scoping, ticket visibility matrix, BYON
//     ownership, canManageNode, ...) is the real authorization boundary.
var ExemptRoutes = map[string]bool{
	// --- PUBLIC (no session auth) ---
	"/healthz":        true,
	"/api":            true, // /api subrouter mount point, not a handler
	"/api/auth/login": true,
	// Clears the session cookie and nothing else. Unauthenticated on purpose:
	// the state most in need of clearing is a session that can no longer
	// authenticate.
	"/api/auth/logout": true,
	// Authenticated, but capability-free: it returns the CALLER's own session
	// and nothing else, and is refused with 404 anywhere but the Beam desktop
	// client's webview origin.
	"/api/auth/session-token":  true,
	"/api/status":              true,
	"/api/setup/status":        true,
	"/api/setup/admin":         true,
	"/api/auth/demo-login":     true,
	"/api/system/capabilities": true,
	"/api/system/core-info":    true,
	"/api/share/{token}":       true,
	// The share token IS the credential, and this answers only "which host
	// serves it, and will you be asked to sign in" - never a byte of the tab.
	// Rate limited at the route because it is anonymous.
	"/api/tabproxy/{token}/resolve": true,

	"/api/auth/registration-status":     true,
	"/api/auth/register":                true,
	"/api/auth/verify-email":            true,
	"/api/auth/resend-verification":     true,
	"/api/auth/forgot-password":         true,
	"/api/auth/validate-reset-token":    true,
	"/api/auth/reset-password":          true,
	"/api/auth/security-questions/pool": true,

	"/api/maintenance":       true, // public banner state, never blocked by maintenance
	"/api/versions/software": true,
	"/api/tools/beam":        true, // redirect to the beam-relay download, no session

	"/api/beam/download": true, // beam-relay download redirect, no session

	// /api/node/connect used to sit here, exempted as "the node's own identity
	// proof (enroll token / secret) is the credential". It presented neither: the
	// handler accepted a bare node token, which GET /api/nodes publishes. Route
	// and handler are both gone; see the note at its old registration in routes.go.

	// Warp API-key auth (not a user session): the key IS the credential.
	"/api/warp/enroll":     true,
	"/api/warp/link-boot":  true,
	"/api/warp/assignment": true,

	// Store service-to-service calls (shared X-Store-Key header, NOT a panel
	// session): requireStoreKey is the boundary, same category as warp's API key.
	"/api/store/link/verify": true,
	"/api/store/verify-user": true,
	"/api/store/usage":       true,
	"/api/store/provision":   true,
	// Platform defaults, no tenant in the request: what ONE unit includes, so
	// the storefront can print the number without holding a copy of it.
	"/api/store/backup-defaults": true,

	// External API surface: Authorization: Bearer dyl_<key>, not session authz.
	// These are NOT ungated - each declares its capability at
	// APIKeyServerRoute/APIKeyOwnerRoute, which resolves it against the scope of
	// the key through the same HasCap chokepoint. They sit here because
	// requiredCaps describes SESSION authorization and a key is a different
	// credential class, the same reason the warp and store rows above are here.
	"/api/external/rcon/{uuid}/exec":                                  true,
	"/api/external/servers":                                           true,
	"/api/external/usage":                                             true,
	"/api/external/servers/{uuid}":                                    true,
	"/api/external/servers/{uuid}/power":                              true,
	"/api/external/servers/{uuid}/console/history":                    true,
	"/api/external/servers/{uuid}/console/command":                    true,
	"/api/external/servers/{uuid}/stats/history":                      true,
	"/api/external/servers/{uuid}/backup-jobs":                        true,
	"/api/external/servers/{uuid}/backup-jobs/{jobId:[0-9]+}/trigger": true,

	// Public Solder API (Technic Launcher) - registered on the ROOT router
	// with no setup-lock/maintenance/auth middleware, including its own
	// subrouter mount point.
	"/solder":                                       true,
	"/solder/u/{handle}/api":                        true,
	"/solder/u/{handle}/api/":                       true,
	"/solder/u/{handle}/api/modpack":                true,
	"/solder/u/{handle}/api/modpack/{slug}":         true,
	"/solder/u/{handle}/api/modpack/{slug}/{build}": true,
	"/solder/u/{handle}/api/verify/{key}":           true,
	"/solder/api":                                   true,
	"/solder/api/":                                  true,
	"/solder/mirror/{rest:.*}":                      true,

	// --- AUTHED-EXEMPT (AuthMiddleware only; in-handler filter / self / helper) ---

	// Self-service 2FA + profile: the caller manages their OWN account.
	"/api/auth/profile":                     true, // authed; own profile
	"/api/auth/2fa/setup":                   true, // authed; own 2FA
	"/api/auth/2fa/verify":                  true, // authed; own 2FA
	"/api/auth/2fa/disable":                 true, // authed; own 2FA
	"/api/auth/2fa/regenerate-backup-codes": true, // authed; own 2FA
	"/api/auth/2fa/status":                  true, // authed; own 2FA

	// Read-only capability catalog: any authed user, not yet consulted elsewhere.
	"/api/authz/catalog": true, // authed; read-only reference data
	"/api/authz/presets": true, // authed; read-only preset reference data
	"/api/authz/mode":    true, // authed; read-only delegation mode, owner-inclusive

	// Caller's own metered usage / billing / history / regions.
	"/api/me/usage":               true, // authed; own usage
	"/api/me/billing":             true, // authed; own billing
	"/api/me/entitlement":         true, // authed; own entitlement (what I may use)
	"/api/me/username-history":    true, // authed; own history
	"/api/me/regions":             true, // authed; own region assignment
	"/api/me/security-questions":  true, // authed; own security questions
	"/api/me/updates-seen":        true, // authed; own update-feed badge marker
	"/api/me/servers/via-tickets": true, // authed; own tickets sidebar

	// The caller's OWN machine. Not capability-gated on purpose: no customer
	// holds nodes.delete, and these two answer ONLY for a node whose owner_id is
	// the caller - an admin asking about somebody else's gets a 404 here and uses
	// the capability-gated /api/nodes/{id} instead.
	"/api/me/nodes/{id:[0-9]+}":          true, // authed; own machine, owner-checked in the handler
	"/api/me/nodes/{id:[0-9]+}/contents": true, // authed; own machine, owner-checked in the handler

	// Sessions helpers: SSE ticket mint, SSE stream, platform-wide feature flags.
	"/api/sse-ticket":      true, // authed; mints own disposable SSE ticket
	"/api/system/events":   true, // authed; SSE stream, ticket-authed per-connection
	"/api/system/features": true, // authed; read for every authenticated user

	// Coarse storage connection state: the initial value for the
	// storage.connection.changed events on that same SSE stream, and no more
	// revealing than they are (no cause, path, bucket or endpoint).
	"/api/storage/connection": true, // authed; read-only coarse state

	// BYON node enrollment tokens: per-user, own tokens only (byonCallerID scoping).
	"/api/nodes/enroll-token":      true, // authed; in-handler owner filter
	"/api/nodes/enroll-token/{id}": true, // authed; in-handler owner filter

	// Node INSPECT reads: canManageNode admin-vs-BYON-owner data filter is the boundary.
	"/api/nodes/{id:[0-9]+}/servers":       true, // authed; in-handler canManageNode filter
	"/api/nodes/{id:[0-9]+}/storage":       true, // authed; in-handler canManageNode filter
	"/api/nodes/{id:[0-9]+}/deploy-bundle": true, // authed; in-handler canManageNode filter
	"/api/nodes/{id:[0-9]+}/cpu":           true, // authed; in-handler canManageNode filter

	// Placement helpers: node-picking reads/preview, not privilege-sensitive alone.
	"/api/placement/pick":    true, // authed; helper
	"/api/placement/tags":    true, // authed; helper
	"/api/placement/regions": true, // authed; helper

	// User-facing servers list/create and enabled-region picker.
	"/api/servers": true, // authed; own servers (list/create), plan-limited in-handler
	"/api/regions": true, // authed; enabled-region picker

	// Cron preview: pure transform, no server scoping needed.
	"/api/scheduled-tasks/validate": true, // authed; pure preview

	// Modrinth external proxy: read-only, cached, rate-limited, all authed.
	"/api/modrinth/search":                  true, // authed; external proxy
	"/api/modrinth/project/{slug}":          true, // authed; external proxy
	"/api/modrinth/project/{slug}/versions": true, // authed; external proxy
	"/api/modrinth/version/{id}":            true, // authed; external proxy
	"/api/modrinth/categories":              true, // authed; external proxy
	"/api/modrinth/game-versions":           true, // authed; external proxy

	// Library browse/download: platform-shared catalog, no gate beyond auth
	// (Phase 4 Task 20 INSPECT result; mutations ARE RequireCap-gated).
	"/api/library":          true, // authed; platform-shared catalog read
	"/api/library/download": true, // authed; platform-shared catalog read

	// Gateway tenant self-service helpers: in-handler owner filter is the boundary.
	"/api/gateway/check-domain":            true, // authed; owner-facing availability hint
	"/api/gateway/route-options":           true, // authed; user-facing route form config
	"/api/gateway/link-routes":             true, // authed; in-handler owner filter
	"/api/gateway/link-routes/{domain:.+}": true, // authed; in-handler owner filter

	// Warp route-only link kits: tenant self-service, BYON-gated + owner-filtered in-handler.
	"/api/warp/link-kits":               true, // authed; in-handler owner filter
	"/api/warp/link-kits/{linkID}":      true, // authed; in-handler owner filter
	"/api/warp/link-kits/{linkID}/roll": true, // authed; in-handler owner filter, same as the revoke beside it

	// Beam: caller's own servers/ticket/config.
	"/api/beam/servers": true, // authed; own beam-eligible servers
	"/api/beam/ticket":  true, // authed; own beam ticket
	"/api/beam/config":  true, // authed; needed by every user for the Files tab

	// User-facing filemanager limits: every authed user needs their own limits.
	"/api/settings/filemanager/limits": true, // authed; own upload/download limits

	// Store panel-session calls (distinct from the service-to-service PUBLIC ones above).
	"/api/store/link/start":             true, // authed; panel-user session
	"/api/warp/node-keys":               true, // authed; own BYON node keys, BYON+gateway gated in-handler
	"/api/warp/node-keys/{nodeID}":      true, // authed; own BYON node key (owner-checked in-handler)
	"/api/warp/node-keys/{nodeID}/roll": true, // authed; own BYON node key (owner-checked in-handler)
	// Overlay addresses for the deploy snippet. Read-only RFC1918 values that
	// authorize nothing without a warp key, needed by exactly the tenants who
	// mint one on /nodes.
	"/api/warp/deploy-config":    true, // authed; no per-owner data to filter
	"/api/store/status":          true, // authed; panel-user session
	"/api/store/account-summary": true, // authed; the caller's OWN account only
	"/api/store/billing-consent": true, // authed; the caller's OWN account only

	// Versions: read for any authed user (software list itself is PUBLIC above).
	"/api/versions": true, // authed; version info

	// Tickets: user-facing subsystem (Phase 4 Task 15) - the owner/watcher/support
	// ACL matrix in tickets.go (canSeeTicket/canReply/canMutate) or, for DELETE,
	// the admin-only in-handler gate in ticket_deletions.go, is the boundary.
	"/api/ticket-categories":                                     true, // authed; enabled-only list for create form
	"/api/ticket-canned-responses":                               true, // authed; in-handler support-or-admin check
	"/api/tickets":                                               true, // authed; own tickets list/create
	"/api/tickets/{id:[0-9]+}":                                   true, // authed; GET canSeeTicket / DELETE admin-in-handler
	"/api/tickets/{id:[0-9]+}/messages":                          true, // authed; in-handler canReply
	"/api/tickets/{id:[0-9]+}/status":                            true, // authed; in-handler canMutate
	"/api/tickets/{id:[0-9]+}/priority":                          true, // authed; in-handler canMutate
	"/api/tickets/{id:[0-9]+}/assignment":                        true, // authed; in-handler canMutate
	"/api/tickets/{id:[0-9]+}/watchers":                          true, // authed; in-handler support-or-admin check
	"/api/tickets/{id:[0-9]+}/watchers/{userId:[0-9a-f-]{36}}":   true, // authed; in-handler ACL
	"/api/tickets/{id:[0-9]+}/attachments":                       true, // authed; in-handler ACL
	"/api/tickets/{id:[0-9]+}/attachments/{aid:[0-9]+}":          true, // authed; in-handler ACL
	"/api/tickets/{id:[0-9]+}/attachments/{aid:[0-9]+}/download": true, // authed; in-handler ACL

	// Notifications: in-app inbox, scoped to the caller's own userID in-handler.
	"/api/notifications":                  true, // authed; own inbox
	"/api/notifications/unread-count":     true, // authed; own inbox
	"/api/notifications/{id:[0-9]+}/read": true, // authed; own inbox
	"/api/notifications/read-all":         true, // authed; own inbox

	// In-panel update feed: admin-only, gated in-handler via IsAdmin.
	"/api/updates": true, // authed; admin-only in-handler (IsAdmin)
}

// InHandlerAuthzRoutes are routes whose scope object is NOT a path {id}/{uuid}
// (file browser via ?server_uuid=, backup jobs/runs via the job/run row, power
// dispatched per body action). RequireCap cannot resolve them at the route, so
// authorization runs in-handler through the SAME resolver. Listed here so
// strict coverage treats them as covered while documenting they are NOT public.
var InHandlerAuthzRoutes = map[string]bool{
	// Custom-domain ownership claims. The scope object is the CALLER: each
	// handler reads userID from the session and only ever touches that user's
	// rows. A capability would be the wrong shape here - there is no such thing
	// as reading "someone else's" claim to gate.
	"/api/gateway/custom-domains":                        true,
	"/api/gateway/custom-domains/{domain:.+}/txt-token":  true,
	"/api/gateway/custom-domains/{domain:.+}/verify-txt": true,

	"/api/servers/{id:[0-9]+}/power":           true, // per-action power.* in-handler
	"/api/files":                               true,
	"/api/files/content":                       true,
	"/api/files/save":                          true,
	"/api/files/create":                        true,
	"/api/files/rename":                        true,
	"/api/files/copy":                          true,
	"/api/files/delete":                        true,
	"/api/files/download":                      true,
	"/api/files/download/selective":            true,
	"/api/files/upload":                        true,
	"/api/backup-jobs/{jobId:[0-9]+}":          true,
	"/api/backup-jobs/{jobId:[0-9]+}/trigger":  true,
	"/api/backup-jobs/{jobId:[0-9]+}/runs":     true,
	"/api/backup-runs/{runId:[0-9]+}/download": true,
	"/api/backup-runs/{runId:[0-9]+}/restore":  true,
	"/api/backup-runs/{runId:[0-9]+}":          true,
}

// RouteCoverageViolations walks router and returns a human-readable list of
// routes that lack a declared capability. required maps a route path template
// to its capability id; ExemptRoutes and InHandlerAuthzRoutes are always
// allowed to be uncovered (the latter enforces via the resolver in-handler
// instead of at the route).
//
// Phase 1 runs in PERMISSIVE mode (strict=false): it returns nil so the test is
// green before any route is annotated. Phase 2 populates required for every
// route and calls this with strict=true from a test that walks the REAL router
// (extracted into a testable builder), asserting zero violations - the standing
// guarantee that every route is gated by a meaningful capability.
func RouteCoverageViolations(router *mux.Router, required map[string]string, strict bool) ([]string, error) {
	if !strict {
		return nil, nil
	}
	var violations []string
	err := router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tmpl, terr := route.GetPathTemplate()
		if terr != nil {
			// Routes without a path template (pure matchers / middleware mounts)
			// carry nothing to gate; skip them.
			return nil
		}
		if ExemptRoutes[tmpl] || InHandlerAuthzRoutes[tmpl] {
			return nil
		}
		if _, ok := required[tmpl]; !ok {
			methods, _ := route.GetMethods()
			violations = append(violations, fmt.Sprintf("%v %s", methods, tmpl))
		}
		return nil
	})
	return violations, err
}
