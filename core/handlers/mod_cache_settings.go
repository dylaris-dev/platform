package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/handlers/httpx"
	"dylaris-core/services"
)

// Where the Modrinth metadata cache lives.
//
// It defaults to the Redis Core already has, and that default is correct for
// almost every install: Redis is mandatory here, so the cache always has a home
// and nothing has to be configured before mods or modpacks can be used.
//
// The reason a second endpoint is offered at all is measured, not theoretical:
// one Modrinth version list runs 290 KB to 1.2 MB, the proxy keys them per
// filter combination for an hour, and the shipped Redis runs with no maxmemory.
// The per-response cap in the proxy is the first answer to that; pointing the
// cache somewhere else entirely is the second, for an operator who would rather
// their control plane never share memory with a cache at all.
//
// It is deliberately NOT a precondition for using mods or modpacks. A gate on
// something the whole system already requires can never fail, and would only add
// a screen every operator has to visit to be told to keep what they have.

const (
	settingModCacheAddr     = "mod_cache_redis_addr"
	settingModCacheUsername = "mod_cache_redis_username"
	settingModCachePassword = "mod_cache_redis_password" // encrypted at rest
	settingModCacheDB       = "mod_cache_redis_db"
	settingModCacheTLS      = "mod_cache_redis_tls"
)

type ModCacheSettingsHandler struct {
	state *AppState
}

func NewModCacheSettingsHandler(state *AppState) *ModCacheSettingsHandler {
	return &ModCacheSettingsHandler{state: state}
}

type modCacheSettings struct {
	Addr     string `json:"addr"`
	Username string `json:"username"`
	DB       int    `json:"db"`
	TLS      bool   `json:"tls"`
	// PasswordSet reports that a password is stored without returning it. The
	// password itself never leaves the server, the same rule the SMTP and DNS
	// credentials follow.
	PasswordSet bool `json:"passwordSet"`
	// Password is write-only: a blank value on save keeps the stored one.
	Password string `json:"password,omitempty"`

	Status services.CacheStatus `json:"status"`
}

// CacheConfigFromSettings reads the stored cache endpoint. Returns a zero config
// when none is set, which the cache reads as "use the Redis Core already has".
func (s *AppState) CacheConfigFromSettings() services.CacheConfig {
	get := func(k string) string {
		v, _ := s.Store.GetSetting(k)
		return v
	}
	db, _ := strconv.Atoi(get(settingModCacheDB))
	return services.CacheConfig{
		Addr:     strings.TrimSpace(get(settingModCacheAddr)),
		Username: get(settingModCacheUsername),
		Password: get(settingModCachePassword),
		DB:       db,
		TLS:      get(settingModCacheTLS) == "true",
	}
}

// ApplyCacheSettings points the cache at whatever the settings say, at BOOT.
//
// It adopts rather than reconfigures, so a stored dedicated endpoint that is not
// answering yet stays the target instead of falling back to the control-plane
// Redis - the one thing configuring it was meant to prevent. The error is worth
// logging and is not a reason to refuse the setting. The save path is the
// opposite and validates before storing; see Cache.Adopt and Cache.Reconfigure.
func (s *AppState) ApplyCacheSettings(ctx context.Context) error {
	if s.Cache == nil {
		return nil
	}
	return s.Cache.Adopt(ctx, s.CacheConfigFromSettings())
}

// Get GET /api/admin/settings/mod-cache
func (h *ModCacheSettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	cfg := h.state.CacheConfigFromSettings()
	out := modCacheSettings{
		Addr:        cfg.Addr,
		Username:    cfg.Username,
		DB:          cfg.DB,
		TLS:         cfg.TLS,
		PasswordSet: cfg.Password != "",
	}
	if h.state.Cache != nil {
		out.Status = h.state.Cache.Status(r.Context())
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "settings": out})
}

