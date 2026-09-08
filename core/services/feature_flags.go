package services

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// settingsReader is the slice of the Store interface this service needs.
// Defining it locally avoids an import cycle between services and store.
type settingsReader interface {
	GetSetting(key string) (string, error)
}

// FeatureFlags is a 60-second-cached read of boolean platform feature toggles
// stored in the settings table. The cache is best-effort — a writer that
// flips a flag should call Invalidate(key) so its own subsequent reads pick
// up the new value within the same Core process; cross-process staleness is
// bounded by the cache TTL.
type FeatureFlags struct {
	store    settingsReader
	mu       sync.Mutex
	cache    map[string]cachedFlag
	cacheInt map[string]cachedInt
	cacheTTL time.Duration
}

type cachedFlag struct {
	value bool
	at    time.Time
}

type cachedInt struct {
	value int
	at    time.Time
}

func NewFeatureFlags(st settingsReader) *FeatureFlags {
	return &FeatureFlags{
		store:    st,
		cache:    map[string]cachedFlag{},
		cacheInt: map[string]cachedInt{},
		cacheTTL: 60 * time.Second,
	}
}

// Get returns the boolean value for key, defaulting to defaultV when the
// setting is missing or unparseable.
func (f *FeatureFlags) Get(_ context.Context, key string, defaultV bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cf, ok := f.cache[key]; ok && time.Since(cf.at) < f.cacheTTL {
		return cf.value
	}
	v, err := f.store.GetSetting(key)
	if err != nil || v == "" {
		return defaultV
	}
	b, parseErr := strconv.ParseBool(v)
	if parseErr != nil {
		return defaultV
	}
	f.cache[key] = cachedFlag{value: b, at: time.Now()}
	return b
}

// GetInt returns the integer value for key, defaulting to defaultV when the
// setting is missing or unparseable. Cached like Get.
func (f *FeatureFlags) GetInt(_ context.Context, key string, defaultV int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ci, ok := f.cacheInt[key]; ok && time.Since(ci.at) < f.cacheTTL {
		return ci.value
	}
	v, err := f.store.GetSetting(key)
	if err != nil || v == "" {
		return defaultV
	}
	n, perr := strconv.Atoi(strings.TrimSpace(v))
	if perr != nil {
		return defaultV
	}
	f.cacheInt[key] = cachedInt{value: n, at: time.Now()}
	return n
}

// IsModpacksEnabled gates the modpack authoring subsystem. Default = false (the
// feature ships OFF and the admin opts in via Settings -> Features). Existing
// installs keep their stored value; only a brand-new/unset install lands off.
func (f *FeatureFlags) IsModpacksEnabled(ctx context.Context) bool {
	return f.Get(ctx, "feature_modpacks_enabled", false)
}

// IsModpackAuthoringEnabled gates END-USER modpack authoring. It is the second
// half of the modpack switch: IsModpacksEnabled turns the subsystem on (admins
// can author), and this opens it to everyone else. Default = false, so enabling
// modpacks alone stays admin-only. Non-admin write routes require BOTH this and
// the caller's per-user can_create_modpacks, which makes this a hard ceiling: a
// user whose flag was set by hand still loses authoring when this goes off.
func (f *FeatureFlags) IsModpackAuthoringEnabled(ctx context.Context) bool {
	return f.Get(ctx, "feature_modpack_authoring_enabled", false)
}

// IsTicketsEnabled gates the ticket subsystem (tickets, categories, canned
// responses, attachments, notifications, settings, deletion log). Default =
// false (the feature ships OFF and the admin opts in via Settings → Features).
func (f *FeatureFlags) IsTicketsEnabled(ctx context.Context) bool {
	return f.Get(ctx, "feature_tickets_enabled", false)
}

