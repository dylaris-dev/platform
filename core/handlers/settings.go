package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"dylaris-core/services"

	"github.com/redis/go-redis/v9"
)

type SettingsHandler struct {
	state *AppState
}

func NewSettingsHandler(state *AppState) *SettingsHandler {
	return &SettingsHandler{state: state}
}

// NOTE: the legacy /settings/library CRUD (GetLibrarySettings/SaveLibrarySettings)
// and its /settings/library/test probe (TestLibraryConnection, further down this
// file) were removed. Since the Core-stateless-storage rework, Library reads its
// provider exclusively from the shared Core file storage config
// (handlers.LibraryHandler.buildProvider -> AppState.buildCoreStorageProvider);
// the library_type/library_path/library_s3_* settings these handlers read/wrote
// were dead - SaveLibrarySettings' "success" response changed nothing observable,
// and TestLibraryConnection's "ok" was unrelated to the real (Core storage)
// provider. That live UI trap is gone; configure storage at Settings -> Core
// Storage (CoreStorageHandler, /api/settings/core-storage*) instead.

// --- File Manager Settings ---

// FileManagerSettings holds the four browser transfer ceilings, on the platform
// limit convention: nil is no limit, 0 is none (transfers refused), n is the cap
// in bytes.
//
// They were plain int64 and the enforcement site guarded on `n > 0`, so a stored
// 0 fell through to the built-in default - which made "nobody may upload" say
// two gigabytes, and left no way to express "no limit" at all. Both ends of the
// range were unreachable from the screen that edits them.
type FileManagerSettings struct {
	AdminUploadLimit   *int64 `json:"adminUploadLimit"`   // bytes
	AdminDownloadLimit *int64 `json:"adminDownloadLimit"` // bytes
	UserUploadLimit    *int64 `json:"userUploadLimit"`    // bytes
	UserDownloadLimit  *int64 `json:"userDownloadLimit"`  // bytes
}

var defaultFileManagerSettings = FileManagerSettings{
	AdminUploadLimit:   services.LimitPtr(2 * 1024 * 1024 * 1024), // 2 GB
	AdminDownloadLimit: services.LimitPtr(5 * 1024 * 1024 * 1024), // 5 GB
	UserUploadLimit:    services.LimitPtr(500 * 1024 * 1024),      // 500 MB
	UserDownloadLimit:  services.LimitPtr(1 * 1024 * 1024 * 1024), // 1 GB
}

// GetFileManagerSettings GET /api/settings/filemanager - PANEL settings.read (RequireCap at the route).
func (h *SettingsHandler) GetFileManagerSettings(w http.ResponseWriter, r *http.Request) {
	settings := h.loadFileManagerSettings()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"settings": settings,
	})
}

// SaveFileManagerSettings POST /api/settings/filemanager - PANEL settings.write (RequireCap at the route).
func (h *SettingsHandler) SaveFileManagerSettings(w http.ResponseWriter, r *http.Request) {
	var req FileManagerSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Through the shared formatter, so a nil cap stores the word rather than an
	// empty string: empty means "never saved" and would be read back as the
	// default, silently discarding a deliberate "no limit".
	pairs := []struct{ k, v string }{
		{"fm.admin_upload_limit", services.FormatLimitSetting(req.AdminUploadLimit)},
		{"fm.admin_download_limit", services.FormatLimitSetting(req.AdminDownloadLimit)},
		{"fm.user_upload_limit", services.FormatLimitSetting(req.UserUploadLimit)},
		{"fm.user_download_limit", services.FormatLimitSetting(req.UserDownloadLimit)},
	}

	for _, p := range pairs {
		if err := h.state.Store.SetSetting(p.k, p.v); err != nil {
			sendJSONError(w, "Failed to save setting: "+p.k, http.StatusInternalServerError)
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *SettingsHandler) loadFileManagerSettings() FileManagerSettings {
	settings := defaultFileManagerSettings

	// ParseLimitSetting, not a hand-rolled Sscanf with `n > 0`. That guard is
	// what threw away a stored 0 - the one value meaning "none" was the one value
	// that fell back to the default.
	getLimit := func(key string, def *int64) *int64 {
		val, err := h.state.Store.GetSetting(key)
		if err != nil {
			return def
		}
		return services.ParseLimitSetting(val, def)
	}

	settings.AdminUploadLimit = getLimit("fm.admin_upload_limit", settings.AdminUploadLimit)
	settings.AdminDownloadLimit = getLimit("fm.admin_download_limit", settings.AdminDownloadLimit)
	settings.UserUploadLimit = getLimit("fm.user_upload_limit", settings.UserUploadLimit)
	settings.UserDownloadLimit = getLimit("fm.user_download_limit", settings.UserDownloadLimit)

	return settings
}

// GetUploadLimitForUser returns the upload cap for the current user. nil means
// no limit; 0 means uploads are refused.
func (h *SettingsHandler) GetUploadLimitForUser(isAdmin bool) *int64 {
	settings := h.loadFileManagerSettings()
	if isAdmin {
		return settings.AdminUploadLimit
	}
	return settings.UserUploadLimit
}

// GetDownloadLimitForUser returns the download cap for the current user. nil
// means no limit; 0 means downloads are refused.
func (h *SettingsHandler) GetDownloadLimitForUser(isAdmin bool) *int64 {
	settings := h.loadFileManagerSettings()
	if isAdmin {
		return settings.AdminDownloadLimit
	}
	return settings.UserDownloadLimit
}

// GetUserLimits GET /api/settings/filemanager/limits — available to ALL authenticated users
func (h *SettingsHandler) GetUserLimits(w http.ResponseWriter, r *http.Request) {
	isAdmin := IsAdmin(r)
	settings := h.loadFileManagerSettings()

	var uploadLimit, downloadLimit *int64
	if isAdmin {
		uploadLimit = settings.AdminUploadLimit
		downloadLimit = settings.AdminDownloadLimit
	} else {
		uploadLimit = settings.UserUploadLimit
		downloadLimit = settings.UserDownloadLimit
	}

	// null on the wire is "no limit", which is what the panel's LimitField reads
	// as its unlimited state.
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"uploadLimit":   uploadLimit,
		"downloadLimit": downloadLimit,
	})
}

// --- Feature Settings ---

// FeatureSettings carries only the MC-proxy toggle. There is deliberately no
// gateway flag here: the gateway is on exactly when routing_mode is gateway or
// both (see AppState.gatewayEnabled), which is what every gateway gate checks.
// A second stored flag could only ever disagree with it.
type FeatureSettings struct {
	ProxyEnabled bool `json:"proxyEnabled"`
}

// GetFeatureSettings GET /api/settings/features - which optional features are
// switched on. Readable by any authenticated user because the panel needs it
// to decide what to render at all.
func (h *SettingsHandler) GetFeatureSettings(w http.ResponseWriter, r *http.Request) {
	settings := h.LoadFeatureSettings()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"settings": settings,
	})
}

