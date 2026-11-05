package services

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

// settingsReader is the slice of the Store interface this service needs.
// Defining it locally avoids an import cycle between services and store.
type settingsReader interface {
	GetSetting(key string) (string, error)
}

// FeatureFlags reads boolean, integer and string platform toggles from the
// settings table. It holds no cache of its own: the store answers GetSetting
// from its in-memory settings snapshot (store/read_cache.go), which is at most
// a second old and invalidated by every write through the store. There used to
// be a 60-second cache here as well, with an Invalidate every writer had to
// remember to call; a flag flipped on another replica took up to a minute.
type FeatureFlags struct {
	store settingsReader
}

func NewFeatureFlags(st settingsReader) *FeatureFlags {
	return &FeatureFlags{store: st}
}

// Get returns the boolean value for key, defaulting to defaultV when the
// setting is missing or unparseable.
func (f *FeatureFlags) Get(_ context.Context, key string, defaultV bool) bool {
	v, err := f.store.GetSetting(key)
	if err != nil || v == "" {
		return defaultV
	}
	b, parseErr := strconv.ParseBool(v)
	if parseErr != nil {
		return defaultV
	}
	return b
}

// GetFailClosed is Get for a flag whose OFF state opens something. A setting
// that was never saved still reads as defaultV, but a settings table that cannot
// be read answers true, so a database fault never switches a fence off.
func (f *FeatureFlags) GetFailClosed(_ context.Context, key string, defaultV bool) bool {
	v, err := f.store.GetSetting(key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return true
	}
	b, parseErr := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil || v == "" || parseErr != nil {
		b = defaultV
	}
	return b
}

// BYONOwnershipInForce is IsBYONEnabled for the node-ownership fences, failing
// closed: while the flag cannot be read, a customer's machine stays theirs.
// Feature gates that OPEN something for tenants keep using IsBYONEnabled.
func (f *FeatureFlags) BYONOwnershipInForce(ctx context.Context) bool {
	return f.GetFailClosed(ctx, "feature_byon_enabled", false)
}

// RouteOnlyOwnershipInForce is BYONOwnershipInForce for a tenant's link kits:
// in force while either product is, failing closed. Route-only on beside BYON
// off would otherwise let any admin roll a customer's kit and read the new key.
func (f *FeatureFlags) RouteOnlyOwnershipInForce(ctx context.Context) bool {
	return f.BYONOwnershipInForce(ctx) || f.GetFailClosed(ctx, "feature_route_only_enabled", false)
}

