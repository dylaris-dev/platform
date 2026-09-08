package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"dylaris-core/authz"
)

// FeatureSettingsHandler is the admin GET/PUT bundle for platform-wide
// feature toggles. Keeping all toggles behind a single endpoint avoids
// scattering "feature_*_enabled" into per-feature settings handlers and
// gives the panel a single round-trip to render the Features admin tab.
type FeatureSettingsHandler struct {
	state *AppState
}

func NewFeatureSettingsHandler(state *AppState) *FeatureSettingsHandler {
	return &FeatureSettingsHandler{state: state}
}

// featureSettingsPayload is the wire shape of GET + PUT. New platform-wide
// feature flags get added here as we ship them.
//
// Every flag is a POINTER on the way in: absent means "this request is not
// about that flag" and the stored value is left alone. GET always fills all of
// them, so a reader sees plain booleans.
//
// It became partial because the screen it serves had grown into one card with
// eleven switches and a capability matrix, all sharing a single save. Splitting
// that into tabs the operator can reason about - subsystems, infrastructure,
// API keys - is only safe if each tab can save its own half without sending a
// stale copy of the other tab's. With whole-payload writes, two admins on two
// tabs would each silently revert the other.
type featureSettingsPayload struct {
	Tickets  *bool `json:"tickets"`
	Modpacks *bool `json:"modpacks"`
	// Library is the shared file catalog. It had no flag at all until now - the
	// module row was its only switch, and that hides a navbar entry rather than
	// closing anything.
	Library *bool `json:"library"`
	// ModpackAuthoring opens modpack authoring to non-admin users. Meaningless
	// on its own: the subsystem must be on (Modpacks) for anyone, admin or not,
	// to author at all.
	ModpackAuthoring *bool `json:"modpackAuthoring"`
	// ApplyAuthoringToManual decides what a change to ModpackAuthoring does to
	// users whose per-user flag an admin set BY HAND. false (the safe default)
	// leaves those rows alone; true overwrites them and drops their manual
	// marker, so they follow the global switch again from here on. Write-only:
	// it is an instruction for this request, not stored state.
	ApplyAuthoringToManual bool  `json:"applyAuthoringToManual"`
	AutoMove               *bool `json:"autoMove"`
	Byon                   *bool `json:"byon"`
	// NO metrics flag here, deliberately. Long-term statistics are switched on
	// by MetricsDBHandler, together with the database they record into, because
	// those two are one decision: the resolution is fixed at the moment
	// recording starts and nothing can be backfilled, so a switch that could be
	// flipped without naming a target was a way to lose the only chance to
	// choose. `feature_metrics_enabled` is still the setting key - only who
	// writes it moved.
	// UserAPIKeys decides whether a NON-ADMIN may hold an API key at all. It is
	// enforced at mint AND at use (see APIKeysHandler.ownerStillHolds): turning
	// it off has to stop keys that already exist, or an operator who switched it
	// off would still be running every key created before that.
	UserAPIKeys *bool `json:"userApiKeys"`
	// UserAPIKeyAllowedCaps narrows what a non-admin may put on a key, as a
	// comma-separated capability list. EMPTY MEANS NO EXTRA RESTRICTION - the
	// delegation subset check already stops a key from exceeding its creator.
	//
	// A pointer for the same reason as the booleans: an empty string is a
	// meaningful value here (no restriction), so "not sent" needs its own shape
	// or the API-keys tab would have to be saved from every other tab.
	UserAPIKeyAllowedCaps *string `json:"userApiKeyAllowedCaps"`
}

// boolPtr / strPtr fill the GET response, which always states every flag.
func boolPtr(b bool) *bool { return &b }

// pick is the partial-update rule in one place: the submitted value when the
// request mentioned the flag, the stored one when it did not.
func pick(submitted *bool, stored bool) bool {
	if submitted != nil {
		return *submitted
	}
	return stored
}

