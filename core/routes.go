package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"dylaris-core/handlers"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// routeCfg carries the boot-time config values that buildAPIRouter's handler
// constructors and routes need, threaded explicitly (instead of a raw
// *config.Config) so this file's dependency surface stays small and testable.
type routeCfg struct {
	JWTSecret          string
	Region             string
	CoreID             string
	TabProxyHostSuffix string
	ClusterSecret      string
	GatewayHubURL      string
	ModrinthUA         string
	// Panel is the embedded panel bundle, mounted as the router's fallback.
	// Nil in tests, where nothing asks for a page.
	Panel http.Handler
}

// routeExtras carries the handler/service instances main() still needs after
// buildAPIRouter returns: the warp service (boot-time resync watcher) and the
// settings handler (boot-time firewall-allowlist publish), plus the proxy
// handler, which main wires into the host dispatch that takes tab-content
// requests off the wire before this router sees them.
type routeExtras struct {
	settingsHandler *handlers.SettingsHandler
	warpService     *services.WarpService
	proxyHandler    *handlers.ProxyHandler
}

// requiredCaps maps a route's mux path template to the capability id that
// must gate it. Empty until the cut-over batches (Phase 4, Tasks 3+) fill it
// in; coverage runs in permissive mode (see routes_coverage_test.go) until then.
var requiredCaps = map[string]string{
	// Phase 4 Task 3: server lifecycle / power / proxy / storage.
	// POST /api/servers/{id:[0-9]+}/power is deliberately absent: the action
	// (start|stop|restart|kill) lives in the body, so it is in-handler-enforced
	// (Rule R5) rather than RequireCap-gated at the route.
	"/api/servers/{id:[0-9]+}/install-cooldown":            "overview.read",
	"/api/servers/{id:[0-9]+}/setup":                       "server.settings.write",
	"/api/servers/{id:[0-9]+}/runtime":                     "server.settings.write",
	"/api/servers/{id:[0-9]+}/reinstall":                   "server.settings.write",
	"/api/servers/{id:[0-9]+}/version-update":              "server.settings.write",
	"/api/servers/{id:[0-9]+}/copy-sub-server":             "server.settings.write",
	"/api/servers/{id:[0-9]+}/switch":                      "server.settings.write",
	"/api/servers/{id:[0-9]+}/name":                        "server.settings.write",
	"/api/servers/{id:[0-9]+}/resources":                   "server.settings.write",
	"/api/servers/{id:[0-9]+}/loader-metadata":             "server.settings.write",
	"/api/servers/{id:[0-9]+}":                             "server.delete",
	"/api/servers/{id:[0-9]+}/sub-servers/{subServerName}": "server.delete",
	"/api/servers/{id:[0-9]+}/proxy":                       "network.write",
	"/api/servers/{id:[0-9]+}/proxy-endpoint":              "network.read",
	// storage-path answers NODE-wide (every path on the machine + capacity), so
	// it takes the same cap as the migrate action it feeds - see the handler.
	"/api/servers/{id:[0-9]+}/storage-path":     "server.settings.write",
	"/api/servers/{id:[0-9]+}/migrate-storage":  "server.settings.write",
	"/api/servers/{id:[0-9]+}/sftp-credentials": "sftp.access",
	"/api/servers/{id:[0-9]+}/migration-status": "overview.read",
	"/api/servers/{id:[0-9]+}/automove":         "server.settings.write",
	"/api/servers/{id:[0-9]+}/transfer":         "server.settings.write",
	"/api/servers/{id:[0-9]+}/edge-motd":        "server.settings.write",

	// Phase 4 Task 4: console + RCON. /rcon/config serves both GET (config.read)
	// and PUT (config.write) on the same template; config.read is the
	// representative here, the per-method fine cap lives at each RequireCap call.
	"/api/servers/{id:[0-9]+}/console/history": "console.read",
	"/api/servers/{id:[0-9]+}/console/stream":  "console.read",
	"/api/servers/{id:[0-9]+}/console/command": "console.send",
	"/api/servers/{id:[0-9]+}/rcon":            "rcon.exec",

	// Player management. players.read/players.manage were in the catalog and in
	// four presets and enforced NOWHERE - the Players page ran on rcon.exec plus
	// files.read, so delegating "ban someone" meant handing out every RCON
	// command and the whole filesystem. These three are what makes the pair mean
	// something; see handlers/players.go.
	"/api/servers/{id:[0-9]+}/players/lists":  "players.read",
	"/api/servers/{id:[0-9]+}/players/online": "players.read",
	"/api/servers/{id:[0-9]+}/players/action": "players.manage",
	"/api/servers/{id:[0-9]+}/rcon/config":    "config.read",

	// Phase 4 Task 6: mods + custom tabs. GET+POST /mods share one template ->
	// mods.read is the representative; the fine POST->mods.write lives at the
	// RequireCap call. GET+POST /tabs share -> tabs.read representative;
	// PATCH+DELETE /tabs/{tabId} share -> tabs.write.
	"/api/servers/{id:[0-9]+}/mods":                             "mods.read",
	"/api/servers/{id:[0-9]+}/mods/{modId:[0-9]+}":              "mods.delete",
	"/api/servers/{id:[0-9]+}/mods/compat":                      "mods.read",
	"/api/servers/{id:[0-9]+}/mods/unmanaged":                   "mods.read",
	"/api/servers/{id:[0-9]+}/mods/history":                     "mods.read",
	"/api/servers/{id:[0-9]+}/mods/identify":                    "mods.write",
	"/api/servers/{id:[0-9]+}/modpack-contents":                 "mods.read",
	"/api/servers/{id:[0-9]+}/installs":                         "server.settings.write",
	"/api/servers/{id:[0-9]+}/tabs":                             "tabs.read",
	"/api/servers/{id:[0-9]+}/tabs/{tabId:[0-9]+}":              "tabs.write",
	"/api/servers/{id:[0-9]+}/tabs/{tabId:[0-9]+}/share-link":   "tabs.write",
	"/api/servers/{id:[0-9]+}/tabs/{tabId:[0-9]+}/proxy-ticket": "tabs.read",

	// Phase 4 Task 7: scheduled tasks + spark. GET+POST /scheduled-tasks share
	// one template -> schedule.read representative; the fine POST->schedule.write
	// lives at the RequireCap call. PATCH+DELETE /scheduled-tasks/{taskId} share
	// -> schedule.write representative; the fine DELETE->schedule.delete lives at
	// the RequireCap call. /scheduled-tasks/validate has no {id} and is authed-
	// exempt (pure cron preview) - intentionally NOT listed here.
	"/api/servers/{id:[0-9]+}/scheduled-tasks":                   "schedule.read",
	"/api/servers/{id:[0-9]+}/scheduled-tasks/{taskId:[0-9]+}":   "schedule.write",
	"/api/servers/{id:[0-9]+}/spark/profiles":                    "spark.use",
	"/api/servers/{id:[0-9]+}/spark/profiles/{profileId:[0-9]+}": "spark.use",

	// Phase 4 Task 8: stats + per-server audit. Stats reads rely on the demo-read
	// short-circuit (stats.read is not in authz.demoReadDeny) rather than any
	// in-handler isDemoServer check. Audit view is server.audit.read (it was
	// overview.read, which every invite grants - see the route registration
	// below); audit/force moves from admin-only to server.settings.write so an
	// owner (or a role-holder with that cap) controls their own server's forced
	// audit - admin still passes via the resolver's admin short-circuit.
	"/api/servers/{id:[0-9]+}/stats/stream":  "stats.read",
	"/api/servers/{id:[0-9]+}/stats/history": "stats.read",
	"/api/servers/{id:[0-9]+}/stats/disk":    "stats.read",
	"/api/traffic-limits":                    "settings.write",
	"/api/traffic-limits/resolve":            "settings.read",
	"/api/gateway-bandwidth/overview":        "settings.read",
	"/api/gateway-bandwidth/history":         "settings.read",
	"/api/gateway-bandwidth/rebalance":       "settings.read",
	"/api/servers/{id:[0-9]+}/audit":         "server.audit.read",
	"/api/servers/{id:[0-9]+}/audit/status":  "server.audit.read",
	"/api/servers/{id:[0-9]+}/audit/force":   "server.settings.write",

	// Phase 4 Task 9: members (SERVER scope) + owner server-roles/grants (OWNER
	// scope). GET+POST /members share one template -> members.read
	// representative; the fine POST->members.write lives at the RequireCap
	// call. PATCH+DELETE /members/{userId} share -> members.write
	// representative; the fine DELETE->members.delete lives at the RequireCap
	// call. GET+POST /server-roles share -> roles.read representative;
	// PATCH+DELETE /server-roles/{id} share -> roles.write representative
	// (fine DELETE->roles.delete at the call). /grants (POST+DELETE, no path
	// {id}) is gated with the OWNER-scope roles.write/roles.delete, NOT the
	// SERVER-scope members.write/delete: members.* is ScopeServer, and
	// RequireCap 403s unconditionally for a ScopeServer cap when the path has
	// no {id}/{uuid} to resolve a server from (see middleware.go), which would
	// permanently break this route for every caller including admins. Using
	// the OWNER-scope roles.* cap instead reproduces the F2 chokepoint-open
	// pattern (serverID==0 -> ownerSelf -> any authenticated user passes). The
	// real per-target delegation check (members.write/delete against the
	// TARGET server) stays enforced inside AssignGrant/RevokeGrant (Phase 3)
	// and is untouched.
	"/api/servers/{id:[0-9]+}/members":                        "members.read",
	"/api/servers/{id:[0-9]+}/members/inherited":              "members.read",
	"/api/servers/{id:[0-9]+}/members/{userId:[0-9a-f-]{36}}": "members.write",
	"/api/server-roles":                                       "roles.read",
	"/api/server-roles/{id:[0-9]+}":                           "roles.write",
	"/api/grants":                                             "roles.write",

	// Phase 4 Task 10: backups. backup-storages are platform-shared storage-
	// provider configs -> PANEL settings.read/write (controller decision #4).
	// The /backup-jobs/{jobId} and /backup-runs/{runId} families resolve their
	// server from the job/run row, not a path {id}/{uuid} -> in-handler only
	// (Rule R5), deliberately NOT listed here (they go into
	// authz.InHandlerAuthzRoutes in Task 22).
	"/api/backup-storages":                  "settings.read",
	"/api/backup-storages/{id:[0-9]+}":      "settings.write",
	"/api/backup-storages/{id:[0-9]+}/test": "settings.write",
	// A TENANT's own bucket, which is a different thing from the platform's:
	// owner-scoped capabilities, and every handler is anchored to the caller's
	// own id rather than filtering an all-rows read.
	"/api/me/backup-storages":                  "backupstorage.read",
	"/api/me/backup-storages/{id:[0-9]+}":      "backupstorage.write",
	"/api/me/backup-storages/{id:[0-9]+}/test": "backupstorage.write",
	// storage-connections are the named, shared storage-provider configs that
	// generalize backup-storages -> same PANEL settings.read/write treatment.
	"/api/storage-connections":                  "settings.read",
	"/api/storage-connections/{id:[0-9]+}":      "settings.write",
	"/api/storage-connections/{id:[0-9]+}/test": "settings.write",
	// Same capability as the saved-connection probe: it runs the same write,
	// read-back and delete against an operator-supplied endpoint.
	"/api/storage-connections/test": "settings.write",
	// The platform's OWN backups. settings.write throughout, the DOWNLOAD
	// included: a platform bundle is the whole database, and the database holds
	// every node secret, every storage credential and every user row. Reading
	// configuration and downloading the installation are not the same
	// permission.
	"/api/platform-backups/jobs":                      "settings.write",
	"/api/platform-backups/jobs/{id:[0-9]+}":          "settings.write",
	"/api/platform-backups/jobs/{id:[0-9]+}/run":      "settings.write",
	"/api/platform-backups/jobs/{id:[0-9]+}/runs":     "settings.write",
	"/api/platform-backups/runs/{id:[0-9]+}/download": "settings.write",
	"/api/platform-backups/targets":                   "settings.write",
	"/api/platform-backups/passphrase":                "settings.write",
	"/api/platform-backups/inspect":                   "settings.write",
	"/api/platform-backups/restore":                   "settings.write",
	"/api/platform-backups/runs/{id:[0-9]+}/restore":  "settings.write",
	"/api/servers/{id:[0-9]+}/backup-jobs":            "backups.read",
	"/api/servers/{id:[0-9]+}/backup-restores":        "backups.read",
	"/api/servers/{id:[0-9]+}/backup-usage":           "backups.read",

	// Phase 4 Task 11: per-server network (gateway) routes. GET+POST /routes
	// share one template -> network.read representative; the fine
	// POST->network.write lives at the RequireCap call. /gateway/link-routes/*
	// (tenant self-service, no server {id}) is EXEMPT-authed (controller
	// decision #2) - deliberately NOT listed here.
	"/api/servers/{id:[0-9]+}/routes":             "network.read",
	"/api/servers/{id:[0-9]+}/routes/{domain:.+}": "network.write",

	// Phase 4 Task 12: users + roles + panel-roles + admin 2FA reset (PANEL
	// users.*/panelroles.*). GET+POST /users share one template -> users.read
	// representative; the fine POST->users.write lives at the RequireCap call.
	// /me/username-history is EXEMPT-authed (own history) - deliberately NOT
	// listed here. See the panel-role escalation note at the route registration
	// above: panelroles.write has no delegation check, accepted as safe-by-
	// default for this cut-over.
	"/api/users":                                           "users.read",
	"/api/users/{id:[0-9a-f-]{36}}":                        "users.delete",
	"/api/users/{id:[0-9a-f-]{36}}/password":               "users.write",
	"/api/users/{id:[0-9a-f-]{36}}/2fa":                    "users.write",
	"/api/users/{id:[0-9a-f-]{36}}/route-limit":            "users.read",
	"/api/admin/users/{id:[0-9a-f-]{36}}/cancel-deletion":  "users.write",
	"/api/admin/users/{id:[0-9a-f-]{36}}/role":             "users.write",
	"/api/admin/users/{id:[0-9a-f-]{36}}/permissions":      "users.write",
	"/api/admin/users/{id:[0-9a-f-]{36}}/username-history": "users.read",
	"/api/admin/users/{id:[0-9a-f-]{36}}/username":         "users.write",
	"/api/admin/users/{id:[0-9a-f-]{36}}/email":            "users.write",
	"/api/admin/users/{id:[0-9a-f-]{36}}/panel-role":       "panelroles.write",
	"/api/admin/panel-roles":                               "panelroles.read",
	"/api/admin/panel-roles/{id:[0-9]+}":                   "panelroles.write",

	// Phase 4 Task 13: nodes / admission / disk-orphans / admin servers (PANEL
	// nodes.*/servers.*). "/api/nodes" (GET list + POST create share one
	// template) is deliberately NOT listed: GET is authed-exempt (owner-inclusive
	// filter) while POST is RequireCap("nodes.write")-gated at the route: since
	// this map has no per-method granularity, recording either cap here would
	// misrepresent the other method, so the shared-template reconciliation for
	// "/api/nodes" is deferred to the Task 22/23 ExemptRoutes pass. The 4
	// INSPECT node reads (servers/storage/deploy-bundle/cpu - each carries its
	// own canManageNode admin-vs-BYON-owner data filter), /nodes/enroll-token*,
	// and /placement/* likewise stay authed-exempt and are NOT listed here (see
	// the route registration comments).
	"/api/admin/settings/node-admission":               "nodes.read",
	"/api/admin/settings/node-admission/cidrs":         "nodes.write",
	"/api/admin/settings/node-admission/cidrs/{id}":    "nodes.delete",
	"/api/nodes/{id:[0-9]+}":                           "nodes.write",
	"/api/nodes/{id:[0-9]+}/config":                    "nodes.write",
	"/api/nodes/{id:[0-9]+}/storage-placement":         "nodes.write",
	"/api/settings/storage-placement":                  "settings.read",
	"/api/nodes/{id:[0-9]+}/force":                     "nodes.delete",
	"/api/nodes/{id:[0-9]+}/placement":                 "nodes.write",
	"/api/admin/servers":                               "servers.read",
	"/api/admin/servers/{id:[0-9]+}/owner":             "servers.write",
	"/api/admin/servers/{id:[0-9]+}/move":              "servers.write",
	"/api/admin/servers/{id:[0-9]+}/migration/cancel":  "servers.write",
	"/api/admin/nodes/{id:[0-9]+}/disk-analysis":       "nodes.read",
	"/api/admin/nodes/{id:[0-9]+}/orphan":              "nodes.delete",
	"/api/admin/nodes/{id:[0-9]+}/reset-pairing":       "nodes.write",
	"/api/disk/orphans/{nodeId:[0-9]+}/{uuid}/files":   "nodes.read",
	"/api/disk/orphans/{nodeId:[0-9]+}/{uuid}/content": "nodes.read",
	"/api/disk/orphans/{nodeId:[0-9]+}/{uuid}/inspect": "nodes.read",
	"/api/disk/orphans/assign":                         "nodes.write",

	// Phase 4 Task 14: region admin (PANEL regions.*). "/api/regions" and
	// "/api/me/regions" stay authed-exempt (enabled-region picker / own
	// assignment) and are deliberately NOT listed here.
	"/api/admin/regions":                          "regions.read",
	"/api/admin/regions/{id}":                     "regions.write",
	"/api/admin/users/{id:[0-9a-f-]{36}}/regions": "regions.read",

	// Phase 4 Task 15: ticket admin management (PANEL tickets.*). The
	// tickets subsystem MIXES two authz models: this batch only lists the
	// ADMIN-management routes below. User-facing ticket routes (/tickets,
	// /tickets/{id} incl. its DELETE, messages, status/priority/assignment/
	// watchers, attachments, the public /ticket-categories and
	// /ticket-canned-responses GETs, /notifications*, /me/servers/via-tickets)
	// stay authed-exempt on purpose and are deliberately NOT listed here -
	// they keep authorizing in-handler via the owner/watcher/support ACL in
	// tickets.go (or, for DELETE /tickets/{id}, the admin-only in-handler
	// gate in ticket_deletions.go), which is the real boundary for them.
	"/api/admin/ticket-categories":                   "tickets.read",
	"/api/admin/ticket-categories/{id:[0-9]+}":       "tickets.write",
	"/api/tickets/inbox":                             "tickets.read",
	"/api/admin/settings/tickets":                    "tickets.read",
	"/api/admin/ticket-canned-responses":             "tickets.read",
	"/api/admin/ticket-canned-responses/{id:[0-9]+}": "tickets.write",
	"/api/admin/tickets/migration/status":            "tickets.read",
	"/api/admin/tickets/migration/test-connection":   "tickets.write",
	"/api/admin/tickets/migration/dry-run":           "tickets.write",
	"/api/admin/tickets/migration/execute":           "tickets.write",
	"/api/admin/tickets/backup":                      "tickets.write",
	"/api/admin/tickets/backups":                     "tickets.read",
	"/api/admin/tickets/backups/{name}/download":     "tickets.read",
	"/api/admin/tickets/backups/{name}":              "tickets.write",
	"/api/admin/tickets/restore/init":                "tickets.write",
	"/api/admin/tickets/restore/execute":             "tickets.write",
	"/api/admin/tickets/deletion-log":                "tickets.read",

	// Phase 4 Task 16: plans + billing admin (PANEL plans.*). "/api/me/usage"
	// and "/api/me/billing" are the caller's OWN data and stay authed-exempt
	// (deliberately NOT listed here).
	"/api/admin/users/{id:[0-9a-f-]{36}}/limit-overrides":   "plans.write",
	"/api/admin/settings/billing":                           "plans.read",
	"/api/admin/users/{id:[0-9a-f-]{36}}/billing":           "plans.read",
	"/api/admin/users/{id:[0-9a-f-]{36}}/billing-overrides": "plans.write",
	// One path, three methods: plans.read for the GET and plans.write for the
	// grant/revoke. The map is keyed by path, so the stricter cap is recorded
	// here and each route carries its own RequireCap.
	"/api/admin/users/{id:[0-9a-f-]{36}}/entitlement": "plans.write",
	"/api/admin/usage": "plans.read",

	// Phase 4 Task 17: admin platform-settings surface (PANEL settings.*).
	// Uniform mapping: reads -> settings.read, writes (POST/PUT/PATCH/DELETE) ->
	// settings.write (there is no settings.delete cap). GET /api/maintenance
	// (public banner), GET /api/system/features (Task 19), /api/gateway/route-options,
	// and /api/settings/filemanager/limits stay authed/public-exempt (deliberately
	// NOT listed here). Three routes share their template with an EXEMPT-authed GET
	// counterpart on the SAME path (GET /api/settings/features, GET /api/settings/beam,
	// GET /api/settings/routing-mode all read for every authed user; only their
	// POST save is settings.write) - like the "/api/nodes" precedent (Task 13), a
	// shared template can't record two different per-method caps here, so those
	// three are deliberately NOT listed and are deferred to the Task 22/23
	// per-method ExemptRoutes reconciliation.
	"/api/admin/settings/users":                          "settings.read",
	"/api/admin/settings/modpacks":                       "settings.read",
	"/api/admin/settings/mod-cache":                      "settings.read",
	"/api/admin/settings/link-updates":                   "settings.read",
	"/api/nodes/link-updates":                            "nodes.read",
	"/api/admin/settings/modpacks/delivery-capabilities": "settings.read",
	"/api/admin/users/{id:[0-9a-f-]{36}}/modpack-flag":   "settings.write",
	"/api/admin/settings/permissions-mode":               "settings.write",
	"/api/admin/settings/features":                       "settings.read",
	"/api/admin/settings/tab-proxy":                      "settings.read",
	"/api/admin/settings/metrics-db":                     "settings.read",
	"/api/admin/settings/metrics-db/test":                "settings.write",
	"/api/admin/health":                                  "settings.read",
	"/api/admin/db/migration":                            "settings.read",
	"/api/admin/db/migration/test-connection":            "settings.write",
	"/api/admin/db/migration/verify":                     "settings.write",
	"/api/admin/db/hypertable":                           "settings.read",
	"/api/admin/db/hypertable/convert":                   "settings.write",
	// Blob storage migration (Task 15). /api/admin/storage/migration is one
	// template shared by GET (job status) and POST (start): the manifest's
	// documented convention records the representative .read cap here and the
	// per-method cap lives at the RequireCap call, same as
	// /api/admin/db/migration above. That is why 7 routes yield 6 entries.
	"/api/admin/storage/overview":                     "settings.read",
	"/api/admin/storage/migration":                    "settings.read",
	"/api/admin/storage/migration/cancel":             "settings.write",
	"/api/admin/storage/manifests":                    "settings.read",
	"/api/admin/storage/manifests/{id:[0-9]+}":        "settings.write",
	"/api/admin/storage/manifests/{id:[0-9]+}/export": "settings.read",
	"/api/admin/maintenance":                          "settings.write",
	"/api/admin/settings/audit":                       "settings.read",
	"/api/admin/xdp/config":                           "settings.read",
	"/api/settings/core-storage":                      "settings.read",
	"/api/settings/core-storage/test":                 "settings.write",
	"/api/settings/storage-reach":                     "settings.read",
	"/api/settings/filemanager":                       "settings.read",
	"/api/settings/gateway":                           "settings.read",
	"/api/settings/gateway/dns":                       "settings.read",
	// settings.write, not read: it sends an operator-supplied credential to a
	// third-party DNS API.
	"/api/settings/gateway/dns/probe":             "settings.write",
	"/api/settings/placement":                     "settings.read",
	"/api/admin/servers/{id:[0-9]+}/demo":         "settings.write",
	"/api/admin/settings/demo-account":            "settings.read",
	"/api/settings/servers":                       "settings.read",
	"/api/settings/backup":                        "settings.read",
	"/api/settings/warp-firewall":                 "settings.read",
	"/api/admin/settings/security-questions-pool": "settings.read",
	"/api/admin/settings/auth":                    "settings.read",
	"/api/admin/settings/smtp":                    "settings.read",
	"/api/admin/settings/smtp/test":               "settings.write",
	"/api/admin/mail/templates":                   "settings.write",
	"/api/admin/mail/templates/{key}":             "settings.write",
	"/api/admin/mail/templates/{key}/preview":     "settings.write",
	"/api/admin/mail/templates/{key}/test":        "settings.write",

	// Phase 4 Task 18: gateway/warp topology + dns + infrastructure oversight
	// (PANEL topology.*). /api/warp/enroll, /api/warp/assignment, /api/warp/link-boot
	// (warp API-key auth, not a session) and /api/warp/link-kits/* (tenant
	// self-service, BYON-gated + owner-filtered in-handler) are deliberately NOT
	// listed here - they stay EXEMPT-authed (Phase 4 controller decision #2).
	"/api/gateway/links":                     "topology.read",
	"/api/gateway/edges":                     "topology.read",
	"/api/gateway/routes":                    "topology.read",
	"/api/gateway/dns-check":                 "topology.read",
	"/api/gateway/routes/suffixes":           "topology.read",
	"/api/gateway/routes/bulk-delete":        "topology.write",
	"/api/gateway/routes/{domain:.+}":        "topology.write",
	"/api/gateway/logs":                      "topology.read",
	"/api/gateway/stats":                     "topology.read",
	"/api/gateway/sync":                      "topology.write",
	"/api/gateway/errors":                    "topology.read",
	"/api/infrastructure/overview":           "topology.read",
	"/api/admin/metrics/catalog":             "topology.read",
	"/api/admin/metrics/series":              "topology.read",
	"/api/admin/metrics/summary":             "topology.read",
	"/api/admin/metrics/export":              "topology.read",
	"/api/infrastructure/routing-migration":  "topology.read",
	"/api/warp/regions":                      "topology.read",
	"/api/warp/regions/{region}":             "topology.write",
	"/api/warp/leaders":                      "topology.write",
	"/api/warp/leaders/{leaderId}":           "topology.write",
	"/api/admin/warp/keys":                   "topology.write",
	"/api/admin/warp/keys/{id:[0-9]+}":       "topology.write",
	"/api/admin/warp/keys/{id:[0-9]+}/purge": "topology.write",

	// Phase 4 Task 19: module mutations (PANEL settings.write - there is no
	// dedicated modules.* cap). GET /api/modules is EXEMPT-authed (navbar
	// render for every authenticated user, filtered in-handler by role) and
	// shares its bare "/api/modules" template with POST /api/modules; like the
	// "/api/nodes" (Task 13) and Task 17 settings/features|beam|routing-mode
	// precedent, a shared exempt-GET/gated-write template can't record two
	// different per-method caps in this map, so "/api/modules" is deliberately
	// NOT listed here and is deferred to the Task 22/23 per-method ExemptRoutes
	// reconciliation. Its mutation siblings each have their own {id}-suffixed
	// template and ARE listed. /versions, /versions/software, /system/features,
	// /authz/catalog, /system/events, /sse-ticket, and /me/security-questions
	// stay authed-exempt on purpose and are deliberately NOT listed here.
	"/api/modules/{id:[0-9]+}":          "settings.write",
	"/api/modules/{id:[0-9]+}/toggle":   "settings.write",
	"/api/modules/{id:[0-9]+}/position": "settings.write",
	"/api/modules/{id:[0-9]+}/role":     "settings.write",

	// Phase 4 Task 20: modpack builder + solder owner tools (OWNER modpack.*).
	// These routes are chokepoint-open by design for any authenticated user
	// (serverID==0 -> ownerSelf per the resolver); the real per-realm boundary
	// is the handler's own owner-filter (packsHandler.ownsPack's p.OwnerID ==
	// userID check, solder_manage.go's solderCaller/pack.OwnerID checks), which
	// is untouched. Read-vs-write-vs-delete representative per shared template,
	// same convention as every other batch; the fine per-method cap lives at
	// each RequireCap call. GET /api/modrinth/* (the external proxy) and GET
	// /api/library + GET /api/library/download stay authed-exempt and are
	// deliberately NOT listed here (see the route registration comments).
	"/api/me/modrinth-pat":                                                                          "modpack.read",
	"/api/me/packs":                                                                                 "modpack.read",
	"/api/me/packs/import-solder/preview":                                                           "modpack.write",
	"/api/me/packs/import-solder":                                                                   "modpack.write",
	"/api/packs/{id:[0-9]+}":                                                                        "modpack.read",
	"/api/packs/{id:[0-9]+}/builds":                                                                 "modpack.read",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}":                                                "modpack.write",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content":                                        "modpack.read",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/modrinth":                               "modpack.write",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/upload":                                 "modpack.write",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/{modversionId:[0-9]+}":                  "modpack.delete",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/{modversionId:[0-9]+}/side":             "modpack.write",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/{modversionId:[0-9]+}/text":             "modpack.read",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/publish":                                        "modpack.write",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/export":                                         "modpack.read",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/loader":                                         "modpack.read",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/{modversionId:[0-9]+}/replace-modrinth": "modpack.write",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/compat":                                         "modpack.read",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/migrate":                                        "modpack.write",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/update-mods":                                    "modpack.write",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/publish-solder":                                 "modpack.write",
	"/api/packs/{id:[0-9]+}/solder-config":                                                          "modpack.write",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/share-link":                                     "modpack.write",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/share-links":                                    "modpack.read",
	"/api/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/share-links/{linkId:[0-9]+}":                    "modpack.delete",
	"/api/solder/clients":                                                                           "modpack.read",
	"/api/solder/clients/{id:[0-9]+}":                                                               "modpack.delete",
	"/api/packs/{id:[0-9]+}/clients":                                                                "modpack.read",
	"/api/packs/{id:[0-9]+}/clients/{clientId:[0-9]+}":                                              "modpack.write",
	"/api/solder/handle":                                                                            "modpack.read",
	"/api/solder/keys":                                                                              "modpack.read",
	"/api/solder/keys/{id:[0-9]+}":                                                                  "modpack.delete",

	// Phase 4 Task 20: /library/* CONTENT. INSPECTED and found to be a single
	// platform-shared file catalog (buildProvider has no per-owner scoping;
	// LibraryView is reachable by any authenticated user via the navbar, used
	// during server setup to pick pre-uploaded files - see LibraryPicker.tsx),
	// NOT a per-user OWNER realm - so the library.read/write/delete catalog
	// caps (ScopeOwner) do not fit and are deliberately NOT used here. Read
	// (GET /api/library, GET /api/library/download) has no gate beyond auth in
	// the handler (any authed user may browse/download; admin-disabled paths
	// are redacted in-handler, not chokepoint-enforced) and stays authed-exempt,
	// same call as Task 19's GET /api/modules. The four mutations
	// (mkdir/upload/delete/toggle) were hard `if !isAdmin` gated in-handler with
	// no dedicated library-admin cap in the catalog, so they RequireCap
	// ("settings.write") - the same faithful mapping used for /api/modules
	// mutations in Task 19.
	"/api/library/delete": "settings.write",
	"/api/library/mkdir":  "settings.write",
	"/api/library/upload": "settings.write",
	"/api/library/toggle": "settings.write",

	// Phase 4 Task 21: /me/api-keys OWNER apikeys.read/write/delete, the last
	// annotation batch. GET+POST share the read cap as the representative
	// (the finer write/delete cap lives at the per-method RequireCap call
	// above); DELETE gets its own delete-cap entry.
	"/api/me/api-keys":             "apikeys.read",
	"/api/me/api-keys/{id:[0-9]+}": "apikeys.delete",

	// Phase 4 Task 22: split-template entries. Each of these templates is
	// shared by an authed-exempt GET (in ExemptRoutes, see coverage.go) and a
	// RequireCap-gated write method; the write cap is the representative here
	// so the coverage presence-check passes without misrepresenting the GET.
	"/api/nodes":                 "nodes.write",    // GET list exempt (owner filter), POST create nodes.write
	"/api/settings/features":     "settings.write", // GET exempt (feature state), POST settings.write
	"/api/settings/beam":         "settings.write", // GET exempt, POST settings.write
	"/api/settings/routing-mode": "settings.write", // GET exempt, POST settings.write
	"/api/modules":               "settings.write", // GET exempt (navbar), POST settings.write
}

