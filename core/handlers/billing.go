package handlers

import (
	"dylaris-core/services"
	"dylaris-core/store"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

// BillingHandler exposes the BYON non-payment lifecycle: a tenant reads their own
// status (for the panel banner); an admin sets status + per-user retention
// overrides. Stripe/other webhooks will later call the same service methods.
type BillingHandler struct {
	state *AppState
}

func NewBillingHandler(state *AppState) *BillingHandler {
	return &BillingHandler{state: state}
}

// trafficGB is the divisor for the traffic ceiling: DECIMAL gigabytes, 10^9.
// Bandwidth is metered decimally and the store computes the ceiling that way, so
// comparing it against a GiB figure would move the threshold by about 7% without
// anyone noticing. Not to be confused with usageGiB in usage.go, which is the
// binary divisor the warn-only limits use.
const trafficGB = int64(1_000_000_000)

// myTrafficStatus describes how close the caller is to the point where their
// traffic stops being free. Reported only when the store has told us the deal
// (a non-zero ceiling); a self-hosted install meters nothing and gets nil, which
// is what keeps the banner off screens it means nothing on.
type myTrafficStatus struct {
	// UsedGB, CeilingGB and Pct describe the pool the tenant is CLOSEST to
	// losing, not a sum. A total is the one number that cannot stop anybody:
	// somebody comfortably inside three allowances and past the fourth is stopped
	// by the fourth, and a banner reading 40% next to a halted server is worse
	// than no banner.
	UsedGB    int64 `json:"usedGb"`
	CeilingGB int64 `json:"ceilingGb"`
	// Pct is uncapped on the way up on purpose - someone at 300% should see 300%,
	// not a reassuring 100%.
	Pct int `json:"pct"`
	// BillingEnabled is whether the tenant has agreed to be charged past the
	// allowance. When false, reaching it STOPS their services instead of billing
	// them, which is the thing the banner has to say out loud.
	BillingEnabled bool `json:"billingEnabled"`
	// Warn is the highest threshold ANY pool has reached: 0, 80, 90 or 100.
	Warn int `json:"warn"`
	// Pools is every allowance the tenant is judged against, so the screen can
	// show player traffic per region beside file transfers rather than one bar
	// standing in for all of them.
	Pools []trafficPool `json:"pools"`
}

// trafficPool is one allowance as the panel draws it.
type trafficPool struct {
	Kind   string `json:"kind"`   // edge (player traffic) | relay (file transfers)
	Region string `json:"region"` // "*" for a pool that is not per region
	UsedGB int64  `json:"usedGb"`
	// IncludedGB is nil when nothing is configured for this pool. Nil is not
	// zero: zero would draw a full bar for a tenant nobody has limited.
	IncludedGB *int64 `json:"includedGb"`
	Pct        int    `json:"pct"`
	Warn       int    `json:"warn"`
	// ByProductBytes splits UsedGB by which product moved the bytes ("byon",
	// "route", "" for rows written before the split existed).
	//
	// BYTES, and the field name says so, because the rest of this struct is in
	// GB: a share under a gigabyte truncates to 0, and two of them beside a
	// total of 1 GB reads as a bug rather than as rounding. The panel formats
	// them.
	//
	// A breakdown, never a second pool. The allowance is granted per unit HELD
	// and pooled across products, so these shares are judged against nothing -
	// giving each product its own ceiling would hand a tenant the same free
	// allowance once per product they own.
	ByProductBytes map[string]int64 `json:"byProductBytes,omitempty"`
}

// GetMyBilling GET /api/me/billing - the caller's lifecycle state for the banner.
// Always succeeds (zero = active).
func (h *BillingHandler) GetMyBilling(w http.ResponseWriter, r *http.Request) {
	userID := byonCallerID(r)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	b, err := h.state.Store.GetUserBilling(userID)
	if err != nil {
		sendJSONError(w, "Lookup failed", http.StatusInternalServerError)
		return
	}
	payment := ""
	if h.state.Billing != nil {
		payment = h.state.Billing.PaymentURL()
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"status":     b.Status,
		"graceUntil": b.GraceUntil,
		"paymentUrl": payment,
		"traffic":    h.trafficStatusFor(userID, b),
	})
}

// trafficWarnLevel is the one answer every surface gives about the same number.
// Must stay in step with billing.WarnLevel in the store API, which decides the
// same thing for the account page.
func trafficWarnLevel(usedGB int64, includedGB *int64) int {
	if includedGB == nil || *includedGB <= 0 {
		return 0
	}
	switch pct := usedGB * 100 / *includedGB; {
	case pct >= 100:
		return 100
	case pct >= 90:
		return 90
	case pct >= 80:
		return 80
	default:
		return 0
	}
}

