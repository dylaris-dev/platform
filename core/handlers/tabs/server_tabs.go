package tabs

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/handlers"

	"github.com/gorilla/mux"
)

// Per-server "Tabs". User defines extra tabs that show up in the
// Server Detail nav strip and render a URL (Minimap, BlueMap, custom plugin
// dashboards) either inside an iframe in the panel (open_in_panel=true) or
// by opening the URL in a new tab. V1 stops there — automatic Gateway
// route creation + container-side HTTP reverse-proxy is a follow-up.

type ServerTabsHandler struct {
	state *handlers.AppState
}

func NewServerTabsHandler(state *handlers.AppState) *ServerTabsHandler {
	return &ServerTabsHandler{state: state}
}

type serverTabResponse struct {
	ID             int        `json:"id"`
	ServerID       int        `json:"serverId"`
	Name           string     `json:"name"`
	Icon           string     `json:"icon"`
	URL            string     `json:"url"`
	Position       int        `json:"position"`
	Enabled        bool       `json:"enabled"`
	OpenInPanel    bool       `json:"openInPanel"`
	Mode           string     `json:"mode"`
	TargetPort     int        `json:"targetPort"`
	TargetPath     string     `json:"targetPath"`
	Surface        string     `json:"surface"`
	Visibility     string     `json:"visibility"`
	ShareToken     string     `json:"shareToken"`
	ShareExpiresAt *time.Time `json:"shareExpiresAt"`
	// SubServerName pins a proxied tab to one sub-server. Empty means every
	// sub-server, which is the default: a tab for a plugin that every world
	// runs should not have to be created once per world.
	SubServerName string `json:"subServerName"`
	// ProxyOrigin is the browser-facing origin this tab is served on, built
	// from its host label. Empty for a direct tab and for any tab while
	// TAB_PROXY_HOST_SUFFIX is unset - which is also how the panel knows not to
	// render a frame that could never load.
	ProxyOrigin string `json:"proxyOrigin"`
}

type serverTabRequest struct {
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	URL         string `json:"url"`
	Position    int    `json:"position"`
	Enabled     *bool  `json:"enabled,omitempty"`
	OpenInPanel *bool  `json:"openInPanel,omitempty"`
	Mode        string `json:"mode"`
	// SubServerName is a POINTER for the reason ShareExpiresAt below is: on a
	// PATCH, absent means "keep", "" means "clear, show on every sub-server",
	// and those are not the same request.
	SubServerName *string `json:"subServerName"`
	TargetPort    *int    `json:"targetPort,omitempty"`
	TargetPath    string  `json:"targetPath"`
	Surface       string  `json:"surface"`
	Visibility    string  `json:"visibility"`
	// ShareExpiresAt is a POINTER because three states have to be tellable
	// apart on a PATCH: absent (leave whatever is stored), "" (clear it, the
	// link stops expiring) and an RFC3339 instant (set it). See
	// parseShareExpiry.
	ShareExpiresAt *string `json:"shareExpiresAt,omitempty"`
}

// parseShareExpiry turns the wire value into what the write should do.
//
// The column existed, three queries read it, Public answered 410 on it and the
// standalone page rendered an "expired" card - and NOTHING ever wrote it, so a
// share link could not expire and none of that could fire. This is the missing
// write.
//
// set=false means the caller did not mention the field, which on a PATCH means
// "keep". An explicit empty string is how a caller says "no expiry" - JSON null
// decodes to the same nil a missing field does, so it cannot carry that meaning.
// A time already in the past is refused rather than stored: it would mint a link
// that is dead on arrival, which reads as a broken feature, not as a choice.
func parseShareExpiry(raw *string, now time.Time) (value interface{}, set bool, err error) {
	if raw == nil {
		return nil, false, nil
	}
	s := strings.TrimSpace(*raw)
	if s == "" {
		return nil, true, nil
	}
	t, perr := time.Parse(time.RFC3339, s)
	if perr != nil {
		return nil, false, errBadTabURL("share expiry must be an RFC3339 timestamp or empty")
	}
	if !t.After(now) {
		return nil, false, errBadTabURL("share expiry must be in the future")
	}
	return t.UTC(), true, nil
}

// serverExists is a pure data-existence check: the route's RequireCap
// (tabs.read for List, tabs.write for the mutating handlers) already enforces
// access before any of these handlers run, so this no longer branches on
// config-vs-overview like the old checkServerAccess-based canAccess did.
func (h *ServerTabsHandler) serverExists(serverID int) bool {
	_, err := h.state.Store.GetServerByID(serverID)
	return err == nil
}

