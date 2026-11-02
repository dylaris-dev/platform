package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/authz"
	"dylaris-core/handlers/httpx"
	"dylaris-core/services"
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
	// RouteOnly is written only when sent. Unset it follows Byon, so a save that
	// wrote it along with the other switches would pin it for good.
	RouteOnly *bool `json:"routeOnly"`
	// ProxyProtocol opens the real player IP switches. Off until the gateway
	// pieces that carry the address are deployed.
	ProxyProtocol *bool `json:"proxyProtocol"`
	// VoiceChat is the global scope of the voice chat permission; role, user
	// and server rows in voice_permissions override it.
	VoiceChat *bool `json:"voiceChat"`
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
	// ModConfigs is who is offered the beta config editor: off, admin or all.
	ModConfigs *string `json:"modConfigs"`
	// Bedrock is who may give a route Bedrock (the gateway's Geyser): off,
	// admin or all. Its own switch, apart from voice chat and extra ports.
	Bedrock *string `json:"bedrock"`
	// OwnStorage is who may keep backups and templates on their own S3 target:
	// off, admin or all.
	OwnStorage *string `json:"ownStorage"`
	// Templates is who is offered server templates: off, admin or all. The
	// limits are strings in the platform limit convention ("unlimited", "0" or
	// a number); the storage connection id is 0 for Core storage.
	Templates                   *string `json:"templates"`
	TemplatesMaxPerUser         *string `json:"templatesMaxPerUser"`
	TemplatesMaxSizeMB          *string `json:"templatesMaxSizeMb"`
	TemplatesMaxSizeMBAdmin     *string `json:"templatesMaxSizeMbAdmin"`
	TemplatesContentPolicy      *string `json:"templatesContentPolicy"`
	TemplatesPlatformForUsers   *bool   `json:"templatesPlatformForUsers"`
	TemplatesCreateRequiresRole *bool   `json:"templatesCreateRequiresRole"`
	TemplatesStorageConnection  *int    `json:"templatesStorageConnectionId"`
}