// trafficUnits is how many countable products the tenant holds, which is how
// many times the per-unit allowance is granted.
//
// nil and 0 both count as none, and for once that is not the limits convention
// speaking - it is that this is a QUANTITY rather than a cap. A cap of 0 means
// "none allowed" (see services.Exceeds and the Limits section of CLAUDE.md);
// zero units bought means the same thing here for a different reason, and the
// store clears the override rather than writing 0 anyway. An admin granting a
// node allowance is not the same as a customer buying one.
func trafficUnits(b *store.UserBilling) int64 {
	var n int64
	if b.MaxNodes != nil && *b.MaxNodes > 0 {
		n += *b.MaxNodes
	}
	if b.MaxLinks != nil && *b.MaxLinks > 0 {
		n += *b.MaxLinks
	}
	return n
}

// trafficStatusFor builds the tenant's pools from this month's usage and the
// limits configured in the panel.
//
// The allowance is Core's own, resolved per (region, kind). The store used to
// push one summed ceiling here, which meant the number the tenant saw and the
// number that stopped them came from two places that drifted apart - the
// pricing page promised a second allowance for file transfers that nothing
// measured. Core owns it now; the store owns only the consent to be billed.
//
// Deliberately the INCLUDED allowance, not the fair-use ceiling. The buffer is
// grace, not allowance: a warning that only fires past it arrives after the
// tenant could still have acted on it.
//
// A usage read that fails is reported as nil rather than as zero usage: "we
// could not tell" must not render as a comfortable empty bar.
func (h *BillingHandler) trafficStatusFor(userID string, b *store.UserBilling) *myTrafficStatus {
	if b == nil {
		return nil
	}
	now := time.Now().UTC()
	period := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	cells, err := h.state.Store.GetTrafficUsageRegions(userID, period)
	if err != nil {
		return nil
	}
	units := trafficUnits(b)

	out := &myTrafficStatus{BillingEnabled: b.TrafficBillingEnabled}
	limited := false
	for _, c := range withIdleAllowances(foldTrafficCells(cells)) {
		lim, err := services.ResolveTrafficLimit(h.state.Store, userID, c.region, c.kind)
		if err != nil {
			return nil
		}
		usedGB := c.bytes / trafficGB
		p := trafficPool{Kind: c.kind, Region: c.region, UsedGB: usedGB, ByProductBytes: c.byProduct}
		if lim.IncludedGB != nil {
			limited = true
			total := *lim.IncludedGB * units
			p.IncludedGB = &total
			if total > 0 {
				p.Pct = int(usedGB * 100 / total)
			} else {
				// No allowance at all is not 0% of one: a tenant who holds
				// nothing is already past it.
				p.Pct = 100
			}
			p.Warn = trafficWarnLevel(usedGB, &total)
		}
		if p.Warn > out.Warn {
			out.Warn = p.Warn
		}
		// The pool closest to stopping them is the one the banner speaks for.
		if p.IncludedGB != nil && p.Pct >= out.Pct {
			out.Pct, out.UsedGB, out.CeilingGB = p.Pct, p.UsedGB, *p.IncludedGB
		}
		out.Pools = append(out.Pools, p)
	}
	// Nothing configured anywhere means nothing is metered against a limit, which
	// is what a self-hosted install reads - and what keeps the banner off screens
	// it would mean nothing on.
	if !limited {
		return nil
	}
	return out
}