// Get GET /api/admin/settings/features — current bundle of platform toggles.
// PANEL settings.read (RequireCap at the route).
func (h *FeatureSettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	caps := strings.Join(h.state.FeatureFlags.UserAPIKeyAllowedCaps(r.Context()), ",")
	out := featureSettingsPayload{
		Tickets:               boolPtr(h.state.FeatureFlags.IsTicketsEnabled(r.Context())),
		Modpacks:              boolPtr(h.state.FeatureFlags.IsModpacksEnabled(r.Context())),
		Library:               boolPtr(h.state.FeatureFlags.IsLibraryEnabled(r.Context())),
		ModpackAuthoring:      boolPtr(h.state.FeatureFlags.IsModpackAuthoringEnabled(r.Context())),
		AutoMove:              boolPtr(h.state.FeatureFlags.IsAutoMoveEnabled(r.Context())),
		Byon:                  boolPtr(h.state.FeatureFlags.IsBYONEnabled(r.Context())),
		UserAPIKeys:           boolPtr(h.state.FeatureFlags.UserAPIKeysEnabled(r.Context())),
		UserAPIKeyAllowedCaps: &caps,
	}
	resp := map[string]interface{}{
		"success":  true,
		"features": out,
	}
	// How many ENABLED ticket categories exist, because switching Tickets on
	// without one produces a support system that accepts nothing: creating a
	// ticket requires a valid, enabled category, so every customer attempt
	// fails with "Unknown or disabled category" and the operator's own screen
	// looks fine. Measured in production on 2026-09-03: the flag was off and
	// ticket_categories held zero rows, so turning it on would have shipped
	// exactly that.
	//
	// Same shape as accountsMissingSecurityQuestions on the auth policy: a
	// number the screen needs in order to say what a switch will really do.
	// Best-effort - a count that cannot be read omits the hint rather than
	// failing the settings screen.
	if h.state.Store != nil {
		if cats, err := h.state.Store.ListTicketCategories(false); err == nil {
			resp["enabledTicketCategories"] = len(cats)
		} else {
			log.Printf("feature-settings: could not count ticket categories: %v", err)
		}
	}
	json.NewEncoder(w).Encode(resp)
}