// IsLibraryEnabled gates the shared file library: the browsable catalog of
// server jars and archives an operator uploads once for everyone to install
// from.
//
// It exists because the Library module row used to be the only gate, and a
// module row hides a NAVBAR ENTRY. /library itself had no check and its Core
// routes are capability-gated, so switching the module off removed the link and
// left the page and its API open to anyone who knew the URL. A feature that can
// only be hidden is not a feature that can be turned off.
//
// Default = false, matching the module row's own seeded default.
func (f *FeatureFlags) IsLibraryEnabled(ctx context.Context) bool {
	return f.Get(ctx, "feature_library_enabled", false)
}

// IsAutoMoveEnabled gates the gateway-only auto-move (server migration between
// nodes) feature. Default = false (opt-in, and only meaningful while gateway
// routing is active — the gateway is what lets a server keep its address after
// it changes node). The handler layer ANDs this with the live routing mode.
func (f *FeatureFlags) IsAutoMoveEnabled(ctx context.Context) bool {
	return f.Get(ctx, "feature_auto_move_enabled", false)
}

// IsBYONEnabled gates the bring-your-own-node multi-tenancy (per-user node
// enrollment, node ownership scoping, plans/billing). Default = false: the
// platform ships as today's single-operator panel and the operator opts in.
func (f *FeatureFlags) IsBYONEnabled(ctx context.Context) bool {
	return f.Get(ctx, "feature_byon_enabled", false)
}

// UserAPIKeysEnabled gates whether a NON-ADMIN may hold an API key at all.
//
// Default = false. A key is a second credential class with a life of its own -
// it outlives a session, it is not covered by the account's 2FA, and it is
// revoked separately. A fresh install should not start handing those out
// because the software supports them; an operator turning this on is a decision
// with a date attached. Admins are unaffected: they can always mint their own.
func (f *FeatureFlags) UserAPIKeysEnabled(ctx context.Context) bool {
	return f.Get(ctx, "apikeys_user_enabled", false)
}

