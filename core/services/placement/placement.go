// Package placement scores nodes for a new server: capacity against the
// overcommit ratios, RAM padding and disk promises, then load. The HTTP side
// (preview endpoint, scope of an automatic pick) is handlers/servers.
package placement

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// PickNodeRequest describes the resource shape a new server needs.
//
// Filters are AND-combined: Region (exact match), Tags (every tag must be
// present on the node), NodeID (single-node pin). Any combination is valid;
// an empty PickNodeRequest considers every online node.
//
// `Tag` (singular) is kept for backward-compat with older callers — it is
// folded into Tags on entry.
type PickNodeRequest struct {
	Region   string   `json:"region"`
	Tags     []string `json:"tags"`
	Tag      string   `json:"tag"`    // deprecated, single-tag legacy field
	NodeID   int      `json:"nodeId"` // honored for capacity-check only
	RAMMB    int      `json:"ramMb"`
	CPUCores float64  `json:"cpuCores"`
	DiskGB   int      `json:"diskGb"`

	// RAMPaddingMB is the new server's own padding override, nil = inherit.
	// Capacity counts RAM + the padding the server would get on each node.
	RAMPaddingMB *int `json:"ramPaddingMb,omitempty"`

	// OwnerScope, when non-nil, restricts placement to nodes OWNED by this user id
	// (their own BYON nodes only — platform nodes are excluded for self-service).
	// Set internally by CreateServer for a non-admin caller in BYON mode so the
	// scheduler never picks a node the placement gate would reject. The admin
	// preview endpoint leaves it nil (sees every node). Not JSON-decoded.
	OwnerScope *string `json:"-"`

	// PlatformOnly excludes every TENANT-owned node from the pick. Set by
	// CreateServer when the caller is an admin and no node was named, which is
	// the case where a machine gets chosen and nobody decided which one.
	//
	// Without it, auto-placement for an admin considered the whole fleet, so a
	// server could be put on a customer's own hardware - hardware that customer
	// has root on - because it happened to be the least loaded box in the
	// region. An admin who genuinely wants that can still say so by naming the
	// node: this only governs the automatic pick.
	//
	// Not set on the preview endpoint, so an admin's capacity view still shows
	// every node. Not JSON-decoded.
	PlatformOnly bool `json:"-"`
}

// NodeCandidate is one node considered for placement. `Available` means
// the server would fit inside `physical * overcommit_ratio` after the
// addition. Score is "lower is better" — sum of relative load + spare-
// disk-percent penalty + server-count tiebreaker.
type NodeCandidate struct {
	NodeID        int     `json:"nodeId"`
	NodeName      string  `json:"nodeName"`
	Available     bool    `json:"available"`
	Reason        string  `json:"reason"`
	Score         float64 `json:"score"`
	AllocRAMMB    int64   `json:"allocRamMb"`
	AllocCPU      float64 `json:"allocCpu"`
	TotalRAMMB    int64   `json:"totalRamMb"`
	TotalCPU      float64 `json:"totalCpu"`
	OvercommitRAM float64 `json:"overcommitRam"`
	OvercommitCPU float64 `json:"overcommitCpu"`
	ServerCount   int     `json:"serverCount"`
}

type PickNodeResponse struct {
	Success    bool            `json:"success"`
	Picked     *NodeCandidate  `json:"picked,omitempty"`
	Candidates []NodeCandidate `json:"candidates"`
	Reason     string          `json:"reason"`
}