// modCacheCredentialRebound reports whether this save would send the STORED
// Redis password to a different endpoint than the one it was entered for,
// without the caller having supplied a new one.
//
// Same guard as smtpCredentialRebound and the three storage configs before it,
// and it is on the same side of the line as SMTP: RESP AUTH sends the password
// to the server, so moving the address moves the credential. Set does not even
// wait for a later use - Reconfigure dials and authenticates inside this
// request, before anything is stored, so a rebound save delivers the password
// whether or not it ends up being saved.
//
// The username is compared for the same reason it is on the SMTP path: a new
// username paired with the old password is an auth failure nothing points at.
// TLS is deliberately NOT compared - turning it off exposes the credential on
// the wire, but to the same party, which is a downgrade rather than a rebind.
//
// A submitted password is a rotation and is always allowed; clearing the
// address drops the credential outright and so cannot rebind it.
func modCacheCredentialRebound(req modCacheSettings, stored func(string) string) bool {
	if strings.TrimSpace(req.Password) != "" || strings.TrimSpace(req.Addr) == "" || stored(settingModCachePassword) == "" {
		return false
	}
	return strings.TrimSpace(req.Addr) != strings.TrimSpace(stored(settingModCacheAddr)) ||
		req.Username != stored(settingModCacheUsername)
}

// Set PUT /api/admin/settings/mod-cache
func (h *ModCacheSettingsHandler) Set(w http.ResponseWriter, r *http.Request) {
	var req modCacheSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Addr = strings.TrimSpace(req.Addr)
	if req.DB < 0 || req.DB > 15 {
		httpx.SendJSONError(w, "db must be between 0 and 15", http.StatusBadRequest)
		return
	}

	stored := func(k string) string {
		v, _ := h.state.Store.GetSetting(k)
		return v
	}
	if modCacheCredentialRebound(req, stored) {
		httpx.SendJSONError(w, "the Redis address or username changed, so the stored password cannot be reused - re-enter the password with this change", http.StatusBadRequest)
		return
	}

	// A blank password on save keeps the stored one, so an operator editing the
	// host does not have to retype a credential the form never showed them.
	password := strings.TrimSpace(req.Password)
	if password == "" {
		password = stored(settingModCachePassword)
	}
	// Clearing the address clears the whole endpoint, credential included: a
	// password left behind for a host that is no longer used is a secret kept
	// for no reason.
	if req.Addr == "" {
		password = ""
		req.Username = ""
		req.DB = 0
		req.TLS = false
	}

	candidate := services.CacheConfig{
		Addr:     req.Addr,
		Username: req.Username,
		Password: password,
		DB:       req.DB,
		TLS:      req.TLS,
	}
	// Prove the endpoint works BEFORE storing it. Storing first would leave the
	// cache pointed at something unreachable, and because a cache failure is a
	// silent miss rather than an error, the only symptom would be a panel that
	// got slower.
	if h.state.Cache != nil {
		if err := h.state.Cache.Reconfigure(r.Context(), candidate); err != nil {
			httpx.SendJSONError(w, "Could not reach that Redis: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	set := func(k, v string) {
		if err := h.state.Store.SetSetting(k, v); err != nil {
			log.Printf("mod cache settings: failed to save %s: %v", k, err)
		}
	}
	set(settingModCacheAddr, req.Addr)
	set(settingModCacheUsername, req.Username)
	set(settingModCachePassword, password)
	set(settingModCacheDB, strconv.Itoa(req.DB))
	set(settingModCacheTLS, strconv.FormatBool(req.TLS))

	h.state.Events.Publish(r.Context(), "settings.changed", map[string]interface{}{"section": "mod-cache"})
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": cacheSavedMessage(req.Addr),
		"status":  h.state.Cache.Status(r.Context()),
	})
}

func cacheSavedMessage(addr string) string {
	if addr == "" {
		return "Mod metadata is cached in the Redis this panel already uses."
	}
	return "Mod metadata is now cached in " + addr + "."
}