// GetInt returns the integer value for key, defaulting to defaultV when the
// setting is missing or unparseable.
func (f *FeatureFlags) GetInt(_ context.Context, key string, defaultV int) int {
	v, err := f.store.GetSetting(key)
	if err != nil || v == "" {
		return defaultV
	}
	n, perr := strconv.Atoi(strings.TrimSpace(v))
	if perr != nil {
		return defaultV
	}
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

// IsAutoMoveEnabled gates auto-move (server migration between nodes when one is
// overloaded). Default = false (opt-in). It runs in either routing mode: with
// the gateway the route follows the server, without it the server takes its
// new node's address.
func (f *FeatureFlags) IsAutoMoveEnabled(ctx context.Context) bool {
	return f.Get(ctx, "feature_auto_move_enabled", false)
}

// ProxyProtocolSettingKey is the operator switch for real player IP
// forwarding (PROXY v2 through the gateway).
const ProxyProtocolSettingKey = "gateway.proxy_protocol_enabled"

// IsProxyProtocolEnabled gates the per-server and per-route real-IP switches.
// Default = false: the switches stay hidden and refused until every gateway
// piece that carries the address has been deployed.
func (f *FeatureFlags) IsProxyProtocolEnabled(ctx context.Context) bool {
	return f.Get(ctx, ProxyProtocolSettingKey, false)
}

// VoiceChatSettingKey is the GLOBAL scope of the voice chat permission: the
// answer for a server when no server, user or role row says anything.
const VoiceChatSettingKey = "gateway.voice_chat_enabled"

// IsVoiceChatEnabled is that global scope. Default = false: voice stays off
// everywhere until the operator, or a narrower scope, allows it.
func (f *FeatureFlags) IsVoiceChatEnabled(ctx context.Context) bool {
	return f.Get(ctx, VoiceChatSettingKey, false)
}

// ModConfigsSettingKey holds who is offered the beta mod and plugin config
// editor: "off" (the default), "admin" or "all". Presentation only - the
// editor reads and writes through the file endpoints, which check files.read
// and files.write like the file manager does.
const ModConfigsSettingKey = "feature_mod_configs"

// ModConfigsAudience is that setting, with anything unrecognised read as off.
func (f *FeatureFlags) ModConfigsAudience(ctx context.Context) string {
	switch v := f.GetString(ctx, ModConfigsSettingKey, "off"); v {
	case "admin", "all":
		return v
	}
	return "off"
}

// BedrockSettingKey holds who may give a route Bedrock (players through the
// gateway's Geyser): "off" (the default), "admin" or "all". Anything but off
// also keeps the routes that already have it running; off pauses them without
// forgetting the owner's choice.
const BedrockSettingKey = "feature_bedrock"

// BedrockAudience is that setting, with anything unrecognised read as off.
func (f *FeatureFlags) BedrockAudience(ctx context.Context) string {
	switch v := f.GetString(ctx, BedrockSettingKey, "off"); v {
	case "admin", "all":
		return v
	}
	return "off"
}

// TemplatesAudience is who is offered Templates: "off" (the default), "admin"
// or "all", with anything unrecognised read as off.
func (f *FeatureFlags) TemplatesAudience(ctx context.Context) string {
	switch v := f.GetString(ctx, TemplatesSettingKey, "off"); v {
	case "admin", "all":
		return v
	}
	return "off"
}

// OwnStorageSettingKey holds who may keep backups and templates on their own S3
// target: "off", "admin" or "all". Unset is "all", which is how the platform
// behaved before the switch existed.
const OwnStorageSettingKey = "feature_own_storage"

// OwnStorageAudience is that setting, with anything unrecognised read as all.
// A nil f (a caller wired without flags) is also all.
func (f *FeatureFlags) OwnStorageAudience(ctx context.Context) string {
	if f == nil {
		return "all"
	}
	switch v := f.GetString(ctx, OwnStorageSettingKey, "all"); v {
	case "off", "admin":
		return v
	}
	return "all"
}

// OwnStorageAllowed resolves the audience for one user. It decides where NEW
// data may go only; data already on an own target stays reachable either way.
func (f *FeatureFlags) OwnStorageAllowed(ctx context.Context, isAdmin bool) bool {
	a := f.OwnStorageAudience(ctx)
	return a == "all" || (a == "admin" && isAdmin)
}

// IsBYONEnabled gates the bring-your-own-node multi-tenancy (per-user node
// enrollment, node ownership scoping, plans/billing). Default = false: the
// platform ships as today's single-operator panel and the operator opts in.
func (f *FeatureFlags) IsBYONEnabled(ctx context.Context) bool {
	return f.Get(ctx, "feature_byon_enabled", false)
}

// IsRouteOnlyEnabled gates route-only: tenants' link kits and the addresses
// they point at servers the tenant runs themselves. Its own switch, because it
// needs no BYON at all - a platform can protect customers' servers without ever
// letting a customer bring a node.
//
// Unset, it FOLLOWS the BYON flag, which is what it was gated on until it had
// one: an operator who never touches it sees no change.
func (f *FeatureFlags) IsRouteOnlyEnabled(ctx context.Context) bool {
	return f.Get(ctx, "feature_route_only_enabled", f.IsBYONEnabled(ctx))
}

// IsTenancyEnabled is BYON or route-only: whether customers can hold anything
// the entitlement, billing and metering planes have to account for.
func (f *FeatureFlags) IsTenancyEnabled(ctx context.Context) bool {
	return f.IsBYONEnabled(ctx) || f.IsRouteOnlyEnabled(ctx)
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

// IsTabProxyEnabled is the WS5 master toggle. Default false: the custom-tab
// reverse proxy is inert until an admin enables it.
func (f *FeatureFlags) IsTabProxyEnabled(ctx context.Context) bool {
	return f.Get(ctx, "feature_tab_proxy_enabled", false)
}

// IsDNSUpdaterEnabled gates the direct-mode domain reconciler. Default false:
// nothing is written to anyone's DNS until an admin turns it on.
func (f *FeatureFlags) IsDNSUpdaterEnabled(ctx context.Context) bool {
	return f.Get(ctx, "feature_dns_updater_enabled", false)
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
// setting is missing or empty.
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
