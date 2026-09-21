package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"dylaris-core/database"
	"dylaris-core/services"
	"dylaris-core/services/redisacl"
	"dylaris-pkg/errlog"
)

// HealthHandler powers the admin Status page. It runs a set of on-demand
// component checks (DB, metrics extension, Redis, nodes, gateway, storage) and
// reports each as up / degraded / down / disabled with a human-readable reason.
// It does no background work of its own.
//
// Most checks run per request behind healthCheckTimeout so a hung dependency
// cannot block the response. Two do not, and saying "every check" here was
// wrong: nodesComponent calls Store.ListNodes, which takes no context at all,
// so a wedged database blocks it for as long as the driver allows; and
// storageComponent takes no timeout because it needs none, reading only
// atomics that the storage layer's own background watchers maintain.
type HealthHandler struct {
	state *AppState
}

func NewHealthHandler(state *AppState) *HealthHandler {
	return &HealthHandler{state: state}
}

// healthComponent is one row on the status page.
type healthComponent struct {
	Key    string       `json:"key"`
	Name   string       `json:"name"`
	Status string       `json:"status"`           // up | degraded | down | disabled
	Detail string       `json:"detail,omitempty"` // short one-line summary
	Reason string       `json:"reason,omitempty"` // why it's degraded/down
	Items  []healthItem `json:"items,omitempty"`  // sub-rows (per node, per edge, ...)

	// Cause is a machine-readable class for the failure, where the component
	// has one. Status alone cannot carry this: two components can both be
	// "down" for reasons that need completely different actions, and the panel
	// has no way to tell them apart from a free-text reason. Only the Redis
	// component sets it today (database.RedisFailure.Slug); an empty value
	// means the component has no classification and the panel falls back to
	// showing Detail and Reason alone.
	Cause string `json:"cause,omitempty"`
}

// healthItem is a sub-row (e.g. a single node) under a component.
type healthItem struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type healthReport struct {
	Overall    string            `json:"overall"` // healthy | degraded | down
	Components []healthComponent `json:"components"`
	CheckedAt  string            `json:"checkedAt"`
}

const healthCheckTimeout = 3 * time.Second

// GetStatus GET /api/admin/health
//
// Aggregated platform health, PANEL settings.read (RequireCap at the route).
// Each component is checked independently; a failing optional component
// degrades the overall status but only a DB/Redis outage marks the platform
// "down".
func (h *HealthHandler) GetStatus(w http.ResponseWriter, r *http.Request) {
	components := []healthComponent{}

	dbUp := false
	components = append(components, h.databaseComponent(r.Context(), &dbUp))
	components = append(components, h.metricsComponent(r.Context(), dbUp))
	redisUp := false
	components = append(components, h.redisComponent(r.Context(), &redisUp))
	components = append(components, h.nodesComponent())
	components = append(components, h.gatewayComponent(r.Context()))
	components = append(components, h.storageComponent())
	components = append(components, h.redisACLComponent(r.Context(), redisUp))
	components = append(components, h.storefrontComponent(r.Context()))

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"health": healthReport{
			Overall:    overallStatus(components),
			Components: components,
			CheckedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	})
}

// overallStatus rolls components up: a DB/Redis outage is "down" (Core can't
// function); any other down/degraded component is "degraded"; "disabled" is
// neutral and never lowers the overall status.
func overallStatus(components []healthComponent) string {
	overall := "healthy"
	for _, c := range components {
		switch {
		case c.Status == "down" && (c.Key == "database" || c.Key == "redis"):
			overall = "down"
		case c.Status == "down" || c.Status == "degraded":
			if overall == "healthy" {
				overall = "degraded"
			}
		}
	}
	return overall
}

func (h *HealthHandler) databaseComponent(ctx context.Context, up *bool) healthComponent {
	comp := healthComponent{Key: "database", Name: "Database (PostgreSQL)"}
	cctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	if err := h.state.Store.Ping(cctx); err != nil {
		comp.Status = "down"
		comp.Detail = "Connection failed"
		comp.Reason = err.Error()
		return comp
	}
	*up = true
	comp.Status = "up"
	comp.Detail = "Connection alive"
	return comp
}