// UserAPIKeyAllowedCaps is the capability subset a non-admin may put on a key,
// as a comma-separated setting. EMPTY MEANS NO EXTRA RESTRICTION, not "none":
// the delegation subset check in the create handler already prevents a key from
// exceeding its creator, so an operator who has expressed no opinion gets that
// behaviour rather than a feature that silently does nothing.
//
// Returning nil for empty is what lets the caller tell the two apart.
func (f *FeatureFlags) UserAPIKeyAllowedCaps(ctx context.Context) []string {
	raw := strings.TrimSpace(f.GetString(ctx, "apikeys_user_allowed_caps", ""))
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// IsShareLinksEnabled gates CREATION of tokenized modpack share/download links.
// Default = false (opt-in distribution surface; the admin enables it in
// Settings -> Modpacks). Existing links keep serving while the parent modpacks
// feature is on; this only gates minting new links.
func (f *FeatureFlags) IsShareLinksEnabled(ctx context.Context) bool {
	return f.Get(ctx, "modpack_share_links_enabled", false)
}

// IsTabProxyEnabled is the WS5 master toggle. Default false: the custom-tab
// reverse proxy is inert until an admin enables it.
func (f *FeatureFlags) IsTabProxyEnabled(ctx context.Context) bool {
	return f.Get(ctx, "feature_tab_proxy_enabled", false)
}

// TabProxyAllowPublicLinks gates anonymous public share links. Default false.
func (f *FeatureFlags) TabProxyAllowPublicLinks(ctx context.Context) bool {
	return f.Get(ctx, "tab_proxy_allow_public_links", false)
}

// TabProxyMaxPerUserPerServer caps how many proxied tabs ONE user may have on
// ONE server. Default 3 (floored >0).
//
// Per user, not per server: a shared server would otherwise let whoever got
// there first spend the whole allowance, and the limit exists to bound what a
// single account can ask the proxy to carry.
func (f *FeatureFlags) TabProxyMaxPerUserPerServer(ctx context.Context) int {
	if v := f.GetInt(ctx, "tab_proxy_max_per_user_per_server", 3); v > 0 {
		return v
	}
	return 3
}

// TabProxyMaxPerUserTotal caps a user's proxied tabs across every server they
// have. Default 10 (floored >0).
//
// The per-server cap alone is not a ceiling: a user with twenty servers could
// hold twenty times it. This is the one that actually bounds an account.
func (f *FeatureFlags) TabProxyMaxPerUserTotal(ctx context.Context) int {
	if v := f.GetInt(ctx, "tab_proxy_max_per_user_total", 10); v > 0 {
		return v
	}
	return 10
}

// TabProxyAudience is who the Custom Tabs navbar entry is shown to: "admin" or
// "all". Default "all" - the tabs are a tenant-facing feature and a server owner
// who has one is the person meant to open it.
//
// It lives here rather than on the module row because the module row is DERIVED
// from it (handlers/custom_tabs_module.go). Two editable copies of one answer is
// how the row and the gate come to disagree.
func (f *FeatureFlags) TabProxyAudience(ctx context.Context) string {
	if v := f.GetString(ctx, "tab_proxy_audience", "all"); v == "admin" || v == "all" {
		return v
	}
	return "all"
}

// TabProxyMaxShareLinksPerUser caps active share links per user. Default 20.
func (f *FeatureFlags) TabProxyMaxShareLinksPerUser(ctx context.Context) int {
	if v := f.GetInt(ctx, "tab_proxy_max_share_links_per_user", 20); v > 0 {
		return v
	}
	return 20
}

// GetString returns the raw string value for key, defaulting to defaultV when the
// setting is missing or empty. Unlike Get/GetInt it is not cached (callers are
// low-frequency), so it always reflects the latest write without invalidation.
func (f *FeatureFlags) GetString(_ context.Context, key, defaultV string) string {
	v, err := f.store.GetSetting(key)
	if err != nil || strings.TrimSpace(v) == "" {
		return defaultV
	}
	return strings.TrimSpace(v)
}

// WarpRebalanceMode reports the F3 rebalancer mode: "off" (default), "dry-run"
// (compute + log + surface, no moves) or "armed" (apply). Any unrecognized value
// is treated as "off" so a typo can never silently arm the rebalancer.
func (f *FeatureFlags) WarpRebalanceMode(ctx context.Context) string {
	switch f.GetString(ctx, "warp_rebalance_mode", "off") {
	case "dry-run":
		return "dry-run"
	case "armed":
		return "armed"
	default:
		return "off"
	}
}

// WarpRebalancePct is the sustained utilisation percent above which a warp
// leader's host is relieved. Default 80 (aligned with the F2 alert); values
// outside 50..100 fall back to the default.
func (f *FeatureFlags) WarpRebalancePct(ctx context.Context) int {
	if v := f.GetInt(ctx, "warp_rebalance_pct", 80); v >= 50 && v <= 100 {
		return v
	}
	return 80
}

// WarpRebalanceSustainMin is the window (minutes) a host must stay over the
// threshold before any move. Default 10; non-positive falls back to the default.
func (f *FeatureFlags) WarpRebalanceSustainMin(ctx context.Context) int {
	if v := f.GetInt(ctx, "warp_rebalance_sustain_min", 10); v > 0 {
		return v
	}
	return 10
}

// WarpRebalanceIntervalMin is the evaluation cadence (minutes). Default 5;
// non-positive falls back to the default.
func (f *FeatureFlags) WarpRebalanceIntervalMin(ctx context.Context) int {
	if v := f.GetInt(ctx, "warp_rebalance_interval_min", 5); v > 0 {
		return v
	}
	return 5
}

// Invalidate drops the cached entry for a key so the next Get re-reads from
// the store. Called by settings PUT handlers after a write.
func (f *FeatureFlags) Invalidate(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.cache, key)
	delete(f.cacheInt, key)
}
