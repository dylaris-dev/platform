package billing

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"
)

// TrafficLimitHandler exposes the per-(region, kind) traffic limits.
//
// The limits live in Core rather than in the store because the per-user
// override does: users are Core's. Two scopes, most specific first:
// "user:<id>" then "user_default". Stripe keeps the price IDs; Core keeps who
// may use how much, and Core is the ONLY place that number exists - the store
// used to hold a second copy for the pricing page and the two drifted apart.
type TrafficLimitHandler struct {
	state *handlers.AppState
}

func NewTrafficLimitHandler(state *handlers.AppState) *TrafficLimitHandler {
	return &TrafficLimitHandler{state: state}
}

// trafficLimitBody is the wire shape for a write.
//
// Mode, not a bare number, for the same reason the route limit uses one: the
// three states an operator means are "the scope above decides" (default),
// "decided here, no limit" (unlimited) and "decided here, this many"
// (custom, including zero). A bare nullable number cannot tell the first two
// apart, and that ambiguity is what made a stored 0 mean two different things
// depending on which row it sat in.
type trafficLimitBody struct {
	Scope  string `json:"scope"`  // user:<id> | user_default
	Region string `json:"region"` // e.g. eu-central; ignored for non-regional kinds
	Kind   string `json:"kind"`   // edge | relay | warp

	IncludedMode string `json:"includedMode"` // default | unlimited | custom
	IncludedGB   int64  `json:"includedGb"`

	PurchaseMode string `json:"purchaseMode"` // default | unlimited | custom
	PurchaseGB   int64  `json:"purchaseGb"`
}

// ListTrafficLimits GET /api/traffic-limits - every configured row.
//
// Returns rows only. A region with NO row is not listed and that is the honest
// answer: nothing has been decided about it, so nothing limits it. The panel
// has to show that gap against the regions the edges actually report, because
// "no row" and "no limit" look identical from here and only one of them is
// something an operator meant.
func (h *TrafficLimitHandler) ListTrafficLimits(w http.ResponseWriter, r *http.Request) {
	rows, err := h.state.Store.ListTrafficLimits()
	if err != nil {
		http.Error(w, "Failed to read traffic limits", http.StatusInternalServerError)
		return
	}
	if rows == nil {
		// An explicit empty array, not null: the panel maps over this, and a
		// null would have to be defended against at every call site instead of
		// once here.
		rows = []models.TrafficLimit{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"limits":  rows,
	})
}

// ResolveTrafficLimit GET /api/traffic-limits/resolve?user=&region=&kind=
// answers what actually applies, and says which scope decided it.
//
// The PANEL does not call this, and that is not an oversight. The screen shows
// the user_default rows and the user: overrides as two lists side by side, so an
// operator sees which scope holds what by reading the rows themselves rather
// than a computed answer - which is the better version of what this endpoint was
// originally written to give them.
//
// It stays because the scope walk it exposes (services.ResolveTrafficLimit, also
// used by the billing and store handlers) is the one thing an API-key integration
// would otherwise have to reimplement, and reimplementing a precedence rule is
// how two answers to the same question start to disagree.
func (h *TrafficLimitHandler) ResolveTrafficLimit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	region, kind := strings.TrimSpace(q.Get("region")), strings.TrimSpace(q.Get("kind"))
	if region == "" || kind == "" {
		http.Error(w, "region and kind are required", http.StatusBadRequest)
		return
	}
	res, err := services.ResolveTrafficLimit(h.state.Store, strings.TrimSpace(q.Get("user")), region, kind)
	if err != nil {
		http.Error(w, "Failed to resolve traffic limit", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"region":        region,
		"kind":          kind,
		"includedGb":    res.IncludedGB,
		"maxPurchaseGb": res.MaxPurchaseGB,
		"decidedBy":     res.Scope, // "" = nothing decided anywhere
	})
}