// Pick is the core scheduling logic, behind the preview endpoint and
// CreateServer. gatewayOn is whether platform routing uses the gateway now.
func Pick(ctx context.Context, st store.Store, rdb *redis.Client, gatewayOn bool, req PickNodeRequest) PickNodeResponse {
	all, err := st.ListNodes()
	if err != nil {
		return PickNodeResponse{Reason: "store error: " + err.Error()}
	}

	// Normalize filter inputs once.
	region := strings.ToLower(strings.TrimSpace(req.Region))
	tags := normalizeTagList(req.Tags)
	if req.Tag != "" {
		tags = append(tags, strings.ToLower(strings.TrimSpace(req.Tag)))
	}
	tags = dedupe(tags)

	// Roll up live heartbeat into a token→stats lookup so we don't ping
	// each node individually inside the loop.
	heartbeats := services.LoadHeartbeats(ctx, rdb)

	// Counted so the failure can SAY that customer machines were passed over.
	// Without it an operator reads "no node available" while looking at a panel
	// full of online nodes with room, and the reason is invisible.
	skippedTenantNodes := 0
	candidates := make([]NodeCandidate, 0, len(all))
	for i := range all {
		n := &all[i]
		if n.Status != "online" {
			continue
		}
		// External/home nodes only route via gateway+beam. When the platform is
		// in ip_port mode they have no usable path, so exclude them from placement
		// entirely — never deploy a new server onto a node we can't reach.
		// Servers already running on an external node when gateway is later
		// disabled are intentionally left as-is; the routing migration handles
		// their redeploy. Only new placements are blocked here.
		if n.IsExternal() && !gatewayOn {
			continue
		}
		// BYON: a tenant may only auto-place on THEIR OWN node. Platform nodes
		// (owner nil) are operator-only for self-service; tenants reach them via
		// the rented-server path instead. Mirrors canPlaceOnNode so the pick can't
		// be rejected downstream for ownership.
		if req.OwnerScope != nil && (n.OwnerID == nil || *n.OwnerID != *req.OwnerScope) {
			continue
		}
		// The mirror of the line above, for the caller who has no owner scope
		// of their own. See PlatformOnly.
		if req.PlatformOnly && n.OwnerID != nil {
			skippedTenantNodes++
			continue
		}
		if req.NodeID > 0 && n.ID != req.NodeID {
			continue
		}
		if region != "" && !strings.EqualFold(n.Region, region) {
			continue
		}
		if len(tags) > 0 && !nodeHasAllTags(n, tags) {
			continue
		}

		c := ScoreNode(ctx, st, rdb, n, req, heartbeats[n.Token])
		candidates = append(candidates, c)
	}

	// Sort by Available DESC (eligible first), then Score ASC.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Available != candidates[j].Available {
			return candidates[i].Available
		}
		return candidates[i].Score < candidates[j].Score
	})

	resp := PickNodeResponse{Candidates: candidates}
	for i := range candidates {
		if candidates[i].Available {
			resp.Picked = &candidates[i]
			resp.Success = true
			resp.Reason = candidates[i].Reason
			return resp
		}
	}
	parts := []string{}
	if region != "" {
		parts = append(parts, "region="+region)
	}
	if len(tags) > 0 {
		parts = append(parts, "tags="+strings.Join(tags, "+"))
	}
	if len(parts) > 0 {
		resp.Reason = "no eligible node found for " + strings.Join(parts, ", ")
	} else {
		resp.Reason = "no eligible node found"
	}
	if skippedTenantNodes > 0 {
		resp.Reason += fmt.Sprintf(" (%d customer-owned node(s) were not considered: automatic placement never uses them. Name a node explicitly to place there.)", skippedTenantNodes)
	}
	return resp
}

