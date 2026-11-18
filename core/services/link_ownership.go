package services

import (
	"context"
	"dylaris-core/models"
	"dylaris-core/store"
	"dylaris-pkg/protocol"
	"encoding/json"

	"github.com/redis/go-redis/v9"
)

// Which links are OURS and which belong to a customer.
//
// Redis knows neither. A link registers itself under its own token
// (`link:<token>`, `online_link:<token>`) and carries no owner, so the live view
// is a flat list in which the operator's DC link and a customer's route-only kit
// on a machine nobody here has ever seen look identical.
//
// That mattered as soon as one number was built out of the list. A route-only
// customer who powers their box down at night is not an outage of this platform,
// and a status page that turns amber because of it is a status page an operator
// learns to ignore - which is the failure mode worth avoiding, because the next
// amber might be ours.
//
// The ownership is in the DATABASE, in two places:
//
//   - the link token of a node that has an owner_id (a BYON machine): the one
//     the Hub named for it, else the derived one (plus a legacy `link_secret`)
//   - `core_link_routes.link_token`, which is a route-only kit by construction:
//     every row there belongs to a tenant
//
// Anything not in either is ours: a DC link derived from CLUSTER_SECRET, or the
// link sidecar of a platform or external node.
type LinkOwnership struct {
	// customer holds link TOKENS, which are credentials. It stays in memory,
	// is never logged, and never reaches a response body - callers ask it a
	// yes/no question and publish the answer, not the key.
	customer map[string]struct{}
	// loaded is false when the database could not be read. Callers then treat
	// every link as ours, which OVER-reports our own fleet rather than
	// under-reporting it: a missed customer link shows as one of ours and can
	// raise a false alarm, while the opposite would silently drop a real
	// outage of ours out of the count.
	loaded bool
}

// LoadLinkOwnership reads the customer link tokens.
//
// Best-effort by design: a failure yields a set that classifies nothing as a
// customer's, which is the conservative direction (see `loaded`).
func LoadLinkOwnership(st store.Store, clusterSecret string) LinkOwnership {
	if st == nil {
		return LinkOwnership{customer: map[string]struct{}{}}
	}
	nodes, err := st.ListNodes()
	if err != nil {
		return LinkOwnership{customer: map[string]struct{}{}}
	}
	return LinkOwnershipFrom(st, nodes, clusterSecret)
}

// LinkOwnershipFrom is LoadLinkOwnership for a caller that already has the
// nodes. The metrics collector lists them every sample anyway, and reading the
// table twice on the same tick to answer one question would be a second query
// per 30 seconds for no new information.
func LinkOwnershipFrom(st store.Store, nodes []models.Node, clusterSecret string) LinkOwnership {
	o := LinkOwnership{customer: map[string]struct{}{}}
	if st == nil {
		return o
	}
	for i := range nodes {
		n := &nodes[i]
		if n.Kind() != models.NodeKindBYON {
			continue
		}
		// The enrol flow never writes link_secret: the link runs under the
		// token the Hub named for it, else the derived one.
		o.customer[effectiveLinkToken(n, clusterSecret)] = struct{}{}
		if n.LinkSecret != "" {
			o.customer[n.LinkSecret] = struct{}{}
		}
	}
	routes, err := st.ListCoreLinkRoutes()
	if err != nil {
		// Nodes were read and routes were not. Keep what was learned rather
		// than discarding it: a partially known set still classifies the BYON
		// links correctly, and the route-only ones fall back to "ours".
		o.loaded = true
		return o
	}
	for _, r := range routes {
		if r.LinkToken != "" {
			o.customer[r.LinkToken] = struct{}{}
		}
	}
	o.loaded = true
	return o
}

// IsCustomer reports whether this link belongs to a tenant rather than to us.
func (o LinkOwnership) IsCustomer(token string) bool {
	if !o.loaded {
		return false
	}
	_, ok := o.customer[token]
	return ok
}

// LinkSplit is a set of links divided by who runs them.
type LinkSplit struct {
	Ours           []GatewayLinkStatus
	OursOnline     int
	Customer       []GatewayLinkStatus
	CustomerOnline int
}

// SplitLinks divides the live link list.
func (o LinkOwnership) SplitLinks(links []GatewayLinkStatus) LinkSplit {
	var s LinkSplit
	for _, l := range links {
		if o.IsCustomer(l.Token) {
			s.Customer = append(s.Customer, l)
			if l.Online {
				s.CustomerOnline++
			}
			continue
		}
		s.Ours = append(s.Ours, l)
		if l.Online {
			s.OursOnline++
		}
	}
	return s
}

// SplitNodes divides nodes the same way, by the classification models.Node
// already carries. Ours is platform + EXTERNAL: an external node is hardware the
// operator registered and is responsible for, unlike a BYON machine.
func SplitNodes(nodes []models.Node) (ours, customer []models.Node) {
	for i := range nodes {
		if nodes[i].Kind() == models.NodeKindBYON {
			customer = append(customer, nodes[i])
			continue
		}
		ours = append(ours, nodes[i])
	}
	return ours, customer
}

// WarpPeersActive sums how many overlay tunnels the warp leaders currently see
// handshaking, across every leader.
//
// known=false means NO leader reported the figure, which is the state right
// after Core is updated and before the gateway is. The caller must show a total
// with no online count then, because a confident "0 of 12 up" for a fleet
// nobody measured is worse than saying nothing - it reads as a total outage.
//
// Read from the mirror the bandwidth consumer already maintains rather than
// from the streams, so this costs one SCAN and no consumer group.
func WarpPeersActive(ctx context.Context, rdb *redis.Client) (int, bool) {
	if rdb == nil {
		return 0, false
	}
	total, known := 0, false
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "dylaris:gwbw:component:warp:*", 100).Result()
		if err != nil {
			return 0, false
		}
		for _, k := range keys {
			raw, err := rdb.Get(ctx, k).Result()
			if err != nil {
				continue
			}
			var st protocol.GatewayStats
			if json.Unmarshal([]byte(raw), &st) != nil {
				continue
			}
			v, ok := st.Gauges["peers_active"]
			if !ok {
				continue // a leader too old to report it
			}
			known = true
			total += int(v)
		}
		if next == 0 {
			return total, known
		}
		cursor = next
	}
}