// SaveFeatureSettings POST /api/settings/features - PANEL settings.write (RequireCap at the route).
func (h *SettingsHandler) SaveFeatureSettings(w http.ResponseWriter, r *http.Request) {
	var req FeatureSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	proxyVal := "false"
	if req.ProxyEnabled {
		proxyVal = "true"
	}

	if err := h.state.Store.SetSetting("feature_proxy_enabled", proxyVal); err != nil {
		sendJSONError(w, "Failed to save setting", http.StatusInternalServerError)
		return
	}

	h.state.Events.Publish(r.Context(), "features.changed", nil)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// LoadFeatureSettings reads feature flags from the database. Available to all authenticated users.
func (h *SettingsHandler) LoadFeatureSettings() FeatureSettings {
	proxyVal, _ := h.state.Store.GetSetting("feature_proxy_enabled")
	return FeatureSettings{
		ProxyEnabled: proxyVal != "false", // default true
	}
}

// --- Gateway Settings ---

type GatewaySettings struct {
	Limits               GatewayLimits  `json:"limits"`
	HosterDomains        []HosterDomain `json:"hosterDomains"`
	CustomDomainsEnabled bool           `json:"customDomainsEnabled"`
	// CnameTarget is a single DNS LABEL (e.g. "route"), not a full domain. It is
	// expanded per hoster domain into route.<base>, so one setting covers every
	// region: a user picks whichever base matches the region they want.
	CnameTarget string `json:"cnameTarget"`
	// BlockedRoutePrefixes are leftmost labels users may not register as a route
	// (e.g. "admin", "dylaris"). Applies to the hoster-subdomain picker and the
	// leftmost label of custom/raw domains. Even though only :25565 MC traffic is
	// routed, reserving these keeps confusable / impersonating names off the table.
	BlockedRoutePrefixes []string `json:"blockedRoutePrefixes"`
}

// defaultBlockedRoutePrefixes seeds a protective reserved list when the admin has
// never saved one. Once saved (even empty), the admin's list wins.
// "route" is reserved because it is the default custom-domain CNAME label: a
// user who claimed route.<base> would take over the very name other users are
// told to point their own domains at.
// The second group is the set a hoster wants for its OWN flagship server and a
// tenant must not be able to take first: "play.<base>" and "mc.<base>" read as
// the platform's official address, not as one customer's. Admins are exempt
// (resolveRouteDomain's allowReserved), so reserving them costs the operator
// nothing and keeps them available.
var defaultBlockedRoutePrefixes = []string{
	"admin", "dylaris", "app", "api", "www", "panel", "gateway", "edge", "hub",
	"link", "warp", "beam", "mail", "ns", "status", "support", "staff", "system", "root",
	"route",
	"minecraft", "mc", "play", "server", "store", "shop", "billing", "account",
	"login", "auth", "cdn", "docs", "help",
}

// GatewayLimits are the platform-wide address caps, one per scope.
//
// Each is a POINTER so the wire can carry the platform convention intact:
// null is no cap, 0 is none, n is the cap (see services.Limits). The panel used
// to spell "unlimited" as -1 here while the user scope spelled "disabled" as 0,
// so the same field meant different things in different rows and neither could
// express the third state at all.
type GatewayLimits struct {
	Global      *int `json:"global"`
	UserDefault *int `json:"userDefault"`
	// PerServer and PortMc were here, stored and read back, and enforced by
	// nothing: no code in either repository ever read the per_server or
	// port:25565 scope. A control that promises a cap and applies none is worse
	// than an absent one - an operator sets it, believes the cap is in force,
	// and nothing anywhere says otherwise. Removed rather than wired up, because
	// capping addresses per MC server is a feature to decide on, not a
	// regression to repair. PortMcEnabled stays: that one IS enforced.
	PortMcEnabled bool `json:"portMcEnabled"`
}

// HosterDomain is one of the platform-provided base domains under which a
// user can register a route by entering only a subdomain. The validation
// mode controls what characters are accepted in that subdomain field.
type HosterDomain struct {
	Domain     string `json:"domain"`     // e.g. "dylaris.com"
	Validation string `json:"validation"` // "letters" | "alphanumeric" | "dns"
}

// validHosterValidation returns true for the three accepted modes —
// kept narrow on purpose so future modes have to be added explicitly.
func validHosterValidation(v string) bool {
	return v == "letters" || v == "alphanumeric" || v == "dns"
}

// GetGatewaySettings GET /api/settings/gateway - PANEL settings.read (RequireCap at the route).
func (h *SettingsHandler) GetGatewaySettings(w http.ResponseWriter, r *http.Request) {
	getSetting := func(key string) string {
		val, _ := h.state.Store.GetSetting(key)
		return val
	}

	// No row and a NULL row both read as "no cap" to the panel. They differ only
	// for the scope WALK (a missing row defers to the next scope, a NULL one
	// answers), and these four are the scopes being walked TO - there is nothing
	// below them to defer to, so the distinction has no meaning here.
	getLimit := func(scope string) *int {
		l, err := h.state.Store.GetGatewayRouteLimit(scope)
		if err != nil || l == nil {
			return nil
		}
		return l.MaxRoutes
	}

	settings := GatewaySettings{
		Limits: GatewayLimits{
			Global:        getLimit("global"),
			UserDefault:   getLimit("user_default"),
			PortMcEnabled: getSetting("gateway_port_mc_enabled") != "false",
		},
		HosterDomains:        h.loadHosterDomains(),
		CustomDomainsEnabled: getSetting("gateway_custom_domains_enabled") == "true",
		CnameTarget:          getSetting("gateway_cname_target"),
		BlockedRoutePrefixes: h.loadBlockedRoutePrefixes(),
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"settings": settings,
	})
}

// loadHosterDomains parses the persisted hoster-domain list from settings.
// Returns an empty slice if unset or malformed.
func (h *SettingsHandler) loadHosterDomains() []HosterDomain {
	raw, _ := h.state.Store.GetSetting("gateway_hoster_domains")
	if raw == "" {
		return []HosterDomain{}
	}
	var out []HosterDomain
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []HosterDomain{}
	}
	return out
}

// loadBlockedRoutePrefixes parses the persisted reserved-prefix list. When the
// setting was never saved (raw == "") it falls back to the protective default;
// once the admin saves a list (even an empty one) that explicit choice is used.
func (h *SettingsHandler) loadBlockedRoutePrefixes() []string {
	raw, _ := h.state.Store.GetSetting("gateway_blocked_route_prefixes")
	if raw == "" {
		return append([]string(nil), defaultBlockedRoutePrefixes...)
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	return out
}

// LoadGatewayDomainConfig is the read-side helper for code paths that need
// just the domain configuration (route create handler, public route-options
// endpoint) without re-running the admin-only GetGatewaySettings.
func (h *SettingsHandler) LoadGatewayDomainConfig() (hosters []HosterDomain, customEnabled bool, cnameTarget string) {
	hosters = h.loadHosterDomains()
	v, _ := h.state.Store.GetSetting("gateway_custom_domains_enabled")
	customEnabled = v == "true"
	cnameTarget, _ = h.state.Store.GetSetting("gateway_cname_target")
	return
}

// GetGatewayRouteOptions GET /api/gateway/route-options
// Available to all authenticated users — the user-facing route form needs
// the hoster list + custom-domain config to render itself.
func (h *SettingsHandler) GetGatewayRouteOptions(w http.ResponseWriter, r *http.Request) {
	hosters, customEnabled, cname := h.LoadGatewayDomainConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":              true,
		"hosterDomains":        hosters,
		"customDomainsEnabled": customEnabled,
		"cnameTarget":          cname,
	})
}