// buildAPIRouter constructs every request handler + the warp service and
// registers every route exactly as main() used to inline them, so a test can
// walk the REAL router. Pure extraction: no route, method, or middleware
// changed relative to the prior inline registration in main().
func buildAPIRouter(appState *handlers.AppState, authHandler *handlers.AuthHandler, cfg routeCfg) (*mux.Router, *routeExtras) {
	// Initialize handlers
	serverHandler := handlers.NewServerHandler(appState)
	nodeHandler := handlers.NewNodeHandler(appState)
	userHandler := handlers.NewUserHandler(appState)
	panelRolesHandler := handlers.NewPanelRolesHandler(appState)
	serverRolesHandler := handlers.NewServerRolesHandler(appState)
	moduleHandler := handlers.NewModuleHandler(appState)
	systemHandler := handlers.NewSystemHandler(cfg.Region, cfg.CoreID, cfg.TabProxyHostSuffix)
	fileHandler := handlers.NewFileHandler(appState)
	libraryHandler := handlers.NewLibraryHandler(appState)
	settingsHandler := handlers.NewSettingsHandler(appState)
	authzHandler := handlers.NewAuthzHandler()
	permissionsModeHandler := handlers.NewPermissionsModeHandler(appState)
	coreStorageHandler := handlers.NewCoreStorageHandler(appState)
	storageConnectionHandler := handlers.NewStorageConnectionHandler(appState)

	// Warp: external/home node WireGuard bridge (multi-hub registry).
	// NewWarpService needs EnrollPeerTx, which the store.Store interface
	// (appState.Store's static type) does not declare - only the concrete
	// Postgres store does. Assert to it: production always sets appState.Store
	// to exactly this concrete type, so this recovers the identical object the
	// old inline main() code passed; nil-safe fallback for test fakes that
	// never exercise a warp route.
	pgStore, _ := appState.Store.(*store.PostgresStore)
	warpService := services.NewWarpService(pgStore, appState.Redis, cfg.ClusterSecret)
	warpHandler := handlers.NewWarpHandler(appState, warpService)

	placementHandler := handlers.NewPlacementHandler(appState)
	consoleHandler := handlers.NewConsoleHandler(appState)
	statsHandler := handlers.NewStatsHandler(appState)
	memberHandler := handlers.NewMemberHandler(appState)
	versionHandler := handlers.NewVersionHandler(appState)
	beamHandler := handlers.NewBeamHandler(appState, cfg.JWTSecret, cfg.ClusterSecret)
	updatesHandler := handlers.NewUpdatesHandler(appState, appState.UpdatesURLPlatform, appState.UpdatesURLHosted)
	backupHandler := handlers.NewBackupHandler(appState)
	platformBackupHandler := handlers.NewPlatformBackupHandler(appState)
	storageConnectionsHandler := handlers.NewStorageConnectionsHandler(appState)
	regionsHandler := handlers.NewRegionsHandler(appState)
	userRegionsHandler := handlers.NewUserRegionsHandler(appState)
	authSettingsHandler := handlers.NewAuthSettingsHandler(appState)
	registrationHandler := handlers.NewRegistrationHandler(appState)
	passwordResetHandler := handlers.NewPasswordResetHandler(appState)
	securityQuestionsHandler := handlers.NewSecurityQuestionsHandler(appState)
	maintenanceHandler := handlers.NewMaintenanceHandler(appState)
	ticketCategoriesHandler := handlers.NewTicketCategoriesHandler(appState)
	ticketsHandler := handlers.NewTicketsHandler(appState)
	ticketSettingsHandler := handlers.NewTicketSettingsHandler(appState)
	ticketAttachmentsHandler := handlers.NewTicketAttachmentsHandler(appState)
	cannedResponsesHandler := handlers.NewCannedResponsesHandler(appState)
	notificationsHandler := handlers.NewNotificationsHandler(appState)
	serverAuditHandler := handlers.NewServerAuditHandler(appState)
	auditSettingsHandler := handlers.NewAuditSettingsHandler(appState)
	ticketMigrationHandler := handlers.NewTicketMigrationHandler(appState)
	systemEventsHandler := handlers.NewSystemEventsHandler(appState)
	scheduledTasksHandler := handlers.NewScheduledTasksHandler(appState)
	rconHandler := handlers.NewRconHandler(appState)
	apiKeysHandler := handlers.NewAPIKeysHandler(appState)
	modrinthHandler := handlers.NewModrinthHandler(appState, cfg.ModrinthUA)
	serverModsHandler := handlers.NewServerModsHandler(appState)
	sparkHandler := handlers.NewSparkHandler(appState)
	serverTabsHandler := handlers.NewServerTabsHandler(appState)
	proxyHandler := handlers.NewProxyHandler(appState, authHandler)
	modrinthPATHandler := handlers.NewModrinthPATHandler(appState, cfg.ClusterSecret)
	packsHandler := handlers.NewPacksHandler(appState)
	packsHandler.SetPATLoader(modrinthPATHandler)
	solderHandler := handlers.NewSolderHandler(appState)
	usernameHistoryHandler := handlers.NewUsernameHistoryHandler(appState)
	accountPolicyHandler := handlers.NewAccountPolicyHandler(appState)
	modpackSettingsHandler := handlers.NewModpackSettingsHandler(appState)
	modCacheSettingsHandler := handlers.NewModCacheSettingsHandler(appState)
	systemFeaturesHandler := handlers.NewSystemFeaturesHandler(appState)
	featureSettingsHandler := handlers.NewFeatureSettingsHandler(appState)
	tabProxySettingsHandler := handlers.NewTabProxySettingsHandler(appState)
	usageHandler := handlers.NewUsageHandler(appState)
	billingHandler := handlers.NewBillingHandler(appState)
	entitlementHandler := handlers.NewEntitlementHandler(appState)
	plansHandler := handlers.NewPlansHandler(appState)
	healthHandler := handlers.NewHealthHandler(appState)
	dbMigrationHandler := handlers.NewDBMigrationHandler(appState)
	storageMigrationHandler := handlers.NewStorageMigrationHandler(appState)
	cpuPinningHandler := handlers.NewCPUPinningHandler(appState)
	nodeEnrollHandler := handlers.NewNodeEnrollHandler(appState)
	nodeAdmissionHandler := handlers.NewNodeAdmissionHandler(appState)
	gatewayDNSHandler := handlers.NewGatewayDNSHandler(appState)
	gatewayBandwidthHandler := handlers.NewGatewayBandwidthHandler(appState)
	ticketDeletionsHandler := handlers.NewTicketDeletionsHandler(appState)
	setupHandler := handlers.NewSetupHandler(appState, authHandler)

	// Set up router and API endpoints
	r := mux.NewRouter()

	r.NotFoundHandler = notFoundHandler(cfg.Panel)

	r.MethodNotAllowedHandler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "Core Error: Method " + req.Method + " not allowed for " + req.URL.Path,
		})
	})

	// Unauthenticated infra readiness probe (Docker/Swarm HEALTHCHECK, load
	// balancers). Registered on the root router, so it bypasses AuthMiddleware
	// AND the /api subrouter's setup-lock + maintenance middleware below -
	// infra healthchecks must keep answering through Fresh-Install/Lost-Admin
	// setup states and maintenance windows.
	r.HandleFunc("/healthz", healthHandler.Healthz).Methods("GET")

	api := r.PathPrefix("/api").Subrouter().StrictSlash(true)

	// Setup-lock middleware wraps every /api/* route. /api/setup/*
	// is short-circuited by the middleware itself so the wizard endpoints stay
	// reachable in Fresh-Install mode (when every other API route returns 503
	// setup_required). In Lost-Admin + Complete modes this is a tiny COUNT()
	// per request that we deliberately don't cache so a wizard completion on
	// one Core unlocks others on their very next request.
	api.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(appState.RequireSetupComplete(next.ServeHTTP))
	})

	// Maintenance gate: blocks writes / all traffic per block_level while
	// maintenance is active. Runs before per-route AuthMiddleware, so it
	// resolves admin status from the token itself - admins always pass and
	// can toggle maintenance back off.
	api.Use(handlers.MaintenanceMuxMiddleware(appState, authHandler.IsAdminToken))

	// --- PUBLIC SOLDER API (Technic Launcher) ---
	// Registered on the ROOT router with NO .Use(...) - it deliberately bypasses
	// the setup-lock, maintenance, and auth middleware so the launcher can reach
	// published packs at all times. The modpacks feature is gated IN-HANDLER
	// (Solder-shaped {"error":...} JSON), not by the 503 feature middleware.
	solder := r.PathPrefix("/solder").Subrouter()
	// Addressed per ACCOUNT. One shared /solder/api served every tenant, which
	// forced pack slugs to be unique across customers and listed everyone's
	// public packs together; see database.applySolderTenancySchema.
	//
	// Both spellings of the API root, answering identically rather than
	// redirecting. This is the URL an operator types into the Technic Platform,
	// and mux matched only the trailing-slash form once - so it fell through to
	// the panel's HTML catch-all and Technic reported an invalid Solder URL for
	// a Solder that was working. Upstream TechnicSolder answers both. A 301
	// would also work for a client that follows redirects, which is not
	// something to assume about somebody else's HTTP client.
	solder.HandleFunc("/u/{handle}/api", solderHandler.Info).Methods("GET")
	solder.HandleFunc("/u/{handle}/api/", solderHandler.Info).Methods("GET")
	solder.HandleFunc("/u/{handle}/api/modpack", solderHandler.ListModpacks).Methods("GET")
	solder.HandleFunc("/u/{handle}/api/modpack/{slug}", solderHandler.GetModpack).Methods("GET")
	solder.HandleFunc("/u/{handle}/api/modpack/{slug}/{build}", solderHandler.GetBuild).Methods("GET")
	solder.HandleFunc("/u/{handle}/api/verify/{key}", solderHandler.VerifyKey).Methods("GET")
	// The retired shared root, answering in the Solder shape so the operator who
	// typed the old URL reads a sentence instead of a web page. Only the ROOT:
	// that is the URL a person enters into the Technic Platform and the one it
	// probes, and a launcher holding a deeper old path cannot exist - the shared
	// URL never served a published pack.
	solder.HandleFunc("/api", solderHandler.LegacyAPI).Methods("GET")
	solder.HandleFunc("/api/", solderHandler.LegacyAPI).Methods("GET")
	// The mirror is the one route here that serves BYTES rather than JSON and
	// it is unauthenticated, so it gets the same kind of per-IP limiter its
	// sibling /api/share already had. Its own instance, not a shared one: a
	// client exhausting one budget must not lock the other.
	//
	// The ceiling is deliberately high, and it is the one number here that is a
	// judgement call rather than a measurement. This budget counts REQUESTS,
	// while a launcher installing a pack fetches one zip PER MOD - several
	// hundred for a large pack, and quickly on a fast link - so a tight limit
	// would return 429 in the middle of a legitimate install. Whether the
	// launcher retries a 429 is not something this codebase can verify, so the
	// number is set where no plausible install reaches it and only sustained
	// automated pulling does. Raise it if a large pack ever trips it.
	//
	// It is not what stops the resource abuse that mattered, either. The
	// handler used to hold each requested object in memory in full; it streams
	// now, so concurrency costs a copy buffer rather than a whole pack. This
	// limiter only bounds egress.
	//
	// (This comment used to end by saying the budget was forgeable, because
	// clientIP took the leftmost X-Forwarded-For value with no trusted-proxy
	// check. That is no longer true and has not been for some time: clientIP
	// anchors on RemoteAddr, ignores XFF entirely unless the peer is a
	// configured trusted proxy, and then walks the header from the RIGHT. Left
	// standing, the note told a reader that this limiter and the login and
	// share-link ones were decorative, which is a good way to get one deleted.)
	mirrorLimiter := handlers.NewIPRateLimiter()
	solder.HandleFunc("/mirror/{rest:.*}", mirrorLimiter.Limit(handlers.SolderMirrorRequestsPerMinute, solderHandler.SolderMirror)).Methods("GET")

	// --- PUBLIC SHARE-LINK DOWNLOAD ---
	// Sibling of the /solder block on the ROOT router: bypasses the /api
	// subrouter's setup-lock + maintenance middleware AND auth, because the
	// token is the credential (like /solder/api/verify/{key}). Per-IP rate
	// limited; the modpacks feature is gated in-handler with a uniform 404.
	shareLimiter := handlers.NewIPRateLimiter()
	r.HandleFunc("/api/share/{token}", shareLimiter.Limit(30, packsHandler.ServeShare)).Methods("GET")

	// Custom-tab reverse proxy: the CONTENT never appears on this router.
	//
	// A proxied tab is served at the root of its own host ("<label>.<suffix>"),
	// which handlers.TabProxyHostMux takes off the wire before this router or
	// CORS ever sees it. That is the point - a tenant's container must not share
	// an origin with the panel or the API. What stays here is the one lookup the
	// share-wrapper page needs: token to content host. Anonymous, because the
	// token is the credential for a public link and this answers nothing else,
	// and rate limited because the endpoint is anonymous.
	r.HandleFunc("/api/tabproxy/{token}/resolve", shareLimiter.Limit(30, proxyHandler.ResolveShare)).Methods("GET")

	// Per-IP rate limiter for public auth endpoints - blunts brute-force and
	// credential-stuffing on login/register/reset/setup.
	//
	// ONE instance, shared by every route below that passes it, and that is
	// load-bearing: IPRateLimiter buckets by client IP alone, NOT by route. So
	// all of them draw on a single per-IP counter and each per-route number is a
	// ceiling on that shared count, not an independent budget. An attacker
	// rotating login -> forgot-password -> validate-reset-token gets one budget
	// between them, which is the point; the mirror and share limiters above are
	// separate instances for the opposite reason, so a launcher install cannot
	// eat the auth budget.
	//
	// The consequence to keep in mind before changing a number: raising one
	// route's limit raises the shared count every other route here measures
	// against. Splitting them per route (keying the bucket by route+IP) would
	// make each number mean what it looks like, and would also hand an anonymous
	// caller the SUM of them, so it is deliberately not done.
	authLimiter := handlers.NewIPRateLimiter()

	// --- PUBLIC ENDPOINTS ---
	api.HandleFunc("/auth/login", authLimiter.Limit(10, handlers.LimitBody(handlers.CredentialBodyLimit, authHandler.LoginHandler))).Methods("POST")
	// Unauthenticated: it only clears the session cookie, and the state somebody
	// most needs to clear is an expired session they can no longer authenticate
	// with. Rate-limited anyway, so it cannot be used as free traffic.
	api.HandleFunc("/auth/logout", authLimiter.Limit(30, authHandler.Logout)).Methods("POST")
	// A bearer copy of the caller's own session, for the Beam desktop client's
	// native side. Authenticated like any other route AND refused with 404
	// anywhere but the Wails webview origin - see SessionToken for why the
	// second condition is the important one.
	api.HandleFunc("/auth/session-token", authLimiter.Limit(20, authHandler.AuthMiddleware(authHandler.SessionToken))).Methods("POST")
	api.HandleFunc("/status", authHandler.StatusHandler).Methods("GET")
	api.HandleFunc("/system/capabilities", systemHandler.GetCapabilities).Methods("GET")
	// Public - used by the topbar to display "Connected to <region> Core".
	api.HandleFunc("/system/core-info", systemHandler.GetCoreInfo).Methods("GET")
	// SSE stream of platform-wide config-change events. Panel
	// subscribes once on boot and refreshes its caches reactively. Auth via
	// ?token= query param since EventSource can't set Authorization headers.
	api.HandleFunc("/system/events", authHandler.AuthMiddleware(systemEventsHandler.StreamEvents)).Methods("GET")
	// Mint a short-lived SSE auth ticket so EventSource streams carry a
	// disposable ?ticket= in the URL instead of the long-lived session JWT.
	api.HandleFunc("/sse-ticket", authHandler.AuthMiddleware(authHandler.MintSSETicket)).Methods("POST")

	// Setup wizard. Open routes; /api/setup/* is also exempt from
	// the setup-lock middleware so they remain reachable in Fresh-Install
	// mode. Atomic CTE in Store.CreateFirstAdmin prevents racing inserts
	// across N Cores.
	api.HandleFunc("/setup/status", setupHandler.Status).Methods("GET")
	api.HandleFunc("/setup/admin", authLimiter.Limit(10, handlers.LimitBody(handlers.CredentialBodyLimit, setupHandler.CreateAdmin))).Methods("POST")

	// --- Scheduled Tasks ---
	// Cron preview - pure transform, available to anyone authed.
	api.HandleFunc("/scheduled-tasks/validate", authHandler.AuthMiddleware(scheduledTasksHandler.ValidateCron)).Methods("POST")
	// Per-server CRUD, gated at the chokepoint by the finest matching cap.
	api.HandleFunc("/servers/{id:[0-9]+}/scheduled-tasks", authHandler.AuthMiddleware(appState.Authz.RequireCap("schedule.read")(scheduledTasksHandler.List))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/scheduled-tasks", authHandler.AuthMiddleware(appState.Authz.RequireCap("schedule.write")(scheduledTasksHandler.Create))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/scheduled-tasks/{taskId:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("schedule.write")(scheduledTasksHandler.Update))).Methods("PATCH")
	api.HandleFunc("/servers/{id:[0-9]+}/scheduled-tasks/{taskId:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("schedule.delete")(scheduledTasksHandler.Delete))).Methods("DELETE")

	// --- RCON + API keys ---
	// Panel-internal RCON. Power-class permission enforced inside the handler.
	api.HandleFunc("/servers/{id:[0-9]+}/rcon", authHandler.AuthMiddleware(appState.Authz.RequireCap("rcon.exec")(rconHandler.ExecForUser))).Methods("POST")

	// Player management: the roster and the three list files behind players.read,
	// a fixed set of player commands behind players.manage. The point is that a
	// moderator needs NEITHER rcon.exec (every command there is, including stop
	// and save-off) NOR files.read (the whole server filesystem) to do the job.
	playersHandler := handlers.NewPlayersHandler(appState)
	api.HandleFunc("/servers/{id:[0-9]+}/players/lists", authHandler.AuthMiddleware(appState.Authz.RequireCap("players.read")(playersHandler.GetLists))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/players/online", authHandler.AuthMiddleware(appState.Authz.RequireCap("players.read")(playersHandler.GetOnline))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/players/action", authHandler.AuthMiddleware(appState.Authz.RequireCap("players.manage")(playersHandler.Action))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/rcon/config", authHandler.AuthMiddleware(appState.Authz.RequireCap("config.read")(rconHandler.GetConfig))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/rcon/config", authHandler.AuthMiddleware(appState.Authz.RequireCap("config.write")(rconHandler.SetConfig))).Methods("PUT")
	// Per-user API key CRUD (panel surface). Phase 4 Task 21: OWNER
	// apikeys.read/write/delete, chokepoint-open for any authed user (no
	// server {id} in the path). The handler's own per-user scoping (keys are
	// listed/created/revoked for the acting userID only) is the real boundary,
	// same as Task 20's modpack.*/library.* OWNER routes.
	api.HandleFunc("/me/api-keys", authHandler.AuthMiddleware(appState.Authz.RequireCap("apikeys.read")(apiKeysHandler.List))).Methods("GET")
	// Rate limited like the auth routes, and body-capped, because Create now
	// checks a PASSWORD (see requireReauth). A password check on an unthrottled
	// route is a guessing oracle, and this one sits behind a session the guesser
	// already holds - which is exactly the situation the check exists for.
	api.HandleFunc("/me/api-keys", authLimiter.Limit(10, handlers.LimitBody(handlers.CredentialBodyLimit, authHandler.AuthMiddleware(appState.Authz.RequireCap("apikeys.write")(apiKeysHandler.Create))))).Methods("POST")
	api.HandleFunc("/me/api-keys/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("apikeys.delete")(apiKeysHandler.Revoke))).Methods("DELETE")
	// --- External API surface: Authorization: Bearer dyl_<key> ---
	//
	// A separate surface from the panel routes above, addressing servers by
	// UUID rather than by the sequential id (see handlers/external_api.go).
	// APIKeyServerRoute / APIKeyOwnerRoute declare the shape: which one is used
	// decides whether the SERVER capabilities of the key count at all, and it
	// is never inferred from the path.
	//
	// The ExternalServerRoute wrapper resolves {uuid} to the {id} + identity
	// the panel handler behind it expects, so these are adapters, not copies -
	// the suspension, quota and audit guards inside those handlers keep
	// applying to key traffic.
	api.HandleFunc("/external/servers", apiKeysHandler.APIKeyOwnerRoute("")(apiKeysHandler.ListExternalServers)).Methods("GET")
	api.HandleFunc("/external/usage", apiKeysHandler.APIKeyOwnerRoute("usage.read")(apiKeysHandler.ExternalOwnerRoute(usageHandler.GetMyUsage))).Methods("GET")

	api.HandleFunc("/external/servers/{uuid}", apiKeysHandler.APIKeyServerRoute("overview.read")(apiKeysHandler.GetExternalServer)).Methods("GET")
	// POWER carries no route capability for the same reason the panel route
	// does not (Rule R5: the action is in the body). APIKeyPowerGate resolves
	// power.<action> against the KEY; ServerPowerHandler then resolves it again
	// against the OWNER, which is the check that keeps a revoked member's key
	// from working.
	api.HandleFunc("/external/servers/{uuid}/power", apiKeysHandler.APIKeyServerRoute("")(apiKeysHandler.APIKeyPowerGate(apiKeysHandler.ExternalServerRoute(serverHandler.ServerPowerHandler)))).Methods("POST")
	api.HandleFunc("/external/servers/{uuid}/console/history", apiKeysHandler.APIKeyServerRoute("console.read")(apiKeysHandler.ExternalServerRoute(consoleHandler.GetHistory))).Methods("GET")
	api.HandleFunc("/external/servers/{uuid}/console/command", apiKeysHandler.APIKeyServerRoute("console.send")(apiKeysHandler.ExternalServerRoute(consoleHandler.SendCommand))).Methods("POST")
	// Stats are the recorded history, not a live sample: the node only produces
	// live stats while a watcher key is held (StreamStats sets it), so a
	// one-shot live read would answer whatever the last SSE viewer left behind.
	api.HandleFunc("/external/servers/{uuid}/stats/history", apiKeysHandler.APIKeyServerRoute("stats.read")(apiKeysHandler.ExternalServerRoute(statsHandler.GetHistory))).Methods("GET")
	api.HandleFunc("/external/servers/{uuid}/backup-jobs", apiKeysHandler.APIKeyServerRoute("backups.read")(apiKeysHandler.ExternalServerRoute(backupHandler.ListJobs))).Methods("GET")
	api.HandleFunc("/external/servers/{uuid}/backup-jobs/{jobId:[0-9]+}/trigger", apiKeysHandler.APIKeyServerRoute("backups.create")(apiKeysHandler.ExternalJobInServer(apiKeysHandler.ExternalServerRoute(backupHandler.TriggerJob)))).Methods("POST")
	// External RCON: the original key route. Scope check on the path-uuid
	// happens in the middleware itself.
	api.HandleFunc("/external/rcon/{uuid}/exec", apiKeysHandler.APIKeyServerRoute("rcon.exec")(rconHandler.ExecExternal)).Methods("POST")

	// --- Modrinth proxy + per-server mod install ---
	// Browse + project metadata are cached in Redis (5 min / 1 h). All authed.
	// Per-IP rate limit on the Modrinth proxy: each distinct query is a cache
	// miss that both hits Modrinth and grows the Redis cache, so cap volume.
	modrinthLimiter := handlers.NewIPRateLimiter()
	api.HandleFunc("/modrinth/search", modrinthLimiter.Limit(120, authHandler.AuthMiddleware(modrinthHandler.Search))).Methods("GET")
	api.HandleFunc("/modrinth/project/{slug}", modrinthLimiter.Limit(120, authHandler.AuthMiddleware(modrinthHandler.Project))).Methods("GET")
	api.HandleFunc("/modrinth/project/{slug}/versions", modrinthLimiter.Limit(120, authHandler.AuthMiddleware(modrinthHandler.ProjectVersions))).Methods("GET")
	api.HandleFunc("/modrinth/version/{id}", modrinthLimiter.Limit(120, authHandler.AuthMiddleware(modrinthHandler.Version))).Methods("GET")
	api.HandleFunc("/modrinth/categories", modrinthLimiter.Limit(120, authHandler.AuthMiddleware(modrinthHandler.Categories))).Methods("GET")
	api.HandleFunc("/modrinth/game-versions", modrinthLimiter.Limit(120, authHandler.AuthMiddleware(modrinthHandler.GameVersions))).Methods("GET")
	// Per-server installed mods + install/uninstall dispatch.
	api.HandleFunc("/servers/{id:[0-9]+}/mods", authHandler.AuthMiddleware(appState.Authz.RequireCap("mods.read")(serverModsHandler.List))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/mods", authHandler.AuthMiddleware(appState.Authz.RequireCap("mods.write")(serverModsHandler.Install))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/mods/{modId:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("mods.delete")(serverModsHandler.Uninstall))).Methods("DELETE")
	// Cross-Minecraft-version availability + the move itself. These literals
	// cannot be swallowed by the /mods/{modId} pattern above: that one is
	// numeric-constrained.
	api.HandleFunc("/servers/{id:[0-9]+}/mods/compat", authHandler.AuthMiddleware(appState.Authz.RequireCap("mods.read")(serverModsHandler.ServerCompat))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/mods/unmanaged", authHandler.AuthMiddleware(appState.Authz.RequireCap("mods.read")(serverModsHandler.UnmanagedMods))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/mods/history", authHandler.AuthMiddleware(appState.Authz.RequireCap("mods.read")(serverModsHandler.ModHistory))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/mods/identify", authHandler.AuthMiddleware(appState.Authz.RequireCap("mods.write")(serverModsHandler.IdentifyMods))).Methods("POST")
	// A version move and a sub-server copy both rewrite what the server IS, so
	// they take the same capability as reinstall rather than a mods one.
	api.HandleFunc("/servers/{id:[0-9]+}/version-update", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverModsHandler.VersionUpdate))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/copy-sub-server", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverModsHandler.CopySubServer))).Methods("POST")
	// Read-only modpack snapshot backing the Content-tab cross-check.
	api.HandleFunc("/servers/{id:[0-9]+}/modpack-contents", authHandler.AuthMiddleware(appState.Authz.RequireCap("mods.read")(serverModsHandler.ModpackContents))).Methods("GET")
	// How each sub-server was installed. Gated with the SETUP capability rather
	// than a read one: this exists to prefill the setup form, and there is no
	// server.settings.read - the whole tab is a write surface, so anyone who may
	// not change the setup has no screen to show this on.
	api.HandleFunc("/servers/{id:[0-9]+}/installs", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverHandler.SubServerInstalls))).Methods("GET")

	// --- Spark profiles ---
	api.HandleFunc("/servers/{id:[0-9]+}/spark/profiles", authHandler.AuthMiddleware(appState.Authz.RequireCap("spark.use")(sparkHandler.List))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/spark/profiles", authHandler.AuthMiddleware(appState.Authz.RequireCap("spark.use")(sparkHandler.Record))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/spark/profiles/{profileId:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("spark.use")(sparkHandler.Delete))).Methods("DELETE")

	// --- Custom Tabs ---
	api.HandleFunc("/servers/{id:[0-9]+}/tabs", authHandler.AuthMiddleware(appState.Authz.RequireCap("tabs.read")(serverTabsHandler.List))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/tabs", authHandler.AuthMiddleware(appState.Authz.RequireCap("tabs.write")(serverTabsHandler.Create))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/tabs/{tabId:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("tabs.write")(serverTabsHandler.Update))).Methods("PATCH")
	api.HandleFunc("/servers/{id:[0-9]+}/tabs/{tabId:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("tabs.write")(serverTabsHandler.Delete))).Methods("DELETE")
	api.HandleFunc("/servers/{id:[0-9]+}/tabs/{tabId:[0-9]+}/share-link", authHandler.AuthMiddleware(appState.Authz.RequireCap("tabs.write")(serverTabsHandler.RotateShareLink))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/tabs/{tabId:[0-9]+}/share-link", authHandler.AuthMiddleware(appState.Authz.RequireCap("tabs.write")(serverTabsHandler.RevokeShareLink))).Methods("DELETE")
	// The ticket a proxied tab's iframe needs, minted HERE - on the panel's own
	// origin, the only place the session cookie is sent. The panel then presents
	// it to the tab's content host, which is the only place it can be STORED as a
	// cookie. See ProxyHandler.MintTicket.
	api.HandleFunc("/servers/{id:[0-9]+}/tabs/{tabId:[0-9]+}/proxy-ticket", authHandler.AuthMiddleware(appState.Authz.RequireCap("tabs.read")(proxyHandler.MintTicket))).Methods("POST")
	// The ticket cookie is NOT minted here.
	//
	// It is minted on the tab's own content host (handlers.HostMint, behind the
	// same AuthMiddleware), because a cookie can only be set for the host that
	// answers the request and the content host is not this one. The panel
	// reaches it cross-origin with its session as a Bearer HEADER, which works
	// precisely because the panel token lives in localStorage and not in a
	// cookie: the other origin cannot read it, but the panel can send it.

	// --- Modrinth PAT ---
	// Phase 4 Task 20: OWNER modpack.* - chokepoint-open by design (serverID==0
	// -> ownerSelf for any authed user); RequireCap sits inside AuthMiddleware
	// and outside the existing feature gates (Rule R1). The caller's own PAT
	// row is the realm (userID from context, no path {id}), so there is no
	// separate owner-filter to keep or remove here.
	api.HandleFunc("/me/modrinth-pat", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.AllowReadOnlyWhenDisabled(modrinthPATHandler.Status)))).Methods("GET")
	api.HandleFunc("/me/modrinth-pat", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(modrinthPATHandler.Set))))).Methods("PUT")
	api.HandleFunc("/me/modrinth-pat", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(modrinthPATHandler.Clear))))).Methods("DELETE")
	// --- Unified pack builder (Solder + Modrinth). Reuses the modpacks feature gates. ---
	// Phase 4 Task 20: OWNER modpack.read/write/delete, chokepoint-open by
	// design. The real per-realm boundary is packsHandler.ownsPack's
	// p.OwnerID == userID check (List/Update/Delete/CreateBuild/... inline the
	// same check) - untouched, it is the F2 boundary this cap layer sits in
	// front of.
	api.HandleFunc("/me/packs", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.AllowReadOnlyWhenDisabled(packsHandler.List)))).Methods("GET")
	api.HandleFunc("/me/packs", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.Create))))).Methods("POST")
	api.HandleFunc("/me/packs/import-solder/preview", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.ImportSolderPreview))))).Methods("POST")
	api.HandleFunc("/me/packs/import-solder", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.ImportSolder))))).Methods("POST")
	api.HandleFunc("/packs/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.AllowReadOnlyWhenDisabled(packsHandler.Get)))).Methods("GET")
	api.HandleFunc("/packs/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.Update))))).Methods("PATCH")
	api.HandleFunc("/packs/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.delete")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.Delete))))).Methods("DELETE")
	api.HandleFunc("/packs/{id:[0-9]+}/builds", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.AllowReadOnlyWhenDisabled(packsHandler.ListBuilds)))).Methods("GET")
	api.HandleFunc("/packs/{id:[0-9]+}/builds", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.CreateBuild))))).Methods("POST")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.UpdateBuild))))).Methods("PATCH")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.delete")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.DeleteBuild))))).Methods("DELETE")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.AllowReadOnlyWhenDisabled(packsHandler.ListContent)))).Methods("GET")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/modrinth", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.AddModrinth))))).Methods("POST")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/upload", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.UploadContent))))).Methods("POST")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/{modversionId:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.delete")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.RemoveContent))))).Methods("DELETE")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/{modversionId:[0-9]+}/side", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.SetSide))))).Methods("PATCH")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/{modversionId:[0-9]+}/text", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.AllowReadOnlyWhenDisabled(packsHandler.GetContentText)))).Methods("GET")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/{modversionId:[0-9]+}/text", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.SetContentText))))).Methods("PUT")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/publish", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.PublishModrinth))))).Methods("POST")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/export", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.AllowReadOnlyWhenDisabled(packsHandler.ExportMrpack)))).Methods("GET")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/loader", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.AllowReadOnlyWhenDisabled(packsHandler.GetBuildLoader)))).Methods("GET")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/content/{modversionId:[0-9]+}/replace-modrinth", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.ReplaceWithModrinth))))).Methods("POST")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/update-mods", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.UpdateMods))))).Methods("POST")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/compat", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.AllowReadOnlyWhenDisabled(packsHandler.BuildCompat)))).Methods("GET")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/migrate", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.MigrateBuild))))).Methods("POST")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/publish-solder", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.PublishSolder))))).Methods("POST")
	api.HandleFunc("/packs/{id:[0-9]+}/solder-config", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.SetSolderConfig))))).Methods("PATCH")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/share-link", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireShareLinksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.CreateShareLink)))))).Methods("POST")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/share-links", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.AllowReadOnlyWhenDisabled(packsHandler.ListShareLinks)))).Methods("GET")
	api.HandleFunc("/packs/{id:[0-9]+}/builds/{buildId:[0-9]+}/share-links/{linkId:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.delete")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(packsHandler.RevokeShareLink))))).Methods("DELETE")

	// --- Solder client/key management (authed) ---
	// Phase 4 Task 20 INSPECT result: these ARE session-authed (authHandler.
	// AuthMiddleware, userID from the JWT context via solderCaller/ownsPack) -
	// NOT the Technic-launcher API-key protocol (that is the separate,
	// deliberately unauthenticated "/solder/api/*" root-router block above,
	// left untouched). So these get OWNER modpack.* like the pack-builder
	// routes above; solder_manage.go's solderCaller(r)-scoped store calls and
	// ownsPackAndClient's pack.OwnerID/client-owner checks are the kept
	// per-realm boundary.
	api.HandleFunc("/solder/clients", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(solderHandler.ListClients))))).Methods("GET")
	api.HandleFunc("/solder/clients", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(solderHandler.CreateClient))))).Methods("POST")
	api.HandleFunc("/solder/clients/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.delete")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(solderHandler.DeleteClient))))).Methods("DELETE")

	api.HandleFunc("/packs/{id:[0-9]+}/clients", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(solderHandler.ListPackClientsHandler))))).Methods("GET")
	api.HandleFunc("/packs/{id:[0-9]+}/clients/{clientId:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(solderHandler.AddPackClient))))).Methods("POST")
	api.HandleFunc("/packs/{id:[0-9]+}/clients/{clientId:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.delete")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(solderHandler.RemovePackClient))))).Methods("DELETE")

	// The account's own Solder address. Same gates as the keys beside it: the
	// address and the key are the two halves of one setup, and a caller who may
	// not hold a key has nothing to address.
	api.HandleFunc("/solder/handle", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(solderHandler.GetHandle))))).Methods("GET")
	api.HandleFunc("/solder/handle", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(solderHandler.SetHandle))))).Methods("POST")
	api.HandleFunc("/solder/keys", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(solderHandler.ListKeys))))).Methods("GET")
	api.HandleFunc("/solder/keys", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.write")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(solderHandler.CreateKey))))).Methods("POST")
	api.HandleFunc("/solder/keys/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.delete")(appState.RequireModpacksEnabled(appState.RequireUserCanCreateModpacks(solderHandler.DeleteKey))))).Methods("DELETE")

	// --- Username history + account policy ---
	// /me/usage is the caller's OWN metered usage: EXEMPT-authed, no RequireCap
	// (not in requiredCaps). /admin/usage is PANEL plans.read.
	api.HandleFunc("/me/usage", authHandler.AuthMiddleware(usageHandler.GetMyUsage)).Methods("GET")
	api.HandleFunc("/admin/usage", authHandler.AuthMiddleware(appState.Authz.RequireCap("plans.read")(appState.RequireBYONEnabled(usageHandler.GetAllUsage)))).Methods("GET")

	// /me/billing is the caller's OWN lifecycle state: EXEMPT-authed, no RequireCap
	// (not in requiredCaps). The admin billing routes below are PANEL plans.*.
	api.HandleFunc("/me/billing", authHandler.AuthMiddleware(billingHandler.GetMyBilling)).Methods("GET")

	// The caller's OWN machine: EXEMPT-authed, no RequireCap (deliberately NOT in
	// requiredCaps), because no customer holds nodes.delete and the handler here
	// answers only for a node whose owner_id IS the caller. The admin route
	// DELETE /nodes/{id} is untouched and stays capability-gated - opening THAT
	// one to tenants would have let any of them delete any node, since it has no
	// ownership check of its own.
	api.HandleFunc("/me/nodes/{id:[0-9]+}/contents", authHandler.AuthMiddleware(appState.RequireBYONEnabled(nodeHandler.GetMyNodeContents))).Methods("GET")
	api.HandleFunc("/me/nodes/{id:[0-9]+}", authHandler.AuthMiddleware(appState.RequireBYONEnabled(nodeHandler.DeleteMyNode))).Methods("DELETE")
	// Own entitlement: what the caller may use. Deliberately NOT gated on
	// RequireBYONEnabled - with BYON off the tenant UI still asks, and needs the
	// answer "no" rather than a 503 it would have to special-case.
	api.HandleFunc("/me/entitlement", authHandler.AuthMiddleware(entitlementHandler.GetMine)).Methods("GET")
	api.HandleFunc("/admin/settings/billing", authHandler.AuthMiddleware(appState.Authz.RequireCap("plans.read")(appState.RequireBYONEnabled(appState.RequireStoreEnabled(billingHandler.GetBillingSettings))))).Methods("GET")
	api.HandleFunc("/admin/settings/billing", authHandler.AuthMiddleware(appState.Authz.RequireCap("plans.write")(appState.RequireBYONEnabled(appState.RequireStoreEnabled(billingHandler.SetBillingSettings))))).Methods("PUT")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/billing", authHandler.AuthMiddleware(appState.Authz.RequireCap("plans.read")(appState.RequireBYONEnabled(billingHandler.GetUserBilling)))).Methods("GET")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/billing", authHandler.AuthMiddleware(appState.Authz.RequireCap("plans.write")(appState.RequireBYONEnabled(billingHandler.SetBillingStatus)))).Methods("PATCH")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/billing-overrides", authHandler.AuthMiddleware(appState.Authz.RequireCap("plans.write")(appState.RequireBYONEnabled(appState.RequireStoreEnabled(billingHandler.SetBillingOverrides))))).Methods("PATCH")
	// Entitlement = WHAT a tenant may use (BYON / route-only), as opposed to the
	// billing routes above, which are status and HOW MUCH.
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/entitlement", authHandler.AuthMiddleware(appState.Authz.RequireCap("plans.read")(appState.RequireBYONEnabled(entitlementHandler.GetForUser)))).Methods("GET")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/entitlement", authHandler.AuthMiddleware(appState.Authz.RequireCap("plans.write")(appState.RequireBYONEnabled(entitlementHandler.Grant)))).Methods("POST")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/entitlement", authHandler.AuthMiddleware(appState.Authz.RequireCap("plans.write")(appState.RequireBYONEnabled(entitlementHandler.Revoke)))).Methods("DELETE")

	// --- BYON plans + per-user plan/limit overrides (admin, PANEL plans.*) ---
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/limit-overrides", authHandler.AuthMiddleware(appState.Authz.RequireCap("plans.write")(appState.RequireBYONEnabled(plansHandler.SetUserLimitOverrides)))).Methods("PATCH")

	// /me/username-history is the caller's OWN history: EXEMPT-authed, no
	// RequireCap (not in requiredCaps). The admin two below are PANEL users.*.
	api.HandleFunc("/me/username-history", authHandler.AuthMiddleware(usernameHistoryHandler.Me)).Methods("GET")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/username-history", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.read")(usernameHistoryHandler.Admin))).Methods("GET")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/username", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.write")(usernameHistoryHandler.AdminRename))).Methods("PATCH")
	// The address, which nothing could change before - see handlers/user_email.go.
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/email", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.write")(handlers.NewUserEmailHandler(appState).SetEmail))).Methods("PATCH")
	api.HandleFunc("/admin/settings/users", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(accountPolicyHandler.Get))).Methods("GET")
	api.HandleFunc("/admin/settings/users", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(accountPolicyHandler.Set))).Methods("PUT")
	// --- Modpack settings + system feature flags (PANEL settings.*; Phase 4 Task 17) ---
	api.HandleFunc("/admin/settings/modpacks", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(modpackSettingsHandler.Get))).Methods("GET")
	api.HandleFunc("/admin/settings/modpacks", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(modpackSettingsHandler.Set))).Methods("PUT")
	api.HandleFunc("/admin/settings/modpacks/delivery-capabilities", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(modpackSettingsHandler.DeliveryCapabilities))).Methods("GET")
	// Where Modrinth metadata is cached. Optional by design: unset means the
	// Redis Core already has, which is why nothing is gated on it.
	api.HandleFunc("/admin/settings/mod-cache", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(modCacheSettingsHandler.Get))).Methods("GET")
	api.HandleFunc("/admin/settings/mod-cache", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(modCacheSettingsHandler.Set))).Methods("PUT")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/modpack-flag", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(modpackSettingsHandler.SetUserFlag))).Methods("PATCH")
	// DELETE = stop overriding this user, not "revoke": it clears the manual
	// marker so they follow the platform authoring toggle again.
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/modpack-flag", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(modpackSettingsHandler.ClearUserFlagOverride))).Methods("DELETE")
	// GET /system/features stays EXEMPT-authed here (Phase 4 Task 19 territory - not touched by Task 17).
	api.HandleFunc("/system/features", authHandler.AuthMiddleware(systemFeaturesHandler.Get)).Methods("GET")
	// Bundled admin GET/PUT for all platform-wide feature toggles. Replaces
	// the per-feature toggle that used to live inside /admin/settings/modpacks
	// (still works for back-compat; this is the new canonical surface).
	api.HandleFunc("/admin/settings/features", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(featureSettingsHandler.Get))).Methods("GET")
	api.HandleFunc("/admin/settings/features", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(featureSettingsHandler.Set))).Methods("PUT")
	api.HandleFunc("/admin/settings/tab-proxy", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(tabProxySettingsHandler.Get))).Methods("GET")
	api.HandleFunc("/admin/settings/tab-proxy", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(tabProxySettingsHandler.Set))).Methods("PUT")
	api.HandleFunc("/admin/settings/permissions-mode", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(permissionsModeHandler.SetMode))).Methods("PUT")
	// P0b-5 node admission (PANEL nodes.read/write/delete; Phase 4 Task 13).
	api.HandleFunc("/admin/settings/node-admission", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.read")(nodeAdmissionHandler.GetAdmission))).Methods("GET")
	api.HandleFunc("/admin/settings/node-admission", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.write")(nodeAdmissionHandler.SetAdmission))).Methods("PUT")
	api.HandleFunc("/admin/settings/node-admission/cidrs", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.write")(nodeAdmissionHandler.AddCIDR))).Methods("POST")
	api.HandleFunc("/admin/settings/node-admission/cidrs/{id}", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.delete")(nodeAdmissionHandler.DeleteCIDR))).Methods("DELETE")
	// --- Platform status / health (admin Status page; PANEL settings.read) ---
	api.HandleFunc("/admin/health", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(healthHandler.GetStatus))).Methods("GET")

	// In-panel cross-database migration (admin-only, PANEL settings.*). Shared job
	// is pollable by every settings.read holder; the copy runs under maintenance
	// mode on whichever Core started it.
	api.HandleFunc("/admin/db/migration", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(dbMigrationHandler.GetMigration))).Methods("GET")
	api.HandleFunc("/admin/db/migration", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(dbMigrationHandler.StartMigration))).Methods("POST")
	api.HandleFunc("/admin/db/migration/test-connection", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(dbMigrationHandler.TestConnection))).Methods("POST")
	api.HandleFunc("/admin/db/migration/verify", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(dbMigrationHandler.VerifyMigration))).Methods("POST")
	// In-place hypertable upgrade + TimescaleDB recommendation (same database).
	api.HandleFunc("/admin/db/hypertable", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(dbMigrationHandler.HypertableStatus))).Methods("GET")
	api.HandleFunc("/admin/db/hypertable/convert", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(dbMigrationHandler.ConvertHypertable))).Methods("POST")
	// Blob storage migration: inventory, copy, verify, optional source delete.
	// Admin-gated exactly like the DB migration (settings.read / settings.write).
	api.HandleFunc("/admin/storage/overview", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(storageMigrationHandler.Overview))).Methods("GET")
	api.HandleFunc("/admin/storage/migration", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(storageMigrationHandler.GetJob))).Methods("GET")
	api.HandleFunc("/admin/storage/migration", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(storageMigrationHandler.Start))).Methods("POST")
	api.HandleFunc("/admin/storage/migration/cancel", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(storageMigrationHandler.Cancel))).Methods("POST")
	api.HandleFunc("/admin/storage/manifests", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(storageMigrationHandler.ListManifests))).Methods("GET")
	api.HandleFunc("/admin/storage/manifests/{id:[0-9]+}/export", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(storageMigrationHandler.ExportManifest))).Methods("GET")
	api.HandleFunc("/admin/storage/manifests/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(storageMigrationHandler.DeleteManifest))).Methods("DELETE")
	// Warp enrollment (warp API-key auth, NOT user session)
	api.HandleFunc("/warp/enroll", warpHandler.WarpAPIKeyMiddleware(warpHandler.Enroll)).Methods("POST")
	api.HandleFunc("/warp/assignment", warpHandler.WarpAPIKeyMiddleware(warpHandler.Assignment)).Methods("GET")
	// Warp admin registry: regions + leaders (PANEL topology.*; Phase 4 Task 18)
	api.HandleFunc("/warp/regions", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(warpHandler.ListRegions))).Methods("GET")
	api.HandleFunc("/warp/regions", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.write")(warpHandler.UpsertRegion))).Methods("POST")
	api.HandleFunc("/warp/regions/{region}", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.write")(warpHandler.DeleteRegion))).Methods("DELETE")
	api.HandleFunc("/warp/leaders", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.write")(warpHandler.UpsertLeader))).Methods("POST")
	api.HandleFunc("/warp/leaders/{leaderId}", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.write")(warpHandler.DeleteLeader))).Methods("DELETE")
	api.HandleFunc("/admin/warp/keys", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.write")(warpHandler.MintAPIKey))).Methods("POST")
	api.HandleFunc("/admin/warp/keys", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(warpHandler.ListAPIKeys))).Methods("GET")
	api.HandleFunc("/admin/warp/keys/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.write")(warpHandler.RevokeAPIKey))).Methods("DELETE")
	api.HandleFunc("/admin/warp/keys/{id:[0-9]+}/purge", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.write")(warpHandler.DeleteAPIKey))).Methods("DELETE")
	// Route-only link kits (tenant self-service; BYON-gated inside the handler)
	api.HandleFunc("/warp/link-kits", authHandler.AuthMiddleware(warpHandler.ListLinkKits)).Methods("GET")
	api.HandleFunc("/warp/link-kits", authHandler.AuthMiddleware(warpHandler.MintLinkKit)).Methods("POST")
	api.HandleFunc("/warp/link-boot",
		authLimiter.Limit(30, warpHandler.WarpAPIKeyMiddleware(warpHandler.LinkBoot))).Methods("POST")
	api.HandleFunc("/warp/link-kits/{linkID}", authHandler.AuthMiddleware(warpHandler.RevokeLinkKit)).Methods("DELETE")
	api.HandleFunc("/warp/link-kits/{linkID}/roll", authHandler.AuthMiddleware(warpHandler.RollLinkKit)).Methods("POST")
	// BYON node warp keys (tenant self-service; BYON + gateway gated inside the
	// handler, capped on max_nodes). Separate from link kits by the "node-"
	// node_id prefix - see MintNodeWarpKey.
	api.HandleFunc("/warp/node-keys", authHandler.AuthMiddleware(warpHandler.MintNodeWarpKey)).Methods("POST")
	api.HandleFunc("/warp/node-keys", authHandler.AuthMiddleware(warpHandler.ListNodeWarpKeys)).Methods("GET")
	api.HandleFunc("/warp/node-keys/{nodeID}", authHandler.AuthMiddleware(warpHandler.RevokeNodeWarpKey)).Methods("DELETE")
	api.HandleFunc("/warp/node-keys/{nodeID}/roll", authHandler.AuthMiddleware(warpHandler.RollNodeWarpKey)).Methods("POST")
	// Overlay addresses for the deploy snippets. Authed but uncapped for the same
	// reason as the kits above: the tenant minting the key is the one who needs
	// them, and they are RFC1918 addresses that authorize nothing on their own.
	api.HandleFunc("/warp/deploy-config", authHandler.AuthMiddleware(warpHandler.GetDeployConfig)).Methods("GET")

	// GET/POST /node/connect is GONE, along with handlers/node_grpc.go.
	//
	// It was a "Legacy / Status Check" WebSocket left over from before the node
	// moved to Redis + gRPC, and nothing had called it since: the node's own
	// NodeConnect is the gRPC NodeService method, the panel never referenced it,
	// nor did the log-shipper or the gateway.
	//
	// Dead is only half of why it is removed. It was registered with no
	// AuthMiddleware and no limiter, took a bare node token, and on a match wrote
	// nodes.status = online, then offline again when the socket closed. The node
	// token is not a secret in the way that arrangement needed: models.Node
	// serializes it as `json:"token"`, so GET /api/nodes hands it to every caller
	// who can see the node. Its live sibling - the discovery heartbeat - verifies
	// an HMAC over (token, timestamp) with the per-node secret and logs "bad or
	// stale signature" on failure, which is the whole point of the pairing-auth
	// work; this path checked possession of the identifier and nothing else, so a
	// node's online/offline state could be flipped with no session at all.
	//
	// Both allow-lists that carried it (authz.ExemptRoutes and
	// anonymousUnlimitedRoutes) justified it as "the enroll token / node secret is
	// the credential". Neither was ever presented to it.

	// --- PROTECTED ENDPOINTS ---
	// Read-only capability catalog for the permission-system redesign
	// (foundation phase). Not yet consulted by any other route (phase 2).
	api.HandleFunc("/authz/catalog", authHandler.AuthMiddleware(authzHandler.Catalog)).Methods("GET")
	api.HandleFunc("/authz/presets", authHandler.AuthMiddleware(authzHandler.Presets)).Methods("GET")
	api.HandleFunc("/authz/mode", authHandler.AuthMiddleware(permissionsModeHandler.GetMode)).Methods("GET")
	api.HandleFunc("/auth/profile", authHandler.AuthMiddleware(authHandler.GetProfileHandler)).Methods("GET")
	api.HandleFunc("/auth/profile", authHandler.AuthMiddleware(authHandler.UpdateProfileHandler)).Methods("PUT")
	api.HandleFunc("/auth/2fa/setup", authHandler.AuthMiddleware(authHandler.SetupTOTPHandler)).Methods("POST")
	// The three below compare the account password, so they are rate- and
	// body-limited like every other credential endpoint: an authenticated
	// session must not be a password-guessing oracle just because the guesser is
	// already logged in (a borrowed laptop, a stolen token).
	api.HandleFunc("/auth/2fa/verify", authLimiter.Limit(10, handlers.LimitBody(handlers.CredentialBodyLimit, authHandler.AuthMiddleware(authHandler.VerifyTOTPHandler)))).Methods("POST")
	api.HandleFunc("/auth/2fa/disable", authLimiter.Limit(10, handlers.LimitBody(handlers.CredentialBodyLimit, authHandler.AuthMiddleware(authHandler.DisableTOTPHandler)))).Methods("POST")
	api.HandleFunc("/auth/2fa/regenerate-backup-codes", authLimiter.Limit(10, handlers.LimitBody(handlers.CredentialBodyLimit, authHandler.AuthMiddleware(authHandler.RegenerateBackupCodesHandler)))).Methods("POST")
	api.HandleFunc("/auth/2fa/status", authHandler.AuthMiddleware(authHandler.Get2FAStatusHandler)).Methods("GET")
	api.HandleFunc("/users/{id:[0-9a-f-]{36}}/2fa", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.write")(authHandler.AdminResetTOTPHandler))).Methods("DELETE")

	api.HandleFunc("/users", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.read")(userHandler.GetAllUsers))).Methods("GET")
	api.HandleFunc("/users", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.write")(userHandler.CreateUser))).Methods("POST")
	api.HandleFunc("/users/{id:[0-9a-f-]{36}}", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.delete")(userHandler.DeleteUser))).Methods("DELETE")
	api.HandleFunc("/users/{id:[0-9a-f-]{36}}/password", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.write")(userHandler.ResetUserPassword))).Methods("PUT")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/cancel-deletion", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.write")(userHandler.CancelUserDeletion))).Methods("POST")

	// --- Roles + capability flags (PANEL users.write) ---
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/role", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.write")(userHandler.SetUserRoleHandler))).Methods("PUT")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/permissions", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.write")(userHandler.SetUserPermissionsHandler))).Methods("PUT")

	// --- Panel roles (level-1 staff roles; PANEL panelroles.*) ---
	// KNOWN ESCALATION (Phase 4 Task 12, accepted for the cut-over): panel-role
	// CRUD/assignment has no delegation/subset check (unlike owner server-roles),
	// so a non-admin holding panelroles.write could create a role carrying every
	// panel cap (or assign an admin-equivalent role to a user) and self-escalate.
	// This is safe-by-default: the seed roles are admin (holds panelroles.*) and
	// support (does not), so behavior is unchanged unless an admin deliberately
	// grants panelroles.write to a custom role. No delegation check is added here
	// (out of scope); flagged for a future delegation-check phase.
	api.HandleFunc("/admin/panel-roles", authHandler.AuthMiddleware(appState.Authz.RequireCap("panelroles.read")(panelRolesHandler.ListPanelRoles))).Methods("GET")
	api.HandleFunc("/admin/panel-roles", authHandler.AuthMiddleware(appState.Authz.RequireCap("panelroles.write")(panelRolesHandler.CreatePanelRole))).Methods("POST")
	api.HandleFunc("/admin/panel-roles/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("panelroles.write")(panelRolesHandler.UpdatePanelRole))).Methods("PATCH")
	api.HandleFunc("/admin/panel-roles/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("panelroles.delete")(panelRolesHandler.DeletePanelRole))).Methods("DELETE")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/panel-role", authHandler.AuthMiddleware(appState.Authz.RequireCap("panelroles.write")(userHandler.SetUserPanelRoleHandler))).Methods("PUT")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/panel-role", authHandler.AuthMiddleware(appState.Authz.RequireCap("panelroles.read")(userHandler.GetUserPanelRoleHandler))).Methods("GET")

	// --- Server roles (level-2 owner realm) ---
	// OWNER-scope caps (roles.*): neither route has a path {id}/{uuid}, so
	// RequireCap resolves serverID==0 and every authenticated user passes via
	// the ownerSelf short-circuit (Phase 4 Task 9, F2). This is
	// belt-and-suspenders (it blocks the unauthenticated case and documents
	// the cap); the real per-realm boundary is the handler's own
	// owner_user_id scoping (Phase 3), which is untouched. /grants uses the
	// same roles.write/roles.delete caps rather than members.write/delete
	// (see the requiredCaps comment above for why) - the handler's own
	// members.write/delete delegation check against the TARGET server
	// (AssignGrant/RevokeGrant) is the real boundary for third-party grants
	// and stays untouched.
	api.HandleFunc("/server-roles", authHandler.AuthMiddleware(appState.Authz.RequireCap("roles.read")(serverRolesHandler.ListServerRoles))).Methods("GET")
	api.HandleFunc("/server-roles", authHandler.AuthMiddleware(appState.Authz.RequireCap("roles.write")(serverRolesHandler.CreateServerRole))).Methods("POST")
	api.HandleFunc("/server-roles/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("roles.write")(serverRolesHandler.UpdateServerRole))).Methods("PATCH")
	api.HandleFunc("/server-roles/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("roles.delete")(serverRolesHandler.DeleteServerRole))).Methods("DELETE")
	api.HandleFunc("/grants", authHandler.AuthMiddleware(appState.Authz.RequireCap("roles.read")(serverRolesHandler.ListGrants))).Methods("GET")
	api.HandleFunc("/grants", authHandler.AuthMiddleware(appState.Authz.RequireCap("roles.write")(serverRolesHandler.AssignGrant))).Methods("POST")
	api.HandleFunc("/grants", authHandler.AuthMiddleware(appState.Authz.RequireCap("roles.delete")(serverRolesHandler.RevokeGrant))).Methods("DELETE")

	// --- Maintenance mode ---
	// Public state - drives the banner; never blocked by the maintenance middleware.
	// EXEMPT (Phase 4 Task 17): unauthenticated, not RequireCap-gated, not in requiredCaps.
	api.HandleFunc("/maintenance", maintenanceHandler.GetState).Methods("GET")
	api.HandleFunc("/admin/maintenance", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(maintenanceHandler.SaveState))).Methods("PUT")

	// --- Tickets ---
	// Every ticket-related endpoint is wrapped in RequireTicketsEnabled so the
	// platform-wide tickets toggle flips the entire subsystem (UI + API +
	// notifications fan-out) without a per-handler check.
	//
	// Phase 4 Task 15: this subsystem MIXES two authz models and they are not
	// collapsed. Admin ticket MANAGEMENT (categories, canned responses,
	// settings, migration/backup/restore, inbox, deletion-log) is RequireCap
	// gated here with PANEL tickets.* - the former IsAdmin gate is removed
	// from those handlers. User-facing ticket routes (create/list/get/reply/
	// attachments/status/priority/assignment/watchers, plus the public
	// category/canned-response GETs, notifications, and via-tickets) stay
	// authed-exempt: they keep authorizing in-handler via canSeeTicket/
	// canReply/canMutate (owner/watcher/support matrix) or an equivalent
	// support-or-admin check specific to that route - that check IS the
	// boundary and is not superseded by a route-level cap.
	// Categories: public list (enabled only) for create form, admin CRUD for management.
	api.HandleFunc("/ticket-categories", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketCategoriesHandler.ListCategories))).Methods("GET")
	api.HandleFunc("/admin/ticket-categories", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.read")(appState.RequireTicketsEnabled(ticketCategoriesHandler.AdminListCategories)))).Methods("GET")
	api.HandleFunc("/admin/ticket-categories", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.write")(appState.RequireTicketsEnabled(ticketCategoriesHandler.CreateCategory)))).Methods("POST")
	api.HandleFunc("/admin/ticket-categories/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.write")(appState.RequireTicketsEnabled(ticketCategoriesHandler.UpdateCategory)))).Methods("PATCH")
	api.HandleFunc("/admin/ticket-categories/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.delete")(appState.RequireTicketsEnabled(ticketCategoriesHandler.DeleteCategory)))).Methods("DELETE")

	// Tickets: user CRUD + support inbox. GET/POST /tickets and GET/DELETE
	// /tickets/{id} stay authed-exempt (the owner/watcher/support ACL in
	// tickets.go, or - for DELETE - the admin-only gate in ticket_deletions.go,
	// is the boundary). GET /tickets/inbox IS admin/support management (the
	// support triage view), so it is RequireCap("tickets.read")-gated here and
	// its in-handler "support or admin" pure gate was removed.
	api.HandleFunc("/tickets", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketsHandler.ListMyTickets))).Methods("GET")
	api.HandleFunc("/tickets", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketsHandler.CreateTicket))).Methods("POST")
	api.HandleFunc("/tickets/inbox", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.read")(appState.RequireTicketsEnabled(ticketsHandler.ListInboxTickets)))).Methods("GET")
	api.HandleFunc("/tickets/{id:[0-9]+}", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketsHandler.GetTicket))).Methods("GET")
	api.HandleFunc("/tickets/{id:[0-9]+}", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketDeletionsHandler.DeleteTicket))).Methods("DELETE")
	api.HandleFunc("/tickets/{id:[0-9]+}/messages", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketsHandler.AddReply))).Methods("POST")
	api.HandleFunc("/tickets/{id:[0-9]+}/status", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketsHandler.UpdateStatus))).Methods("PATCH")
	api.HandleFunc("/tickets/{id:[0-9]+}/priority", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketsHandler.UpdatePriority))).Methods("PATCH")
	api.HandleFunc("/tickets/{id:[0-9]+}/assignment", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketsHandler.UpdateAssignment))).Methods("PATCH")
	api.HandleFunc("/tickets/{id:[0-9]+}/watchers", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketsHandler.AddWatcher))).Methods("POST")
	api.HandleFunc("/tickets/{id:[0-9]+}/watchers/{userId:[0-9a-f-]{36}}", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketsHandler.RemoveWatcher))).Methods("DELETE")

	// Sidebar source for support's "Via tickets" tab.
	api.HandleFunc("/me/servers/via-tickets", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketsHandler.ListMyServersViaTickets))).Methods("GET")

	// Settings (admin management -> PANEL tickets.*).
	api.HandleFunc("/admin/settings/tickets", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.read")(appState.RequireTicketsEnabled(ticketSettingsHandler.GetSettings)))).Methods("GET")
	api.HandleFunc("/admin/settings/tickets", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.write")(appState.RequireTicketsEnabled(ticketSettingsHandler.SaveSettings)))).Methods("PUT")

	// --- Tickets: attachments, canned responses, notifications ---
	api.HandleFunc("/tickets/{id:[0-9]+}/attachments", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(appState.RequireCoreStorageConfigured(appState.RequireCoreStorageReachable(ticketAttachmentsHandler.UploadAttachment))))).Methods("POST")
	api.HandleFunc("/tickets/{id:[0-9]+}/attachments", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(ticketAttachmentsHandler.ListAttachments))).Methods("GET")
	api.HandleFunc("/tickets/{id:[0-9]+}/attachments/{aid:[0-9]+}/download", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(appState.RequireCoreStorageReachable(ticketAttachmentsHandler.DownloadAttachment)))).Methods("GET")
	api.HandleFunc("/tickets/{id:[0-9]+}/attachments/{aid:[0-9]+}", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(appState.RequireCoreStorageReachable(ticketAttachmentsHandler.DeleteAttachment)))).Methods("DELETE")

	// Canned responses: support sees the read list (authed-exempt - its own
	// in-handler support-or-admin check is the boundary and is unchanged),
	// admin manages (RequireCap-gated, IsAdmin gate removed).
	api.HandleFunc("/ticket-canned-responses", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(cannedResponsesHandler.ListForSupport))).Methods("GET")
	api.HandleFunc("/admin/ticket-canned-responses", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.read")(appState.RequireTicketsEnabled(cannedResponsesHandler.AdminList)))).Methods("GET")
	api.HandleFunc("/admin/ticket-canned-responses", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.write")(appState.RequireTicketsEnabled(cannedResponsesHandler.Create)))).Methods("POST")
	api.HandleFunc("/admin/ticket-canned-responses/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.write")(appState.RequireTicketsEnabled(cannedResponsesHandler.Update)))).Methods("PATCH")
	api.HandleFunc("/admin/ticket-canned-responses/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.delete")(appState.RequireTicketsEnabled(cannedResponsesHandler.Delete)))).Methods("DELETE")

	// Notifications: in-app inbox. Currently ticket-driven; gated with the
	// rest of the ticket subsystem.
	api.HandleFunc("/notifications", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(notificationsHandler.List))).Methods("GET")
	api.HandleFunc("/notifications/unread-count", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(notificationsHandler.UnreadCount))).Methods("GET")
	api.HandleFunc("/notifications/{id:[0-9]+}/read", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(notificationsHandler.MarkRead))).Methods("POST")
	api.HandleFunc("/notifications/read-all", authHandler.AuthMiddleware(appState.RequireTicketsEnabled(notificationsHandler.MarkAllRead))).Methods("POST")

	// --- Server audit ---
	// View is server.audit.read: owner + admin via the resolver short-circuits,
	// plus anyone the owner explicitly delegates it to. It used to be
	// overview.read, which is the cap EVERY invite carries - so every invited
	// member could read the log of what all the other members did, including the
	// IP address and user agent of the owner and of each of them. The panel has
	// always said otherwise ("Audit tab. Owner + admin only; non-owners, even
	// with permission bundles, can't see who changed what on someone else's
	// server"), but it only greyed the tab out; the API answered anyone.
	// Force-on is server.settings.write: the owner (or a role-holder with that
	// cap) controls their own server's forced audit; admin still passes via the
	// resolver's admin short-circuit.
	api.HandleFunc("/servers/{id:[0-9]+}/audit", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.audit.read")(serverAuditHandler.ListAudit))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/audit/status", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.audit.read")(serverAuditHandler.GetStatus))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/audit/force", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverAuditHandler.SetForce))).Methods("PUT")
	// Platform-wide audit retention policy (PANEL settings.*).
	api.HandleFunc("/admin/settings/audit", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(auditSettingsHandler.GetPolicy))).Methods("GET")
	api.HandleFunc("/admin/settings/audit", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(auditSettingsHandler.SavePolicy))).Methods("PUT")

	// --- Ticket DB migration + backups (admin management -> PANEL tickets.*) ---
	// GET reads map to tickets.read; every mutation (including the backup
	// delete and the restore Danger Zone, which keep their own 2FA/cooldown/
	// typed-phrase checks in-handler) maps to tickets.write.
	api.HandleFunc("/admin/tickets/migration/status", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.read")(appState.RequireTicketsEnabled(ticketMigrationHandler.GetStatus)))).Methods("GET")
	api.HandleFunc("/admin/tickets/migration/test-connection", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.write")(appState.RequireTicketsEnabled(ticketMigrationHandler.TestExternalConnection)))).Methods("POST")
	api.HandleFunc("/admin/tickets/migration/dry-run", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.write")(appState.RequireTicketsEnabled(ticketMigrationHandler.DryRunMigration)))).Methods("POST")
	api.HandleFunc("/admin/tickets/migration/execute", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.write")(appState.RequireTicketsEnabled(ticketMigrationHandler.ExecuteMigration)))).Methods("POST")
	// Backups: create, list, download, delete.
	api.HandleFunc("/admin/tickets/backup", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.write")(appState.RequireTicketsEnabled(appState.RequireCoreStorageConfigured(appState.RequireCoreStorageReachable(ticketMigrationHandler.CreateBackup)))))).Methods("POST")
	api.HandleFunc("/admin/tickets/backups", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.read")(appState.RequireTicketsEnabled(appState.RequireCoreStorageReachable(ticketMigrationHandler.ListBackups))))).Methods("GET")
	api.HandleFunc("/admin/tickets/backups/{name}/download", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.read")(appState.RequireTicketsEnabled(appState.RequireCoreStorageReachable(ticketMigrationHandler.DownloadBackup))))).Methods("GET")
	api.HandleFunc("/admin/tickets/backups/{name}", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.write")(appState.RequireTicketsEnabled(appState.RequireCoreStorageReachable(ticketMigrationHandler.DeleteBackup))))).Methods("DELETE")
	// Restore: two-step Danger Zone (init + execute) - 2FA + 15s timer + typed phrase.
	api.HandleFunc("/admin/tickets/restore/init", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.write")(appState.RequireTicketsEnabled(appState.RequireCoreStorageReachable(ticketMigrationHandler.InitRestore))))).Methods("POST")
	api.HandleFunc("/admin/tickets/restore/execute", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.write")(appState.RequireTicketsEnabled(appState.RequireCoreStorageReachable(ticketMigrationHandler.ExecuteRestore))))).Methods("POST")
	// Deletion audit log (admin management -> PANEL tickets.read). Note: the
	// DELETE /tickets/{id} handler itself (ticket_deletions.go) stays
	// authed-exempt with its in-handler IsAdmin gate untouched - that route
	// is shared with the user-facing GET /tickets/{id} template and is
	// listed as authed-exempt in the batch brief, so it is NOT RequireCap'd.
	api.HandleFunc("/admin/tickets/deletion-log", authHandler.AuthMiddleware(appState.Authz.RequireCap("tickets.read")(appState.RequireTicketsEnabled(ticketDeletionsHandler.ListDeletions)))).Methods("GET")
	api.HandleFunc("/users/{id:[0-9a-f-]{36}}/route-limit", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.read")(userHandler.GetUserRouteLimit))).Methods("GET")
	api.HandleFunc("/users/{id:[0-9a-f-]{36}}/route-limit", authHandler.AuthMiddleware(appState.Authz.RequireCap("users.write")(userHandler.SetUserRouteLimit))).Methods("PUT")

	// Phase 4 Task 19: GET /modules stays authed-exempt - it renders the navbar
	// for every authenticated user and filters admin-only modules in-handler by
	// role (GetModulesHandler), which is the boundary for that read. The
	// mutations (create/delete/toggle/position/role) are admin-only and there
	// is no dedicated modules.* PANEL cap, so they RequireCap("settings.write")
	// (the faithful mapping per the Phase 4 controller decision).
	api.HandleFunc("/modules", authHandler.AuthMiddleware(moduleHandler.GetModulesHandler)).Methods("GET")
	api.HandleFunc("/modules", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(moduleHandler.CreateModuleHandler))).Methods("POST")
	api.HandleFunc("/modules/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(moduleHandler.DeleteModuleHandler))).Methods("DELETE")
	api.HandleFunc("/modules/{id:[0-9]+}/toggle", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(moduleHandler.ToggleModuleHandler))).Methods("PATCH")
	api.HandleFunc("/modules/{id:[0-9]+}/position", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(moduleHandler.UpdateModulePositionHandler))).Methods("PATCH")
	api.HandleFunc("/modules/{id:[0-9]+}/role", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(moduleHandler.SetModuleAccessRoleHandler))).Methods("PATCH")

	// Phase 4 Task 13: GET /nodes stays authed-exempt. Its admin-vs-owner filter
	// (nodes.go GetNodes: admin sees all, a BYON tenant sees only nodes they own)
	// is the boundary; gating it PANEL nodes.read would regress BYON owners out
	// of their own node list, so it deliberately keeps bare AuthMiddleware.
	api.HandleFunc("/nodes", authHandler.AuthMiddleware(nodeHandler.GetNodes)).Methods("GET")
	// Link sidecar image updates. The node replaces its own Link container; these
	// only carry the operator's preference and the manual trigger, and they are
	// admin-only because most panel users never see a Link at all.
	api.HandleFunc("/nodes/link-updates", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.read")(nodeHandler.GetLinkUpdateStates))).Methods("GET")
	api.HandleFunc("/nodes/link-updates", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.write")(nodeHandler.TriggerLinkUpdate))).Methods("POST")
	api.HandleFunc("/admin/settings/link-updates", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(settingsHandler.GetLinkUpdateSettings))).Methods("GET")
	api.HandleFunc("/admin/settings/link-updates", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(settingsHandler.UpdateLinkUpdateSettings))).Methods("PUT")
	api.HandleFunc("/nodes", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.write")(nodeHandler.CreateNode))).Methods("POST")
	api.HandleFunc("/nodes/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.write")(nodeHandler.UpdateNode))).Methods("PUT")
	// Adopt an auto-discovered node: admin sets name/region/tags (DB precedence).
	api.HandleFunc("/nodes/{id:[0-9]+}/config", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.write")(nodeHandler.ConfigureNode))).Methods("PATCH")
	api.HandleFunc("/nodes/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.delete")(nodeHandler.DeleteNode))).Methods("DELETE")
	// These 4 reads carry their own admin-vs-BYON-owner data filter (canManageNode,
	// tenancy.go) rather than a pure IsAdmin gate, so like GET /nodes they stay
	// authed-exempt (Phase 4 Task 13 INSPECT ruling) - gating PANEL nodes.read here
	// would regress BYON owner access to their own node's servers/storage/deploy
	// bundle/cpu topology.
	api.HandleFunc("/nodes/{id:[0-9]+}/servers", authHandler.AuthMiddleware(nodeHandler.GetNodeServers)).Methods("GET")
	api.HandleFunc("/nodes/{id:[0-9]+}/force", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.delete")(nodeHandler.ForceDeleteNode))).Methods("DELETE")
	api.HandleFunc("/nodes/{id:[0-9]+}/storage", authHandler.AuthMiddleware(nodeHandler.GetNodeStorage)).Methods("GET")
	api.HandleFunc("/nodes/{id:[0-9]+}/storage-placement", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.write")(nodeHandler.SetNodeStoragePlacement))).Methods("PUT")
	api.HandleFunc("/settings/storage-placement", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(nodeHandler.GetFleetStoragePlacement))).Methods("GET")
	api.HandleFunc("/settings/storage-placement", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(nodeHandler.SetFleetStoragePlacement))).Methods("PUT")
	api.HandleFunc("/nodes/{id:[0-9]+}/deploy-bundle", authHandler.AuthMiddleware(nodeHandler.GetDeployBundle)).Methods("GET")
	api.HandleFunc("/nodes/{id:[0-9]+}/cpu", authHandler.AuthMiddleware(cpuPinningHandler.GetNodeCPU)).Methods("GET")
	// BYON per-user node enrollment tokens (feature-gated inside the handlers).
	api.HandleFunc("/nodes/enroll-token", authHandler.AuthMiddleware(nodeEnrollHandler.MintToken)).Methods("POST")
	api.HandleFunc("/nodes/enroll-token", authHandler.AuthMiddleware(nodeEnrollHandler.ListTokens)).Methods("GET")
	api.HandleFunc("/nodes/enroll-token/{id}", authHandler.AuthMiddleware(nodeEnrollHandler.RevokeToken)).Methods("DELETE")

	// Admin endpoints
	api.HandleFunc("/admin/servers", authHandler.AuthMiddleware(appState.Authz.RequireCap("servers.read")(serverHandler.GetAdminServers))).Methods("GET")
	api.HandleFunc("/admin/servers/{id:[0-9]+}/owner", authHandler.AuthMiddleware(appState.Authz.RequireCap("servers.write")(serverHandler.AdminUpdateServerOwner))).Methods("PATCH")
	api.HandleFunc("/admin/nodes/{id:[0-9]+}/disk-analysis", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.read")(nodeHandler.GetDiskAnalysis))).Methods("GET")
	api.HandleFunc("/admin/nodes/{id:[0-9]+}/orphan", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.delete")(nodeHandler.DeleteOrphanedFolder))).Methods("DELETE")
	api.HandleFunc("/admin/nodes/{id:[0-9]+}/reset-pairing", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.write")(nodeAdmissionHandler.ResetPairing))).Methods("POST")

	// Orphan file browser (PANEL nodes.read/write; read-only browse, write on assign - no DB servers row required)
	api.HandleFunc("/disk/orphans/{nodeId:[0-9]+}/{uuid}/files", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.read")(nodeHandler.ListOrphanFiles))).Methods("GET")
	api.HandleFunc("/disk/orphans/{nodeId:[0-9]+}/{uuid}/content", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.read")(nodeHandler.GetOrphanFileContent))).Methods("GET")
	api.HandleFunc("/disk/orphans/{nodeId:[0-9]+}/{uuid}/inspect", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.read")(nodeHandler.InspectOrphan))).Methods("GET")
	api.HandleFunc("/disk/orphans/assign", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.write")(nodeHandler.AssignOrphan))).Methods("POST")

	api.HandleFunc("/servers", authHandler.AuthMiddleware(serverHandler.GetServers)).Methods("GET")
	api.HandleFunc("/servers", authHandler.AuthMiddleware(serverHandler.CreateServer)).Methods("POST")
	// POWER is in-handler-enforced (Rule R5): the action is in the request body
	// (start|stop|restart|kill), so a single route-level RequireCap would wrongly
	// block e.g. a kill-only holder from starting. ServerPowerHandler resolves
	// "power."+action itself via appState.Authz.Resolve.
	api.HandleFunc("/servers/{id:[0-9]+}/power", authHandler.AuthMiddleware(serverHandler.ServerPowerHandler)).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/install-cooldown", authHandler.AuthMiddleware(appState.Authz.RequireCap("overview.read")(serverHandler.GetInstallCooldown))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/setup", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverHandler.SetupServer))).Methods("POST")
	// Java image and JVM flags WITHOUT reinstalling. Same capability as setup:
	// it is the same screen and the same decision, only the non-destructive half.
	api.HandleFunc("/servers/{id:[0-9]+}/runtime", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverHandler.UpdateServerRuntime))).Methods("PATCH")
	api.HandleFunc("/servers/{id:[0-9]+}/reinstall", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverHandler.ReinstallServer))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/switch", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverHandler.SwitchSubServer))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/name", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverHandler.UpdateServerName))).Methods("PATCH")
	api.HandleFunc("/servers/{id:[0-9]+}/resources", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverHandler.UpdateServerResources))).Methods("PATCH")
	// Metadata-only declare (no reinstall): persists installerType/minecraftVersion/
	// buildNumber for an imported server so the Content tab's loader/version
	// auto-filtering starts working. See servers_metadata.go.
	api.HandleFunc("/servers/{id:[0-9]+}/loader-metadata", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverHandler.DeclareServerLoaderMetadata))).Methods("PATCH")
	api.HandleFunc("/servers/{id:[0-9]+}/console/history", authHandler.AuthMiddleware(appState.Authz.RequireCap("console.read")(consoleHandler.GetHistory))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/console/stream", authHandler.AuthMiddleware(appState.Authz.RequireCap("console.read")(consoleHandler.StreamConsole))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/console/command", authHandler.AuthMiddleware(appState.Authz.RequireCap("console.send")(consoleHandler.SendCommand))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/stats/stream", authHandler.AuthMiddleware(appState.Authz.RequireCap("stats.read")(statsHandler.StreamStats))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/stats/history", authHandler.AuthMiddleware(appState.Authz.RequireCap("stats.read")(statsHandler.GetHistory))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/stats/disk", authHandler.AuthMiddleware(appState.Authz.RequireCap("stats.read")(statsHandler.GetDisk))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/members", authHandler.AuthMiddleware(appState.Authz.RequireCap("members.read")(memberHandler.GetMembers))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/members", authHandler.AuthMiddleware(appState.Authz.RequireCap("members.write")(memberHandler.InviteMember))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/members/inherited", authHandler.AuthMiddleware(appState.Authz.RequireCap("members.read")(memberHandler.GetInheritedMembers))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/members/{userId:[0-9a-f-]{36}}", authHandler.AuthMiddleware(appState.Authz.RequireCap("members.write")(memberHandler.UpdateMemberPermissions))).Methods("PATCH")
	api.HandleFunc("/servers/{id:[0-9]+}/members/{userId:[0-9a-f-]{36}}", authHandler.AuthMiddleware(appState.Authz.RequireCap("members.delete")(memberHandler.RemoveMember))).Methods("DELETE")
	api.HandleFunc("/servers/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.delete")(serverHandler.DeleteServer))).Methods("DELETE")
	api.HandleFunc("/servers/{id:[0-9]+}/sub-servers/{subServerName}", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.delete")(serverHandler.DeleteSubServer))).Methods("DELETE")
	api.HandleFunc("/servers/{id:[0-9]+}/proxy", authHandler.AuthMiddleware(appState.Authz.RequireCap("network.write")(serverHandler.LinkServerToProxy))).Methods("PUT")
	api.HandleFunc("/servers/{id:[0-9]+}/proxy", authHandler.AuthMiddleware(appState.Authz.RequireCap("network.write")(serverHandler.UnlinkServerFromProxy))).Methods("DELETE")
	api.HandleFunc("/servers/{id:[0-9]+}/proxy-endpoint", authHandler.AuthMiddleware(appState.Authz.RequireCap("network.read")(serverHandler.GetProxyEndpoint))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/storage-path", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverHandler.GetServerStoragePath))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/migrate-storage", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverHandler.MigrateServerStorage))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/sftp-credentials", authHandler.AuthMiddleware(appState.Authz.RequireCap("sftp.access")(serverHandler.GetSftpCredentials))).Methods("GET")

	api.HandleFunc("/versions/software", versionHandler.GetSoftwareList).Methods("GET")
	api.HandleFunc("/versions", authHandler.AuthMiddleware(versionHandler.GetVersions)).Methods("GET")

	// --- Gateway Endpoints ---
	gatewayHandler := handlers.NewGatewayHandler(appState)
	dnsHandler := handlers.NewDNSHandler(appState)
	trafficLimitHandler := handlers.NewTrafficLimitHandler(appState)
	infrastructureHandler := handlers.NewInfrastructureHandler(appState)
	metricsHandler := handlers.NewMetricsHandler(appState)
	metricsDBHandler := handlers.NewMetricsDBHandler(appState)

	// Admin endpoints (PANEL topology.* - global gateway oversight; Phase 4 Task 18)
	api.HandleFunc("/gateway/links", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(gatewayHandler.GetLinks))).Methods("GET")
	api.HandleFunc("/gateway/edges", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(gatewayHandler.GetEdges))).Methods("GET")
	api.HandleFunc("/gateway/routes", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(gatewayHandler.GetAllRoutes))).Methods("GET")
	api.HandleFunc("/gateway/dns-check", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(dnsHandler.CheckDNS))).Methods("GET")
	api.HandleFunc("/gateway/routes/suffixes", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(gatewayHandler.GetRouteSuffixes))).Methods("GET")
	api.HandleFunc("/gateway/routes/bulk-delete", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.write")(gatewayHandler.BulkDeleteRoutesBySuffix))).Methods("POST")
	api.HandleFunc("/gateway/routes/{domain:.+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.write")(gatewayHandler.AdminDeleteRoute))).Methods("DELETE")
	// check-domain is an owner-facing route-creation helper (RouteDomainPicker in
	// the new-server + routes UI): any authed user needs the domain-availability
	// hint for their OWN route. Read-only, non-sensitive -> authed-exempt (T22).
	api.HandleFunc("/gateway/check-domain", authHandler.AuthMiddleware(gatewayHandler.CheckDomainAvailability)).Methods("GET")
	api.HandleFunc("/gateway/logs", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(gatewayHandler.GetLogs))).Methods("GET")
	api.HandleFunc("/gateway/stats", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(gatewayHandler.GetStats))).Methods("GET")
	api.HandleFunc("/gateway/sync", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.write")(gatewayHandler.TriggerSync))).Methods("POST")
	api.HandleFunc("/gateway/errors", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(gatewayHandler.GetErrors))).Methods("GET")

	// User endpoints (per-server routes, identified by domain)
	api.HandleFunc("/servers/{id:[0-9]+}/routes", authHandler.AuthMiddleware(appState.Authz.RequireCap("network.read")(gatewayHandler.GetServerRoutes))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/routes", authHandler.AuthMiddleware(appState.Authz.RequireCap("network.write")(appState.RequireGatewayEnabled(gatewayHandler.CreateServerRoute)))).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/routes/{domain:.+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("network.write")(gatewayHandler.DeleteServerRoute))).Methods("DELETE")

	// Route-only (external origin): a protected address pointed at a server the
	// user already runs, no managed node. Owner-scoped; create is gateway-gated.
	// Tenant self-service with NO server {id} and no matching OWNER cap - stays
	// authed-exempt (Phase 4 controller decision #2); the in-handler owner
	// filter (resolveOwnedLinkToken / ListLinkRoutes / DeleteLinkRoute ownership
	// scan) is the boundary. A future links.* owner cap is a deferred follow-up.
	// Custom-domain ownership proof. Scoped to the caller inside each handler -
	// a claim is one user's relationship to one domain, and the block is per
	// (user, domain) so nobody can lock a competitor's domain out globally.
	customDomainHandler := handlers.NewCustomDomainHandler(appState, services.NewNetResolver())
	api.HandleFunc("/gateway/custom-domains", authHandler.AuthMiddleware(customDomainHandler.List)).Methods("GET")
	api.HandleFunc("/gateway/custom-domains/{domain:.+}/txt-token",
		authHandler.AuthMiddleware(customDomainHandler.IssueTXTToken)).Methods("POST")
	// Rate-limited: it triggers an outbound DNS lookup and is the way back from a
	// permanent block, so it is the one endpoint here worth hammering.
	api.HandleFunc("/gateway/custom-domains/{domain:.+}/verify-txt",
		authLimiter.Limit(20, authHandler.AuthMiddleware(customDomainHandler.VerifyTXT))).Methods("POST")

	api.HandleFunc("/gateway/link-routes", authHandler.AuthMiddleware(gatewayHandler.ListLinkRoutes)).Methods("GET")
	api.HandleFunc("/gateway/link-routes", authHandler.AuthMiddleware(appState.RequireGatewayEnabled(gatewayHandler.CreateLinkRoute))).Methods("POST")
	api.HandleFunc("/gateway/link-routes/{domain:.+}", authHandler.AuthMiddleware(gatewayHandler.DeleteLinkRoute)).Methods("DELETE")

	// Infrastructure overview + migration status (PANEL topology.read; Phase 4 Task 18)
	// Traffic limits: what a tenant may use and buy, per (region, kind). The
	// list and the write share one path and therefore one capability entry, so
	// the read is gated on settings.write too - deliberate: the rows are policy,
	// and there is no reason to show them to someone who may not change them.
	api.HandleFunc("/traffic-limits", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(trafficLimitHandler.ListTrafficLimits))).Methods("GET")
	api.HandleFunc("/traffic-limits", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(trafficLimitHandler.SetTrafficLimit))).Methods("PUT")
	api.HandleFunc("/traffic-limits/resolve", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(trafficLimitHandler.ResolveTrafficLimit))).Methods("GET")

	api.HandleFunc("/infrastructure/overview", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(infrastructureHandler.GetOverview))).Methods("GET")

	// The long-term record. Read-only, behind the same capability as the rest
	// of Infrastructure, because that is the tab it is read from and the
	// numbers describe the same fleet.
	api.HandleFunc("/admin/metrics/catalog", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(metricsHandler.Catalog))).Methods("GET")
	api.HandleFunc("/admin/metrics/series", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(metricsHandler.Series))).Methods("GET")
	api.HandleFunc("/admin/metrics/summary", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(metricsHandler.Summary))).Methods("GET")
	api.HandleFunc("/admin/metrics/export", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(metricsHandler.Export))).Methods("GET")
	// Where the long-term statistics are written. `test` is settings.WRITE even
	// though it stores nothing: it opens a connection to a host the caller
	// names, which is not something a read-only role should be able to make
	// this server do.
	api.HandleFunc("/admin/settings/metrics-db", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(metricsDBHandler.Get))).Methods("GET")
	api.HandleFunc("/admin/settings/metrics-db", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(metricsDBHandler.Set))).Methods("PUT")
	api.HandleFunc("/admin/settings/metrics-db/test", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(metricsDBHandler.Test))).Methods("POST")
	api.HandleFunc("/infrastructure/routing-migration", authHandler.AuthMiddleware(appState.Authz.RequireCap("topology.read")(infrastructureHandler.GetRoutingMigrationStatus))).Methods("GET")

	// XDP / eBPF DDoS Protection (deployment-wide config managed by Panel,
	// consumed by every Edge replica via Redis poll)
	xdpHandler := handlers.NewXDPHandler(appState)
	api.HandleFunc("/admin/xdp/config", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(xdpHandler.GetConfig))).Methods("GET")
	api.HandleFunc("/admin/xdp/config", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(xdpHandler.UpdateConfig))).Methods("PUT")

	api.HandleFunc("/files", authHandler.AuthMiddleware(fileHandler.GetFilesHandler)).Methods("GET")
	api.HandleFunc("/files/content", authHandler.AuthMiddleware(fileHandler.GetFileContentHandler)).Methods("GET")
	api.HandleFunc("/files/save", authHandler.AuthMiddleware(fileHandler.SaveFileHandler)).Methods("POST")
	api.HandleFunc("/files/create", authHandler.AuthMiddleware(fileHandler.CreateFileHandler)).Methods("POST")
	api.HandleFunc("/files/rename", authHandler.AuthMiddleware(fileHandler.RenameFileHandler)).Methods("POST")
	api.HandleFunc("/files/copy", authHandler.AuthMiddleware(fileHandler.CopyFileHandler)).Methods("POST")
	api.HandleFunc("/files/delete", authHandler.AuthMiddleware(fileHandler.DeleteFileHandler)).Methods("POST")
	api.HandleFunc("/files/download", authHandler.AuthMiddleware(fileHandler.DownloadFileHandler)).Methods("GET")
	api.HandleFunc("/files/download/selective", authHandler.AuthMiddleware(fileHandler.SelectiveDownloadHandler)).Methods("GET")
	api.HandleFunc("/files/upload", authHandler.AuthMiddleware(fileHandler.UploadFileHandler)).Methods("POST")

	// Library endpoints. Phase 4 Task 20 INSPECT result: this is a single
	// platform-shared file catalog (buildProvider has no per-owner scoping),
	// not a per-user OWNER realm, so the library.* catalog caps (ScopeOwner) do
	// not fit - PANEL settings.write is used instead, same call as Task 19's
	// /api/modules mutations. GET (browse) and GET /download have no gate
	// beyond auth in the handler today (any authenticated user may browse/
	// download during server setup - see LibraryPicker.tsx; admin-disabled
	// paths are redacted in-handler, not chokepoint-enforced) and stay
	// authed-exempt, deliberately NOT RequireCap-wrapped and NOT listed in
	// requiredCaps. The four mutations were hard `if !isAdmin` gated in-handler
	// with no dedicated library-admin cap - that gate is now the chokepoint's
	// job, so it is removed from library.go in the same commit.
	api.HandleFunc("/library", authHandler.AuthMiddleware(appState.RequireCoreStorageReachable(libraryHandler.GetLibraryHandler))).Methods("GET")
	api.HandleFunc("/library/delete", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(appState.RequireCoreStorageReachable(libraryHandler.DeleteLibraryHandler)))).Methods("POST")
	api.HandleFunc("/library/mkdir", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(appState.RequireCoreStorageConfigured(appState.RequireCoreStorageReachable(libraryHandler.MkdirLibraryHandler))))).Methods("POST")
	api.HandleFunc("/library/upload", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(appState.RequireCoreStorageConfigured(appState.RequireCoreStorageReachable(libraryHandler.UploadLibraryHandler))))).Methods("POST")
	api.HandleFunc("/library/download", authHandler.AuthMiddleware(appState.RequireCoreStorageReachable(libraryHandler.DownloadLibraryHandler))).Methods("GET")
	api.HandleFunc("/library/toggle", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(libraryHandler.ToggleLibraryPathHandler))).Methods("POST")

	// Settings endpoints (PANEL settings.*; Phase 4 Task 17)
	// NOTE: /settings/library + /settings/library/test (legacy library storage
	// CRUD + test-connection) were removed - dead since the Core-stateless-storage
	// rework; see the removal note above handlers.NewSettingsHandler. Use
	// /settings/core-storage (coreStorageHandler, registered further below)
	// instead.
	api.HandleFunc("/settings/filemanager", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(settingsHandler.GetFileManagerSettings))).Methods("GET")
	api.HandleFunc("/settings/filemanager", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(settingsHandler.SaveFileManagerSettings))).Methods("POST")
	// /settings/filemanager/limits stays EXEMPT-authed: every authenticated user
	// (not just admins) needs their own upload/download limit to drive the file
	// browser UI - not in requiredCaps.
	api.HandleFunc("/settings/filemanager/limits", authHandler.AuthMiddleware(settingsHandler.GetUserLimits)).Methods("GET")
	// GET /settings/features is EXEMPT-authed: LoadFeatureSettings is documented
	// "available to all authenticated users" and the handler carries no admin
	// gate (proxy/gateway flags drive UI for every user). The POST save IS
	// admin-only -> PANEL settings.write. Same template split as GET /nodes vs
	// POST /nodes (Task 13): not added to requiredCaps here, deferred to the
	// Task 22/23 per-method ExemptRoutes reconciliation.
	api.HandleFunc("/settings/features", authHandler.AuthMiddleware(settingsHandler.GetFeatureSettings)).Methods("GET")
	api.HandleFunc("/settings/features", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(settingsHandler.SaveFeatureSettings))).Methods("POST")
	api.HandleFunc("/settings/gateway", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(settingsHandler.GetGatewaySettings))).Methods("GET")
	api.HandleFunc("/settings/gateway", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(settingsHandler.SaveGatewaySettings))).Methods("POST")

	// Hub-admin Redis provisioner (TP2b): admin-only create/roll of the
	// gw-hub-admin ACL user (PANEL settings.*). GET status carries no secret;
	// the password is returned once by POST provision/roll.
	// The DNS credential is stored by the gateway HUB, not here. These two
	// forward the panel's form; Core holds no copy. See handlers/gateway_dns.go.
	api.HandleFunc("/settings/gateway/dns", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(gatewayDNSHandler.Get))).Methods("GET")
	api.HandleFunc("/settings/gateway/dns", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(gatewayDNSHandler.Save))).Methods("PUT")
	api.HandleFunc("/settings/gateway/dns/probe", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(gatewayDNSHandler.Probe))).Methods("POST")
	api.HandleFunc("/gateway-bandwidth/overview", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(gatewayBandwidthHandler.GetOverview))).Methods("GET")
	api.HandleFunc("/gateway-bandwidth/history", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(gatewayBandwidthHandler.GetHistory))).Methods("GET")
	api.HandleFunc("/gateway-bandwidth/rebalance", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(gatewayBandwidthHandler.GetRebalance))).Methods("GET")
	api.HandleFunc("/gateway-bandwidth/rebalance", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(gatewayBandwidthHandler.SetRebalanceMode))).Methods("POST")

	// Placement / Scheduling. /placement/pick + /tags + /regions stay authed-exempt
	// (Phase 4 Task 13): they are node-picking helper reads/preview any authed user
	// may call (no server {id}, not privilege-sensitive on their own).
	api.HandleFunc("/settings/placement", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(settingsHandler.GetPlacementSettings))).Methods("GET")
	api.HandleFunc("/settings/placement", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(settingsHandler.SavePlacementSettings))).Methods("POST")
	api.HandleFunc("/placement/pick", authHandler.AuthMiddleware(placementHandler.PickNode)).Methods("POST")
	api.HandleFunc("/placement/tags", authHandler.AuthMiddleware(placementHandler.AvailableTagsHandler)).Methods("GET")
	api.HandleFunc("/placement/regions", authHandler.AuthMiddleware(placementHandler.AvailableRegionsHandler)).Methods("GET")
	api.HandleFunc("/nodes/{id:[0-9]+}/placement", authHandler.AuthMiddleware(appState.Authz.RequireCap("nodes.write")(placementHandler.SetNodePlacement))).Methods("PUT")

	// Server auto-move toggle - gated on the feature flag AND active gateway.
	api.HandleFunc("/servers/{id:[0-9]+}/automove", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(appState.RequireAutoMoveEnabled(serverHandler.SetServerAutoMove)))).Methods("PATCH")
	// Manual node-to-node move (admin oversight) - gateway-only; enqueues onto the orchestrator.
	api.HandleFunc("/admin/servers/{id:[0-9]+}/move", authHandler.AuthMiddleware(appState.Authz.RequireCap("servers.write")(appState.RequireGatewayEnabled(serverHandler.MoveServer)))).Methods("POST")
	// Tenant-facing transfer (BYON) - gateway-only; owner-or-admin + placement authz inside.
	api.HandleFunc("/servers/{id:[0-9]+}/transfer", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(appState.RequireGatewayEnabled(serverHandler.TransferServer)))).Methods("POST")
	// Per-server edge transitional-MOTD: GET current config, PATCH mode/text.
	// PATCH publishes to Redis for the gateway edge (auto/custom/off).
	api.HandleFunc("/servers/{id:[0-9]+}/edge-motd", authHandler.AuthMiddleware(appState.Authz.RequireCap("overview.read")(serverHandler.GetServerEdgeMotd))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/edge-motd", authHandler.AuthMiddleware(appState.Authz.RequireCap("server.settings.write")(serverHandler.SetServerEdgeMotd))).Methods("PATCH")
	// Admin cancel of an in-flight migration (pre-cutover rollback to the source node) - gateway-only.
	api.HandleFunc("/admin/servers/{id:[0-9]+}/migration/cancel", authHandler.AuthMiddleware(appState.Authz.RequireCap("servers.write")(appState.RequireGatewayEnabled(serverHandler.CancelMigration)))).Methods("POST")
	// Demo flag (admin, PANEL settings.write) - mark a normal server as a public
	// read-only showcase. The StoreEnabled guard inside SetServerDemo stays: it is
	// feature availability (hosted-build-only), not authorization.
	api.HandleFunc("/admin/servers/{id:[0-9]+}/demo", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(serverHandler.SetServerDemo))).Methods("PATCH")
	// Demo account designation (admin, PANEL settings.*) - the single read-only
	// account that sees the demo servers. Same StoreEnabled guard kept in-handler.
	api.HandleFunc("/admin/settings/demo-account", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(serverHandler.GetDemoAccount))).Methods("GET")
	api.HandleFunc("/admin/settings/demo-account", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(serverHandler.SetDemoAccount))).Methods("PUT")

	// Store-linking (dylaris.com). Gated by StoreEnabled inside each handler.
	// link/start + status are panel-user authed; link/verify + verify-user are
	// service-to-service (shared key in X-Store-Key, no panel session).
	storeHandler := handlers.NewStoreHandler(appState)
	api.HandleFunc("/store/link/start", authHandler.AuthMiddleware(storeHandler.LinkStart)).Methods("POST")
	api.HandleFunc("/store/status", authHandler.AuthMiddleware(storeHandler.Status)).Methods("GET")
	// The tenant's own subscription and the two billing switches. Session-authed
	// and deliberately carrying no uuid: the account acted on is the one the
	// session authenticated, which is what keeps a shared-key channel from
	// becoming a way to read and change anyone's billing.
	api.HandleFunc("/store/account-summary", authHandler.AuthMiddleware(storeHandler.AccountSummary)).Methods("GET")
	api.HandleFunc("/store/billing-consent", authHandler.AuthMiddleware(storeHandler.SetBillingConsent)).Methods("POST")
	api.HandleFunc("/store/link/verify", authLimiter.Limit(20, handlers.LimitBody(handlers.CredentialBodyLimit, storeHandler.LinkVerify))).Methods("POST")
	api.HandleFunc("/store/verify-user", authLimiter.Limit(60, storeHandler.VerifyUser)).Methods("GET")
	api.HandleFunc("/store/usage", authLimiter.Limit(60, storeHandler.GetUsage)).Methods("GET")
	// What one unit includes, so the store can print it on the pricing page
	// without keeping a second copy of a number edited here.
	api.HandleFunc("/store/backup-defaults", authLimiter.Limit(60, storeHandler.BackupDefaults)).Methods("GET")
	api.HandleFunc("/store/provision", authLimiter.Limit(60, handlers.LimitBody(handlers.CredentialBodyLimit, storeHandler.Provision))).Methods("POST")
	// Migration progress poll - owner-or-admin, ungated (reads are harmless).
	api.HandleFunc("/servers/{id:[0-9]+}/migration-status", authHandler.AuthMiddleware(appState.Authz.RequireCap("overview.read")(serverHandler.GetMigrationStatus))).Methods("GET")
	// /gateway/route-options stays EXEMPT-authed: the user-facing route form needs
	// the hoster/custom-domain config to render for any authed user, not just admins.
	api.HandleFunc("/gateway/route-options", authHandler.AuthMiddleware(settingsHandler.GetGatewayRouteOptions)).Methods("GET")
	api.HandleFunc("/settings/servers", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(settingsHandler.GetServerSettings))).Methods("GET")
	api.HandleFunc("/settings/servers", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(settingsHandler.SaveServerSettings))).Methods("POST")
	// GET /settings/beam is EXEMPT-authed: "all authenticated users (relay address
	// + download link needed in Files tab)" per GetBeamSettings' own doc comment,
	// no admin gate in the handler. POST save IS admin-only -> PANEL settings.write.
	// Same split-template pattern as GET /settings/features above: not added to
	// requiredCaps, deferred to Task 22/23.
	api.HandleFunc("/settings/beam", authHandler.AuthMiddleware(settingsHandler.GetBeamSettings)).Methods("GET")
	api.HandleFunc("/settings/beam", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(settingsHandler.SaveBeamSettings))).Methods("POST")
	// GET /settings/routing-mode is EXEMPT-authed: "available to all authenticated
	// users" per GetRoutingMode's own doc comment (drives file-access-mode choice
	// in the UI), no admin gate in the handler. POST save IS admin-only -> PANEL
	// settings.write. Same split-template pattern; not added to requiredCaps.
	api.HandleFunc("/settings/routing-mode", authHandler.AuthMiddleware(settingsHandler.GetRoutingMode)).Methods("GET")
	api.HandleFunc("/settings/routing-mode", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(settingsHandler.SaveRoutingMode))).Methods("POST")
	api.HandleFunc("/settings/backup", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(settingsHandler.GetBackupConfig))).Methods("GET")
	api.HandleFunc("/settings/backup", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(settingsHandler.SaveBackupConfig))).Methods("POST")
	api.HandleFunc("/settings/warp-firewall", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(settingsHandler.GetWarpFirewallSettings))).Methods("GET")
	api.HandleFunc("/settings/warp-firewall", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(settingsHandler.SaveWarpFirewallSettings))).Methods("POST")
	// Shared Core file storage config (Library/ticket-attachments/ticket-backups
	// backend, path or s3) - CRUD + a write+read+delete connection probe.
	api.HandleFunc("/settings/core-storage", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(coreStorageHandler.GetConfig))).Methods("GET")
	api.HandleFunc("/settings/core-storage", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(coreStorageHandler.SaveConfig))).Methods("POST")
	api.HandleFunc("/settings/core-storage/test", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(coreStorageHandler.TestConnection))).Methods("POST")

	// DNS updater configuration. The save probes the provider before accepting,
	// so it is a write even though it stores nothing on a rejected probe; the
	// zone listing reads through the stored credential and is settings.read.
	// Fleet storage health: which Cores are currently failing their shared-
	// storage self-check.
	api.HandleFunc("/settings/storage-reach", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(coreStorageHandler.StorageReachStatus))).Methods("GET")
	// Coarse connection state of the configured core storage backends. Authed,
	// not capability-gated: it is the initial value for the storage.connection
	// .changed SSE events, which every authed panel session already receives,
	// and it carries no cause, path, bucket or endpoint.
	api.HandleFunc("/storage/connection", authHandler.AuthMiddleware(storageConnectionHandler.GetConnection)).Methods("GET")

	// --- Regions ---
	// User-facing: list of enabled regions (drives region pickers).
	api.HandleFunc("/regions", authHandler.AuthMiddleware(regionsHandler.ListRegions)).Methods("GET")
	api.HandleFunc("/me/regions", authHandler.AuthMiddleware(userRegionsHandler.GetMyRegions)).Methods("GET")
	// Admin: full CRUD incl. disabled regions.
	api.HandleFunc("/admin/regions", authHandler.AuthMiddleware(appState.Authz.RequireCap("regions.read")(regionsHandler.AdminListRegions))).Methods("GET")
	api.HandleFunc("/admin/regions", authHandler.AuthMiddleware(appState.Authz.RequireCap("regions.write")(regionsHandler.CreateRegion))).Methods("POST")
	api.HandleFunc("/admin/regions/{id}", authHandler.AuthMiddleware(appState.Authz.RequireCap("regions.write")(regionsHandler.UpdateRegion))).Methods("PATCH")
	api.HandleFunc("/admin/regions/{id}", authHandler.AuthMiddleware(appState.Authz.RequireCap("regions.delete")(regionsHandler.DeleteRegion))).Methods("DELETE")
	// Admin: per-user region assignment.
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/regions", authHandler.AuthMiddleware(appState.Authz.RequireCap("regions.read")(userRegionsHandler.GetUserRegions))).Methods("GET")
	api.HandleFunc("/admin/users/{id:[0-9a-f-]{36}}/regions", authHandler.AuthMiddleware(appState.Authz.RequireCap("regions.write")(userRegionsHandler.SetUserRegions))).Methods("PUT")

	// --- Registration + Email Verify ---
	// Public - login page polls registration-status to decide whether to show the register link.
	api.HandleFunc("/auth/registration-status", registrationHandler.RegistrationStatus).Methods("GET")
	api.HandleFunc("/auth/register", authLimiter.Limit(5, handlers.LimitBody(handlers.CredentialBodyLimit, registrationHandler.Register))).Methods("POST")
	// Public one-click read-only demo session (rate-limited). 404 when no demo account is set.
	// GET on the same path only reports whether a demo exists, so a caller can
	// decide whether to offer it without minting a session to find out.
	api.HandleFunc("/auth/demo-login", authLimiter.Limit(10, handlers.LimitBody(handlers.CredentialBodyLimit, authHandler.DemoLogin))).Methods("POST")
	api.HandleFunc("/auth/demo-login", authLimiter.Limit(30, authHandler.DemoStatus)).Methods("GET")
	api.HandleFunc("/auth/verify-email", authLimiter.Limit(20, handlers.LimitBody(handlers.CredentialBodyLimit, registrationHandler.VerifyEmail))).Methods("POST")
	// Rate-limited like /auth/forgot-password, the other endpoint that sends
	// mail on an anonymous request. The handler additionally holds a per-address
	// cooldown, because a per-IP limit does not protect a mailbox from a caller
	// who changes address.
	api.HandleFunc("/auth/resend-verification", authLimiter.Limit(5, handlers.LimitBody(handlers.CredentialBodyLimit, registrationHandler.ResendVerification))).Methods("POST")

	// --- Password reset - all public, all enumeration-safe ---
	api.HandleFunc("/auth/forgot-password", authLimiter.Limit(5, handlers.LimitBody(handlers.CredentialBodyLimit, passwordResetHandler.ForgotPassword))).Methods("POST")
	api.HandleFunc("/auth/validate-reset-token", authLimiter.Limit(20, handlers.LimitBody(handlers.CredentialBodyLimit, passwordResetHandler.ValidateResetToken))).Methods("POST")
	api.HandleFunc("/auth/reset-password", authLimiter.Limit(10, handlers.LimitBody(handlers.CredentialBodyLimit, passwordResetHandler.ResetPassword))).Methods("POST")

	// --- Security questions ---
	// Public: pool query used by /register and /reset-password to render pickers.
	api.HandleFunc("/auth/security-questions/pool", securityQuestionsHandler.GetPool).Methods("GET")
	// Authenticated: user manages their own questions in profile.
	api.HandleFunc("/me/security-questions", authHandler.AuthMiddleware(securityQuestionsHandler.GetMyQuestions)).Methods("GET")
	api.HandleFunc("/me/security-questions", authLimiter.Limit(10, handlers.LimitBody(handlers.CredentialBodyLimit, authHandler.AuthMiddleware(securityQuestionsHandler.SetMyQuestions)))).Methods("PUT")
	// Admin: pool management (PANEL settings.*).
	api.HandleFunc("/admin/settings/security-questions-pool", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(securityQuestionsHandler.GetAdminPool))).Methods("GET")
	api.HandleFunc("/admin/settings/security-questions-pool", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(securityQuestionsHandler.SetAdminPool))).Methods("PUT")

	// --- Auth policy + SMTP config (admin, PANEL settings.*) ---
	api.HandleFunc("/admin/settings/auth", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(authSettingsHandler.GetAuthPolicy))).Methods("GET")
	api.HandleFunc("/admin/settings/auth", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(authSettingsHandler.SaveAuthPolicy))).Methods("PUT")
	api.HandleFunc("/admin/settings/smtp", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(authSettingsHandler.GetSMTPConfig))).Methods("GET")
	api.HandleFunc("/admin/settings/smtp", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(authSettingsHandler.SaveSMTPConfig))).Methods("PUT")
	api.HandleFunc("/admin/settings/smtp/test", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(authSettingsHandler.TestSendSMTP))).Methods("POST")

	// --- Outgoing mail templates ---
	// settings.write on the READ routes as well, on purpose: a template body is
	// the exact wording of a password-reset mail.
	mailTemplatesHandler := handlers.NewMailTemplatesHandler(appState)
	api.HandleFunc("/admin/mail/templates", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(mailTemplatesHandler.List))).Methods("GET")
	api.HandleFunc("/admin/mail/templates/{key}", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(mailTemplatesHandler.Get))).Methods("GET")
	api.HandleFunc("/admin/mail/templates/{key}", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(mailTemplatesHandler.Save))).Methods("PUT")
	api.HandleFunc("/admin/mail/templates/{key}", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(mailTemplatesHandler.Reset))).Methods("DELETE")
	api.HandleFunc("/admin/mail/templates/{key}/preview", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(mailTemplatesHandler.Preview))).Methods("POST")
	api.HandleFunc("/admin/mail/templates/{key}/test", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(mailTemplatesHandler.TestSend))).Methods("POST")

	// --- Beam Endpoints ---
	api.HandleFunc("/beam/servers", authHandler.AuthMiddleware(beamHandler.GetBeamServers)).Methods("GET")
	api.HandleFunc("/beam/ticket", authHandler.AuthMiddleware(beamHandler.GetBeamTicket)).Methods("GET", "POST")
	api.HandleFunc("/beam/config", authHandler.AuthMiddleware(beamHandler.GetBeamConfig)).Methods("GET")
	// Rate-limited because this is the only session-less route that makes Core
	// fetch a whole file from an external host and stream it: one anonymous
	// request costs Core the full binary inbound plus the same again outbound.
	// Every other public route that costs something is limited (the solder
	// mirror, /api/share/{token}, the auth and store routes); this one was the
	// exception. 10/min per IP is far above any human clicking "download Beam"
	// - the app's own updater fetches from GitHub directly and never comes
	// through here - and caps what a single source can amplify.
	beamDownloadLimiter := handlers.NewIPRateLimiter()
	api.HandleFunc("/beam/download", beamDownloadLimiter.Limit(10, beamHandler.GetBeamDownload)).Methods("GET")
	// Caller's own Beam update-channel preference (own data, authed-exempt).

	// --- In-panel update feed (admin-only "what's new" bell) ---
	// GET /updates is admin-only, gated in-handler via IsAdmin (authed-exempt).
	// PUT /me/updates-seen clears the caller's own badge (own data).
	api.HandleFunc("/updates", authHandler.AuthMiddleware(updatesHandler.GetUpdates)).Methods("GET")
	api.HandleFunc("/me/updates-seen", authHandler.AuthMiddleware(updatesHandler.MarkUpdatesSeen)).Methods("PUT")

	// --- Backup Endpoints ---
	// backup-storages are platform-shared storage-provider configs -> PANEL
	// settings.read/write (Phase 4 Task 10 controller decision #4), not admin-only
	// in-handler anymore. /backup-jobs/{jobId} and /backup-runs/{runId} resolve
	// their server from the job/run row (not a path {id}/{uuid}), so RequireCap
	// cannot gate them at the route (Rule R5); they stay in-handler via the same
	// resolver (backup.go hasServerAccess).
	api.HandleFunc("/backup-storages", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(backupHandler.ListStorages))).Methods("GET")
	api.HandleFunc("/backup-storages", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(backupHandler.CreateStorage))).Methods("POST")
	api.HandleFunc("/backup-storages/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(backupHandler.UpdateStorage))).Methods("PATCH")
	api.HandleFunc("/backup-storages/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(backupHandler.DeleteStorage))).Methods("DELETE")
	api.HandleFunc("/backup-storages/{id:[0-9]+}/test", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(backupHandler.TestStorage))).Methods("POST")

	// A tenant's own backup storage. Owner-scoped capabilities, granted
	// deliberately rather than by any preset: connecting arbitrary S3 endpoints
	// makes Core talk to hosts the operator did not choose.
	api.HandleFunc("/me/backup-storages", authHandler.AuthMiddleware(appState.Authz.RequireCap("backupstorage.read")(backupHandler.ListOwnStorages))).Methods("GET")
	api.HandleFunc("/me/backup-storages", authHandler.AuthMiddleware(appState.Authz.RequireCap("backupstorage.write")(backupHandler.CreateOwnStorage))).Methods("POST")
	api.HandleFunc("/me/backup-storages/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("backupstorage.write")(backupHandler.UpdateOwnStorage))).Methods("PATCH")
	api.HandleFunc("/me/backup-storages/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("backupstorage.delete")(backupHandler.DeleteOwnStorage))).Methods("DELETE")
	api.HandleFunc("/me/backup-storages/{id:[0-9]+}/test", authHandler.AuthMiddleware(appState.Authz.RequireCap("backupstorage.write")(backupHandler.TestOwnStorage))).Methods("POST")

	// storage-connections: named, shared, at-rest-encrypted storage configs that
	// generalize backup-storages. Same PANEL settings.read/write gating.
	api.HandleFunc("/storage-connections", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.read")(storageConnectionsHandler.ListConnections))).Methods("GET")
	api.HandleFunc("/storage-connections", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(storageConnectionsHandler.CreateConnection))).Methods("POST")
	api.HandleFunc("/storage-connections/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(storageConnectionsHandler.UpdateConnection))).Methods("PATCH")
	api.HandleFunc("/storage-connections/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(storageConnectionsHandler.DeleteConnection))).Methods("DELETE")
	api.HandleFunc("/storage-connections/{id:[0-9]+}/test", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(storageConnectionsHandler.TestConnection))).Methods("POST")
	// Registered BEFORE nothing in particular, but note the {id} route above is
	// numeric-constrained, so "test" cannot be swallowed by it.
	api.HandleFunc("/storage-connections/test", authHandler.AuthMiddleware(appState.Authz.RequireCap("settings.write")(storageConnectionsHandler.TestDraftConnection))).Methods("POST")

	api.HandleFunc("/servers/{id:[0-9]+}/backup-jobs", authHandler.AuthMiddleware(appState.Authz.RequireCap("backups.read")(backupHandler.ListJobs))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/backup-jobs", authHandler.AuthMiddleware(appState.Authz.RequireCap("backups.create")(backupHandler.CreateJob))).Methods("POST")
	api.HandleFunc("/backup-jobs/{jobId:[0-9]+}", authHandler.AuthMiddleware(backupHandler.UpdateJob)).Methods("PATCH")
	api.HandleFunc("/backup-jobs/{jobId:[0-9]+}", authHandler.AuthMiddleware(backupHandler.DeleteJob)).Methods("DELETE")
	api.HandleFunc("/backup-jobs/{jobId:[0-9]+}/trigger", authHandler.AuthMiddleware(backupHandler.TriggerJob)).Methods("POST")

	// The platform's own backups. Every route is settings.write, the download
	// included - see the capability map above for why reading configuration and
	// downloading the installation are not the same permission.
	pbCap := appState.Authz.RequireCap("settings.write")
	api.HandleFunc("/platform-backups/jobs", authHandler.AuthMiddleware(pbCap(platformBackupHandler.ListJobs))).Methods("GET")
	api.HandleFunc("/platform-backups/jobs", authHandler.AuthMiddleware(pbCap(platformBackupHandler.CreateJob))).Methods("POST")
	api.HandleFunc("/platform-backups/jobs/{id:[0-9]+}", authHandler.AuthMiddleware(pbCap(platformBackupHandler.UpdateJob))).Methods("PATCH")
	api.HandleFunc("/platform-backups/jobs/{id:[0-9]+}", authHandler.AuthMiddleware(pbCap(platformBackupHandler.DeleteJob))).Methods("DELETE")
	api.HandleFunc("/platform-backups/jobs/{id:[0-9]+}/run", authHandler.AuthMiddleware(pbCap(platformBackupHandler.RunJob))).Methods("POST")
	api.HandleFunc("/platform-backups/jobs/{id:[0-9]+}/runs", authHandler.AuthMiddleware(pbCap(platformBackupHandler.ListRuns))).Methods("GET")
	api.HandleFunc("/platform-backups/runs/{id:[0-9]+}/download", authHandler.AuthMiddleware(pbCap(platformBackupHandler.DownloadRun))).Methods("GET")
	api.HandleFunc("/platform-backups/targets", authHandler.AuthMiddleware(pbCap(platformBackupHandler.ListTargets))).Methods("GET")
	api.HandleFunc("/platform-backups/passphrase", authHandler.AuthMiddleware(pbCap(platformBackupHandler.PassphraseStatus))).Methods("GET")
	api.HandleFunc("/platform-backups/passphrase", authHandler.AuthMiddleware(pbCap(platformBackupHandler.SetPassphrase))).Methods("PUT")

	// Reading a bundle back. Inspect is settings.write like the rest: it takes
	// a passphrase and confirms whether it opens the file, which is a thing
	// only somebody entitled to restore should be able to test.
	api.HandleFunc("/platform-backups/inspect", authHandler.AuthMiddleware(pbCap(platformBackupHandler.InspectBundle))).Methods("POST")
	api.HandleFunc("/platform-backups/restore", authHandler.AuthMiddleware(pbCap(platformBackupHandler.RestoreBundle))).Methods("POST")
	api.HandleFunc("/platform-backups/runs/{id:[0-9]+}/restore", authHandler.AuthMiddleware(pbCap(platformBackupHandler.RestoreRun))).Methods("POST")
	api.HandleFunc("/backup-jobs/{jobId:[0-9]+}/runs", authHandler.AuthMiddleware(backupHandler.ListRuns)).Methods("GET")
	api.HandleFunc("/backup-runs/{runId:[0-9]+}/download", authHandler.AuthMiddleware(backupHandler.DownloadRun)).Methods("GET")
	api.HandleFunc("/backup-runs/{runId:[0-9]+}/restore", authHandler.AuthMiddleware(backupHandler.RestoreRun)).Methods("POST")
	api.HandleFunc("/servers/{id:[0-9]+}/backup-restores", authHandler.AuthMiddleware(appState.Authz.RequireCap("backups.read")(backupHandler.ListRestores))).Methods("GET")
	api.HandleFunc("/servers/{id:[0-9]+}/backup-usage", authHandler.AuthMiddleware(appState.Authz.RequireCap("backups.read")(backupHandler.BackupUsage))).Methods("GET")
	api.HandleFunc("/backup-runs/{runId:[0-9]+}", authHandler.AuthMiddleware(backupHandler.DeleteRun)).Methods("DELETE")
	api.HandleFunc("/tools/beam", beamToolsRedirect).Methods("GET")

	return r, &routeExtras{
		settingsHandler: settingsHandler,
		warpService:     warpService,
		proxyHandler:    proxyHandler,
	}
}

// notFoundHandler decides what an unmatched request gets.
//
// Anything the router did not claim is a panel URL, because the panel is served
// from this same process now: /servers/42 matches no route here and must reach
// the bundle. /api is the exception and keeps its JSON 404 - a client that asked
// for an endpoint wants to be told the endpoint is missing, not handed an HTML
// page to parse, and an SDK that got HTML back would report a parse error
// instead of a 404.
//
// A nil panel (tests, and any build that mounts no bundle) falls back to the
// JSON answer for everything, which is what this did before the panel moved in.
func notFoundHandler(panel http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if panel != nil && !isAPIPath(req.URL.Path) {
			panel.ServeHTTP(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "Core Error: Endpoint not found (" + req.URL.Path + ")",
		})
	}
}

// isAPIPath reports whether a path belongs to the API surface rather than the
// panel. Bare "/api" counts: it is a request for the API root, not for a page.
func isAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/")
}
