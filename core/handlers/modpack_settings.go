package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type ModpackSettingsHandler struct {
	state *AppState
}

func NewModpackSettingsHandler(state *AppState) *ModpackSettingsHandler {
	return &ModpackSettingsHandler{state: state}
}

// modpackSettings is the wire shape used by GET + PUT. The s3 secret is
// write-only from the panel's perspective: GET omits it (returns the empty
// string) so it never lands in the panel state or browser devtools.
type modpackSettings struct {
	FeatureEnabled           bool     `json:"featureEnabled"`
	Provider                 string   `json:"provider"` // "local" | "s3"
	Paths                    []string `json:"paths"`    // local: absolute paths
	S3Endpoint               string   `json:"s3Endpoint"`
	S3Bucket                 string   `json:"s3Bucket"`
	S3Region                 string   `json:"s3Region"`
	S3AccessKey              string   `json:"s3AccessKey"`
	S3SecretKey              string   `json:"s3SecretKey,omitempty"` // write-only
	UpdateCheckIntervalHours int      `json:"updateCheckIntervalHours"`
	ShareLinksEnabled        bool     `json:"shareLinksEnabled"`
	// CorePublicURL is Core's own public origin. A node installing a panel-built
	// pack downloads it from {CorePublicURL}/mirror/ (modpackMirrorBase), and
	// isSnapshotFetchHostAllowed derives its SSRF allowlist from the same value -
	// so an unset one is not cosmetic: a pack install fails to start and the
	// mirror host drops off the allowlist.
	CorePublicURL string `json:"corePublicUrl"`
	// ConnectionID references a saved storage connection. When non-zero, modpack
	// storage is built from that connection (buildModpackStorageProvider) and the
	// inline s3 fields are ignored. 0 = use the inline config.
	ConnectionID int `json:"connectionId"`

	// DetectedCorePublicURL is the origin THIS request arrived on. Offered as a
	// one-click suggestion for CorePublicURL, which an admin otherwise has to
	// retype from memory even though the browser just used it.
	//
	// A SUGGESTION, never an auto-save: the Host header is client-controlled, and
	// an admin reaching Core over an internal address (localhost, a service name,
	// a LAN IP) would otherwise silently persist an origin no launcher on the
	// internet can resolve. Displaying it to an authenticated admin who has to
	// accept it costs nothing and cannot be poisoned into the DB.
	DetectedCorePublicURL string `json:"detectedCorePublicUrl,omitempty"`
}

// requestOrigin reconstructs the scheme://host this request arrived on, honoring
// the usual reverse-proxy headers (Core runs behind one in every real deploy, so
// r.TLS is nil and r.Host alone would lose the scheme). Returns "" if there is no
// usable host.
func requestOrigin(r *http.Request) string {
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	// A comma-joined list means it passed several proxies; the first entry is the
	// origin the client actually used.
	if i := strings.IndexByte(host, ','); i >= 0 {
		host = strings.TrimSpace(host[:i])
	}
	if host == "" {
		return ""
	}
	scheme := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if i := strings.IndexByte(scheme, ','); i >= 0 {
		scheme = strings.TrimSpace(scheme[:i])
	}
	if scheme != "http" && scheme != "https" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	return scheme + "://" + host
}

// Get GET /api/admin/settings/modpacks - PANEL settings.read (RequireCap at the route).
func (h *ModpackSettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	get := func(k string) string {
		v, _ := h.state.Store.GetSetting(k)
		return v
	}
	out := modpackSettings{
		FeatureEnabled: get("feature_modpacks_enabled") == "true",
		Provider:       get("modpack_storage_provider"),
		S3Endpoint:     get("modpack_storage_s3_endpoint"),
		S3Bucket:       get("modpack_storage_s3_bucket"),
		S3Region:       get("modpack_storage_s3_region"),
		S3AccessKey:    get("modpack_storage_s3_access_key"),
		// Secret intentionally omitted on GET.
	}
	if out.Provider == "" {
		out.Provider = "local"
	}
	rawPaths := get("modpack_storage_paths")
	if rawPaths != "" {
		_ = json.Unmarshal([]byte(rawPaths), &out.Paths)
	}
	if out.Paths == nil {
		out.Paths = []string{}
	}
	interval := 24
	if v := get("modpack_update_check_interval_hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = n
		}
	}
	out.UpdateCheckIntervalHours = interval
	out.ShareLinksEnabled = get("modpack_share_links_enabled") == "true"
	out.ConnectionID, _ = strconv.Atoi(get(keyModpackStorageConnectionID)) // "" or bad -> 0 = none
	out.CorePublicURL = get("core_public_url")
	out.DetectedCorePublicURL = requestOrigin(r)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"settings": out,
		// secretSet tells the panel whether to render "(unchanged — leave
		// empty)" vs "(not set yet)" without ever exposing the value.
		"secretSet": get("modpack_storage_s3_secret_key") != "",
	})
}