// SaveGatewaySettings POST /api/settings/gateway - PANEL settings.write (RequireCap at the route).
func (h *SettingsHandler) SaveGatewaySettings(w http.ResponseWriter, r *http.Request) {
	var req GatewaySettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Validate + normalize hoster domains
	cleaned := make([]HosterDomain, 0, len(req.HosterDomains))
	seen := map[string]bool{}
	for _, hd := range req.HosterDomains {
		dom := strings.ToLower(strings.TrimSpace(hd.Domain))
		if dom == "" {
			continue
		}
		if !domainRegex.MatchString(dom) {
			sendJSONError(w, "Invalid hoster domain: "+dom, http.StatusBadRequest)
			return
		}
		if seen[dom] {
			sendJSONError(w, "Duplicate hoster domain: "+dom, http.StatusBadRequest)
			return
		}
		seen[dom] = true
		val := strings.ToLower(strings.TrimSpace(hd.Validation))
		if !validHosterValidation(val) {
			val = "alphanumeric"
		}
		cleaned = append(cleaned, HosterDomain{Domain: dom, Validation: val})
	}
	hostersJSON, _ := json.Marshal(cleaned)

	// The CNAME target is a LABEL, not a domain: it is prefixed onto each hoster
	// base. Reject a full domain outright rather than silently building
	// "route.eu.example.com.eu.example.com", which would resolve nowhere and
	// only surface as a customer whose CNAME never works.
	cnameLabel := strings.ToLower(strings.TrimSpace(req.CnameTarget))
	if cnameLabel != "" && !subRegexDNS.MatchString(cnameLabel) {
		sendJSONError(w, "CNAME target must be a single label such as \"route\", not a full domain — it is combined with each hoster domain automatically", http.StatusBadRequest)
		return
	}

	// Normalize the reserved-prefix list: lowercase, trimmed, deduped, non-empty.
	blockedSeen := map[string]bool{}
	blocked := make([]string, 0, len(req.BlockedRoutePrefixes))
	for _, p := range req.BlockedRoutePrefixes {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || blockedSeen[p] {
			continue
		}
		blockedSeen[p] = true
		blocked = append(blocked, p)
	}
	blockedJSON, _ := json.Marshal(blocked)

	// Save port-enable settings
	portSettings := []struct{ k, v string }{
		{"gateway_port_mc_enabled", fmt.Sprintf("%t", req.Limits.PortMcEnabled)},
		{"gateway_hoster_domains", string(hostersJSON)},
		{"gateway_custom_domains_enabled", fmt.Sprintf("%t", req.CustomDomainsEnabled)},
		{"gateway_cname_target", cnameLabel},
		{"gateway_blocked_route_prefixes", string(blockedJSON)},
	}
	for _, p := range portSettings {
		if err := h.state.Store.SetSetting(p.k, p.v); err != nil {
			sendJSONError(w, "Failed to save setting: "+p.k, http.StatusInternalServerError)
			return
		}
	}

	// Save limits
	limits := []struct {
		scope string
		max   *int
	}{
		{"global", req.Limits.Global},
		{"user_default", req.Limits.UserDefault},
	}
	// An empty field DELETES the row; it does not store NULL.
	//
	// The two are not the same thing, and the difference is what silently
	// switched the global cap off. effectiveRouteLimit walks user:<id> ->
	// user_default -> global, and a row that EXISTS answers even when its value
	// is NULL - "I am set, and I set no cap" - which ends the walk. So saving
	// this page with the per-user default left blank wrote a NULL user_default
	// row, and from then on global was never asked: every tenant was uncapped,
	// the page still displayed the global number, and nothing failed. Measured
	// on the live instance, where all four rows had been NULLed exactly this way.
	//
	// The per-user endpoint (PUT /api/users/{id}/route-limit) deliberately keeps
	// both spellings, because there they mean different things: "default" deletes
	// the row so the platform default applies, and "unlimited" writes NULL so the
	// account stays uncapped even if an operator later tightens that default.
	// This page has no such distinction to express - it IS the platform default,
	// and below it sits only global, so "blank" can only mean "I am not setting
	// this one".
	for _, l := range limits {
		var err error
		if l.max == nil {
			err = h.state.Store.DeleteGatewayRouteLimit(l.scope)
		} else {
			err = h.state.Store.SetGatewayRouteLimit(l.scope, l.max)
		}
		if err != nil {
			sendJSONError(w, "Failed to save limit: "+l.scope, http.StatusInternalServerError)
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// --- Placement Settings ---

// PlacementSettings holds the global defaults used when a new node is
// registered. Per-node overrides are stored directly on the nodes row.
type PlacementSettings struct {
	CPUOvercommitDefault float64 `json:"cpuOvercommitDefault"`
	RAMOvercommitDefault float64 `json:"ramOvercommitDefault"`
	DiskBufferGB         int     `json:"diskBufferGb"`
	RebalanceEnabled     bool    `json:"rebalanceEnabled"`
	RebalanceThreshold   int     `json:"rebalanceThreshold"` // % at which a node is considered overloaded
	PortMode             string  `json:"portMode"`           // "sequential" | "random" — host-port allocation strategy
	ContainerPort        int     `json:"containerPort"`      // default MC port inside the container (usually 25565)
	// PidsLimit caps the process/thread count per server container (cgroup pids
	// controller) as an anti fork-bomb guard. 0 = unlimited (default). Counts
	// threads too, so set it generously (e.g. 4096) — too low throttles heavy
	// modded servers.
	PidsLimit int64 `json:"pidsLimit"`
	// IOWeight is the cgroup blkio relative weight (10–1000) applied to every
	// server container, so a noisy neighbour can't starve others of disk I/O.
	// 0 = unset (default). NOTE: this is a RELATIVE priority, not a hard cap, and
	// only takes effect with an I/O scheduler that honours blkio weight (BFQ/CFQ);
	// on blk-mq with none/mq-deadline it is a no-op. Hard per-device bps caps need
	// the backing block device and are intentionally not wired here.
	IOWeight uint16 `json:"ioWeight"`
	// DiskEnforcement decides what happens when a placement would eat into the
	// disk buffer: "soft" places it anyway and reports, "hard" refuses. It
	// governs ADMISSION only - see services.DiskEnforcementSoft.
	DiskEnforcement string `json:"diskEnforcement"`
	// DiskWarnPercent / DiskCriticalPercent are the PROJECTED fill levels
	// (written + still promised, over total) at which a path is flagged.
	DiskWarnPercent     int `json:"diskWarnPercent"`
	DiskCriticalPercent int `json:"diskCriticalPercent"`
}

var defaultPlacementSettings = PlacementSettings{
	CPUOvercommitDefault: 2.0, // CPU is time-shared, 2.0x = 200% is conservative
	RAMOvercommitDefault: 1.0, // RAM has no default overcommit (safer); 100%
	DiskBufferGB:         services.DefaultDiskHeadroomGB,
	DiskEnforcement:      services.DiskEnforcementSoft,
	DiskWarnPercent:      services.DefaultDiskWarnPercent,
	DiskCriticalPercent:  services.DefaultDiskCritPercent,
	RebalanceEnabled:     false,
	RebalanceThreshold:   90,
	PortMode:             "sequential",
	ContainerPort:        25565,
	PidsLimit:            0, // unlimited by default — opt-in anti fork-bomb cap
	IOWeight:             0, // unset by default — opt-in blkio fair-share
}

// GetPlacementSettings GET /api/settings/placement - PANEL settings.read (RequireCap at the route).
func (h *SettingsHandler) GetPlacementSettings(w http.ResponseWriter, r *http.Request) {
	s := h.LoadPlacementSettings()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"settings": s,
	})
}

// SavePlacementSettings POST /api/settings/placement - PANEL settings.write (RequireCap at the route).
func (h *SettingsHandler) SavePlacementSettings(w http.ResponseWriter, r *http.Request) {
	var req PlacementSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.CPUOvercommitDefault <= 0 || req.RAMOvercommitDefault <= 0 {
		sendJSONError(w, "Overcommit ratios must be > 0", http.StatusBadRequest)
		return
	}
	// The buffer is a floor, so it has a floor of its own: below it a path can be
	// filled until the host itself misbehaves, which takes down every server on
	// that disk rather than just the one that overran.
	if req.DiskBufferGB < services.MinDiskHeadroomGB {
		req.DiskBufferGB = services.MinDiskHeadroomGB
	}
	if req.DiskEnforcement != services.DiskEnforcementHard {
		req.DiskEnforcement = services.DiskEnforcementSoft
	}
	if req.DiskWarnPercent < 1 || req.DiskWarnPercent > 100 {
		req.DiskWarnPercent = services.DefaultDiskWarnPercent
	}
	if req.DiskCriticalPercent < 1 || req.DiskCriticalPercent > 100 {
		req.DiskCriticalPercent = services.DefaultDiskCritPercent
	}
	// A warning that fires after the critical level would never be seen.
	if req.DiskWarnPercent > req.DiskCriticalPercent {
		req.DiskWarnPercent = req.DiskCriticalPercent
	}
	if req.RebalanceThreshold < 50 {
		req.RebalanceThreshold = 50
	}
	if req.RebalanceThreshold > 100 {
		req.RebalanceThreshold = 100
	}
	if req.PortMode != "sequential" && req.PortMode != "random" {
		req.PortMode = "sequential"
	}
	if req.ContainerPort <= 0 || req.ContainerPort > 65535 {
		req.ContainerPort = 25565
	}
	if req.PidsLimit < 0 {
		req.PidsLimit = 0
	}
	// blkio weight is valid only in 10–1000; clamp non-zero values into range.
	if req.IOWeight != 0 {
		if req.IOWeight < 10 {
			req.IOWeight = 10
		} else if req.IOWeight > 1000 {
			req.IOWeight = 1000
		}
	}

	pairs := []struct{ k, v string }{
		{"placement.cpu_overcommit_default", fmt.Sprintf("%g", req.CPUOvercommitDefault)},
		{"placement.ram_overcommit_default", fmt.Sprintf("%g", req.RAMOvercommitDefault)},
		{"placement.disk_buffer_gb", fmt.Sprintf("%d", req.DiskBufferGB)},
		{services.DiskEnforcementSetting, req.DiskEnforcement},
		{services.DiskWarnPercentSetting, fmt.Sprintf("%d", req.DiskWarnPercent)},
		{services.DiskCritPercentSetting, fmt.Sprintf("%d", req.DiskCriticalPercent)},
		{"placement.rebalance_enabled", fmt.Sprintf("%t", req.RebalanceEnabled)},
		{"placement.rebalance_threshold", fmt.Sprintf("%d", req.RebalanceThreshold)},
		{"placement.port_mode", req.PortMode},
		{"placement.container_port", fmt.Sprintf("%d", req.ContainerPort)},
		{"placement.pids_limit", fmt.Sprintf("%d", req.PidsLimit)},
		{"placement.io_weight", fmt.Sprintf("%d", req.IOWeight)},
	}
	for _, p := range pairs {
		if err := h.state.Store.SetSetting(p.k, p.v); err != nil {
			sendJSONError(w, "Failed to save setting: "+p.k, http.StatusInternalServerError)
			return
		}
	}

	// Publish to Redis so nodes pick up the new values via loadModesFromRedis
	// without needing a redeploy. Best-effort, but NOT because "nodes re-read
	// every 30s" - that re-read only overwrites on a non-empty get, so it can
	// never recover a key Redis has lost. What covers a failure here, and a
	// wiped Redis, is services.NodeModePublisher re-asserting these from the
	// database on its own ticker.
	if h.state.Redis != nil {
		ctx := r.Context()
		h.state.Redis.Set(ctx, "dylaris:placement:port_mode", req.PortMode, 0)
		h.state.Redis.Set(ctx, "dylaris:placement:container_port", fmt.Sprintf("%d", req.ContainerPort), 0)
		h.state.Redis.Set(ctx, "dylaris:placement:pids_limit", fmt.Sprintf("%d", req.PidsLimit), 0)
		h.state.Redis.Set(ctx, "dylaris:placement:io_weight", fmt.Sprintf("%d", req.IOWeight), 0)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// LoadPlacementSettings reads placement settings with sensible defaults.
func (h *SettingsHandler) LoadPlacementSettings() PlacementSettings {
	s := defaultPlacementSettings

	getStr := func(k string) string { v, _ := h.state.Store.GetSetting(k); return v }

	if v := getStr("placement.cpu_overcommit_default"); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil && f > 0 {
			s.CPUOvercommitDefault = f
		}
	}
	if v := getStr("placement.ram_overcommit_default"); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil && f > 0 {
			s.RAMOvercommitDefault = f
		}
	}
	if v := getStr(services.DiskEnforcementSetting); v == services.DiskEnforcementHard {
		s.DiskEnforcement = services.DiskEnforcementHard
	}
	if v := getStr(services.DiskWarnPercentSetting); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 1 && n <= 100 {
			s.DiskWarnPercent = n
		}
	}
	if v := getStr(services.DiskCritPercentSetting); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 1 && n <= 100 {
			s.DiskCriticalPercent = n
		}
	}
	if v := getStr("placement.disk_buffer_gb"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			s.DiskBufferGB = n
		}
	}
	if v := getStr("placement.rebalance_enabled"); v != "" {
		s.RebalanceEnabled = v == "true"
	}
	if v := getStr("placement.rebalance_threshold"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 50 && n <= 100 {
			s.RebalanceThreshold = n
		}
	}
	if v := getStr("placement.port_mode"); v == "sequential" || v == "random" {
		s.PortMode = v
	}
	if v := getStr("placement.container_port"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 65535 {
			s.ContainerPort = n
		}
	}
	if v := getStr("placement.pids_limit"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			s.PidsLimit = n
		}
	}
	if v := getStr("placement.io_weight"); v != "" {
		var n uint16
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && (n == 0 || (n >= 10 && n <= 1000)) {
			s.IOWeight = n
		}
	}
	return s
}