// validTemplateLimit accepts what an operator can type into a limit field.
func validTemplateLimit(v string) bool {
	if v == services.LimitUnlimited {
		return true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return err == nil && n >= 0
}

// validateTemplateFields rejects the whole PUT on the first bad Templates value,
// like the other audience settings, so nothing is half-written.
func validateTemplateFields(req *featureSettingsPayload) string {
	if a := req.Templates; a != nil && *a != "off" && *a != "admin" && *a != "all" {
		return "templates must be off, admin or all"
	}
	if v := req.TemplatesMaxPerUser; v != nil && !validTemplateLimit(*v) {
		return "templatesMaxPerUser must be unlimited or a number of 0 or more"
	}
	for _, f := range []struct {
		name string
		v    *string
	}{{"templatesMaxSizeMb", req.TemplatesMaxSizeMB}, {"templatesMaxSizeMbAdmin", req.TemplatesMaxSizeMBAdmin}} {
		if f.v == nil {
			continue
		}
		if !validTemplateLimit(*f.v) {
			return f.name + " must be unlimited or a number of 0 or more"
		}
		if n, _ := strconv.ParseInt(*f.v, 10, 64); *f.v != services.LimitUnlimited && n > services.TemplatesMaxSizeMBCap {
			return fmt.Sprintf("%s must be at most %d (1 TiB) or unlimited", f.name, services.TemplatesMaxSizeMBCap)
		}
	}
	if v := req.TemplatesContentPolicy; v != nil && *v != services.TemplatePolicyNoJars && *v != services.TemplatePolicyUploads {
		return "templatesContentPolicy must be no-jars or uploads"
	}
	if v := req.TemplatesStorageConnection; v != nil && *v < 0 {
		return "templatesStorageConnectionId must be 0 (Core storage) or a connection id"
	}
	return ""
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
		ModpackAuthoring:      boolPtr(h.state.FeatureFlags.IsModpackAuthoringEnabled(r.Context())),
		AutoMove:              boolPtr(h.state.FeatureFlags.IsAutoMoveEnabled(r.Context())),
		Byon:                  boolPtr(h.state.FeatureFlags.IsBYONEnabled(r.Context())),
		RouteOnly:             boolPtr(h.state.FeatureFlags.IsRouteOnlyEnabled(r.Context())),
		ProxyProtocol:         boolPtr(h.state.FeatureFlags.IsProxyProtocolEnabled(r.Context())),
		VoiceChat:             boolPtr(h.state.FeatureFlags.IsVoiceChatEnabled(r.Context())),
		UserAPIKeys:           boolPtr(h.state.FeatureFlags.UserAPIKeysEnabled(r.Context())),
		UserAPIKeyAllowedCaps: &caps,
	}
	modConfigs := h.state.FeatureFlags.ModConfigsAudience(r.Context())
	out.ModConfigs = &modConfigs
	bedrock := h.state.FeatureFlags.BedrockAudience(r.Context())
	out.Bedrock = &bedrock
	ownStorage := h.state.FeatureFlags.OwnStorageAudience(r.Context())
	out.OwnStorage = &ownStorage
	ts := services.LoadTemplateSettings(r.Context(), h.state.FeatureFlags)
	out.Templates = &ts.Audience
	maxPerUser := services.FormatLimitSetting(nil)
	if ts.MaxPerUser != nil {
		maxPerUser = strconv.Itoa(*ts.MaxPerUser)
	}
	sizeMB := func(b *int64) *string {
		v := services.FormatLimitSetting(nil)
		if b != nil {
			v = strconv.FormatInt(*b>>20, 10)
		}
		return &v
	}
	out.TemplatesMaxPerUser = &maxPerUser
	out.TemplatesMaxSizeMB = sizeMB(ts.MaxSizeBytes)
	out.TemplatesMaxSizeMBAdmin = sizeMB(ts.MaxSizeBytesAdmin)
	out.TemplatesContentPolicy = &ts.ContentPolicy
	out.TemplatesPlatformForUsers = &ts.PlatformForUsers
	out.TemplatesCreateRequiresRole = &ts.CreateRequiresRole
	out.TemplatesStorageConnection = &ts.StorageConnectionID
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
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.ModConfigs != nil && *req.ModConfigs != "off" && *req.ModConfigs != "admin" && *req.ModConfigs != "all" {
		httpx.SendJSONError(w, "modConfigs must be off, admin or all", http.StatusBadRequest)
		return
	}
	if req.Bedrock != nil && *req.Bedrock != "off" && *req.Bedrock != "admin" && *req.Bedrock != "all" {
		httpx.SendJSONError(w, "bedrock must be off, admin or all", http.StatusBadRequest)
		return
	}
	if req.OwnStorage != nil && *req.OwnStorage != "off" && *req.OwnStorage != "admin" && *req.OwnStorage != "all" {
		httpx.SendJSONError(w, "ownStorage must be off, admin or all", http.StatusBadRequest)
		return
	}
	if msg := validateTemplateFields(&req); msg != "" {
		httpx.SendJSONError(w, msg, http.StatusBadRequest)
		return
	}
	// A setting naming a connection that does not exist would make every new
	// template fail; storage connections refuse deletion while named here.
	if id := req.TemplatesStorageConnection; id != nil && *id > 0 {
		conn, err := h.state.Store.GetStorageConnection(*id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("feature settings: look up storage connection %d: %v", *id, err)
			httpx.SendJSONError(w, "Could not check the storage connection", http.StatusInternalServerError)
			return
		}
		if conn == nil {
			httpx.SendJSONError(w, "templatesStorageConnectionId must be 0 (Core storage) or an existing storage connection", http.StatusBadRequest)
			return
		}
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
		{"feature_modpack_authoring_enabled", authoringNow, req.ModpackAuthoring != nil || forceAuthoringWrite, "feature_modpack_authoring_enabled", "modpackAuthoring"},
		{"feature_auto_move_enabled", pick(req.AutoMove, false), req.AutoMove != nil, "feature_auto_move_enabled", "autoMove"},
		{"feature_byon_enabled", pick(req.Byon, false), req.Byon != nil, "feature_byon_enabled", "byon"},
		{"feature_route_only_enabled", pick(req.RouteOnly, false), req.RouteOnly != nil, "feature_route_only_enabled", "routeOnly"},
		{"apikeys_user_enabled", pick(req.UserAPIKeys, false), req.UserAPIKeys != nil, "apikeys_user_enabled", "userApiKeys"},
		{services.ProxyProtocolSettingKey, pick(req.ProxyProtocol, false), req.ProxyProtocol != nil, services.ProxyProtocolSettingKey, "proxyProtocol"},
		{services.VoiceChatSettingKey, pick(req.VoiceChat, false), req.VoiceChat != nil, services.VoiceChatSettingKey, "voiceChat"},
	}
	for _, kv := range writes {
		if !kv.send {
			continue
		}
		if err := h.state.Store.SetSetting(kv.key, boolStr(kv.val)); err != nil {
			httpx.SendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
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
	if req.ModConfigs != nil {
		if err := h.state.Store.SetSetting(services.ModConfigsSettingKey, *req.ModConfigs); err != nil {
			httpx.SendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		h.state.FeatureFlags.Invalidate(services.ModConfigsSettingKey)
		h.state.Events.Publish(r.Context(), "features.changed", map[string]interface{}{
			"feature": "modConfigs",
			"enabled": *req.ModConfigs != "off",
		})
	}

	if req.OwnStorage != nil {
		if err := h.state.Store.SetSetting(services.OwnStorageSettingKey, *req.OwnStorage); err != nil {
			httpx.SendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		h.state.FeatureFlags.Invalidate(services.OwnStorageSettingKey)
		h.state.Events.Publish(r.Context(), "features.changed", map[string]interface{}{
			"feature": "ownStorage",
			"enabled": *req.OwnStorage != "off",
		})
	}

	if req.Bedrock != nil {
		if err := h.state.Store.SetSetting(services.BedrockSettingKey, *req.Bedrock); err != nil {
			httpx.SendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		h.state.FeatureFlags.Invalidate(services.BedrockSettingKey)
		h.state.Events.Publish(r.Context(), "features.changed", map[string]interface{}{
			"feature": "bedrock",
			"enabled": *req.Bedrock != "off",
		})
		// Pause or resume the routes that have Bedrock now rather than at the
		// next reconcile. Non-fatal: the setting is saved, and the reconcile
		// applies it within a minute.
		if h.state.Redis != nil {
			svc := services.NewBedrockService(h.state.Store, h.state.Redis)
			if err := svc.PublishEditions(r.Context(), *req.Bedrock != "off"); err != nil {
				log.Printf("features: bedrock: publish route editions: %v", err)
			}
		}
	}

	if err := h.saveTemplateSettings(r.Context(), &req); err != nil {
		httpx.SendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if req.UserAPIKeyAllowedCaps != nil {
		if err := h.state.Store.SetSetting("apikeys_user_allowed_caps", sanitizeKeyCapList(*req.UserAPIKeyAllowedCaps)); err != nil {
			httpx.SendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
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

	// Tickets follow the same rule, for enabled only - who sees them stays the
	// operator's choice in Settings -> Modules. Before this, Tickets had two
	// independent switches and a module row left off while the feature was on
	// produced a working ticket system with no way into it.
	if err := syncModuleEnabled(h.state, ticketsModuleName, ticketsNow); err != nil {
		log.Printf("features: syncing the %s module row failed: %v", ticketsModuleName, err)
	}

	// Templates: the navbar row follows the audience, enabled and who sees it.
	if err := syncTemplatesModule(h.state, h.state.FeatureFlags.TemplatesAudience(r.Context())); err != nil {
		log.Printf("features: syncing the Templates module row failed: %v", err)
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

// saveTemplateSettings writes whichever Templates settings the request named.
func (h *FeatureSettingsHandler) saveTemplateSettings(ctx context.Context, req *featureSettingsPayload) error {
	type row struct {
		key, evt string
		val      *string
	}
	flag := func(b *bool) *string {
		if b == nil {
			return nil
		}
		v := boolStr(*b)
		return &v
	}
	var storage *string
	if req.TemplatesStorageConnection != nil {
		v := ""
		if *req.TemplatesStorageConnection > 0 {
			v = strconv.Itoa(*req.TemplatesStorageConnection)
		}
		storage = &v
	}
	rows := []row{
		{services.TemplatesSettingKey, "templates", req.Templates},
		{services.TemplatesMaxPerUserKey, "templatesMaxPerUser", req.TemplatesMaxPerUser},
		{services.TemplatesMaxSizeMBKey, "templatesMaxSizeMb", req.TemplatesMaxSizeMB},
		{services.TemplatesMaxSizeMBAdminKey, "templatesMaxSizeMbAdmin", req.TemplatesMaxSizeMBAdmin},
		{services.TemplatesContentPolicyKey, "templatesContentPolicy", req.TemplatesContentPolicy},
		{services.TemplatesPlatformForUsersKey, "templatesPlatformForUsers", flag(req.TemplatesPlatformForUsers)},
		{services.TemplatesCreateRequiresRoleKey, "templatesCreateRequiresRole", flag(req.TemplatesCreateRequiresRole)},
		{services.TemplatesStorageConnectionKey, "templatesStorageConnectionId", storage},
	}
	for _, r := range rows {
		if r.val == nil {
			continue
		}
		if err := h.state.Store.SetSetting(r.key, *r.val); err != nil {
			return err
		}
		h.state.FeatureFlags.Invalidate(r.key)
		h.state.Events.Publish(ctx, "features.changed", map[string]interface{}{
			"feature": r.evt,
			"enabled": *r.val != "off",
		})
	}
	return nil
}