// validModpackProvider mirrors the switch in storage/modpack/factory.go's
// NewProviderFromSettings. Without this allowlist a typo (or an
// aspirationally-named future backend) would persist to
// modpack_storage_provider and only fail much later when a pack is actually
// read or written.
//
// NOTE: this is an input allowlist, not an authorization boundary, same as
// validBackupProvider in backup.go.
func validModpackProvider(p string) bool {
	switch p {
	case "local", "s3", "core-storage":
		return true
	}
	return false
}

// validatePublicBaseURL accepts an empty value (the feature stays unconfigured
// and says so) or an absolute http/https URL with a host and no credentials.
//
// Stricter than validateS3Endpoint, which only has to survive being handed to
// the AWS SDK. This value is published to third-party launchers as the base
// they download from, AND it is the host isSnapshotFetchHostAllowed compares
// against, so a relative or scheme-less string would both hand clients a URL
// they cannot resolve and widen an SSRF allowlist by accident.
func validatePublicBaseURL(subject, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", subject, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must start with http:// or https://", subject)
	}
	if u.Host == "" {
		return fmt.Errorf("%s must include a host, e.g. https://panel.example.com", subject)
	}
	if u.User != nil {
		return fmt.Errorf("%s must not contain credentials", subject)
	}
	return nil
}

// modpackS3SecretRebound reports whether this save would point the STORED s3
// secret somewhere else without the caller having supplied a new one.
//
// The third copy of a guard core storage has always had
// (mergeCoreStorageCandidate) and backup storages gained later
// (mergeBackupStorageSecret), comparing the same trio - endpoint, bucket,
// access key - for the same two reasons:
//
//   - Security. settings.write is a delegatable panel capability, and Get
//     deliberately omits the secret on every read. A holder who cannot READ it
//     could point it at an endpoint and bucket of their choosing merely by
//     submitting those fields with the secret left blank. SigV4 signs with the
//     secret rather than sending it, so this is not a plaintext leak - it is
//     credential rebinding: an attacker-chosen host receives validly signed
//     requests carrying every pack the operator has published.
//   - Usability. Changing only the access key while leaving the secret blank
//     (because the form never shows it) would otherwise persist a NEW access
//     key paired with the OLD secret. Every pack read against that target then
//     fails a signature check with nothing pointing at the edit that caused it.
//
// A submitted secret is a genuine rotation and is always allowed; with no
// stored secret there is nothing to rebind.
func modpackS3SecretRebound(req modpackSettings, get func(string) string) bool {
	if strings.TrimSpace(req.S3SecretKey) != "" || get("modpack_storage_s3_secret_key") == "" {
		return false
	}
	return req.S3Endpoint != get("modpack_storage_s3_endpoint") ||
		req.S3Bucket != get("modpack_storage_s3_bucket") ||
		req.S3AccessKey != get("modpack_storage_s3_access_key")
}