// --- Server Settings ---

// SettingMaxSubServers is the settings key. Named so the setter, the loader and
// the enforcement site in servers_lifecycle.go cannot drift apart by a typo -
// they used to spell it out three times.
const SettingMaxSubServers = "srv.max_sub_servers"

// ServerSettings carries the per-server caps an operator types in.
//
// MaxSubServers follows the platform limit convention: nil is no cap, 0 is a
// real "none", n is the cap. It used to be a plain int documented as
// "0 = unlimited" - and it was neither, because the enforcement site guarded on
// `n > 0` and silently discarded the zero. Measured before this change: saving
// 0 produced a cap of THREE, so neither meaning an operator might have intended
// was reachable and nothing said so.
type ServerSettings struct {
	MaxSubServers *int64 `json:"maxSubServers"`
}

// defaultMaxSubServers is the product default, used when the setting has never
// been saved. Deliberately a cap and not "unlimited": a fresh install should not
// let one server spawn sub-servers without bound.
var defaultMaxSubServers = services.LimitPtr(3)

var defaultServerSettings = ServerSettings{
	MaxSubServers: defaultMaxSubServers,
}

// GetServerSettings GET /api/settings/servers - PANEL settings.read (RequireCap at the route).
func (h *SettingsHandler) GetServerSettings(w http.ResponseWriter, r *http.Request) {
	settings := h.LoadServerSettings()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"settings": settings,
	})
}