// metricsComponent reports whether time-series history is fully available. This
// is the "DB green, but history features limited because TimescaleDB is
// missing" signal: it never marks the platform down, only degraded.
func (h *HealthHandler) metricsComponent(ctx context.Context, dbUp bool) healthComponent {
	comp := healthComponent{Key: "metrics", Name: "Metrics history (time-series)"}
	if !dbUp {
		comp.Status = "degraded"
		comp.Detail = "Unknown"
		comp.Reason = "database unreachable, extension state cannot be checked"
		return comp
	}
	cctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	enabled, err := h.state.Store.TimescaleEnabled(cctx)
	switch {
	case err != nil:
		comp.Status = "degraded"
		comp.Detail = "Unknown"
		comp.Reason = "extension check failed: " + err.Error()
	case enabled:
		comp.Status = "up"
		comp.Detail = "TimescaleDB hypertable active (native 24h retention)"
	case h.state.DBType == "postgres":
		// Intended plain-PostgreSQL deployment (DB_TYPE=postgres): server_stats
		// is a regular table and retention is enforced by the hourly sweep. This
		// is a supported configuration, not a fault.
		comp.Status = "up"
		comp.Detail = "Plain PostgreSQL (server_stats table, 24h retention via hourly sweep)"
	default:
		// DB_TYPE=timescaledb was requested but the extension isn't loaded — a
		// real misconfiguration. The platform still works (plain table + sweep),
		// so it's degraded, not down.
		comp.Status = "degraded"
		comp.Detail = "TimescaleDB requested but extension not installed"
		comp.Reason = "DB_TYPE=timescaledb but the timescaledb extension is not loaded. server_stats falls back to a plain table with the hourly retention sweep, so live + recent stats work; only TimescaleDB's hypertable optimizations are missing. Set DB_TYPE=postgres to silence this, or load the extension."
	}
	return comp
}

// redisComponent pings Redis and reports WHY it failed, not just that it did.
//
// A single PING is enough to separate the classes: verified against valkey 8, a
// user without the +ping permission gets NOPERM from PING itself rather than a
// generic failure, so no second probe command is needed to detect an ACL gap.
//
// The reply is still carried verbatim in Reason. This route requires
// settings.read, so the server's own message - which names the command a NOPERM
// refers to - is exactly what an operator needs to fix it.
func (h *HealthHandler) redisComponent(ctx context.Context, up *bool) healthComponent {
	comp := healthComponent{Key: "redis", Name: "Redis"}
	cctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	err := h.state.Redis.Ping(cctx).Err()
	failure := database.ClassifyRedisError(err)
	if failure != database.RedisOK {
		comp.Status = "down"
		comp.Detail = failure.Summary()
		comp.Cause = failure.Slug()
		comp.Reason = err.Error()
		return comp
	}

	*up = true
	comp.Status = "up"
	comp.Detail = failure.Summary()
	return comp
}

// nodesComponent reports node fleet health from the persisted node.Status,
// which the discovery service keeps current from heartbeats.
// nodeSelfReportedFailureMaxAge bounds how old a node's own report may be
// before the status page stops repeating it.
//
// A stream entry outlives the condition it describes: the node writes "pin
// mismatch", the operator fixes it, and the node then cannot write a recovery
// line because a node that recovers is online and no longer has a row here to
// annotate. Without a bound, one solved problem would be reported next to that
// node forever. An hour is long enough to still be showing the cause while
// someone investigates the outage it explains.
const nodeSelfReportedFailureMaxAge = time.Hour

// nodeSelfReportedFailure returns the newest reason this node gave for being
// unable to reach Core, or "" when there is none, it is stale, or Redis is
// unavailable.
//
// Best-effort in every direction: this decorates a status row, and a Redis
// hiccup while rendering it must not turn the whole health check into an error.
func (h *HealthHandler) nodeSelfReportedFailure(nodeToken string) string {
	if h.state.Redis == nil || nodeToken == "" {
		return ""
	}
	entries, err := errlog.ReadEntries(h.state.Redis, "dylaris:errors:node:"+nodeToken, 1)
	if err != nil || len(entries) == 0 {
		return ""
	}
	e := entries[0]
	// An Info entry is the node saying it recovered, so it is not a failure to
	// report - and it is the newest line precisely when everything is fine.
	if e.Level != "ERROR" && e.Level != "WARN" {
		return ""
	}
	if ts, perr := time.Parse(time.RFC3339, e.Timestamp); perr == nil {
		if time.Since(ts) > nodeSelfReportedFailureMaxAge {
			return ""
		}
	}
	return e.Message
}

