package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"dylaris-core/authz"
	"dylaris-core/handlers/httpx"
	"dylaris-core/services"
	"dylaris-core/store"
)

// adminGatewayListCap bounds every list of AdminUserGateway; a longer one is
// cut and flagged <list>Truncated. Plans cap links and addresses far below it.
const adminGatewayListCap = 200

type adminGatewayKit struct {
	LinkID    string `json:"linkId"`
	Name      string `json:"name"`
	Region    string `json:"region"`
	CreatedAt string `json:"createdAt"`
	// Online is nil when Core could not ask Redis, never a guessed false.
	Online   *bool    `json:"online"`
	Caps     []string `json:"caps"`
	RetireAt string   `json:"retireAt,omitempty"`
}

type adminGatewayRoute struct {
	Domain     string `json:"domain"`
	TargetHost string `json:"targetHost"`
	TargetPort int    `json:"targetPort"`
	// LinkID is the owner's kit the route runs over, "" when none of the
	// owner's live kits carries it.
	LinkID          string     `json:"linkId"`
	ProxyProtocol   bool       `json:"proxyProtocol"`
	SuspendedAt     *time.Time `json:"suspendedAt,omitempty"`
	SuspendedReason string     `json:"suspendedReason,omitempty"`
	// Players is absent when nobody is on the address (or it is suspended).
	Players *adminGatewayPlayers `json:"players,omitempty"`
}

// adminGatewayPlayers is play sessions right now as the edges count them: a
// display, up to about 12 s old.
type adminGatewayPlayers struct {
	Java    int `json:"java"`
	Bedrock int `json:"bedrock"`
}

// adminGatewayPeakDay is one UTC day's player peak, a lower bound.
type adminGatewayPeakDay struct {
	Day     string `json:"day"` // YYYY-MM-DD, UTC
	Java    int    `json:"java"`
	Bedrock int    `json:"bedrock"`
	Total   int    `json:"total"`
}

type adminGatewayClaim struct {
	Domain    string     `json:"domain"`
	State     string     `json:"state"`
	Attempts  int        `json:"attempts"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
	LapsesAt  *time.Time `json:"lapsesAt,omitempty"`
}

type adminGatewayRegion struct {
	Region  string `json:"region"`
	Kind    string `json:"kind"`
	Product string `json:"product"`
	Bytes   int64  `json:"bytes"`
}

type adminGatewayBeamKey struct {
	Kid       string     `json:"kid"`
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

type adminGatewayAPIKey struct {
	ID          int        `json:"id"`
	Name        string     `json:"name"`
	Permissions []string   `json:"permissions"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
}

// capped is list cut to adminGatewayListCap, and whether it was cut.
func capped[T any](list []T) ([]T, bool) {
	if len(list) > adminGatewayListCap {
		return list[:adminGatewayListCap], true
	}
	return list, false
}