// SaveServerSettings POST /api/settings/servers - PANEL settings.write (RequireCap at the route).
func (h *SettingsHandler) SaveServerSettings(w http.ResponseWriter, r *http.Request) {
	var req ServerSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.MaxSubServers != nil && *req.MaxSubServers < 0 {
		sendJSONError(w, "Sub-server limit must be 0 or more (0 means none; leave it unset for no limit)", http.StatusBadRequest)
		return
	}
	if err := h.state.Store.SetSetting(SettingMaxSubServers, services.FormatLimitSetting(req.MaxSubServers)); err != nil {
		sendJSONError(w, "Failed to save setting", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// LoadServerSettings reads server settings from the database.
func (h *SettingsHandler) LoadServerSettings() ServerSettings {
	settings := defaultServerSettings
	val, err := h.state.Store.GetSetting(SettingMaxSubServers)
	if err != nil {
		return settings
	}
	settings.MaxSubServers = services.ParseLimitSetting(val, defaultMaxSubServers)
	return settings
}

// publishLimit mirrors an operator-typed cap into the Redis key the node reads.
//
// A nil cap DELETES the key rather than writing 0. On the reading side a missing
// key already meant "no cap", so this keeps "no limit" and "a limit of none"
// distinguishable end to end without changing the wire format - the value stays
// a plain number, and neither side has to negotiate anything.
func publishLimit(ctx context.Context, rdb *redis.Client, key string, v *int64) {
	if rdb == nil {
		return
	}
	if v == nil {
		rdb.Del(ctx, key)
		return
	}
	rdb.Set(ctx, key, strconv.FormatInt(*v, 10), 0)
}

// ─── Beam Settings ───────────────────────────────────────────────────

type BeamSettings struct {
	RelayAddress string `json:"relayAddress"` // Effective relay (discovered or manual override)
	// ManualOverride is the admin-configured override; empty means use
	// auto-discovery.
	//
	// A POINTER, and that is the whole fix for a defect that cost an operator
	// their relay failover without telling them: the read returns the EFFECTIVE
	// relay in RelayAddress, the write used to store whatever arrived there into
	// beam.relay_address - which IS the override. So loading this page and
	// saving any unrelated field on it pinned the currently discovered relay as
	// a permanent manual override, after which discovery, failover and
	// multi-region selection silently stopped.
	//
	// Absent (nil) keeps the legacy behaviour for a client that does not know
	// this field; present is authoritative, including an explicit "" meaning
	// "go back to discovery".
	ManualOverride   *string         `json:"manualOverride"`
	PublicHost       string          `json:"publicHost"`       // Externally reachable hostname for discovered relays (e.g. beam.dylaris.com)
	DiscoveredRelays []BeamRelayInfo `json:"discoveredRelays"` // Currently registered relays (read-only)
	// BwLimit is the legacy single-value Node throttle (bytes/sec, 0 =
	// unlimited). It stays the source of truth for the actual Node-side
	// rate.Limiter so older deploys keep working. The four-direction
	// fields below are saved alongside and will replace it once asymmetric
	// node + dedicated relay throttles ship.
	BwLimit      int64  `json:"bwLimit"`
	Enabled      bool   `json:"enabled"`
	DownloadLink string `json:"downloadLink"` // Optional CDN URL — overrides relay-served download

	// There is deliberately no force-update floor here. It comes from the SIGNED
	// release manifest - the same artifact the app verifies before it
	// self-updates - so whoever cuts the release sets it. An operator-typed
	// second opinion could only ever disagree with the binary being shipped, and
	// a typo in it locks every client out. See effectiveMinVersion.

	// Throttle splits (bytes/sec, 0 = unlimited). Stored verbatim. Until
	// the per-direction limiters land in node + relay these are advisory:
	// BwLimit (above) is computed by SaveBeamSettings as the lower of
	// the two internal directions so existing behavior is preserved.
	BwUpInternal   int64 `json:"bwUpInternal"`
	BwDownInternal int64 `json:"bwDownInternal"`
	BwUpExternal   int64 `json:"bwUpExternal"`
	BwDownExternal int64 `json:"bwDownExternal"`

	// Reference values the admin records about the host hardware
	// (datacenter internal + external uplink). Pure informational —
	// never enforced. Help operators size their throttle values.
	RefUpInternal   int64 `json:"refUpInternal"`
	RefDownInternal int64 `json:"refDownInternal"`
	RefUpExternal   int64 `json:"refUpExternal"`
	RefDownExternal int64 `json:"refDownExternal"`

	// Upload limits (bytes), on the platform limit convention: nil is no cap, 0
	// is a real "none" (uploads not allowed), n is the cap. Enforced by the node
	// on the beam upload path, which bypasses Core's HTTP body-size cap and disk
	// precheck. MaxUploadBytes is an absolute per-upload cap; DailyUploadBytes is
	// a per-user daily total.
	//
	// Published to Redis keys beam:max_upload_bytes / beam:daily_upload_bytes,
	// which the node reads per upload. No cap DELETES the key rather than writing
	// 0 - the wire format stays a plain number, and "missing" already meant no
	// cap on the reading side, so the two states stay distinguishable without a
	// format change either end has to negotiate.
	MaxUploadBytes   *int64 `json:"maxUploadBytes"`
	DailyUploadBytes *int64 `json:"dailyUploadBytes"`
}

// GetBeamSettings GET /api/settings/beam — all authenticated users (relay address + download link needed in Files tab)
func (h *SettingsHandler) GetBeamSettings(w http.ResponseWriter, r *http.Request) {
	settings := h.LoadBeamSettings()
	settings.DiscoveredRelays = DiscoverBeamRelays(r.Context(), h.state.Redis)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"settings": settings,
	})
}

// beamManualOverride decides what gets written to beam.relay_address, which is
// the MANUAL OVERRIDE and not the effective relay.
//
// The read returns the resolved relay in RelayAddress - discovered when no
// override is set - so echoing that field back on save pinned a discovered relay
// as a permanent override. It looked like nothing had happened; what had
// happened was that failover stopped.
//
// A client that sends the field is authoritative, including an explicit empty
// string meaning "back to discovery". A client that does not send it at all is
// an older panel, and gets the old behaviour rather than having its override
// silently cleared by a build that never knew about it.
func beamManualOverride(req BeamSettings) string {
	if req.ManualOverride != nil {
		return strings.TrimSpace(*req.ManualOverride)
	}
	return strings.TrimSpace(req.RelayAddress)
}

// SaveBeamSettings POST /api/settings/beam - PANEL settings.write (RequireCap at the route).
func (h *SettingsHandler) SaveBeamSettings(w http.ResponseWriter, r *http.Request) {
	var req BeamSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// MinVersion is deliberately not read from the request: the floor lives in
	// the signed manifest and this endpoint no longer accepts one.

	// The download link is a URL Core itself GETs and then streams to the
	// caller of the unauthenticated /api/beam/download, so it gets the same
	// http/https + host + no-credentials check the other operator-set public
	// URLs get (core public URL, solder mirror URL). The dialer behind that
	// fetch refuses non-public addresses, which is what actually contains an
	// SSRF; this rejects the obviously-wrong value at the point where an
	// operator can still see the error.
	req.DownloadLink = strings.TrimSpace(req.DownloadLink)
	if err := validatePublicBaseURL("beam download link", req.DownloadLink); err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	enabledStr := "false"
	if req.Enabled {
		enabledStr = "true"
	}

	// Until per-direction throttles exist, the effective Node cap is the
	// lower bound of the two internal directions (file write/read both
	// flow over the internal hop). Operators see the same effect they'd
	// get from the legacy single field if they leave the splits at 0.
	effectiveBw := req.BwLimit
	if req.BwUpInternal > 0 || req.BwDownInternal > 0 {
		effectiveBw = minNonZeroInt64(req.BwUpInternal, req.BwDownInternal)
	}

	pairs := []struct{ k, v string }{
		{"beam.relay_address", beamManualOverride(req)},
		{"beam.public_host", strings.TrimSpace(req.PublicHost)},
		{"beam.bw_limit", fmt.Sprintf("%d", effectiveBw)},
		{"beam.enabled", enabledStr},
		{"beam.download_link", req.DownloadLink},

		// New per-direction throttle splits (advisory until relay-side
		// throttle ships). Stored verbatim so the UI round-trips.
		{"beam.bw_up_internal", fmt.Sprintf("%d", req.BwUpInternal)},
		{"beam.bw_down_internal", fmt.Sprintf("%d", req.BwDownInternal)},
		{"beam.bw_up_external", fmt.Sprintf("%d", req.BwUpExternal)},
		{"beam.bw_down_external", fmt.Sprintf("%d", req.BwDownExternal)},

		// Operator-recorded host hardware references (informational only).
		{"beam.ref_up_internal", fmt.Sprintf("%d", req.RefUpInternal)},
		{"beam.ref_down_internal", fmt.Sprintf("%d", req.RefDownInternal)},
		{"beam.ref_up_external", fmt.Sprintf("%d", req.RefUpExternal)},
		{"beam.ref_down_external", fmt.Sprintf("%d", req.RefDownExternal)},

		// Beam upload limits (bytes, 0 = unlimited), enforced node-side.
		{"beam.max_upload_bytes", services.FormatLimitSetting(req.MaxUploadBytes)},
		{"beam.daily_upload_bytes", services.FormatLimitSetting(req.DailyUploadBytes)},
	}
	for _, p := range pairs {
		if err := h.state.Store.SetSetting(p.k, p.v); err != nil {
			sendJSONError(w, "Failed to save", http.StatusInternalServerError)
			return
		}
	}

	// Publish to Redis so Nodes (per-direction internal caps) and the
	// Relay (per-direction external caps) can pick up changes without a
	// restart. The legacy beam:bw_limit key stays for back-compat with
	// older nodes that don't read the split keys yet.
	if h.state.Redis != nil {
		ctx := r.Context()
		h.state.Redis.Set(ctx, "beam:bw_limit", fmt.Sprintf("%d", effectiveBw), 0)
		h.state.Redis.Set(ctx, "beam:bw_up_internal", fmt.Sprintf("%d", req.BwUpInternal), 0)
		h.state.Redis.Set(ctx, "beam:bw_down_internal", fmt.Sprintf("%d", req.BwDownInternal), 0)
		h.state.Redis.Set(ctx, "beam:bw_up_external", fmt.Sprintf("%d", req.BwUpExternal), 0)
		h.state.Redis.Set(ctx, "beam:bw_down_external", fmt.Sprintf("%d", req.BwDownExternal), 0)
		// A nil cap DELETES the key. Writing 0 would publish "uploads are not
		// allowed", which is the opposite of what the operator asked for.
		publishLimit(ctx, h.state.Redis, "beam:max_upload_bytes", req.MaxUploadBytes)
		publishLimit(ctx, h.state.Redis, "beam:daily_upload_bytes", req.DailyUploadBytes)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Beam settings saved",
	})
}