func normalizeTagList(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ScoreNode evaluates one node against the request. Available=false when
// adding the new server would push allocation past physical*overcommit.
// Score is normalized 0..3 (sum of three 0..1 components), lower is better.
func ScoreNode(ctx context.Context, st store.Store, rdb *redis.Client, n *models.Node, req PickNodeRequest, hb *services.NodeHeartbeat) NodeCandidate {
	allocRAM, allocCPU, _ := st.SumAllocatedByNode(n.ID)
	serverCount, _ := st.CountServersByNode(n.ID)

	cand := NodeCandidate{
		NodeID:        n.ID,
		NodeName:      n.Name,
		AllocRAMMB:    allocRAM,
		AllocCPU:      allocCPU,
		TotalRAMMB:    n.TotalRAMMB,
		TotalCPU:      n.TotalCPU,
		OvercommitRAM: n.RAMOvercommitRatio,
		OvercommitCPU: n.CPUOvercommitRatio,
		ServerCount:   serverCount,
		Available:     true,
	}

	// RAM capacity check: the container is RAM + padding, and allocRAM counts
	// every existing server the same way.
	if n.TotalRAMMB > 0 {
		cap := float64(n.TotalRAMMB) * n.RAMOvercommitRatio
		want := req.RAMMB + services.EffectiveRAMPaddingMB(st, req.RAMPaddingMB, n.RAMPaddingMB)
		if float64(allocRAM+int64(want)) > cap {
			cand.Available = false
			cand.Reason = fmt.Sprintf("RAM full: %d + %d > %.0f (overcommit %.2fx, incl. container headroom)",
				allocRAM, want, cap, n.RAMOvercommitRatio)
		}
	}
	// CPU capacity check (only enforced when a positive limit is requested)
	if cand.Available && req.CPUCores > 0 && n.TotalCPU > 0 {
		cap := n.TotalCPU * n.CPUOvercommitRatio
		if allocCPU+req.CPUCores > cap {
			cand.Available = false
			cand.Reason = fmt.Sprintf("CPU full: %.2f + %.2f > %.2f (overcommit %.2fx)",
				allocCPU, req.CPUCores, cap, n.CPUOvercommitRatio)
		}
	}
	// Disk floor — a server lives on ONE path, so measure the path this node
	// would actually choose (taking the largest instead would admit a server the
	// node then places on a smaller disk) and subtract what that path has
	// already PROMISED to the servers on it. Free space alone hides
	// oversubscription until users actually fill their servers.
	if cand.Available && req.DiskGB > 0 && hb != nil {
		limits, err := st.ServerDiskLimitsByNode(n.ID)
		if err != nil {
			limits = nil
		}
		res := services.CheckDiskCapacity(services.DiskCapacityRequest{
			Storage:    hb.Storage,
			Placement:  services.EffectiveNodePlacement(ctx, rdb, n.Token),
			Unwritten:  services.UnwrittenBytesByPath(hb.Storage, limits, services.LoadServerDiskUsage(ctx, rdb, hb.Storage)),
			HeadroomGB: services.DiskHeadroomGB(st),
			WantMB:     int64(req.DiskGB) * 1024,
		})
		if !res.Fits {
			detail := fmt.Sprintf("%s: %d GB free, %d GB already promised to other servers, %d GB buffer -> %d GB available, need %d GB",
				res.Path, res.FreeGB, res.UnwrittenGB, res.HeadroomGB, res.AvailableGB, req.DiskGB)
			// Soft mode still places the server; the operator asked to be told,
			// not to be stopped. Reporting it either way is the point - the old
			// behaviour was to neither stop nor tell.
			if services.DiskEnforcement(st) == services.DiskEnforcementHard {
				cand.Available = false
				cand.Reason = "disk over-promised on " + detail
			} else {
				cand.Reason = "warning: disk over-promised on " + detail
			}
		}
	}

	// Score components (each 0..1, lower is better). Load fractions come from
	// the shared services helpers so the scheduler and the rebalance worker
	// compute node load identically.
	ramLoad := services.NodeRAMLoad(allocRAM, n.TotalRAMMB, n.RAMOvercommitRatio)
	cpuLoad := services.NodeCPULoad(allocCPU, n.TotalCPU, n.CPUOvercommitRatio)
	countPenalty := float64(serverCount) / 100.0 // mild tiebreaker

	cand.Score = ramLoad + cpuLoad + countPenalty
	if cand.Available && cand.Reason == "" {
		cand.Reason = fmt.Sprintf("RAM %.0f%%, CPU %.0f%%, %d servers",
			ramLoad*100, cpuLoad*100, serverCount)
	}
	return cand
}

func nodeHasTag(n *models.Node, tag string) bool {
	for _, t := range strings.Split(n.Tags, ",") {
		if strings.EqualFold(strings.TrimSpace(t), tag) {
			return true
		}
	}
	return false
}

// nodeHasAllTags returns true when every required tag is present on the node.
// Used for AND-semantics multi-tag filtering.
func nodeHasAllTags(n *models.Node, required []string) bool {
	for _, t := range required {
		if !nodeHasTag(n, t) {
			return false
		}
	}
	return true
}