// AdminUserGateway GET /api/admin/users/{id}/gateway - one account's managed gateway as the provider holds it: product, billing status, caps, link kits (online and caps), routes (suspended ones too) with the players on each address right now, players (the account's total, java and bedrock), peaks (the daily peak of concurrent players: day (the server's UTC day), today, last30Days and the days of the last 30 with a row, newest first; each AT LEAST that many; left out when they cannot be read), own domains, this month's traffic, beam key ids and gateway API keys. Billing, product, limits and usage need plans.read too, API keys users.read; a block the caller lacks is left out and named in hidden. Read-only and secret-free: no link token, key hash, beam public key or TXT value. Lists stop at 200 with <list>Truncated.
func (h *GatewayHandler) AdminUserGateway(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.ParseUserID(w, r)
	if !ok {
		return
	}
	st := h.d.Store
	fail := func(what string, err error) {
		log.Printf("admin user gateway %s: %s: %v", userID, what, err)
		httpx.SendJSONError(w, "Failed to load the user's gateway", http.StatusInternalServerError)
	}
	u, err := st.GetUserByID(userID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && u == nil) {
		httpx.SendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	if err != nil {
		fail("user", err)
		return
	}
	now := time.Now().UTC()
	// topology.read (the route) shows the gateway topology. Billing and usage
	// belong to plans.read and another user's API keys to users.read, so a
	// block the caller does not hold is left out and named in "hidden".
	seeBilling, seeKeys := h.callerHolds(r, "plans.read"), h.callerHolds(r, "users.read")

	keys, err := st.ListWarpAPIKeysByOwner(userID)
	if err != nil {
		fail("kits", err)
		return
	}
	kits := []adminGatewayKit{}
	linkOf := map[string]string{} // link token -> kit id
	retiring := 0
	for _, k := range keys {
		// The same table holds the owner's BYON node keys: not link kits.
		if !strings.HasPrefix(k.NodeID, "link-") {
			continue
		}
		if h.d.Gateway != nil {
			linkOf[h.d.Gateway.LinkToken(k.NodeID)] = k.NodeID
		}
		kit := adminGatewayKit{LinkID: k.NodeID, Name: k.Name, Region: k.Region,
			CreatedAt: k.CreatedAt.UTC().Format(time.RFC3339), Caps: []string{}}
		if k.RetireAt != nil {
			kit.RetireAt = k.RetireAt.UTC().Format(time.RFC3339)
			retiring++
		}
		kits = append(kits, kit)
	}
	kits, kitsCut := capped(kits)
	// One pipelined round trip for every listed kit.
	if h.d.Gateway != nil && len(kits) > 0 {
		tokens := make([]string, len(kits))
		for i, k := range kits {
			tokens[i] = h.d.Gateway.LinkToken(k.LinkID)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		online, caps := services.LinkPresence(ctx, h.d.Redis, tokens)
		cancel()
		for i, tok := range tokens {
			if v, ok := online[tok]; ok {
				kits[i].Online = &v
				if c := caps[tok]; v && c != nil {
					kits[i].Caps = c
				}
			}
		}
	}
	used, err := st.CountLinkKitsByOwner(userID)
	if err != nil {
		fail("kit count", err)
		return
	}

	rows, err := st.ListCoreLinkRoutesByOwner(userID)
	if err != nil {
		fail("routes", err)
		return
	}
	// One read of the edge streams for every address; looked up by this
	// owner's own domains only. The map is shared: read it, never write.
	var counts map[string]services.RoutePlayers
	if h.d.Redis != nil {
		counts = services.GatewayPlayersByRoute(r.Context(), h.d.Redis)
	}
	total := adminGatewayPlayers{}
	// Summed before the cap, so a cut list still shows the account's total.
	for _, rt := range rows {
		if rt.SuspendedAt == nil {
			total.Java += counts[rt.Domain].Java
			total.Bedrock += counts[rt.Domain].Bedrock
		}
	}
	rows, routesCut := capped(rows)
	routes := make([]adminGatewayRoute, 0, len(rows))
	for _, rt := range rows {
		// The row's LinkToken is a tunnel credential: it is only ever used as a
		// lookup key here, never written.
		route := adminGatewayRoute{Domain: rt.Domain, TargetHost: rt.TargetHost, TargetPort: rt.TargetPort,
			LinkID: linkOf[rt.LinkToken], ProxyProtocol: rt.ProxyProtocol,
			SuspendedAt: rt.SuspendedAt, SuspendedReason: rt.SuspendedReason}
		if p := counts[rt.Domain]; rt.SuspendedAt == nil && p.Java+p.Bedrock > 0 {
			route.Players = &adminGatewayPlayers{Java: p.Java, Bedrock: p.Bedrock}
		}
		routes = append(routes, route)
	}

	// A failed peaks read drops only the peaks block: the panel already draws
	// the view without one, and the kits and routes are what an admin needs.
	peakRows, peakToday, peak30, peakErr := playerPeaks(st, userID, now)
	if peakErr != nil {
		log.Printf("admin user gateway %s: player peaks left out: %v", userID, peakErr)
	}
	peakDays := make([]adminGatewayPeakDay, 0, len(peakRows))
	for _, p := range peakRows {
		peakDays = append(peakDays, adminGatewayPeakDay{Day: p.Day.Format("2006-01-02"), Java: p.Java, Bedrock: p.Bedrock, Total: p.Total})
	}

	claimRows, err := st.ListCustomDomainClaimsByUser(userID)
	if err != nil {
		fail("custom domains", err)
		return
	}
	claimRows, claimsCut := capped(claimRows)
	claims := make([]adminGatewayClaim, 0, len(claimRows))
	for _, c := range claimRows {
		v := viewOf(c) // the owner's own reading of the lapse
		claims = append(claims, adminGatewayClaim{Domain: c.Domain, State: c.State, Attempts: c.Attempts,
			CheckedAt: c.CheckedAt, LapsesAt: v.LapsesAt})
	}

	beamRows, err := st.ListBeamIssuerKeysByOwner(userID)
	if err != nil {
		fail("beam keys", err)
		return
	}
	beamRows, beamCut := capped(beamRows)
	beamKeys := make([]adminGatewayBeamKey, 0, len(beamRows))
	for _, k := range beamRows {
		beamKeys = append(beamKeys, adminGatewayBeamKey{Kid: k.ID, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt})
	}

	out := map[string]interface{}{
		"success": true,
		"userId":  userID,
		"kits":    kits, "kitsTruncated": kitsCut, "kitsUsed": used, "kitsRetiring": retiring,
		"routes": routes, "routesTruncated": routesCut, "players": total,
		"customDomains": claims, "customDomainsTruncated": claimsCut,
		"beamKeys": beamKeys, "beamKeysTruncated": beamCut,
	}
	if peakErr == nil {
		out["peaks"] = map[string]interface{}{"today": peakToday, "last30Days": peak30, "days": peakDays,
			// The UTC day the windows are measured against, so a client draws the
			// 30 days from the server's calendar rather than its own clock.
			"day": now.Format("2006-01-02")}
	}
	hidden := []string{}

	if seeBilling {
		b, err := st.GetUserBilling(userID)
		if err != nil {
			fail("billing", err)
			return
		}
		if b == nil { // the store answers a default row; never trust that blindly
			b = &store.UserBilling{UserID: userID, Status: "active"}
		}
		// isAdmin=false: the view shows what the account holds as a customer.
		ent, err := services.EffectiveEntitlement(st, userID, now, h.d.StoreEnabled, false)
		if err != nil {
			fail("entitlement", err)
			return
		}
		product, _, err := accountProduct(st, userID, ent)
		if err != nil {
			fail("product", err)
			return
		}
		lim, err := services.EffectiveLimits(st, userID)
		if err != nil {
			fail("limits", err)
			return
		}
		customCap, _ := customDomainCapFor(st, userID)
		out["product"] = product
		out["billingStatus"] = b.Status
		out["limits"] = externalLimits{MaxLinks: lim.MaxLinks, MaxRoutes: h.effectiveRouteLimit(userID), MaxCustomDomains: customCap}
		if b.OverLimitSince != nil {
			out["overLimitSince"] = b.OverLimitSince.UTC().Format(time.RFC3339)
		}

		period := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		tu, err := st.GetTrafficUsage(userID, period)
		if err != nil {
			fail("usage", err)
			return
		}
		regionRows, err := st.GetTrafficUsageRegions(userID, period)
		if err != nil {
			fail("usage regions", err)
			return
		}
		regions := make([]adminGatewayRegion, 0, len(regionRows))
		for _, rg := range regionRows {
			regions = append(regions, adminGatewayRegion{Region: rg.Region, Kind: rg.Kind, Product: rg.Product, Bytes: rg.Bytes})
		}
		usage := map[string]interface{}{"period": period.Format("2006-01"), "edgeBytes": tu.EdgeBytes,
			"relayBytes": tu.RelayBytes, "regions": regions}
		if err := usageStanding(st, userID, period, now, usage); err != nil {
			fail("usage standing", err)
			return
		}
		out["usage"] = usage
	} else {
		hidden = append(hidden, "billing", "usage")
	}

	if seeKeys {
		apiRows, err := st.ListAPIKeysByUser(userID)
		if err != nil {
			fail("api keys", err)
			return
		}
		apiKeys := []adminGatewayAPIKey{}
		for _, k := range apiRows {
			if !hasGatewayPermission(k.Scope.Permissions) {
				continue
			}
			apiKeys = append(apiKeys, adminGatewayAPIKey{ID: k.ID, Name: k.Name, Permissions: k.Scope.Permissions,
				CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt})
		}
		apiKeys, apiCut := capped(apiKeys)
		out["apiKeys"], out["apiKeysTruncated"] = apiKeys, apiCut
	} else {
		hidden = append(hidden, "apiKeys")
	}
	out["hidden"] = hidden

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// callerHolds asks the capability resolver, the one RequireCap uses, whether
// the caller holds capID. No resolver or a failed resolve holds nothing.
func (h *GatewayHandler) callerHolds(r *http.Request, capID string) bool {
	if h.d.Authz == nil {
		return false
	}
	res, err := h.d.Authz.Resolve(authz.IdentityFromContext(r.Context()), 0)
	return err == nil && res != nil && res.HasCap(capID)
}

func hasGatewayPermission(perms []string) bool {
	for _, p := range perms {
		if strings.HasPrefix(p, "gateway.") {
			return true
		}
	}
	return false
}