// LoadBeamSettings loads Beam settings from DB with defaults.
func (h *SettingsHandler) LoadBeamSettings() BeamSettings {
	getSetting := func(key string) string {
		val, _ := h.state.Store.GetSetting(key)
		return val
	}

	manualOverride := getSetting("beam.relay_address")
	publicHost := getSetting("beam.public_host")
	effective, _ := resolveRelay(context.Background(), h.state.Redis, manualOverride, publicHost, "")

	settings := BeamSettings{
		RelayAddress:   effective,
		ManualOverride: &manualOverride,
		PublicHost:     publicHost,
		DownloadLink:   getSetting("beam.download_link"),
		Enabled:        true,
	}

	enabledStr := getSetting("beam.enabled")
	if enabledStr == "false" {
		settings.Enabled = false
	}

	if val := getSetting("beam.bw_limit"); val != "" {
		fmt.Sscanf(val, "%d", &settings.BwLimit)
	}

	// Per-direction throttle splits + operator-recorded references.
	// Defaults stay 0 so they round-trip cleanly when never set.
	fmt.Sscanf(getSetting("beam.bw_up_internal"), "%d", &settings.BwUpInternal)
	fmt.Sscanf(getSetting("beam.bw_down_internal"), "%d", &settings.BwDownInternal)
	fmt.Sscanf(getSetting("beam.bw_up_external"), "%d", &settings.BwUpExternal)
	fmt.Sscanf(getSetting("beam.bw_down_external"), "%d", &settings.BwDownExternal)
	fmt.Sscanf(getSetting("beam.ref_up_internal"), "%d", &settings.RefUpInternal)
	fmt.Sscanf(getSetting("beam.ref_down_internal"), "%d", &settings.RefDownInternal)
	fmt.Sscanf(getSetting("beam.ref_up_external"), "%d", &settings.RefUpExternal)
	fmt.Sscanf(getSetting("beam.ref_down_external"), "%d", &settings.RefDownExternal)

	// Beam upload limits (bytes). Default 0 (unlimited) when never set.
	settings.MaxUploadBytes = services.ParseLimitSetting(getSetting("beam.max_upload_bytes"), nil)
	settings.DailyUploadBytes = services.ParseLimitSetting(getSetting("beam.daily_upload_bytes"), nil)

	return settings
}

// minNonZeroInt64 returns the smaller of two values, treating 0 as
// "unlimited" so it never wins over a real cap. Used to fold the two
// internal-direction throttle splits into a single legacy bw_limit
// until per-direction enforcement ships.
func minNonZeroInt64(a, b int64) int64 {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

// --- Backup Settings ---

// BackupConfig captures the GLOBAL knobs that decide which kind of backup
// storage the panel will let users pick, plus the per-server quota policy.
// Per-instance credentials (S3 keys, NFS paths) stay in the backup_storages
// table; this struct only governs which provider rows are usable and how
// large each server's backup folder is allowed to get on node-local hosts.
type BackupConfig struct {
	// Mode is one of "s3", "node-local", "shared", or "core-storage". It picks which
	// backup_storages rows (by provider) the UI exposes as creatable, and
	// — for node-local — turns on the quota fields below.
	Mode string `json:"mode"`

	// QuotaPerServerGB is the per-server hard cap on the .dylaris-backups/
	// folder when Mode == "node-local", on the platform limit convention: nil is
	// no cap, 0 is a real "none" (node-local backups not allowed), n is the cap.
	// It used to be an int documented as "0 means unlimited", so the one number
	// meaning none was the number that switched the check off.
	//
	// The cap is enforced at the application layer (Core checks current usage
	// before approving a new backup); filesystem-level enforcement via XFS
	// project quotas is intentionally out of scope for this round.
	QuotaPerServerGB *int64 `json:"quotaPerServerGb"`

	// ShareQuotaWithServer folds the backup folder into the same quota
	// the server's container storage uses, instead of accounting for it
	// separately. Useful when ops doesn't want two quotas to monitor.
	// Only honored when Mode == "node-local"; ignored otherwise.
	ShareQuotaWithServer bool `json:"shareQuotaWithServer"`

	// DefaultUserQuotaGB is how much backup storage the PLATFORM holds for a
	// server owner who holds no entitlement, on the limit convention: nil is no
	// cap, 0 is a real "none", n is the cap. Unlike QuotaPerServerGB above it is
	// per OWNER and applies in every mode - the two are different budgets, one a
	// disk on the MC host and one what we store for a tenant.
	//
	// It lived on the Billing screen as billing.r2_quota_gb until 2026-09-07.
	// That screen is hidden without a hosted store, so on a self-hosted install
	// the single control over every user's backup storage sat on a page nobody
	// could open, while the guard reading it ran on every backup.
	// Administrators are exempt from it - see services.BackupAllowanceGB.
	DefaultUserQuotaGB *int64 `json:"defaultUserQuotaGb"`
}

// Sourced from services, not literals, because the enforcement side has to
// resolve the same values on an install that never saved this form - otherwise
// the panel draws a bar against one number and Core refuses against another.
var defaultBackupConfig = BackupConfig{
	Mode:             services.DefaultBackupMode,
	QuotaPerServerGB: services.LimitPtr(services.DefaultBackupQuotaPerServer),
	// nil, so an install that never saved this form keeps the behaviour it had:
	// no platform ceiling on a tenant without an entitlement. A default number
	// here would start refusing backups on upgrade for everyone.
	DefaultUserQuotaGB:   nil,
	ShareQuotaWithServer: false,
}

// validBackupMode keeps the accepted modes literal - adding a new one has to
// be a conscious change here AND in the storage factory. "core-storage" was
// added rather than exposing that provider regardless of Mode: Mode's
// documented job is to describe what the panel offers, and bypassing it would
// falsify its own comment. The default stays "shared", so no existing install
// shifts.
func validBackupMode(m string) bool {
	return m == "s3" || m == "node-local" || m == "shared" || m == "core-storage"
}

// GetBackupConfig GET /api/settings/backup - PANEL settings.read (RequireCap at the route).
func (h *SettingsHandler) GetBackupConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"settings": h.LoadBackupConfig(),
	})
}