func (h *ServerTabsHandler) db() *sql.DB {
	if h.state.Store == nil {
		return nil
	}
	provider, ok := h.state.Store.(handlers.SparkDBProvider)
	if !ok {
		return nil
	}
	return provider.RawDB()
}

// List GET /api/servers/{id}/tabs - the custom tabs configured on one server,
// including share slug and expiry where a proxied tab has them.
func (h *ServerTabsHandler) List(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	if !h.serverExists(serverID) {
		handlers.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	db := h.db()
	if db == nil {
		handlers.SendJSONError(w, "DB unavailable", http.StatusInternalServerError)
		return
	}
	rows, err := db.Query(`SELECT id, server_id, name, icon, url, position, enabled, open_in_panel,
		mode, COALESCE(target_port,0), target_path, surface, visibility,
		COALESCE(share_token,''), share_expires_at, COALESCE(proxy_host_label,''),
		COALESCE(sub_server_name,'')
		FROM server_tabs WHERE server_id=$1 ORDER BY position ASC, id ASC`, serverID)
	if err != nil {
		handlers.SendJSONError(w, "Query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []serverTabResponse{}
	for rows.Next() {
		var t serverTabResponse
		var expires sql.NullTime
		var hostLabel string
		// Dropping a row on a scan error hides a column/type mismatch as an
		// empty list instead of an error - that is exactly how the spark
		// profile list stayed silently empty. Fail loudly instead.
		if err := rows.Scan(&t.ID, &t.ServerID, &t.Name, &t.Icon, &t.URL, &t.Position, &t.Enabled, &t.OpenInPanel,
			&t.Mode, &t.TargetPort, &t.TargetPath, &t.Surface, &t.Visibility, &t.ShareToken, &expires,
			&hostLabel, &t.SubServerName); err != nil {
			handlers.SendJSONError(w, "Query failed", http.StatusInternalServerError)
			return
		}
		if expires.Valid {
			et := expires.Time
			t.ShareExpiresAt = &et
		}
		t.ProxyOrigin = TabProxyContentOrigin(h.state.FrontendURL, h.state.TabProxyHostSuffix, hostLabel)
		out = append(out, t)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"tabs":    out,
	})
}

// Create POST /api/servers/{id}/tabs - adds a custom tab. A proxied tab is the
// gated path: it needs the reverse-proxy feature on (403), public sharing on
// for a public one (403), and it counts against the server's proxied-tab limit
// (409). A proxied page tab is given an unguessable share slug at once.
func (h *ServerTabsHandler) Create(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	if !h.serverExists(serverID) {
		handlers.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req serverTabRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)
	req.Icon = strings.TrimSpace(req.Icon)
	req.Mode = strings.TrimSpace(req.Mode)
	req.TargetPath = strings.TrimSpace(req.TargetPath)
	req.Surface = strings.TrimSpace(req.Surface)
	req.Visibility = strings.TrimSpace(req.Visibility)
	// Same rule the console, backup and mod handlers apply. Empty is legal and
	// means "every sub-server"; anything else has to be a name that could exist,
	// because it ends up in a comparison against active_sub_server and a value
	// that can never match would make the tab permanently invisible.
	subServerName, serr := normalizeTabSubServer(req.SubServerName)
	if serr != nil {
		handlers.SendJSONError(w, serr.Error(), http.StatusBadRequest)
		return
	}
	if req.Icon == "" {
		req.Icon = "layout-grid"
	}
	if req.Name == "" {
		handlers.SendJSONError(w, "Name required", http.StatusBadRequest)
		return
	}
	if req.Mode == "" {
		req.Mode = "direct"
	}
	db := h.db()
	if db == nil {
		handlers.SendJSONError(w, "DB unavailable", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	userID, _ := ctx.Value("userID").(string)

	targetPort := 0
	if req.Mode == "proxied" {
		if req.Surface == "" {
			req.Surface = "tab"
		}
		if req.Visibility == "" {
			req.Visibility = "private"
		}
		if req.TargetPath == "" {
			req.TargetPath = "/"
		}
		if req.TargetPort != nil {
			targetPort = *req.TargetPort
		}
		if err := validateProxiedTab(targetPort, req.TargetPath, req.Surface, req.Visibility); err != nil {
			handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !h.state.FeatureFlags.IsTabProxyEnabled(ctx) {
			handlers.SendJSONError(w, "The tab reverse-proxy feature is disabled by the administrator.", http.StatusForbidden)
			return
		}
		if req.Visibility == "public" && !h.state.FeatureFlags.TabProxyAllowPublicLinks(ctx) {
			handlers.SendJSONError(w, "Public share links are disabled by the administrator.", http.StatusForbidden)
			return
		}
		if msg := h.tabCapExceeded(db, ctx, serverID, userID); msg != "" {
			handlers.SendJSONError(w, msg, http.StatusConflict)
			return
		}
	} else {
		req.Mode = "direct"
		req.Surface = "tab"
		req.Visibility = "private"
		req.TargetPath = "/"
		if err := handlers.ValidateTabURL(req.URL); err != nil {
			handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// A proxied page tab gets an unguessable share slug immediately (per-user
	// cap enforced first).
	var shareToken interface{}
	if req.Mode == "proxied" && (req.Surface == "page" || req.Surface == "both") {
		if userID != "" {
			if used, uerr := h.countUserShareLinks(db, userID); uerr == nil &&
				capReached(used, h.state.FeatureFlags.TabProxyMaxShareLinksPerUser(ctx)) {
				handlers.SendJSONError(w, "You have reached your share-link limit.", http.StatusConflict)
				return
			}
		}
		tok, terr := generateShareToken()
		if terr != nil {
			handlers.SendJSONError(w, "Failed to generate share token", http.StatusInternalServerError)
			return
		}
		shareToken = tok
	}

	// An expiry only ever means anything alongside a share token; on any other
	// tab the column stays NULL, which is what "never expires" already is.
	shareExpires, _, eerr := parseShareExpiry(req.ShareExpiresAt, time.Now())
	if eerr != nil {
		handlers.SendJSONError(w, eerr.Error(), http.StatusBadRequest)
		return
	}
	if shareToken == nil {
		shareExpires = nil
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	openInPanel := true
	if req.OpenInPanel != nil {
		openInPanel = *req.OpenInPanel
	}
	var createdBy interface{}
	if userID != "" {
		createdBy = userID
	}
	var portArg interface{}
	// hostLabel is minted for EVERY proxied tab, not only a shareable one: the
	// in-panel view frames the same content host, so a tab with surface="tab"
	// needs one too. A direct tab gets NULL - it has no data plane of ours.
	var hostLabel interface{}
	if req.Mode == "proxied" {
		portArg = targetPort
		lbl, lerr := generateProxyHostLabel()
		if lerr != nil {
			handlers.SendJSONError(w, "Failed to allocate a tab host", http.StatusInternalServerError)
			return
		}
		hostLabel = lbl
	}
	var id int
	err := db.QueryRow(`INSERT INTO server_tabs
		(server_id, name, icon, url, position, enabled, open_in_panel, created_by,
		 mode, target_port, target_path, surface, visibility, share_token, share_expires_at,
		 proxy_host_label, sub_server_name)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING id`,
		serverID, req.Name, req.Icon, req.URL, req.Position, enabled, openInPanel, createdBy,
		req.Mode, portArg, req.TargetPath, req.Surface, req.Visibility, shareToken, shareExpires,
		hostLabel, subServerName,
	).Scan(&id)
	if err != nil {
		handlers.SendJSONError(w, "Failed to create tab", http.StatusInternalServerError)
		return
	}

	h.state.Events.Publish(r.Context(), "server_tabs.changed", map[string]interface{}{
		"serverId": serverID,
	})

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"id":      id,
	})
}

// Update PATCH /api/servers/{id}/tabs/{tabId} - edits a custom tab, applying
// the same proxied-tab gates as creation.
func (h *ServerTabsHandler) Update(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	if !h.serverExists(serverID) {
		handlers.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	tabID, _ := strconv.Atoi(mux.Vars(r)["tabId"])
	var req serverTabRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Mode = strings.TrimSpace(req.Mode)
	req.TargetPath = strings.TrimSpace(req.TargetPath)
	req.Surface = strings.TrimSpace(req.Surface)
	req.Visibility = strings.TrimSpace(req.Visibility)
	if req.Mode == "proxied" && !h.state.FeatureFlags.IsTabProxyEnabled(r.Context()) {
		handlers.SendJSONError(w, "The tab reverse-proxy feature is disabled by the administrator.", http.StatusForbidden)
		return
	}
	if req.Visibility == "public" && !h.state.FeatureFlags.TabProxyAllowPublicLinks(r.Context()) {
		handlers.SendJSONError(w, "Public share links are disabled by the administrator.", http.StatusForbidden)
		return
	}
	if req.URL != "" {
		if err := handlers.ValidateTabURL(strings.TrimSpace(req.URL)); err != nil {
			handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	targetPort := 0
	if req.TargetPort != nil {
		targetPort = *req.TargetPort
	}
	// PATCH only writes fields the caller actually sent (see COALESCE below),
	// so validate each proxied field the same way: only when present. Render,
	// auth and proxy dispatch downstream trust these DB values, so an invalid
	// port/path/enum must never reach the UPDATE.
	if err := validateProxiedTabPatch(targetPort, req.TargetPath, req.Mode, req.Surface, req.Visibility); err != nil {
		handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Mode == "proxied" {
		port := 0
		if req.TargetPort != nil {
			port = *req.TargetPort
		}
		surface := req.Surface
		if surface == "" {
			surface = "tab"
		}
		visibility := req.Visibility
		if visibility == "" {
			visibility = "private"
		}
		path := req.TargetPath
		if path == "" {
			path = "/"
		}
		if err := validateProxiedTab(port, path, surface, visibility); err != nil {
			handlers.SendJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	db := h.db()
	if db == nil {
		handlers.SendJSONError(w, "DB unavailable", http.StatusInternalServerError)
		return
	}
	// The per-server proxied-tab cap was enforced in Create only, so a PATCH
	// that flips an existing tab to proxied walked straight past it - and
	// DIRECT tabs are uncapped, which makes the whole ceiling two calls per
	// tab away from any tabs.write holder. Only a TRANSITION is counted:
	// editing a tab that is already proxied must keep working once the server
	// sits at its limit, or the cap would freeze every existing tab.
	if req.Mode == "proxied" {
		var currentMode string
		if err := db.QueryRow(`SELECT mode FROM server_tabs WHERE id=$1 AND server_id=$2`,
			tabID, serverID).Scan(&currentMode); err != nil {
			handlers.SendJSONError(w, "Not found", http.StatusNotFound)
			return
		}
		if currentMode != "proxied" {
			// The tab's OWNER is charged, not whoever is editing: created_by is
			// the column the counts measure and the one this row keeps, so
			// billing anyone else would spend the wrong allowance.
			var owner string
			_ = db.QueryRow(`SELECT COALESCE(created_by::text,'') FROM server_tabs WHERE id=$1`, tabID).Scan(&owner)
			if owner == "" {
				// created_by is ON DELETE SET NULL, so a tab outlives the
				// account that made it. With nobody to charge the caps were
				// skipped outright, which turned "delete the creator" into a way
				// to uncap a tab. The caller is who is asking for the work now.
				owner, _ = r.Context().Value("userID").(string)
			}
			if msg := h.tabCapExceeded(db, r.Context(), serverID, owner); msg != "" {
				handlers.SendJSONError(w, msg, http.StatusConflict)
				return
			}
		}
		// A tab that used to be direct has no content host yet.
		if err := h.ensureProxyHostLabel(db, tabID); err != nil {
			handlers.SendJSONError(w, "Failed to allocate a tab host", http.StatusInternalServerError)
			return
		}
	}
	// Patch-style: only fields the client sent are written. COALESCE keeps
	// the existing value for empty strings on optional fields.
	var portArg interface{}
	if req.TargetPort != nil {
		portArg = *req.TargetPort
	}
	// share_expires_at cannot ride on COALESCE like the rest: NULL is a
	// LEGITIMATE value here ("never expires"), so "keep" and "clear" would be
	// the same argument. The explicit set-flag is what tells them apart.
	shareExpires, setExpiry, eerr := parseShareExpiry(req.ShareExpiresAt, time.Now())
	if eerr != nil {
		handlers.SendJSONError(w, eerr.Error(), http.StatusBadRequest)
		return
	}
	subServerName, serr := normalizeTabSubServer(req.SubServerName)
	if serr != nil {
		handlers.SendJSONError(w, serr.Error(), http.StatusBadRequest)
		return
	}
	res, err := db.Exec(`UPDATE server_tabs SET
		name           = COALESCE(NULLIF($3, ''), name),
		icon           = COALESCE(NULLIF($4, ''), icon),
		url            = COALESCE(NULLIF($5, ''), url),
		position       = COALESCE($6, position),
		enabled        = COALESCE($7, enabled),
		open_in_panel  = COALESCE($8, open_in_panel),
		mode           = COALESCE(NULLIF($9, ''),  mode),
		target_port    = COALESCE($10, target_port),
		target_path    = COALESCE(NULLIF($11, ''), target_path),
		surface        = COALESCE(NULLIF($12, ''), surface),
		visibility     = COALESCE(NULLIF($13, ''), visibility),
		share_expires_at = CASE WHEN $14::bool THEN $15::timestamptz ELSE share_expires_at END,
		-- Same three-state problem as share_expires_at: "" is a LEGITIMATE value
		-- here ("every sub-server"), so COALESCE(NULLIF(...)) could not tell
		-- "keep" from "clear". The explicit flag is what separates them.
		sub_server_name = CASE WHEN $16::bool THEN $17::text ELSE sub_server_name END
		WHERE id=$1 AND server_id=$2`,
		tabID, serverID,
		strings.TrimSpace(req.Name),
		strings.TrimSpace(req.Icon),
		strings.TrimSpace(req.URL),
		nullableInt(req.Position),
		req.Enabled, req.OpenInPanel,
		req.Mode,
		portArg,
		req.TargetPath,
		req.Surface,
		req.Visibility,
		setExpiry, shareExpires,
		req.SubServerName != nil, subServerName,
	)
	if err != nil {
		handlers.SendJSONError(w, "Failed to save tab", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		handlers.SendJSONError(w, "Not found", http.StatusNotFound)
		return
	}

	h.state.Events.Publish(r.Context(), "server_tabs.changed", map[string]interface{}{
		"serverId": serverID,
	})

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// Delete DELETE /api/servers/{id}/tabs/{tabId} - removes a custom tab from the
// server in the path.
func (h *ServerTabsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	if !h.serverExists(serverID) {
		handlers.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	tabID, _ := strconv.Atoi(mux.Vars(r)["tabId"])
	db := h.db()
	if db == nil {
		handlers.SendJSONError(w, "DB unavailable", http.StatusInternalServerError)
		return
	}
	res, err := db.Exec(`DELETE FROM server_tabs WHERE id=$1 AND server_id=$2`, tabID, serverID)
	if err != nil {
		handlers.SendJSONError(w, "Delete failed", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		handlers.SendJSONError(w, "Not found", http.StatusNotFound)
		return
	}

	h.state.Events.Publish(r.Context(), "server_tabs.changed", map[string]interface{}{
		"serverId": serverID,
	})

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// validateProxiedTab checks the proxy-mode fields: a container port in
// [1,65535] and a base path anchored at "/". surface/visibility are
// constrained to the known enums so the render + auth logic never sees an
// unexpected value.
func validateProxiedTab(port int, path, surface, visibility string) error {
	if port < 1 || port > 65535 {
		return errBadTabURL("target port must be between 1 and 65535")
	}
	if path == "" || path[0] != '/' {
		return errBadTabURL("target path must start with /")
	}
	switch surface {
	case "tab", "page", "both":
	default:
		return errBadTabURL("surface must be tab, page or both")
	}
	switch visibility {
	case "private", "public":
	default:
		return errBadTabURL("visibility must be private or public")
	}
	return nil
}

// validateProxiedTabPatch is the Update-path counterpart to validateProxiedTab.
// Update is a partial PATCH (COALESCE/NULLIF keeps any field the caller
// didn't send), so unlike Create there is no single point where every
// proxied field is guaranteed present. Each field is therefore validated
// only when the caller actually set it: port != 0, path/mode/surface/
// visibility != "". A field left at its zero value is presumed unset and
// passes through untouched by the UPDATE.
func validateProxiedTabPatch(port int, path, mode, surface, visibility string) error {
	if port != 0 && (port < 1 || port > 65535) {
		return errBadTabURL("target port must be between 1 and 65535")
	}
	if path != "" && path[0] != '/' {
		return errBadTabURL("target path must start with /")
	}
	if mode != "" {
		switch mode {
		case "direct", "proxied":
		default:
			return errBadTabURL("mode must be direct or proxied")
		}
	}
	if surface != "" {
		switch surface {
		case "tab", "page", "both":
		default:
			return errBadTabURL("surface must be tab, page or both")
		}
	}
	if visibility != "" {
		switch visibility {
		case "private", "public":
		default:
			return errBadTabURL("visibility must be private or public")
		}
	}
	return nil
}

type tabErr struct{ msg string }

func (e tabErr) Error() string { return e.msg }
func errBadTabURL(s string) error {
	return tabErr{msg: s}
}

// nullableInt returns nil for 0, the value otherwise — used so a missing
// `position` field in the patch doesn't clobber the existing one.
func nullableInt(v int) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

// Quiet unused-import warning when downstream changes drop time references.
var _ = time.Time{}