// storefrontComponent reports whether the dylaris.com integration actually
// works, which the panel alone can never show.
//
// Every failure of the link-status call - an unreachable storefront, a
// STORE_SHARED_KEY that does not match, a 500 - used to come back as plain
// "not linked", the same answer a perfectly healthy integration gives for a
// customer who simply has not connected their account. So a broken service-to-
// service trust looked exactly like normal operation, on the one path that
// carries money.
//
// The probe uses the all-zero UUID: it is a real request over the real key, and
// the answer for a UUID nobody owns is a valid "not linked" rather than an
// error. What is under test is the CHANNEL, not any particular account.
func (h *HealthHandler) storefrontComponent(ctx context.Context) healthComponent {
	comp := healthComponent{Key: "storefront", Name: "Storefront (billing)"}
	if !h.state.StoreEnabled {
		comp.Status = "disabled"
		comp.Detail = "STORE_URL / STORE_SHARED_KEY not set - open-core build, no billing"
		return comp
	}

	cctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	sh := NewStoreHandler(h.state)
	if _, _, err := sh.probeLinkStatus(cctx, healthProbeUUID); err != nil {
		comp.Status = "down"
		comp.Cause = "storefront_unreachable"
		comp.Detail = "Cannot talk to " + h.state.StoreURL
		comp.Reason = err.Error() + ". Purchases cannot provision and the panel shows every account as unlinked while this stands."
		return comp
	}

	// The second channel, and it has to be asked separately rather than assumed
	// from the first. Core reaches the storefront on THREE paths - link-status,
	// account-summary and billing-consent - and they are not one endpoint: on
	// production only link-status was routed, so this component read "up" while
	// every customer's billing page said the store could not be reached and
	// nobody could consent to metered billing. One probe per reachable surface
	// is what makes that visible here instead of in a support ticket.
	//
	// The all-zero UUID belongs to nobody and the store answers it with a
	// perfectly valid "not linked", so this tests the CHANNEL and touches no
	// account. billing-consent is not probed: it is a write, and there is no
	// request to it that changes nothing.
	if _, err := h.state.storeAccountSummary(cctx, healthProbeUUID); err != nil {
		comp.Status = "down"
		comp.Cause = "storefront_unreachable"
		comp.Detail = "Account details are not reachable at " + h.state.StoreURL
		comp.Reason = err.Error() + ". Link status works, so this is one route rather than the storefront being down: the panel's billing page shows every tenant 'the store could not be reached' and metered billing cannot be switched on."
		return comp
	}

	comp.Status = "up"
	comp.Detail = h.state.StoreURL + " answers and accepts our key"
	return comp
}

// healthProbeUUID is a UUID no account can have, used for read probes against
// the storefront. It gets a real answer over the real key without naming a real
// tenant.
const healthProbeUUID = "00000000-0000-0000-0000-000000000000"