// SaveBackupConfig POST /api/settings/backup - PANEL settings.write (RequireCap at the route).
func (h *SettingsHandler) SaveBackupConfig(w http.ResponseWriter, r *http.Request) {
	var req BackupConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if !validBackupMode(req.Mode) {
		sendJSONError(w, "Invalid backup mode (expected s3, node-local, shared, or core-storage)", http.StatusBadRequest)
		return
	}
	if req.QuotaPerServerGB != nil && *req.QuotaPerServerGB < 0 {
		sendJSONError(w, "Backup quota must be 0 or more GB (0 means none; leave it unset for no limit)", http.StatusBadRequest)
		return
	}
	if req.DefaultUserQuotaGB != nil && *req.DefaultUserQuotaGB < 0 {
		sendJSONError(w, "Default user backup allowance must be 0 or more GB (0 means none; leave it unset for no limit)", http.StatusBadRequest)
		return
	}

	pairs := []struct{ k, v string }{
		{"backup.mode", req.Mode},
		{services.SettingBackupQuotaPerServer, services.FormatLimitSetting(req.QuotaPerServerGB)},
		{services.SettingBackupDefaultUserQuota, services.FormatLimitSetting(req.DefaultUserQuotaGB)},
		{"backup.share_quota_with_server", fmt.Sprintf("%t", req.ShareQuotaWithServer)},
	}
	for _, p := range pairs {
		if err := h.state.Store.SetSetting(p.k, p.v); err != nil {
			sendJSONError(w, "Failed to save setting: "+p.k, http.StatusInternalServerError)
			return
		}
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// LoadBackupConfig reads the persisted BackupConfig, returning defaults
// for any missing keys so the panel always has something usable to render.
func (h *SettingsHandler) LoadBackupConfig() BackupConfig {
	cfg := defaultBackupConfig
	if v, _ := h.state.Store.GetSetting("backup.mode"); v != "" && validBackupMode(v) {
		cfg.Mode = v
	}
	if v, err := h.state.Store.GetSetting(services.SettingBackupQuotaPerServer); err == nil {
		cfg.QuotaPerServerGB = services.ParseLimitSetting(v, defaultBackupConfig.QuotaPerServerGB)
	}
	if v, err := h.state.Store.GetSetting(services.SettingBackupDefaultUserQuota); err == nil {
		cfg.DefaultUserQuotaGB = services.ParseLimitSetting(v, defaultBackupConfig.DefaultUserQuotaGB)
	}
	if v, _ := h.state.Store.GetSetting("backup.share_quota_with_server"); v != "" {
		cfg.ShareQuotaWithServer = v == "true"
	}
	return cfg
}

// --- Routing Mode Settings ---

type RoutingModeSettings struct {
	Mode     string `json:"mode"`     // "ip_port" | "both" | "gateway"
	FileMode string `json:"fileMode"` // "sftp" | "both" | "beam"
}

func validRoutingMode(v string) bool {
	return v == "ip_port" || v == "both" || v == "gateway"
}

func validFileMode(v string) bool {
	return v == "sftp" || v == "both" || v == "beam"
}

// GetRoutingMode GET /api/settings/routing-mode — available to all authenticated users
func (h *SettingsHandler) GetRoutingMode(w http.ResponseWriter, r *http.Request) {
	mode, _ := h.state.Store.GetSetting("routing_mode")
	fileMode, _ := h.state.Store.GetSetting("file_access_mode")
	if mode == "" {
		mode = "ip_port"
	}
	if fileMode == "" {
		fileMode = "sftp"
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"mode":     mode,
		"fileMode": fileMode,
	})
}

// SaveRoutingMode POST /api/settings/routing-mode - PANEL settings.write (RequireCap at the route)
func (h *SettingsHandler) SaveRoutingMode(w http.ResponseWriter, r *http.Request) {
	var req RoutingModeSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if !validRoutingMode(req.Mode) {
		sendJSONError(w, "Invalid mode: must be ip_port, both, or gateway", http.StatusBadRequest)
		return
	}
	if !validFileMode(req.FileMode) {
		sendJSONError(w, "Invalid fileMode: must be sftp, both, or beam", http.StatusBadRequest)
		return
	}

	if err := h.state.Store.SetSetting("routing_mode", req.Mode); err != nil {
		sendJSONError(w, "Failed to save routing_mode", http.StatusInternalServerError)
		return
	}
	if err := h.state.Store.SetSetting("file_access_mode", req.FileMode); err != nil {
		sendJSONError(w, "Failed to save file_access_mode", http.StatusInternalServerError)
		return
	}

	// Publish to Redis so Nodes pick it up within 30s. This MUST stay ahead of
	// the migration below: the node re-reads these keys when it pulls a command,
	// so publishing first is what makes the redeploy use the NEW mode. Queueing
	// before the write would redeploy every server on the old mode.
	ctx := r.Context()
	h.state.Redis.Set(ctx, "dylaris:routing_mode", req.Mode, 0)
	h.state.Redis.Set(ctx, "dylaris:file_access_mode", req.FileMode, 0)

	// Gateway off => auto-move cannot run (a moved server would lose its
	// stable address), so disable the feature and clear every per-server
	// opt-in. Best-effort: a failure here must not abort the routing switch.
	if req.Mode == "ip_port" {
		if err := h.state.Store.SetSetting("feature_auto_move_enabled", "false"); err != nil {
			log.Printf("routing-mode: failed to disable auto-move feature flag: %v", err)
		}
		h.state.FeatureFlags.Invalidate("feature_auto_move_enabled")
		if err := h.state.Store.ResetAllAutoMove(); err != nil {
			log.Printf("routing-mode: failed to reset per-server auto-move opt-ins: %v", err)
		}
	}

	// Kick off migration
	queued := 0
	migrationError := ""
	if h.state.RoutingMigration != nil {
		n, err := h.state.RoutingMigration.Run(ctx, req.Mode)
		queued = n
		if err != nil {
			// The mode itself is already persisted, so the request did not
			// fail - but the fleet has NOT been migrated, and the panel's
			// "Routing mode saved." with no server count is indistinguishable
			// from a platform that simply had nothing to migrate. Say so.
			// The detail stays in the log; the client gets a fixed sentence.
			log.Printf("routing-mode: the migration to %q could not be started: %v", req.Mode, err)
			migrationError = "The routing mode was saved, but the server migration could not be started. The servers are still on the old routing - check the Core log and retry."
		}
	}

	resp := map[string]interface{}{
		"success":       true,
		"serversQueued": queued,
	}
	if migrationError != "" {
		resp["migrationError"] = migrationError
	}
	json.NewEncoder(w).Encode(resp)
}

// --- Warp Spoke Firewall Settings ---

// WarpFirewallRedisKey is the fixed central-Redis key the warp leaders read and
// poll for the admin-configured spoke destination-port allowlist. MUST stay
// byte-identical to gateway/warp firewallAllowedPortsKey.
const WarpFirewallRedisKey = "dylaris:warp:firewall:allowed_ports"

// defaultWarpSpokeAllowedPorts is the compiled-in default allowlist (6379 Redis,
// 25501 Core gRPC, 25551 beam relay). Written in the sorted order
// normalizeWarpPorts emits, so the first-boot value does not visibly reorder on
// the first save. MUST match gateway/warp defaultSpokeAllowedPorts.
//
// 25560, the edge tunnel, is deliberately absent. A customer machine must reach
// an edge over the internet: through the overlay its players share one warp
// leader's bandwidth with that same customer's Redis traffic and Beam uploads,
// and drop every session whenever the leader restarts. This allowlist is the
// only place that can enforce it - LINK_EXTERNAL, NODE_EXTERNAL and NODE_TAGS
// are all env on the customer's own host, so nothing the machine declares about
// itself is evidence.
//
// Adding it back re-opens that path for every tenant at once. The edge must
// publish :25560 instead; a link then fails outright rather than quietly
// degrading onto the overlay and looking healthy.
const defaultWarpSpokeAllowedPorts = "6379,25501,25551"

type WarpFirewallSettings struct {
	AllowedPorts string `json:"allowedPorts"` // comma-separated destination TCP ports the overlay leaders allow spokes to reach

	// TunnelSubnets is the DC overlay CIDR(s) a warp client must route through
	// the tunnel - where Redis and Core gRPC live. Core cannot infer this: the
	// value is a property of the Docker overlay network, which Core never sees,
	// and every client previously had to be told it out of band. Storing it once
	// lets the panel hand out a ready-to-run deploy snippet instead of a
	// placeholder the operator has to look up.
	//
	// Comma-separated, so a fleet spanning several DC ranges can list them all.
	TunnelSubnets string `json:"tunnelSubnets"`
}

// normalizeTunnelSubnets validates a comma-separated CIDR list into a deduped,
// order-preserving CSV. Rejects anything that is not a CIDR, and anything that
// is not a network address (10.20.0.5/16 rather than 10.20.0.0/16): a client
// routes the whole prefix regardless, so accepting a host address would show
// operators a value that silently means something other than what it says.
func normalizeTunnelSubnets(csv string) (string, error) {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(csv, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		ip, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", fmt.Errorf("invalid CIDR %q", p)
		}
		if !ip.Equal(ipnet.IP) {
			return "", fmt.Errorf("%q is a host address; use the network address %q", p, ipnet.String())
		}
		s := ipnet.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return strings.Join(out, ","), nil
}

// normalizeWarpPorts validates and normalizes a comma-separated port list into a
// sorted, deduped CSV. It rejects any non-numeric or out-of-range (1..65535)
// entry so a bad value never reaches the leaders (which trust this key).
func normalizeWarpPorts(csv string) (string, error) {
	seen := map[int]bool{}
	var ports []int
	for _, part := range strings.Split(csv, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", fmt.Errorf("invalid port %q", p)
		}
		if n < 1 || n > 65535 {
			return "", fmt.Errorf("port %d out of range 1-65535", n)
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		ports = append(ports, n)
	}
	sort.Ints(ports)
	strs := make([]string, len(ports))
	for i, p := range ports {
		strs[i] = strconv.Itoa(p)
	}
	return strings.Join(strs, ","), nil
}

// LoadWarpSpokeAllowedPorts returns the persisted allowlist, or the compiled-in
// default when unset. The stored value is already normalized on save.
func (h *SettingsHandler) LoadWarpSpokeAllowedPorts() string {
	v, _ := h.state.Store.GetSetting("warp_spoke_allowed_ports")
	if v == "" {
		return defaultWarpSpokeAllowedPorts
	}
	return v
}

// LoadWarpTunnelSubnets returns the persisted DC overlay CIDR(s), or "" when the
// operator has not set one yet (the deploy snippet then shows a placeholder
// rather than inventing a range).
func (h *SettingsHandler) LoadWarpTunnelSubnets() string {
	v, _ := h.state.Store.GetSetting("warp_tunnel_subnets")
	return v
}

// GetWarpFirewallSettings GET /api/settings/warp-firewall - PANEL settings.read (RequireCap at the route).
func (h *SettingsHandler) GetWarpFirewallSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"settings": WarpFirewallSettings{
			AllowedPorts:  h.LoadWarpSpokeAllowedPorts(),
			TunnelSubnets: h.LoadWarpTunnelSubnets(),
		},
		// Detected rather than stored: the panel pre-fills this when nothing is
		// saved yet, so a self-hoster never has to look the overlay CIDR up.
		"suggestedTunnelSubnets": h.state.suggestTunnelSubnets(),
	})
}

