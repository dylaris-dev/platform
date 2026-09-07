package handlers

import (
	"dylaris-core/services"
	"dylaris-core/store"
	"encoding/json"
	"net/http"
	"time"
)

const usageGiB = int64(1024 * 1024 * 1024)

// usageView inlines a tenant's metered usage and adds their effective limits plus
// per-channel "over" warn flags (traffic is warn-only — these drive a UI banner,
// not a block). A 0 limit means unlimited, so it never flags.
type usageView struct {
	*store.TrafficUsage
	Limits services.Limits `json:"limits"`
	Over   map[string]bool `json:"over"`
}

func (h *UsageHandler) viewFor(u *store.TrafficUsage) usageView {
	lim, _ := services.EffectiveLimits(h.state.Store, u.UserID)
	// EffectiveLimits answers from the per-user overrides alone, which for
	// backup storage is one step of a four-step chain. The card drew that
	// number and the gate refused against the resolved one, so a tenant on an
	// entitlement or on the operator's allowance was never shown as over while
	// their backups were being refused - and an administrator, who has no
	// ceiling at all, was shown one. Both readers now resolve the same way.
	lim.R2QuotaGB = services.BackupAllowanceGB(h.state.Store, u.UserID, h.state.StoreEnabled)
	// nil is no cap; a 0 cap is a real one and is exceeded by the first byte.
	over := func(limGB *int64, bytes int64) bool {
		return limGB != nil && bytes > *limGB*usageGiB
	}
	return usageView{
		TrafficUsage: u,
		Limits:       lim,
		Over: map[string]bool{
			"edge":     over(lim.TrafficEdgeGB, u.EdgeBytes),
			"relay":    over(lim.TrafficRelayGB, u.RelayBytes),
			"combined": over(lim.TrafficCombinedGB, u.EdgeBytes+u.RelayBytes),
			"r2":       over(lim.R2QuotaGB, u.BackupBytes),
		},
	}
}

// UsageHandler serves per-tenant traffic + storage usage for the BYON billing UI.
// In solo/hoster mode nothing is metered, so reads return zero rows.
type UsageHandler struct {
	state *AppState
}

func NewUsageHandler(state *AppState) *UsageHandler {
	return &UsageHandler{state: state}
}

// usagePeriod resolves the billing month from an optional ?period=YYYY-MM query,
// defaulting to the current UTC month (the aggregator's key).
func usagePeriod(r *http.Request) time.Time {
	if p := r.URL.Query().Get("period"); p != "" {
		if t, err := time.Parse("2006-01", p); err == nil {
			return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
	}
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// GetMyUsage GET /api/me/usage — the caller's metered usage for the period.
// Returns a zero-value row when nothing has been metered yet.
func (h *UsageHandler) GetMyUsage(w http.ResponseWriter, r *http.Request) {
	userID := byonCallerID(r)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	u, err := h.state.Store.GetTrafficUsage(userID, usagePeriod(r))
	if err != nil {
		sendJSONError(w, "Lookup failed", http.StatusInternalServerError)
		return
	}
	// entitlement is what the tenant HOLDS against what they bought, which is a
	// different question from traffic usage and has its own deadline. Served here
	// because this is the one endpoint the panel already polls for limits, so the
	// banner does not need a second round trip on every page.
	payload := map[string]interface{}{
		"success": true,
		"usage":   h.viewFor(u),
	}
	if ent := h.entitlementStateFor(userID); ent != nil {
		payload["entitlementState"] = ent
	}
	json.NewEncoder(w).Encode(payload)
}

// entitlementStateFor reports an over-limit tenant and when their grace ends.
// Returns nil for the normal case so the panel has nothing to render and no
// shape to special-case.
func (h *UsageHandler) entitlementStateFor(userID string) map[string]interface{} {
	b, err := h.state.Store.GetUserBilling(userID)
	if err != nil || b == nil || b.OverLimitSince == nil {
		return nil
	}
	return map[string]interface{}{
		"overLimit":      true,
		"overLimitSince": b.OverLimitSince,
		"cutoffAt":       b.OverLimitSince.Add(services.OverLimitGrace),
	}
}

// GetAllUsage GET /api/admin/usage - RequireCap("plans.read") at the route.
// Every tenant's usage for the period, busiest first.
func (h *UsageHandler) GetAllUsage(w http.ResponseWriter, r *http.Request) {
	period := usagePeriod(r)
	list, err := h.state.Store.ListTrafficUsage(period)
	if err != nil {
		sendJSONError(w, "Lookup failed", http.StatusInternalServerError)
		return
	}
	views := make([]usageView, 0, len(list))
	for i := range list {
		views = append(views, h.viewFor(&list[i]))
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"period":  period.Format("2006-01"),
		"usage":   views,
	})
}