// redisACLComponent reports whether every scoped Redis credential Core is
// supposed to have provisioned actually exists in Valkey.
//
// The reconciler sweeps the other direction - it deletes users no node claims -
// and nothing checked for the reverse. A MISSING user is the failure with real
// consequences and no symptom anywhere: the log-shipper for one server gets
// NOPERM, buffers its output and retries forever, so that server's console goes
// blank in the panel and panel-typed commands stop arriving. Java keeps running
// and the container stays Up, so every other signal says healthy. The shipper
// says so clearly, but only on the node's own container stderr - which on a BYON
// machine belongs to the customer, not to whoever is looking at the panel.
//
// There is one shipper user per server, so this also answers "is per-server
// isolation actually provisioned" rather than only "is it implemented".
func (h *HealthHandler) redisACLComponent(ctx context.Context, redisUp bool) healthComponent {
	comp := healthComponent{Key: "redis_acl", Name: "Redis credentials"}
	if !redisUp || h.state.Redis == nil {
		comp.Status = "disabled"
		comp.Detail = "not checked while Redis is unreachable"
		return comp
	}

	nodes, err := h.state.Store.ListNodes()
	if err != nil {
		comp.Status = "degraded"
		comp.Detail = "Could not list nodes"
		comp.Reason = err.Error()
		return comp
	}
	if len(nodes) == 0 {
		comp.Status = "disabled"
		comp.Detail = "no nodes registered"
		return comp
	}

	// Same authoritative shape the provisioner and the prune sweep use: a
	// partial server list would invent missing users out of a lookup failure.
	serversByToken := make(map[string][]string, len(nodes))
	for i := range nodes {
		servers, lerr := h.state.Store.ListServersByNode(nodes[i].ID)
		if lerr != nil {
			comp.Status = "degraded"
			comp.Detail = "Could not list servers"
			comp.Reason = fmt.Sprintf("node %s: %v", nodes[i].Name, lerr)
			return comp
		}
		uuids := make([]string, 0, len(servers))
		for _, s := range servers {
			uuids = append(uuids, s.UUID)
		}
		serversByToken[nodes[i].Token] = uuids
	}
	expected := redisacl.ExpectedNodeACLUsers(serversByToken)

	cctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	have, err := h.state.Redis.Do(cctx, "ACL", "USERS").StringSlice()
	if err != nil {
		comp.Status = "degraded"
		comp.Detail = "Could not read the Redis ACL user list"
		// Verbatim: this route is settings.read, and NOPERM names the command.
		comp.Reason = err.Error()
		return comp
	}
	present := make(map[string]bool, len(have))
	for _, u := range have {
		present[u] = true
	}

	missing := make([]string, 0)
	for want := range expected {
		if !present[want] {
			missing = append(missing, want)
		}
	}
	sort.Strings(missing) // map iteration order would reshuffle the row every poll

	if len(missing) == 0 {
		comp.Status = "up"
		comp.Detail = fmt.Sprintf("%d scoped user(s) provisioned", len(expected))
		return comp
	}
	comp.Status = "degraded"
	comp.Cause = "acl_users_missing"
	comp.Detail = fmt.Sprintf("%d of %d scoped user(s) missing", len(missing), len(expected))
	comp.Reason = "these credentials should exist in Valkey and do not, so whatever holds them gets NOPERM and retries silently: " +
		strings.Join(missing, ", ") +
		". A node reconnect or a server placement re-provisions them; if they stay missing, check that Valkey loaded its aclfile."
	for _, m := range missing {
		comp.Items = append(comp.Items, healthItem{Name: m, Status: "down", Detail: "not provisioned"})
	}
	return comp
}

// nodesComponent covers OUR nodes only: the cluster and the external machines
// the operator registered. A customer's BYON node is excluded, and that is the
// point of this page rather than an omission - it reports whether THIS platform
// is healthy, and a tenant who unplugs their own hardware has not made it
// unhealthy. Counting theirs turned the status amber for something no operator
// here can act on, which is how a status page stops being read.
//
// Their machines are not invisible: Infrastructure tracks them with totals and
// an online count, without a severity attached.
func (h *HealthHandler) nodesComponent() healthComponent {
	comp := healthComponent{Key: "nodes", Name: "Nodes"}
	all, err := h.state.Store.ListNodes()
	if err != nil {
		comp.Status = "down"
		comp.Detail = "Could not list nodes"
		comp.Reason = err.Error()
		return comp
	}
	nodes, customer := services.SplitNodes(all)
	if len(nodes) == 0 {
		comp.Status = "degraded"
		comp.Detail = "No nodes registered"
		comp.Reason = "no compute nodes are registered; servers cannot be placed until a node joins"
		if len(customer) > 0 {
			// Worth saying, because the operator can SEE nodes in the panel and
			// would otherwise read this as simply wrong.
			comp.Reason += fmt.Sprintf(" (%d customer node(s) are not counted here)", len(customer))
		}
		return comp
	}

	online := 0
	items := make([]healthItem, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		item := healthItem{Name: n.Name}
		// The node's own account of its control channel. Core cannot derive this:
		// the channel it would learn from is the one that failed. The node writes
		// it to its Redis error stream over separate credentials, which is the
		// only path that survives a broken control plane.
		why := h.nodeSelfReportedFailure(n.Token)
		switch {
		case n.Status == "online" && why != "":
			// The case that made this necessary. `status` is driven by the node's
			// Redis heartbeat, and the heartbeat does not go over gRPC - so a node
			// whose control channel is refused (a certificate pin that does not
			// match, a rejected proof) keeps reporting itself online. Measured on
			// a live node with TLS disabled against a TLS Core: the panel said
			// "up", every server on it kept running, and every operation that
			// rides the mesh - console, file transfer, RCON, the tab proxy - failed
			// with "Node not connected" and no explanation anywhere.
			//
			// It does NOT count toward `online`. Counting it is the lie.
			item.Status = "degraded"
			item.Detail = n.Region + " - heartbeat is fine, but the control channel to Core is down: " + why
		case n.Status == "online":
			online++
			item.Status = "up"
			item.Detail = n.Region
		default:
			item.Status = "down"
			item.Detail = "offline"
			if n.LastSeenAt != nil {
				item.Detail = "offline, last seen " + n.LastSeenAt.UTC().Format(time.RFC3339)
			}
			if why != "" {
				item.Detail += " - " + why
			}
		}
		items = append(items, item)
	}
	comp.Items = items

	switch {
	case online == len(nodes):
		comp.Status = "up"
		comp.Detail = fmt.Sprintf("%d/%d online", online, len(nodes))
	case online == 0:
		comp.Status = "down"
		comp.Detail = fmt.Sprintf("0/%d online", len(nodes))
		comp.Reason = "all registered nodes are offline"
	default:
		comp.Status = "degraded"
		comp.Detail = fmt.Sprintf("%d/%d online", online, len(nodes))
		// "N offline" was wrong once a node could be reachable and still not
		// usable: a heartbeating node with a dead control channel is neither
		// online nor offline, and calling it offline sends whoever reads this to
		// check whether the machine is powered on.
		comp.Reason = fmt.Sprintf("%d node(s) not fully connected - see the rows below", len(nodes)-online)
	}
	return comp
}

