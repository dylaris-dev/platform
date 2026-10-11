package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

type InfrastructureHandler struct {
	state *AppState
}

func NewInfrastructureHandler(state *AppState) *InfrastructureHandler {
	return &InfrastructureHandler{state: state}
}

// GetOverview GET /api/infrastructure/overview - one payload for the
// infrastructure page: edges, links and nodes with their live heartbeat stats,
// plus route, tunnel and online counts and the recent service errors.
func (h *InfrastructureHandler) GetOverview(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()

	// Edges with full stats (auto-discovered from Redis)
	edges := services.GetEdgesFromRedis(ctx, h.state.Redis)

	// Links with online status
	links := services.GetLinksFromRedis(ctx, h.state.Redis)

	routeCount := services.CountRoutesFromRedis(ctx, h.state.Redis)

	// Nodes with server count + live heartbeat stats
	nodes, err := h.state.Store.ListNodes()
	if err != nil {
		nodes = nil
	}

	// Shared with the metrics collector, which used to read these fields off an
	// unenriched ListNodes result and record the zero value as a measurement.
	services.EnrichNodesWithLiveStats(ctx, h.state.Store, h.state.Redis, nodes)
	setNodesLinkOnline(ctx, h.state, nodes)

	// Aggregate stats. Links are split by who RUNS them: a customer's BYON or
	// route-only link going down is not an outage of this platform, so the
	// operator's own count must not include it. The customer figures are
	// reported beside it, as a count and nothing more - see customerSummary.
	split := services.LoadLinkOwnership(h.state.Store).SplitLinks(links)
	onlineLinks := split.OursOnline

	onlineEdges := 0
	for _, e := range edges {
		if e.Status == "online" {
			onlineEdges++
		}
	}

	// Service errors from Redis Streams
	errors := services.GetAllServiceErrorsFromRedis(h.state.Redis, 20)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"edges":   edges,
		// Ours only. The customer links are counted in `customers` and are
		// deliberately not listed: a link is identified by its TOKEN, which is
		// a credential, and there is nothing an operator can do with somebody
		// else's anyway.
		"links":       split.Ours,
		"nodes":       nodes,
		"routeCount":  routeCount,
		"onlineLinks": onlineLinks,
		"onlineEdges": onlineEdges,
		"customers":   h.customerSummary(ctx, split),
		"fleet":       h.fleet(nodes),
		"errors":      errors,
	})
}

// kindCounts is how many nodes of one kind exist and how many are online.
type kindCounts struct {
	Total  int `json:"total"`
	Online int `json:"online"`
}

// fleetSummary is the at-a-glance census for Infrastructure > Statistics: what
// kind of machines exist and how many distinct customers run them. Counts only;
// no owner ids, names or tokens leave this function.
type fleetSummary struct {
	Nodes struct {
		Platform kindCounts `json:"platform"`
		External kindCounts `json:"external"`
		BYON     kindCounts `json:"byon"`
	} `json:"nodes"`
	// Distinct customer accounts per kind. Route-only = owns at least one
	// route-only route; Total counts an account that is both only once.
	Customers struct {
		BYON      int `json:"byon"`
		RouteOnly int `json:"routeOnly"`
		Total     int `json:"total"`
	} `json:"customers"`
	RouteOnlyRoutes int `json:"routeOnlyRoutes"`
	// Users is every registered account, nil when it could not be counted.
	Users *int `json:"users"`
}

func (h *InfrastructureHandler) fleet(nodes []models.Node) fleetSummary {
	routes, err := h.state.Store.ListCoreLinkRoutes()
	if err != nil {
		routes = nil
	}
	var users *int
	if n, err := h.state.Store.CountUsers(); err == nil {
		users = &n
	}
	return buildFleetSummary(nodes, routes, users)
}