// Set PUT /api/admin/settings/modpacks
//
// Body: full modpackSettings. Empty S3SecretKey means "don't change", so the
// admin can update other fields without re-entering the secret - unless the
// save also moves where that secret would be used, which modpackS3SecretRebound
// refuses.
func (h *ModpackSettingsHandler) Set(w http.ResponseWriter, r *http.Request) {
	var req modpackSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Provider == "" {
		req.Provider = "local"
	}
	if !validModpackProvider(req.Provider) {
		sendJSONError(w, "provider must be local, s3 or core-storage", http.StatusBadRequest)
		return
	}
	// Checked for every provider, not only "s3": the writes below persist the
	// endpoint whatever the provider is, Get echoes it back, and
	// modpackBackendLabel reads it straight out of the settings table. Gating
	// this on req.Provider == "s3" would leave a save with provider "local" as
	// an open path for the same credential.
	if err := validateS3Endpoint("modpacks", req.S3Endpoint); err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	get := func(k string) string {
		v, _ := h.state.Store.GetSetting(k)
		return v
	}
	if modpackS3SecretRebound(req, get) {
		sendJSONError(w, "the s3 endpoint, bucket or access key changed, so the stored secret cannot be reused - re-enter the secret access key with this change",
			http.StatusBadRequest)
		return
	}
	// Validated whatever the provider is, for the same reason the S3 endpoint
	// above is: the write below persists it either way.
	req.CorePublicURL = strings.TrimSpace(req.CorePublicURL)
	if err := validatePublicBaseURL("core public URL", req.CorePublicURL); err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Normalize paths: strip empties + dedupe + MkdirAll so the provider
	// doesn't fail on first Put. We deliberately do NOT validate writability
	// (no Test-write here) — admin can verify out-of-band.
	cleaned := make([]string, 0, len(req.Paths))
	seen := map[string]bool{}
	for _, p := range req.Paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		_ = os.MkdirAll(p, 0o755) // best-effort; surface failure on first Put
		cleaned = append(cleaned, p)
	}
	pathsJSON, _ := json.Marshal(cleaned)

	uch := req.UpdateCheckIntervalHours
	if uch <= 0 {
		uch = 24
	}

	// feature_modpacks_enabled is deliberately NOT written here. It used to be,
	// alongside Settings -> Features writing the same key, so the platform had one
	// switch behind two independent toggles: flipping either moved the other, and
	// with the authoring flag added, one of the two screens would always show a
	// half-truth. Features owns the flags, this screen owns storage.
	// The GET still returns featureEnabled so this tab can display the state.
	writes := []struct{ k, v string }{
		{"modpack_storage_provider", req.Provider},
		{"modpack_storage_paths", string(pathsJSON)},
		{"modpack_storage_s3_endpoint", req.S3Endpoint},
		{"modpack_storage_s3_bucket", req.S3Bucket},
		{"modpack_storage_s3_region", req.S3Region},
		{"modpack_storage_s3_access_key", req.S3AccessKey},
		{"modpack_update_check_interval_hours", strconv.Itoa(uch)},
		{"modpack_share_links_enabled", boolStr(req.ShareLinksEnabled)},
		{keyModpackStorageConnectionID, storageConnIDSetting(req.ConnectionID)},
		{"core_public_url", req.CorePublicURL},
	}
	for _, kv := range writes {
		if err := h.state.Store.SetSetting(kv.k, kv.v); err != nil {
			sendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if req.S3SecretKey != "" {
		if err := h.state.Store.SetSetting("modpack_storage_s3_secret_key", req.S3SecretKey); err != nil {
			sendJSONError(w, "Save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Invalidate the cached feature flag so subsequent reads in this Core
	// pick up the new value instantly; cross-Core staleness is bounded by
	// the 60s cache TTL. feature_modpacks_enabled is not invalidated: this handler
	// no longer writes it, so its cached value is still correct.
	h.state.FeatureFlags.Invalidate("modpack_share_links_enabled")

	// Only modpack_settings.changed: features.changed belonged to the flag write
	// that moved to Settings -> Features, and publishing it from here would tell
	// every panel to re-render its gating over a value that did not change.
	// The cached "is storage configured" answer is now stale; the panel reads it
	// from /api/system/features and refreshes on exactly this event.
	h.state.InvalidateModpackStorage()
	h.state.Events.Publish(r.Context(), "modpack_settings.changed", nil)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// --- Per-user modpack flag ---

type modpackUserFlagRequest struct {
	CanCreate bool `json:"canCreate"`
}

// SetUserFlag PATCH /api/admin/users/{id}/modpack-flag - PANEL settings.write
// (RequireCap at the route).
func (h *ModpackSettingsHandler) SetUserFlag(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseUserID(w, r)
	if !ok {
		return
	}
	var req modpackUserFlagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	// Marks the row manual as a side effect, so the platform authoring toggle's
	// bulk apply leaves this decision alone (see SetUserCanCreateModpacks).
	if err := h.state.Store.SetUserCanCreateModpacks(userID, req.CanCreate); err != nil {
		sendJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "users.changed", map[string]interface{}{"userId": userID})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ClearUserFlagOverride DELETE /api/admin/users/{id}/modpack-flag - PANEL
// settings.write (RequireCap at the route).
//
// Drops the manual marker so the user follows the platform authoring toggle
// again, without changing their current permission. This is the per-user escape
// hatch: the alternative is the Features screen's "also apply to users I set by
// hand" checkbox, which resets EVERY overridden user at once. Without it a row
// marked manual once would be pinned out of every later toggle forever.
func (h *ModpackSettingsHandler) ClearUserFlagOverride(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseUserID(w, r)
	if !ok {
		return
	}
	if err := h.state.Store.ClearUserCanCreateModpacksManual(userID); err != nil {
		sendJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.state.Events.Publish(r.Context(), "users.changed", map[string]interface{}{"userId": userID})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