// gatewayComponent reports gateway liveness. When routing is ip_port the
// subsystem is intentionally dormant ("disabled"), which is not a fault.
func (h *HealthHandler) gatewayComponent(ctx context.Context) healthComponent {
	comp := healthComponent{Key: "gateway", Name: "Gateway"}
	if !h.state.gatewayEnabled() {
		comp.Status = "disabled"
		comp.Detail = "Routing mode is ip_port"
		comp.Reason = "gateway subsystem is dormant; servers are reached via direct host ports"
		return comp
	}

	cctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	edges := services.GetEdgesFromRedis(cctx, h.state.Redis)
	// Links the OPERATOR runs. A customer's BYON or route-only link is theirs to
	// keep up, and it used to turn this component amber - "some gateway
	// components are offline" for a box in somebody's flat.
	split := services.LoadLinkOwnership(h.state.Store).SplitLinks(
		services.GetLinksFromRedis(cctx, h.state.Redis))
	links := split.Ours
	routes := services.CountRoutesFromRedis(cctx, h.state.Redis)

	onlineEdges := 0
	for _, e := range edges {
		if e.Status == "" || e.Status == "online" {
			onlineEdges++
		}
	}
	onlineLinks := split.OursOnline

	status, detail, reason, linkStatus := gatewayVerdict(onlineEdges, len(edges), onlineLinks, len(links), routes)
	comp.Items = []healthItem{
		{Name: "Edges", Status: countStatus(onlineEdges, len(edges)), Detail: fmt.Sprintf("%d/%d online", onlineEdges, len(edges))},
		{Name: "Links", Status: linkStatus, Detail: fmt.Sprintf("%d online", onlineLinks)},
		{Name: "Routes", Status: "up", Detail: fmt.Sprintf("%d active", routes)},
	}
	comp.Status, comp.Detail, comp.Reason = status, detail, reason
	return comp
}

// gatewayVerdict turns the four counts into the component's verdict. Separate
// from the component so it can be read and tested without Redis.
//
// Links are COUNTED, not graded, and that is the whole point of this function.
//
// A link registration (link:<token>) is written with a 24 h TTL and a running
// link refreshes its own; presence (online_link:<token>) lives 15 seconds. So a
// token that stops being used - a link redeployed under a new one, a kit
// revoked, a customer's box switched off - stays in the registration list for
// up to a day after the process behind it is gone. Reading that as "one of our
// links is DOWN" is what put production on "degraded" for a day after a link
// update, with nothing wrong and nothing anybody could do about it. An
// operator who learns the page cries wolf stops reading the page.
//
// Core cannot know how many links OUGHT to be running: nothing declares it, and
// the Hub that provisions the in-cluster ones shares no database with Core.
// What it can say is whether any link is carrying traffic at all while routes
// exist, which is the state that genuinely takes those servers off the air.
func gatewayVerdict(onlineEdges, totalEdges, onlineLinks, totalLinks int, routes int64) (status, detail, reason, linkStatus string) {
	linkStatus = "up"
	if totalLinks > 0 && onlineLinks == 0 && routes > 0 {
		linkStatus = "down"
	}
	switch {
	case onlineEdges == 0:
		// Gateway routing is on but no edge is reachable: every gateway-routed
		// server is currently unreachable. This is the one gateway state that
		// is a real outage rather than a partial degrade.
		return "down", "No edges online",
			"gateway routing is enabled but no edge is reachable; gateway-routed servers are unreachable",
			linkStatus
	case linkStatus == "down":
		return "down", fmt.Sprintf("No links online, %d route(s) configured", routes),
			"routes are configured but no link is online to carry them; those servers are unreachable",
			linkStatus
	case onlineEdges < totalEdges:
		return "degraded", fmt.Sprintf("%d/%d edges online, %d link(s)", onlineEdges, totalEdges, onlineLinks),
			"some gateway components are offline",
			linkStatus
	default:
		return "up", fmt.Sprintf("%d edge(s), %d link(s), %d route(s)", onlineEdges, onlineLinks, routes),
			"", linkStatus
	}
}