// SaveWarpFirewallSettings POST /api/settings/warp-firewall - PANEL settings.write
// (RequireCap at the route). Validates + normalizes the port list, persists it,
// and publishes it to the central-Redis key the warp leaders poll. Requires at
// least one port: an empty allowlist would silently lock every spoke out of all
// internal services.
func (h *SettingsHandler) SaveWarpFirewallSettings(w http.ResponseWriter, r *http.Request) {
	var req WarpFirewallSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	norm, err := normalizeWarpPorts(req.AllowedPorts)
	if err != nil {
		sendJSONError(w, "Invalid ports: "+err.Error(), http.StatusBadRequest)
		return
	}
	if norm == "" {
		sendJSONError(w, "At least one port is required", http.StatusBadRequest)
		return
	}
	subnets, serr := normalizeTunnelSubnets(req.TunnelSubnets)
	if serr != nil {
		sendJSONError(w, "Invalid tunnel subnets: "+serr.Error(), http.StatusBadRequest)
		return
	}
	// Empty is allowed: unset just means the deploy snippet keeps showing a
	// placeholder. Unlike the port allowlist this is client configuration Core
	// hands out, not a rule the leaders enforce, so there is nothing to publish.
	if err := h.state.Store.SetSetting("warp_tunnel_subnets", subnets); err != nil {
		sendJSONError(w, "Failed to save setting", http.StatusInternalServerError)
		return
	}
	if err := h.state.Store.SetSetting("warp_spoke_allowed_ports", norm); err != nil {
		sendJSONError(w, "Failed to save setting", http.StatusInternalServerError)
		return
	}
	propagated := true
	if h.state.Redis != nil {
		if rerr := h.state.Redis.Set(r.Context(), WarpFirewallRedisKey, norm, 0).Err(); rerr != nil {
			log.Printf("warp-firewall: failed to publish allowlist to Redis (leaders keep the stale value until the next successful save): %v", rerr)
			propagated = false
		}
	}
	resp := map[string]interface{}{
		"success":  true,
		"settings": WarpFirewallSettings{AllowedPorts: norm, TunnelSubnets: subnets},
	}
	if !propagated {
		// The Postgres row IS saved (source of truth for a later successful
		// publish), but the warp leaders poll a fixed Redis key with no other
		// reconcile path, so a failed publish leaves the OLD, wider allowlist
		// live. Tell the admin instead of a bare success - silently pretending
		// this succeeded would hide a firewall rule that never tightened.
		resp["success"] = false
		resp["error"] = "Saved, but failed to publish to the warp leaders. The previous allowlist is still active; retry the save."
	}
	json.NewEncoder(w).Encode(resp)
}