// Set PUT /api/admin/settings/features — write the bundle. Each setting key
// is persisted, its cached flag is invalidated, and a features.changed event
// is published so the panel re-renders banners / nav gating.
// PANEL settings.write (RequireCap at the route).
func (h *FeatureSettingsHandler) Set(w http.ResponseWriter, r *http.Request) {
	var req featureSettingsPayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Auto-move only works while the gateway is routing (it's what keeps a
	// server's address stable across a node change). Refuse the whole PUT
	// rather than partially apply, so the admin gets a single clear error and
	// no flag is half-written.
	if req.AutoMove != nil && *req.AutoMove && !h.state.gatewayEnabled() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "gateway_required",
			"message": "Auto-move can only be enabled while gateway routing is active.",
		})
		return
	}

	// Enabling Tickets requires Core file storage (attachments + backups need a
	// durable off-host home). Refuse the whole PUT so nothing is half-written.
	if req.Tickets != nil && *req.Tickets && !h.state.CoreStorageConfigured() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "core_storage_required",
			"message": "Configure Core file storage (Settings -> Core file storage) before enabling Tickets.",
		})
		return
	}

	// The values that will be in force after this request: what it submitted,
	// or what is stored for anything it did not mention.
	ticketsNow := pick(req.Tickets, h.state.FeatureFlags.IsTicketsEnabled(r.Context()))
	modpacksNow := pick(req.Modpacks, h.state.FeatureFlags.IsModpacksEnabled(r.Context()))
	libraryNow := pick(req.Library, h.state.FeatureFlags.IsLibraryEnabled(r.Context()))
	authoringWas := h.state.FeatureFlags.IsModpackAuthoringEnabled(r.Context())
	authoringNow := pick(req.ModpackAuthoring, authoringWas)

	// End-user authoring without the subsystem is not a state that means
	// anything: nobody, admin or user, can author while modpacks are off. Rather
	// than store a flag that does nothing (and would quietly take effect the day
	// modpacks are switched on), fold it down here.
	//
	// The fold can move a flag this request never mentioned - switching Modpacks
	// off has to take authoring with it - so it forces the write rather than
	// relying on the field being present.
	forceAuthoringWrite := false
	if !modpacksNow && authoringNow {
		authoringNow = false
		forceAuthoringWrite = true
	}

	// Whether the authoring flag is actually moving, read BEFORE the writes. The
	// per-user bulk apply only runs on a transition: re-saving the Features form
	// with authoring unchanged must not re-flatten per-user rows.
	authoringChanged := authoringWas != authoringNow

	writes := []struct {
		key      string
		val      bool
		send     bool
		cacheKey string
		evtName  string
	}{
		{"feature_tickets_enabled", ticketsNow, req.Tickets != nil, "feature_tickets_enabled", "tickets"},
		{"feature_modpacks_enabled", modpacksNow, req.Modpacks != nil, "feature_modpacks_enabled", "modpacks"},
		{"feature_library_enabled", libraryNow, req.Library != nil, "feature_library_enabled", "library"},
		{"feature_modpack_authoring_enabled", authoringNow, req.ModpackAuthoring != nil || forceAuthoringWrite, "feature_modpack_authoring_enabled", "modpackAuthoring"},
		{"feature_auto_move_enabled", pick(req.AutoMove, false), req.AutoMove != nil, "feature_auto_move_enabled", "autoMove"},
		{"feature_byon_enabled", pick(req.Byon, false), req.Byon != nil, "feature_byon_enabled", "byon"},
		{"apikeys_user_enabled", pick(req.UserAPIKeys, false), req.UserAPIKeys != nil, "apikeys_user_enabled", "userApiKeys"},
	}
	for _, kv := range writes {
		if !kv.send {
			continue
		}
		if err := h.state.Store.SetSetting(kv.key, boolStr(kv.val)); err != nil {
			sendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		h.state.FeatureFlags.Invalidate(kv.cacheKey)
		h.state.Events.Publish(r.Context(), "features.changed", map[string]interface{}{
			"feature": kv.evtName,
			"enabled": kv.val,
		})
	}

	// The capability whitelist is a string, so it does not fit the boolean write
	// loop above. Unknown or PANEL capabilities are dropped rather than refused:
	// the list is a NARROWING filter, and an entry that no key could carry
	// anyway narrows nothing. Refusing the whole save over one stale id would
	// make the field impossible to edit after a capability is renamed.
	if req.UserAPIKeyAllowedCaps != nil {
		if err := h.state.Store.SetSetting("apikeys_user_allowed_caps", sanitizeKeyCapList(*req.UserAPIKeyAllowedCaps)); err != nil {
			sendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		h.state.FeatureFlags.Invalidate("apikeys_user_allowed_caps")
	}

	// Bring the per-user column in line with the new global answer, so the users
	// list shows the truth rather than a stale TRUE default from before the split.
	// Only on a transition, and non-fatal: the flag above is the authoritative
	// gate (RequireUserCanCreateModpacks checks it first), so a failed bulk write
	// leaves the platform correctly gated, just with rows the admin may need to
	// re-apply. Reporting it is better than 500-ing a settings save that already
	// took effect.
	usersChanged := int64(0)
	if authoringChanged {
		n, err := h.state.Store.BulkSetCanCreateModpacks(authoringNow, req.ApplyAuthoringToManual)
		if err != nil {
			log.Printf("features: modpack authoring bulk apply (to=%v includeManual=%v) failed: %v",
				authoringNow, req.ApplyAuthoringToManual, err)
		}
		usersChanged = n
	}

	// Keep the navbar module in step: it appears when the subsystem is on, and
	// widens from admin-only to everyone when authoring opens. Non-fatal for the
	// same reason - the row is presentation, the flags are the gate.
	if err := syncModpackModule(h.state, modpacksNow, authoringNow); err != nil {
		log.Printf("features: syncing the Modpacks module row failed: %v", err)
	}

	// Tickets and Library follow the same rule, for enabled only - who sees them
	// stays the operator's choice in Settings -> Modules. Before this, Tickets
	// had two independent switches and a module row left off while the feature
	// was on produced a working ticket system with no way into it.
	for _, m := range []struct {
		name string
		on   bool
	}{
		{ticketsModuleName, ticketsNow},
		{libraryModuleName, libraryNow},
	} {
		if err := syncModuleEnabled(h.state, m.name, m.on); err != nil {
			log.Printf("features: syncing the %s module row failed: %v", m.name, err)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"features": req,
		// How many user rows the authoring toggle actually rewrote, so the admin
		// gets "42 users updated" instead of guessing what the switch did.
		"usersChanged": usersChanged,
	})
}

// sanitizeKeyCapList normalizes the operator whitelist: trimmed, de-duplicated,
// and filtered to capabilities a key could actually carry. See the call site
// for why an unknown id is dropped instead of refused.
func sanitizeKeyCapList(raw string) string {
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] || !authz.ValidKeyCap(p) {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return strings.Join(out, ",")
}