func buildFleetSummary(nodes []models.Node, routes []store.CoreLinkRoute, users *int) fleetSummary {
	var f fleetSummary
	f.Users = users
	byon, routeOnly, all := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i := range nodes {
		n := &nodes[i]
		var c *kindCounts
		switch n.Kind() {
		case models.NodeKindBYON:
			c = &f.Nodes.BYON
			byon[*n.OwnerID], all[*n.OwnerID] = true, true
		case models.NodeKindExternal:
			c = &f.Nodes.External
		default:
			c = &f.Nodes.Platform
		}
		c.Total++
		if n.Status == "online" {
			c.Online++
		}
	}
	for _, r := range routes {
		if r.OwnerID != "" {
			routeOnly[r.OwnerID], all[r.OwnerID] = true, true
		}
	}
	f.Customers.BYON, f.Customers.RouteOnly, f.Customers.Total = len(byon), len(routeOnly), len(all)
	f.RouteOnlyRoutes = len(routes)
	return f
}

// GetPlayersOnline GET /api/infrastructure/players-online - players on every
// server right now, as each server's own list ping reports it. Its own route
// rather than a field on the overview because the panel polls it every few
// seconds, and the overview is a dozen Redis reads it does not need that often.
func (h *InfrastructureHandler) GetPlayersOnline(w http.ResponseWriter, r *http.Request) {
	servers, err := h.state.Store.ListServers("")
	if err != nil {
		sendJSONError(w, "Could not list servers", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	n, per, err := services.PlayersOnlineByServer(r.Context(), h.state.Redis, servers, now)
	if err != nil {
		sendJSONError(w, "Could not read live server stats", http.StatusServiceUnavailable)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"playersOnline": n,
		"servers":       per,
		"at":            now.UTC(),
	})
}

// customerCounts is a total and how many of them are up. Nothing else - no
// status, no severity, no list.
//
// That shape is the whole point. These machines belong to tenants, run in places
// nobody here can reach, and are switched off for ordinary reasons. Attaching a
// severity to "3 of 4 up" would put an amber badge on this platform for somebody
// else's laptop being closed, and an operator who cannot act on a warning learns
// to stop reading warnings.
//
// Online is a POINTER because absent and zero are different answers: warp
// liveness comes from a gauge the leaders only publish after the gateway is
// updated, and reporting a confident 0/12 for a fleet nobody measured would be
// worse than saying nothing.
type customerCounts struct {
	Total  int  `json:"total"`
	Online *int `json:"online"`
}

type customerSummaryBlock struct {
	Nodes customerCounts `json:"nodes"`
	Links customerCounts `json:"links"`
	Warps customerCounts `json:"warps"`
}

// customerSummary counts what tenants run: BYON nodes, their links, and their
// warp overlay peers.
func (h *InfrastructureHandler) customerSummary(ctx context.Context, split services.LinkSplit) customerSummaryBlock {
	var out customerSummaryBlock

	all, err := h.state.Store.ListNodes()
	if err == nil {
		_, customer := services.SplitNodes(all)
		online := 0
		for i := range customer {
			if customer[i].Status == "online" {
				online++
			}
		}
		out.Nodes = customerCounts{Total: len(customer), Online: &online}
	}

	linksOnline := split.CustomerOnline
	out.Links = customerCounts{Total: len(split.Customer), Online: &linksOnline}

	// Only peers whose key has a tenant owner. An owner-less key is admin-minted
	// for one of our own machines (an External node's warp), and counting it
	// put the platform's own hardware into the customer figure.
	if customer, platform, perr := h.state.Store.CountWarpPeersByOwnership(); perr == nil {
		out.Warps = customerCounts{Total: customer}
		// Only the warp leader can say whether a tunnel is up, and it publishes
		// ONE aggregate over every peer. Until every leader is on a build that
		// does, or while our own peers are inside that aggregate, the honest
		// answer is a total with no online count: it cannot be split by owner.
		if up, known := services.WarpPeersActive(ctx, h.state.Redis); known && platform == 0 {
			out.Warps.Online = &up
		}
	}
	return out
}

// GetRoutingMigrationStatus GET /api/infrastructure/routing-migration -
// progress of the routing migration job. With no job wired it answers
// running:false instead of failing, so the page still renders on an instance
// that never ran one.
func (h *InfrastructureHandler) GetRoutingMigrationStatus(w http.ResponseWriter, r *http.Request) {
	if h.state.RoutingMigration == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "running": false})
		return
	}
	status := h.state.RoutingMigration.GetStatus(r.Context())
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"running": status.Running,
		"total":   status.Total,
		"done":    status.Done,
		"failed":  status.Failed,
	})
}
