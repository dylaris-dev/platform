package health

import (
	"encoding/json"
	"net/http"
)

type SystemFeaturesHandler struct {
	d Deps
}

func NewSystemFeaturesHandler(d Deps) *SystemFeaturesHandler {
	return &SystemFeaturesHandler{d: d}
}

// Get GET /api/system/features
//
// Returns the platform-wide feature toggles the panel needs to gate UI
// without hitting per-resource endpoints first. Read-only, auth-required
// (so anonymous browsers can't fingerprint the platform config), no admin
// requirement.
func (h *SystemFeaturesHandler) Get(w http.ResponseWriter, r *http.Request) {
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	modConfigs := h.d.FeatureFlags.ModConfigsAudience(r.Context())
	bedrock := h.d.FeatureFlags.BedrockAudience(r.Context())
	templates := h.d.FeatureFlags.TemplatesAudience(r.Context())
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"features": map[string]interface{}{
			"modpacks": h.d.FeatureFlags.IsModpacksEnabled(r.Context()),
			// End-user authoring. Exposed separately because "modpacks on" alone
			// means admins-only, and the panel has to be able to tell a user
			// "authoring is closed here" apart from "modpacks are off entirely".
			"modpackAuthoring": h.d.FeatureFlags.IsModpackAuthoringEnabled(r.Context()),
			"tickets":          h.d.FeatureFlags.IsTicketsEnabled(r.Context()),
			// Raw admin flag; the panel ANDs it with the live routing mode,
			// since auto-move is only effective while the gateway is on.
			"autoMove": h.d.FeatureFlags.IsAutoMoveEnabled(r.Context()),
			// BYON tenancy: drives tenant-facing UI (e.g. the server transfer
			// control). Read-only flag; the actual authz is enforced backend-side.
			"byon": h.d.FeatureFlags.IsBYONEnabled(r.Context()),
			// Resolved: unset follows byon. The panel ANDs it with gateway routing.
			"routeOnly": h.d.FeatureFlags.IsRouteOnlyEnabled(r.Context()),
			// Real player IP forwarding: whether the per-server and per-route
			// switches may be shown and used.
			"proxyProtocol": h.d.FeatureFlags.IsProxyProtocolEnabled(r.Context()),
			// Voice chat, GLOBAL scope only. Whether one server may turn voice on
			// is the resolved "allowed" of GET /api/servers/{id}/voice.
			"voiceChat": h.d.FeatureFlags.IsVoiceChatEnabled(r.Context()),
			// Store integration (dylaris.com): true only when both STORE_URL and
			// STORE_SHARED_KEY are set. Gates the connect-store button and the
			// demo account/server UI; off => clean open-core build.
			"store": h.d.StoreEnabled,
			// The SaaS (DYLARIS_HOSTED). Gates subscription, plan and BYON
			// teaser wording that means nothing on a self-hosted install.
			"hosted": h.d.Hosted,
			// Whether modpack archives have anywhere to go. It is the same call
			// every write path makes before answering 424, exposed so the panel
			// can say so before an author builds a pack rather than at the
			// upload that ends it. Not sensitive: it says a backend exists, not
			// what or where it is.
			"modpackStorage": h.d.CoreStorage.ModpackConfigured(),
			// The beta config editor, resolved for THIS caller: the setting is
			// off, admin or all, and the panel only needs to know whether to
			// offer the tab.
			"modConfigs": modConfigs == "all" || (modConfigs == "admin" && isAdmin),
			// Whether THIS caller may give a route Bedrock. Routes that have it
			// keep running while the setting is admin; GET .../bedrock says so.
			"bedrock": bedrock == "all" || (bedrock == "admin" && isAdmin),
			// Templates is the raw audience (off, admin or all), unlike the
			// booleans above: the panel resolves it with the caller's role, and
			// an admin still reaches Templates while it is off.
			"templates": templates,
			// Whether THIS caller may add, test and choose own S3 targets.
			// Data already on one stays reachable either way.
			"ownStorage": h.d.FeatureFlags.OwnStorageAllowed(r.Context(), isAdmin),
		},
	})
}