// storageComponent reports the connection state of the configured core-storage
// backend: the host-path watchdog's verdict, or the s3 wrapper's.
//
// It touches neither the filesystem nor the network. That is a requirement,
// not an optimisation. Statting a wedged network mount here would block inside
// a syscall no context can interrupt, so the status page would hang on exactly
// the outage it exists to report, and a timeout around it would abandon a
// goroutine pinned to an OS thread on every request. The reading it hands back
// can therefore be up to one probe interval stale.
//
// It is NOT free of I/O altogether, and an earlier version of this comment
// claiming it "reads atomics only" was wrong: LoadCoreStorageConfig below
// issues several settings reads, and Store.GetSetting takes no context, so a
// wedged Postgres blocks this component for as long as the driver allows. That
// is the same hole nodesComponent has and the handler comment above names.
// Reading the config is unavoidable here - which backend is configured decides
// which of the two mechanisms to report - so the honest statement is that this
// component is bounded against a storage outage and not against a database one.
//
// "up" means no evidence of a problem, NOT verified reachable. Neither backend
// proves reachability: the watchdog's verdict lags its probe, and s3 is only
// observed through calls something else made.
//
// A storage outage is degraded, never down. "down" is reserved for the
// dependencies Core cannot run without at all (see overallStatus); with storage
// unreachable the panel still serves, the API still answers and running servers
// keep running. Only the features that touch stored files fail.
//
// Unlike the SSE payload and GET /api/storage/connection, this may carry the
// cause: the route behind it requires the settings.read capability, so the host
// path, the errno and the endpoint are visible to an operator rather than to
// every authenticated user.
func (h *HealthHandler) storageComponent() healthComponent {
	comp := healthComponent{Key: "storage", Name: "Core storage"}
	cfg, err := h.state.effectiveCoreStorageConfig()
	if err != nil {
		// A selected storage connection that no longer resolves: the backend is
		// intended but unusable. Report it rather than describing the stale
		// inline config, which would mislead the operator into fixing the wrong
		// thing.
		comp.Status = "down"
		comp.Detail = "Selected storage connection is unavailable"
		comp.Reason = err.Error()
		return comp
	}

	switch cfg.Backend {
	case "s3":
		reconnecting, since, lastErr := h.state.StorageS3.State()
		if !reconnecting {
			comp.Status = "up"
			comp.Detail = "S3 backend, no connection failure reported"
			return comp
		}
		comp.Status = "degraded"
		comp.Detail = "S3 backend reconnecting since " + since.UTC().Format(time.RFC3339)
		comp.Reason = "the object store stopped answering; replayable operations are paused and retried, uploads wait before they start, and both fail once the retry budget runs out"
		if lastErr != nil {
			comp.Reason += ": " + lastErr.Error()
		}
		return comp

	case "path", "local":
		healthy, cause := h.state.StorageGate.Healthy()
		if healthy {
			comp.Status = "up"
			comp.Detail = "Host path, no connection failure observed"
			return comp
		}
		comp.Status = "degraded"
		comp.Detail = "Host path not answering"
		comp.Reason = "the configured storage path stopped answering, so requests that touch stored files are failed immediately rather than piling up against it"
		if cause != nil {
			comp.Reason += ": " + cause.Error()
		}
		return comp

	default:
		// Neither mechanism applies because no backend is configured yet. That
		// is the fresh-install state, not a fault.
		comp.Status = "disabled"
		comp.Detail = "Not configured"
		comp.Reason = "core file storage has not been configured; features that store files are unavailable until it is"
		return comp
	}
}