// SetTrafficLimit PUT /api/traffic-limits.
//
// "default" for BOTH values deletes the row, because a row that says nothing
// about either question is a row that should not exist - leaving it would make
// the scope answer with two NULLs, which means "no limit", the opposite of
// deferring. That is the one transition where storing and deleting differ, and
// it is the one an operator reaches for when undoing an override.
func (h *TrafficLimitHandler) SetTrafficLimit(w http.ResponseWriter, r *http.Request) {
	var req trafficLimitBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	req.Scope = strings.TrimSpace(req.Scope)
	req.Region = strings.TrimSpace(req.Region)
	req.Kind = strings.TrimSpace(req.Kind)
	if req.Scope == "" || req.Region == "" || req.Kind == "" {
		http.Error(w, "scope, region and kind are required", http.StatusBadRequest)
		return
	}
	if !validTrafficScope(req.Scope) {
		http.Error(w, "scope must be user_default or user:<id>", http.StatusBadRequest)
		return
	}
	// Non-regional kinds are folded onto the one row BEFORE validation, so a
	// client that sends a concrete region for file transfers still writes the
	// row that gets read. Accepting it as typed would store a limit under a
	// region the resolver never asks about: a number an operator set, on a
	// screen that showed it back, that nothing enforces.
	req.Region = services.TrafficLimitRegion(req.Region, req.Kind)
	// A region or kind nobody writes is a limit that limits nothing, and it
	// looks identical to a working one on the settings screen. The producers
	// normalise to [a-z0-9-] (meterRegion, on the edge and the relay), so
	// anything else here could never match a counter.
	if req.Region != services.TrafficRegionAny && !validTrafficLabel(req.Region) {
		http.Error(w, "region must be lowercase letters, digits and dashes", http.StatusBadRequest)
		return
	}
	if !validTrafficLabel(req.Kind) {
		http.Error(w, "kind must be lowercase letters, digits and dashes", http.StatusBadRequest)
		return
	}

	included, err := limitFromMode(req.IncludedMode, req.IncludedGB)
	if err != nil {
		http.Error(w, "includedMode: "+err.Error(), http.StatusBadRequest)
		return
	}
	purchase, err := limitFromMode(req.PurchaseMode, req.PurchaseGB)
	if err != nil {
		http.Error(w, "purchaseMode: "+err.Error(), http.StatusBadRequest)
		return
	}

	// A row is one COMPLETE answer - that is the whole reason both numbers live
	// in one row (see the traffic_limits schema). "default" means this scope
	// says nothing, and a scope cannot say nothing about half a row: storing the
	// deferred side as NULL would make the resolver read it as "decided here, no
	// limit" and stop the walk, so an override meant to cap purchases alone
	// would silently grant unlimited included traffic.
	//
	// Refused rather than guessed: "unlimited" is right there for the operator
	// who does mean no limit, and picking one of the two for them is exactly the
	// ambiguity the mode field exists to remove.
	if (req.IncludedMode == "default") != (req.PurchaseMode == "default") {
		http.Error(w,
			"includedMode and purchaseMode must both be \"default\" or neither: a scope answers a whole row. Use \"unlimited\" for a decided no-limit.",
			http.StatusBadRequest)
		return
	}

	// What the affected tenants were shown before this write. Read now, because
	// after it the old ceiling is gone and there is nothing left to compare.
	// A missing row deliberately reads as nil, "no purchase cap", which is what
	// the resolver would have answered.
	var before *int64
	if row, err := h.state.Store.GetTrafficLimit(req.Scope, req.Region, req.Kind); err == nil && row != nil {
		before = row.MaxPurchaseGB
	}

	if req.IncludedMode == "default" && req.PurchaseMode == "default" {
		if err := h.state.Store.DeleteTrafficLimit(req.Scope, req.Region, req.Kind); err != nil {
			http.Error(w, "Failed to clear traffic limit", http.StatusInternalServerError)
			return
		}
		// Clearing a row is a change like any other: the scope stops answering,
		// so the tenants it covered fall through to whatever the next one says.
		// Their ceiling moved even though nobody typed a number.
		h.notifyPurchaseChange(req.Scope, req.Region, req.Kind, before)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "cleared": true})
		return
	}

	if err := h.state.Store.SetTrafficLimit(req.Scope, req.Region, req.Kind, included, purchase); err != nil {
		http.Error(w, "Failed to save traffic limit", http.StatusInternalServerError)
		return
	}
	h.notifyPurchaseChange(req.Scope, req.Region, req.Kind, before)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// notifyPurchaseChange tells the tenants who agreed to be charged that the most