// SetBillingStatus PATCH /api/admin/users/{id}/billing - RequireCap("plans.write")
// at the route. Admin transitions a tenant between active / past_due /
// suspended. past_due starts the grace window
// + dunning email; suspended is an IMMEDIATE force-suspend (SuspendNow: stops
// servers now and durably revokes every route-only link kit - they do NOT come
// back on reactivation, an admin must re-mint them); active reactivates (no
// auto-start, and only GRACED-suspended links restore automatically). The
// automatic non-payment lifecycle and the store webhook (handlers/store.go)
// keep the graced Suspend (deferred cutoff) - this admin path does not.
func (h *BillingHandler) SetBillingStatus(w http.ResponseWriter, r *http.Request) {
	if h.state.Billing == nil {
		sendJSONError(w, "Billing unavailable", http.StatusServiceUnavailable)
		return
	}
	userID := mux.Vars(r)["id"]
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	var err error
	switch req.Status {
	case "past_due":
		err = h.state.Billing.EnterPastDue(userID)
	case "active":
		err = h.state.Billing.Reactivate(userID)
	case "suspended":
		err = h.state.Billing.SuspendNow(r.Context(), userID)
	default:
		sendJSONError(w, "Invalid status (active|past_due|suspended)", http.StatusBadRequest)
		return
	}
	if err != nil {
		sendJSONError(w, "Update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "status": req.Status})
}

// GetBillingSettings GET /api/admin/settings/billing - RequireCap("plans.read")
// at the route. The platform default grace + retention windows and the
// payment URL the banner links to.
func (h *BillingHandler) GetBillingSettings(w http.ResponseWriter, r *http.Request) {
	get := func(key, def string) string {
		if v, _ := h.state.Store.GetSetting(key); v != "" {
			return v
		}
		return def
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"gracePeriod":   get(services.BillingGracePeriodKey, services.DefaultGracePeriod),
		"r2Retention":   get(services.BillingR2RetentionKey, services.DefaultR2Retention),
		"nodeRetention": get(services.BillingNodeRetentionKey, services.DefaultNodeRetention),
		// The flat per-tenant quota that used to live here has moved to
		// Settings, Backups (services.SettingBackupDefaultUserQuota). It was the
		// fallback for an owner who holds no entitlement, which includes every
		// user of a self-hosted install - and this screen is hidden without a
		// hosted store, so the one control over their backup storage was on a
		// page they could not open while the guard behind it kept running.
		//
		// These two are plain quantities rather than tri-state limits, so they
		// do carry their built-in default: a blank "included" field would read
		// as "this product includes no backup storage", which is a different
		// statement from "nobody has edited this".
		"r2IncludedGb":      get(services.BillingR2IncludedKey, strconv.FormatInt(services.DefaultR2IncludedGB, 10)),
		"r2BookableGb":      get(services.BillingR2BookableKey, strconv.FormatInt(services.DefaultR2BookableGB, 10)),
		"presignTtlNodeMin": get(services.PresignTTLNodeKey, strconv.Itoa(services.DefaultPresignTTLNodeMin)),
		"presignTtlByonMin": get(services.PresignTTLBYONKey, strconv.Itoa(services.DefaultPresignTTLBYONMin)),
		"paymentUrl":        get(services.BillingPaymentURLKey, ""),
	})
}

// SetBillingSettings PUT /api/admin/settings/billing - RequireCap("plans.write")
// at the route. Writes the platform defaults; retention specs are validated,
// the payment URL is free-form.
func (h *BillingHandler) SetBillingSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GracePeriod       string `json:"gracePeriod"`
		R2Retention       string `json:"r2Retention"`
		NodeRetention     string `json:"nodeRetention"`
		R2IncludedGb      string `json:"r2IncludedGb"`
		R2BookableGb      string `json:"r2BookableGb"`
		PresignTtlNodeMin string `json:"presignTtlNodeMin"`
		PresignTtlByonMin string `json:"presignTtlByonMin"`
		PaymentUrl        string `json:"paymentUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	for _, spec := range []string{req.GracePeriod, req.R2Retention, req.NodeRetention} {
		if !services.ValidRetentionSpec(spec) {
			sendJSONError(w, "Invalid retention spec (use e.g. 3d, 2w, 3m)", http.StatusBadRequest)
			return
		}
	}
	// The two allowances are quantities, not tri-state limits: an unlimited
	// included allowance would be free infinite storage and an unlimited
	// bookable one is the open bill the ceiling exists to prevent. Zero is
	// meaningful for both and is stored as the number it is.
	for _, f := range []struct {
		label string
		val   *string
		def   int64
	}{
		{"Included backup storage", &req.R2IncludedGb, services.DefaultR2IncludedGB},
		{"Bookable backup storage", &req.R2BookableGb, services.DefaultR2BookableGB},
	} {
		*f.val = strings.TrimSpace(*f.val)
		if *f.val == "" {
			*f.val = strconv.FormatInt(f.def, 10)
			continue
		}
		if n, err := strconv.ParseInt(*f.val, 10, 64); err != nil || n < 0 {
			sendJSONError(w, f.label+" must be a non-negative number of GB per purchased unit", http.StatusBadRequest)
			return
		}
	}
	for _, ttl := range []string{req.PresignTtlNodeMin, req.PresignTtlByonMin} {
		if n, err := strconv.Atoi(ttl); err != nil || n <= 0 {
			sendJSONError(w, "Presigned URL TTL must be a positive number of minutes", http.StatusBadRequest)
			return
		}
	}
	// The payment URL was the one operator-set URL in the platform stored with
	// no check at all ("free-form"). It is not a display string: GetMyBilling
	// hands it to every tenant, and the panel renders the whole billing banner
	// as <a href={paymentUrl}> (components/BillingBanner.tsx). plans.write is a
	// delegatable panel capability, so "an admin typed it" is not the threat
	// model, and whether a given browser happens to refuse a javascript: or
	// data: href is not something this side should be relying on. Same
	// http/https + host + no-credentials rule the other operator-set public
	// URLs already take; empty stays valid and simply renders no link.
	req.PaymentUrl = strings.TrimSpace(req.PaymentUrl)
	if err := validatePublicBaseURL("payment URL", req.PaymentUrl); err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Read BEFORE the write: the tenants who agreed to be charged were shown the
	// old ceiling, and they are told when it moves. Captured here rather than
	// compared afterwards, when the old value is gone.
	bookableBefore := services.R2BookablePerUnit(h.state.Store)

	writes := map[string]string{
		services.BillingGracePeriodKey:   req.GracePeriod,
		services.BillingR2RetentionKey:   req.R2Retention,
		services.BillingNodeRetentionKey: req.NodeRetention,
		services.BillingR2IncludedKey:    req.R2IncludedGb,
		services.BillingR2BookableKey:    req.R2BookableGb,
		services.PresignTTLNodeKey:       req.PresignTtlNodeMin,
		services.PresignTTLBYONKey:       req.PresignTtlByonMin,
		services.BillingPaymentURLKey:    req.PaymentUrl,
	}
	for k, v := range writes {
		if err := h.state.Store.SetSetting(k, v); err != nil {
			sendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if n := services.NotifyBackupBookableChanged(h.state.Store, bookableBefore,
		services.R2BookablePerUnit(h.state.Store)); n > 0 {
		log.Printf("billing: bookable backup storage changed, notified %d tenant(s) with metered storage on", n)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// SetBillingOverrides PATCH /api/admin/users/{id}/billing-overrides -
// RequireCap("plans.write") at the route. Per-user retention overrides; an
// empty spec clears the override (falls back to the platform default).
func (h *BillingHandler) SetBillingOverrides(w http.ResponseWriter, r *http.Request) {
	userID := mux.Vars(r)["id"]
	var req struct {
		GracePeriod   string `json:"gracePeriod"`
		R2Retention   string `json:"r2Retention"`
		NodeRetention string `json:"nodeRetention"`
		// R2QuotaGb is a pointer so null clears the override (use platform default)
		// while an explicit 0 means "unlimited for this user".
		R2QuotaGb *int64 `json:"r2QuotaGb"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	for _, spec := range []string{req.GracePeriod, req.R2Retention, req.NodeRetention} {
		if spec != "" && !services.ValidRetentionSpec(spec) {
			sendJSONError(w, "Invalid retention spec (use e.g. 3d, 2w, 3m)", http.StatusBadRequest)
			return
		}
	}
	if req.R2QuotaGb != nil && *req.R2QuotaGb < 0 {
		sendJSONError(w, "R2 quota must be a non-negative number of GB (0 means none; leave it empty for no limit)", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetUserBillingOverrides(userID, req.GracePeriod, req.R2Retention, req.NodeRetention, req.R2QuotaGb); err != nil {
		sendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// GetUserBilling GET /api/admin/users/{id}/billing - RequireCap("plans.read")
// at the route. Reads a tenant's full lifecycle state (status + per-user
// retention overrides) plus the platform defaults so the override modal can
// show "uses default" hints. Like GetMyBilling it never 404s: a tenant with
// no row reads back as active with empty overrides.
func (h *BillingHandler) GetUserBilling(w http.ResponseWriter, r *http.Request) {
	userID := mux.Vars(r)["id"]
	b, err := h.state.Store.GetUserBilling(userID)
	if err != nil {
		sendJSONError(w, "Lookup failed", http.StatusInternalServerError)
		return
	}
	get := func(key, def string) string {
		if v, _ := h.state.Store.GetSetting(key); v != "" {
			return v
		}
		return def
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"status":      b.Status,
		"graceUntil":  b.GraceUntil,
		"suspendedAt": b.SuspendedAt,
		"overrides": map[string]interface{}{
			"gracePeriod":       b.GracePeriod,
			"r2Retention":       b.R2Retention,
			"nodeRetention":     b.NodeRetention,
			"r2QuotaGb":         b.R2QuotaGB,
			"maxNodes":          b.MaxNodes,
			"maxLinks":          b.MaxLinks,
			"trafficEdgeGb":     b.TrafficEdgeGB,
			"trafficRelayGb":    b.TrafficRelayGB,
			"trafficCombinedGb": b.TrafficCombinedGB,
		},
		"defaults": map[string]interface{}{
			"gracePeriod":   get(services.BillingGracePeriodKey, services.DefaultGracePeriod),
			"r2Retention":   get(services.BillingR2RetentionKey, services.DefaultR2Retention),
			"nodeRetention": get(services.BillingNodeRetentionKey, services.DefaultNodeRetention),
			// Raw, and read from where the allowance now lives: "" is "never
			// saved" and means no cap, which is not a cap of 0. It defaulted to
			// "0" here, which told the panel the platform hands out no backup
			// storage at all - and the panel rendered that as "default
			// (unlimited)", the exact opposite.
			"r2QuotaGb": get(services.SettingBackupDefaultUserQuota, ""),
		},
	})
}

// trafficCell is one POOL: the unit an allowance is resolved for and compared
// against.
type trafficCell struct {
	region, kind string
	bytes        int64
	// byProduct describes who filled the pool. Never a second pool - see below.
	byProduct map[string]int64
}

// foldTrafficCells collapses the stored breakdown rows into the pools that are
// actually judged.
//
// Two dimensions are folded away here, and both would multiply an allowance if
// they were not:
//
//   - REGION, for kinds that are not regional. File transfers hold one global
//     pool, so leaving them split would hand the tenant the whole allowance
//     once per region their transfers happened to be attributed to.
//   - PRODUCT, always. The included traffic is granted per unit HELD and pooled
//     across products, so a tenant with a node and a route-only address has one
//     allowance and not one each. This is the reason the product split was safe
//     to add at all, and it is the property to check first if a pool ever starts
//     showing twice the ceiling it should.
//
// Order follows first appearance, which is bytes-descending as the store
// returns it, so the busiest pool is drawn first.
func foldTrafficCells(cells []store.RegionUsage) []trafficCell {
	type key struct{ region, kind string }
	at := map[key]int{}
	out := make([]trafficCell, 0, len(cells))
	for _, c := range cells {
		k := key{services.TrafficLimitRegion(c.Region, c.Kind), c.Kind}
		i, seen := at[k]
		if !seen {
			i = len(out)
			at[k] = i
			out = append(out, trafficCell{region: k.region, kind: k.kind, byProduct: map[string]int64{}})
		}
		out[i].bytes += c.Bytes
		out[i].byProduct[c.Product] += c.Bytes
	}
	return out
}

// withIdleAllowances adds the pools a tenant holds but has not moved bytes in.
//
// The stored breakdown only has a row once traffic has flowed, and a screen
// draws what it is given - so a pool with no usage this month vanished from the
// panel entirely. That is how "where is my data traffic" happens: nothing had
// gone through the relay, so the whole data-traffic panel disappeared and the
// reader could not tell "you have used none" from "this is broken". A zero is
// an answer; an absent panel is not.
//
// Only NON-REGIONAL kinds get this, and that is a decision rather than
// convenience. File transfers hold ONE global pool that every tenant has
// whether or not they have used it, so a zero there is a fact about them.
// Player traffic is capped per region, and a tenant does not hold a region -
// players either connect through it or they do not. Seeding those from the
// configured rows would draw a bar for every region an operator has ever
// priced, most of which will never carry a byte for this tenant.
//
// Usage always wins: a cell that already has bytes is left exactly as folded,
// so this can only ever ADD a zero, never restate a number. A pool with no
// configured allowance is still added - it renders as usage without a bar,
// which is the honest shape for "nothing has moved and nothing caps it".
func withIdleAllowances(cells []trafficCell) []trafficCell {
	for _, kind := range []string{store.TrafficKindRelay} {
		if services.RegionalKind(kind) {
			continue
		}
		region := services.TrafficLimitRegion("", kind)
		found := false
		for _, c := range cells {
			if c.kind == kind && c.region == region {
				found = true
				break
			}
		}
		if !found {
			cells = append(cells, trafficCell{region: region, kind: kind, byProduct: map[string]int64{}})
		}
	}
	return cells
}