// countStatus maps an online/total pair to a component status. Zero total means
// nothing of that kind is registered yet, which is neutral ("disabled") rather
// than a fault.
func countStatus(online, total int) string {
	switch {
	case total == 0:
		return "disabled"
	case online == 0:
		return "down"
	case online < total:
		return "degraded"
	default:
		return "up"
	}
}

// Healthz GET /healthz
//
// Unauthenticated infra readiness probe (Docker/Swarm HEALTHCHECK, load
// balancers). Pings only DB + Redis - the two dependencies Core cannot run
// without - skipping the heavier admin checks (nodes, gateway, metrics
// extension) that GetStatus (/api/admin/health) reports. Registered outside
// AuthMiddleware AND outside the /api setup-lock + maintenance middleware
// (see core/main.go), so it keeps answering during Fresh-Install/Lost-Admin
// setup states and while maintenance mode is active.
// Storage is REPORTED here but deliberately does NOT affect the status code.
// This endpoint is what Docker and Swarm consume to decide whether to kill and
// restart the container, and restarting Core cannot make an unreachable NAS or
// object store reachable. Gating on it would turn a storage blip into a restart
// loop that takes the panel and every running server's supervision down with
// it, converting a partial outage into a total one. The flag is there so an
// operator's own monitoring can see the state; the orchestrator ignores it.
//
// The same reasoning is applied to Redis, per class rather than wholesale. An
// UNREACHABLE Redis still fails this check: it can clear on its own, and a
// reschedule may land somewhere it is reachable. A rejected credential or a
// missing ACL permission does NOT, because a restart cannot repair either, and
// gating on them would restart-loop Core over a configuration mistake - taking
// down the panel an operator needs in order to fix it. Both are still reported
// in the body.
//
// The flag is coarse on purpose. This route is unauthenticated, so it must not
// carry the path, the bucket or the errno the admin endpoint reports. It is
// read from atomics only, adding no query to a path that runs on every health
// check and cannot be allowed to hang.
func (h *HealthHandler) Healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()

	dbErr := h.state.Store.Ping(ctx)
	redisErr := h.state.Redis.Ping(ctx).Err()

	// A rejected credential or a missing ACL permission must NOT fail this
	// check, for the same reason storage does not gate it: the orchestrator
	// responds to a failure by killing and restarting the container, and a
	// restart cannot repair a WRONGPASS or a NOPERM. It would restart-loop
	// instead, taking down the very panel an operator needs to fix the ACL and
	// turning a configuration mistake into a total outage. An unreachable Redis
	// still fails the check: that one can clear on its own, and a reschedule
	// may genuinely land somewhere it is reachable.
	//
	// Reported either way in the body, so an operator's own monitoring sees the
	// state even when the orchestrator is told to leave the container alone.
	redisFailure := database.ClassifyRedisError(redisErr)
	redisGates := redisFailure != database.RedisOK && !redisFailure.NeedsOperator()

	// Checked without consulting which backend is configured, which would cost
	// a settings read. The idle mechanism reports ok: the watchdog is stopped
	// when the backend is not a host path, and the s3 state is reset when it is
	// not s3, so at most one of these can be false at a time.
	storageOK := true
	if healthy, _ := h.state.StorageGate.Healthy(); !healthy {
		storageOK = false
	}
	if reconnecting, _, _ := h.state.StorageS3.State(); reconnecting {
		storageOK = false
	}

	w.Header().Set("Content-Type", "application/json")
	if dbErr != nil || redisGates {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "not ready",
			"db":      dbErr == nil,
			"redis":   redisErr == nil,
			"storage": storageOK,
		})
		return
	}

	// Reaching here with a Redis failure means it was one the orchestrator is
	// deliberately not asked to act on, so the body must still say so rather
	// than reporting a flat "ready" - the healthchecks discard it, but an
	// operator reading this endpoint by hand would otherwise be told everything
	// is fine while Redis is rejecting every command.
	status := "ready"
	if redisErr != nil {
		status = "degraded"
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  status,
		"db":      true,
		"redis":   redisErr == nil,
		"storage": storageOK,
	})
}