// they may book has moved.
//
// The AFTER value is re-resolved rather than taken from the request, so a
// cleared row is compared against what now answers instead of against nothing.
// Never fails the operator's save: the limit is already stored, and a
// notification that did not go out is a tenant who was not told rather than a
// change that did not happen.
func (h *TrafficLimitHandler) notifyPurchaseChange(scope, region, kind string, before *int64) {
	var after *int64
	if row, err := h.state.Store.GetTrafficLimit(scope, region, kind); err == nil && row != nil {
		after = row.MaxPurchaseGB
	}
	if n := services.NotifyTrafficPurchaseChanged(h.state.Store, scope, region, kind, before, after); n > 0 {
		log.Printf("traffic-limits: %s/%s/%s purchase cap changed, notified %d tenant(s) with metered traffic on",
			scope, region, kind, n)
	}
}

// limitFromMode turns the three operator-facing modes into the stored value.
//
// "default" and "unlimited" both produce nil HERE; what separates them is
// whether the row exists at all, which the caller decides. Zero is a legal
// custom value and means none - it must survive as a number, never collapse
// into nil, or the one thing an operator can type to switch a feature off
// becomes the thing that switches the check off.
func limitFromMode(mode string, gb int64) (*int64, error) {
	switch strings.TrimSpace(mode) {
	case "", "default", "unlimited":
		return nil, nil
	case "custom":
		if gb < 0 {
			return nil, errNegativeLimit
		}
		v := gb
		return &v, nil
	default:
		return nil, errBadLimitMode
	}
}

// validTrafficScope no longer accepts "global".
//
// It was asked after user_default and could answer nothing user_default could
// not - every byte counted here belongs to a tenant. Two settings doing one job
// is a screen where the value an operator typed stops applying the day somebody
// fills in the other one. Existing rows were folded into user_default by the
// migration in db_traffic.go, and writing a new one is refused rather than
// stored somewhere nothing reads.
func validTrafficScope(scope string) bool {
	if scope == services.TrafficScopeDefault {
		return true
	}
	// A bare "user:" is a scope for nobody: it stores, it never resolves for any
	// user, and it shows on the overrides list as an override of nothing.
	return strings.HasPrefix(scope, "user:") && len(scope) > len("user:")
}

// validTrafficLabel accepts what the producers can actually emit. Both
// meterRegion implementations (edge and beam relay) strip everything outside
// [a-z0-9-], so a label with anything else can never match a counter - and a
// limit that can never match is indistinguishable, on screen, from one that
// works.
func validTrafficLabel(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

var (
	errBadLimitMode  = &limitError{"must be default, unlimited or custom"}
	errNegativeLimit = &limitError{"must not be negative"}
)

type limitError struct{ msg string }

func (e *limitError) Error() string { return e.msg }

// GetDataThrottle GET /api/traffic-limits/data-throttle - the speed beam and
// custom tabs slow to once a tenant without data overage reaches its data
// allowance.
func (h *TrafficLimitHandler) GetDataThrottle(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "mbit": services.DataThrottleMbit(h.state.Store),
		"minMbit": services.DataThrottleMinMbit, "defaultMbit": services.DataThrottleDefaultMbit,
	})
}

// SetDataThrottle PUT /api/traffic-limits/data-throttle {mbit} - at least the
// minimum (20 Mbit/s): slower makes a large upload take days, which is a
// stop in all but name. Applies at the next aggregator tick.
func (h *TrafficLimitHandler) SetDataThrottle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mbit int `json:"mbit"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Mbit < services.DataThrottleMinMbit || req.Mbit > 100000 {
		handlers.SendJSONError(w, fmt.Sprintf("The throttle must be between %d and 100000 Mbit/s", services.DataThrottleMinMbit), http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetSetting(services.DataThrottleSettingKey, strconv.Itoa(req.Mbit)); err != nil {
		handlers.SendJSONError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "mbit": req.Mbit})
}
