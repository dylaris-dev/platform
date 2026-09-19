package config

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/joho/godotenv"
)

type Config struct {
	APIPort     string
	FrontendURL string
	JWTSecret   string

	// AdminSecret is the optional RAM-only break-glass secret. When non-empty,
	// creating an admin via /setup requires this exact value in every mode
	// (fresh_install, lost_admin, complete). Empty disables the feature. Read
	// via ADMIN_SECRET (or ADMIN_SECRET_FILE). Never persisted, never logged.
	AdminSecret string

	// SetupEnabled (env SETUP) decides whether /setup stays reachable on an
	// instance that ALREADY has an admin. It is deliberately the narrow switch:
	//
	//   - No admin exists (fresh install, or every admin lost): ignored entirely,
	//     /setup is open regardless. Otherwise a false here would be a permanent
	//     lockout with no way in to change it.
	//   - An admin exists: false closes the door, true leaves it open behind
	//     ADMIN_SECRET.
	//
	// Defaults to FALSE. Before this, a configured ADMIN_SECRET left /setup
	// answering on every live instance forever, which is a working break-glass
	// but also a permanently mounted door that most operators never wanted and
	// nobody could turn off. Recovering a lost admin on a closed instance is
	// SETUP=true plus a restart, which is a deliberate act rather than a
	// standing exposure.
	SetupEnabled bool

	// Cluster
	ClusterSecret string
	// GatewayHubURL is the gateway Hub's internal base URL, e.g.
	// http://hub:25530. Core stores no DNS credential of its own: the panel's
	// DNS form is forwarded to the Hub, which owns the row and is the only
	// writer of records. Empty means no gateway, and the panel says so rather
	// than offering a form that cannot work.
	GatewayHubURL string
	CoreID        string
	GRPCPort      int
	// GRPCTLSEnabled turns on server-authenticated TLS + fingerprint pinning on
	// the node<->core NodeService channel. ON by default: the channel carries
	// console output, RCON and file transfer, and the justification for shipping
	// it in plaintext was "rely on an encrypted overlay" - which the reference
	// deployment does not provide (dylaris_net is created without --opt
	// encrypted). Set GRPC_TLS_ENABLED=false to opt out.
	//
	// Must hold the SAME value on every Core and every node: a TLS listener
	// refuses a plaintext dial, so a split turns the whole management plane off.
	// Both sides parse with strconv.ParseBool and default to true, so the two
	// cannot disagree about what a given string means.
	GRPCTLSEnabled bool
	// Region — which logical region this Core lives in. Stamped into the
	// system info endpoint so the panel can show a "Connected to <region> Core"
	// chip and downstream consumers can attribute a Core to a region.
	// 'default' for single-region setups.
	Region string

	// Database
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string
	// DBType selects the storage backend for time-series data (server_stats):
	// "timescaledb" promotes it to a hypertable with native retention (best for
	// larger fleets); "postgres" keeps it a plain table with retention enforced
	// by the hourly sweep (fine for small/medium setups, no extension required).
	// Normalized to exactly "timescaledb" or "postgres".
	DBType string

	// NO metrics database variable. Where the long-term statistics are written
	// is a PANEL setting (Settings, Features), stored in the settings table -
	// see services.LoadMetricsDBTarget. It was an environment variable and is
	// not one any more: two places that can answer the same question is one
	// place too many when the wrong answer silently changes the resolution of
	// history that cannot be backfilled.
	//
	// Whatever it points at, a failure there must never block Core. The
	// recorder drops rather than queues; see core/metrics.

	// Core Redis
	RedisAddr string
	RedisUser string
	RedisPass string
	RedisDB   int

	// Optional external ticket DB. When set, the migration UI surfaces this
	// as the target. Read by the migration/backup/restore handler — live
	// runtime queries always target the main DB.
	ExternalTicketDBURL string

	// DNS updater — leader-gated reconciler that points each region's edge
	// wildcard A record (e.g. *.eu.dylaris.com) at the live edge IPs in that
	// region via the DNS provider. Off unless DNS_UPDATER_ENABLED=true AND the
	// provider credentials are present. Credentials live ONLY in Core, never on
	// the edges.
	DNSUpdaterEnabled bool
	DNSProvider       string // see services.SupportedDNSProviders()
	// DNSAPIToken is the provider credential. CF_API_TOKEN is still read as a
	// fallback so an existing Cloudflare deployment keeps working.
	DNSAPIToken string
	// DNSZone is the zone NAME the edge wildcards live in, e.g. "dylaris.com".
	// This replaces CF_ZONE_ID: libdns addresses a zone by name, not by a
	// Cloudflare-assigned id, so the old value cannot be carried over.
	DNSZone string
	// DNSZones is the comma-separated multi-zone form, for a hoster offering
	// several domains from the same edges. DNS_ZONE stays supported and is
	// folded in, so a single-zone deployment needs no change.
	DNSZones string

	// Store integration — the hosted dylaris.com storefront. When BOTH
	// STORE_URL and STORE_SHARED_KEY are set, StoreEnabled flips on and the
	// store-linking + demo-showcase surfaces appear (connect-store button,
	// demo account/servers). Self-hosters without these ENV vars get a clean
	// open-core build with no store or demo surface at all. STORE_SHARED_KEY is
	// the service-to-service trust between Core and dylaris.com (NOT a user
	// proof); it must match the same key configured on dylaris.com.
	StoreURL       string
	StoreSharedKey string
	StoreEnabled   bool

	// SuspendGrace defers the hard cutoff (stop servers + take route-only link
	// tunnels down) for this long after a tenant is marked "suspended", so a transient
	// billing/DB fault cannot instantly kick a paying customer. Env
	// BILLING_SUSPEND_GRACE (Go duration), default 48h; 0 = enforce on the next
	// hourly lifecycle tick (no grace).
	SuspendGrace time.Duration

	// TabProxyHostSuffix is the DNS suffix each proxied custom tab is served
	// under, one host per tab: "<label>.share.example.com". REQUIRED for proxied
	// tabs; empty turns the whole proxied-tab feature off.
	//
	// A host, not a path prefix, and not a second port. Under a prefix the proxy
	// has to rewrite the HTML with a <base href>, which only fixes RELATIVE urls -
	// the path-absolute "/js/app.js" that BlueMap and Dynmap emit resolves against
	// the origin ROOT and misses the prefix entirely. Serving each tab at the root
	// of its own host removes the rewriting and the breakage together, and a
	// different hostname is already a different ORIGIN, so the container's JS
	// cannot reach the panel token in the panel origin's localStorage either.
	//
	// This replaced TAB_PROXY_PORT + TAB_PROXY_ORIGIN. Those were read
	// independently and never compared, so setting only the origin switched
	// isolation on and pointed every iframe at a port nothing was listening on,
	// silently. One variable cannot be half-set.
	TabProxyHostSuffix string
	// PanelAPIURL is empty for the same-origin default; see the read in Load.
	PanelAPIURL string

	// UpdatesURLPlatform / UpdatesURLHosted are the PUBLIC raw URLs of the two
	// release-notes files the updates view fetches: platform.md for people who RUN
	// the platform, hosted.md for DYLARIS customers. Both default to this repo's
	// own public raw URLs.
	//
	// Setting either to empty is meaningful, not a mistake: the build's EMBEDDED
	// copy is used instead, which is what an air-gapped install wants. Fetching
	// fails open to the same copy, so a changelog outage never becomes an error in
	// front of an operator's actual work.
	UpdatesURLPlatform string
	UpdatesURLHosted   string

	// TrustedProxyCIDRs are the networks a reverse proxy in front of Core may
	// occupy. It decides whether an X-Forwarded-For header is believed: the
	// client IP is taken as the first hop, walking the header right-to-left,
	// that is NOT inside one of these networks. A forged XFF entry from a real
	// client sits to the LEFT of the address the trusted proxy appended, so it
	// is never reached - which is what stops a client from spoofing the IP the
	// rate limiters and the audit log key on.
	//
	// Parsed from TRUSTED_PROXY_CIDRS. Unset or empty defaults to the private
	// ranges (the shipped reference proxy sits on the private Docker network),
	// so per-client limiting works out of the box while a public attacker's
	// forged header is ignored. The literal value "none" trusts nothing and
	// makes Core ignore XFF entirely - correct when Core is exposed directly.
	TrustedProxyCIDRs []*net.IPNet
}

func LoadConfig() (Config, error) {
	if _, err := os.Stat(".env"); os.IsNotExist(err) {
		log.Println("No .env file found. Using system environment variables.")
	} else {
		godotenv.Load()
	}

	redisDB, _ := strconv.Atoi(getEnv("REDIS_DB", "0"))
	grpcPort, _ := strconv.Atoi(getEnv("DYLARIS_GRPC_PORT", "25501"))

	coreID := getEnv("DYLARIS_CORE_ID", "")
	if coreID == "" {
		coreID, _ = os.Hostname()
	}

	dnsUpdaterEnabled, _ := strconv.ParseBool(getEnv("DNS_UPDATER_ENABLED", "false"))
	grpcTLSEnabled := ParseBoolEnvDefault("GRPC_TLS_ENABLED", true)

	storeURL := strings.TrimSpace(getEnv("STORE_URL", ""))
	storeSharedKey := getSecret("STORE_SHARED_KEY", "")

	// BILLING_SUSPEND_GRACE is the only time.Duration env. config.go otherwise has
	// no duration env, so this follows the surrounding int-parse style (parse, keep
	// the default on empty) plus getSecret's log-on-fallback: a bad value keeps the
	// 48h default instead of silently yielding 0, which would disable the grace.
	suspendGrace := 48 * time.Hour
	if v := strings.TrimSpace(getEnv("BILLING_SUSPEND_GRACE", "")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			suspendGrace = d
		} else {
			log.Printf("config: invalid BILLING_SUSPEND_GRACE %q: %v; using default %s", v, err, suspendGrace)
		}
	}

	// 25500, and it used to be 25510. Core serves the panel itself now, so the
	// panel's old port answers nothing - a default pointing at it would put a
	// dead host in every verification and password-reset email.
	frontendURL := getEnv("FRONTEND_URL", "http://localhost:25500")
	// PANEL_API_URL is the browser-reachable API base, and it is EMPTY for the
	// normal deployment: Core serves the panel itself, so the API is on the same
	// origin and the panel resolves /api without being told. It stays supported
	// for the one case that still needs it - an operator who terminates the API
	// on a separate hostname - and Core renders it into /config.js and into the
	// CSP rather than a second container doing so from its own copy of the value.
	panelAPIURL := strings.TrimSpace(getEnv("PANEL_API_URL", ""))
	warnPanelAPIURLIsAnotherOrigin(frontendURL, panelAPIURL)
	tabProxyHostSuffix := normalizeTabProxyHostSuffix(getEnv("TAB_PROXY_HOST_SUFFIX", ""))
	warnTabProxySuffixNotSameSite(frontendURL, tabProxyHostSuffix)

	// An unparseable SETUP is treated as off, matching the default. The value is
	// a door: "SETUP=yes" not parsing must not swing it open.
	setupEnabled, _ := strconv.ParseBool(getEnv("SETUP", "false"))

	cfg := Config{
		APIPort:        getEnv("API_PORT", "25500"),
		FrontendURL:    frontendURL,
		JWTSecret:      getSecret("JWT_SECRET", "change-this-secret"),
		AdminSecret:    getSecret("ADMIN_SECRET", ""),
		SetupEnabled:   setupEnabled,
		ClusterSecret:  getSecret("CLUSTER_SECRET", "dylaris-cluster-secret"),
		GatewayHubURL:  strings.TrimSpace(getEnv("GATEWAY_HUB_URL", "")),
		CoreID:         coreID,
		GRPCPort:       grpcPort,
		GRPCTLSEnabled: grpcTLSEnabled,
		Region:         getEnv("DYLARIS_REGION", "default"),

		DBHost:     getEnv("DB_HOST", "localhost"),
		DBPort:     getEnv("DB_PORT", "5432"),
		DBUser:     getEnv("DB_USER", "postgres"),
		DBPassword: getSecret("DB_PASSWORD", ""),
		DBName:     getEnv("DB_NAME", "dylaris"),
		// Defaults to disable to preserve existing internal-Docker setups; set
		// DB_SSLMODE=require (or verify-full) when Postgres is remote.
		DBSSLMode: getEnv("DB_SSLMODE", "disable"),
		// Defaults to timescaledb (the bundled image). Set DB_TYPE=postgres to
		// run on plain PostgreSQL with no TimescaleDB extension.
		DBType: NormalizeDBType(getEnv("DB_TYPE", "timescaledb")),

		RedisAddr: getEnv("REDIS_ADDR", "localhost:6379"),
		RedisUser: getEnv("REDIS_USER", ""),
		RedisPass: getSecret("REDIS_PASSWORD", ""),
		RedisDB:   redisDB,

		ExternalTicketDBURL: getEnv("EXTERNAL_TICKET_DB_URL", ""),

		DNSUpdaterEnabled: dnsUpdaterEnabled,
		DNSProvider:       getEnv("DNS_PROVIDER", "cloudflare"),
		DNSAPIToken:       getSecret("DNS_API_TOKEN", getSecret("CF_API_TOKEN", "")),
		DNSZone:           getEnv("DNS_ZONE", ""),
		DNSZones:          getEnv("DNS_ZONES", ""),

		StoreURL:       storeURL,
		StoreSharedKey: storeSharedKey,
		StoreEnabled:   storeURL != "" && storeSharedKey != "",

		SuspendGrace: suspendGrace,

		TabProxyHostSuffix: tabProxyHostSuffix,
		PanelAPIURL:        panelAPIURL,

		// Platform defaults to its own public repo's raw feed (works once the repo
		// is public + the feed is populated); gateway stays empty until the feed is
		// cross-pushed there. Override both via env for a self-hosted mirror.
		UpdatesURLPlatform: getEnv("UPDATES_URL_PLATFORM", "https://raw.githubusercontent.com/Bartis-Dev/dylaris-platform/main/core/updates/platform.md"),
		UpdatesURLHosted:   getEnv("UPDATES_URL_HOSTED", "https://raw.githubusercontent.com/Bartis-Dev/dylaris-platform/main/core/updates/hosted.md"),

		TrustedProxyCIDRs: ParseTrustedProxyCIDRs(getEnv("TRUSTED_PROXY_CIDRS", "")),
	}

	// Refuse to boot with a predictable signing key. A default/empty JWT_SECRET
	// makes every session token forgeable; a default CLUSTER_SECRET also exposes
	// the derived Warp leader key and inter-service auth.
	if cfg.JWTSecret == "" || cfg.JWTSecret == "change-this-secret" {
		return cfg, fmt.Errorf("JWT_SECRET is unset or still the default placeholder — set a strong random value")
	}
	if cfg.ClusterSecret == "" || cfg.ClusterSecret == "dylaris-cluster-secret" {
		return cfg, fmt.Errorf("CLUSTER_SECRET is unset or still the default placeholder — set a strong random value")
	}

	// ADMIN_SECRET is OPTIONAL (empty = disabled), unlike JWT/Cluster. When set
	// it must be strong enough to gate admin creation; a bad value fails the boot
	// (main.go log.Fatals on any LoadConfig error).
	if err := validateAdminSecret(cfg.AdminSecret); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

// ParseBoolEnvDefault reads a boolean env var, falling back to def when it is
// unset, empty, or not parseable.
//
// Keeping the default on an UNPARSEABLE value is the point, and it is why this
// is not `v, _ := strconv.ParseBool(...)`. That idiom yields false on error,
// which is harmless for a default-off flag and wrong for a default-on one: it
// turns GRPC_TLS_ENABLED=yes - a plausible typo - into a silent opt-out of
// transport security, with the operator's file reading as if they had switched
// it on. A refused value is logged loudly and changes nothing.
//
// Exported because the node agent must agree with Core bit for bit on what a
// given string means; it is a separate module, so it carries its own copy that
// node/grpc_tls_env_test.go pins against these same semantics.
func ParseBoolEnvDefault(key string, def bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		log.Printf("config: %s=%q is not a boolean; keeping the default %v. Use true/false.", key, raw, def)
		return def
	}
	return v
}

// NormalizeDBType maps the various spellings operators might use to the two
// canonical values: "timescaledb" or "postgres". Anything timescale-ish (incl.
// the empty string falling through the default) resolves to "timescaledb"; any
// plain-postgres spelling resolves to "postgres". Unknown values default to
// "postgres" (the safer, extension-free backend).
func NormalizeDBType(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "timescaledb", "timescale", "ts":
		return "timescaledb"
	case "postgres", "postgresql", "pg", "plain":
		return "postgres"
	default:
		return "postgres"
	}
}

// UsesTimescale reports whether the given (already-normalized or raw) DB type
// should use TimescaleDB hypertables + native retention.
func UsesTimescale(dbType string) bool {
	return NormalizeDBType(dbType) == "timescaledb"
}

// getSecret resolves a secret with Docker/Portainer secrets support. Precedence:
//  1. contents of the file named by "<key>_FILE" (trimmed) - the docker-secret /
//     *_FILE convention, so the value never has to live in plain env;
//  2. the plain "<key>" env value;
//  3. the fallback.
//
// An unreadable or empty *_FILE logs and falls through to the env/fallback so a
// misconfigured secret path doesn't silently boot with a blank credential.
func getSecret(key, fallback string) string {
	if path, ok := os.LookupEnv(key + "_FILE"); ok && path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(data)); v != "" {
				return v
			}
			log.Printf("config: %s_FILE (%s) is empty; falling back to %s", key, path, key)
		} else {
			log.Printf("config: failed to read %s_FILE (%s): %v; falling back to %s", key, path, err, key)
		}
	}
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

// validateAdminSecret checks the optional break-glass ADMIN_SECRET. Empty is
// valid and disables the feature. A configured secret must be at least 16
// characters so a trivially-guessable value cannot gate admin creation. Pure so
// it can be unit-tested without booting LoadConfig (which main.go log.Fatals on).
func validateAdminSecret(s string) error {
	if s == "" {
		return nil
	}
	if len(s) < 16 {
		return fmt.Errorf("ADMIN_SECRET must be at least 16 characters when set (got %d); unset it to disable break-glass admin creation", len(s))
	}
	return nil
}

// warnTabProxySuffixNotSameSite reports a tab-proxy suffix that cannot share a
// cookie with the panel.
//
// The whole design rests on one browser rule: the ticket cookie is set on the
// tab's own host by a request the panel makes, and it is carried back by an
// iframe that host serves. Both work while the two are same-SITE - the same
// registrable domain - because a SameSite=Strict cookie is then in play on a
// same-site request. Put the tabs on a different registrable domain and that
// cookie is simply never stored, every proxied tab reports a failure to
// authorize, and nothing anywhere says why.
//
// This is a WARNING rather than a refusal. The rest of the platform is
// unaffected, so declining to boot over a tab setting would trade a broken
// feature for a broken install. It is loud and it names the consequence, which
// is what the DNS_* notice next door does for the same reason.
//
// Skipped entirely when either side has no registrable domain - "localhost",
// a bare IP, an internal name. Browsers treat those by their own rules, a
// developer runs into them constantly, and a warning on every dev boot is a
// warning nobody reads.
func warnTabProxySuffixNotSameSite(frontendURL, suffix string) {
	if suffix == "" {
		return
	}
	u, err := url.Parse(strings.TrimSpace(frontendURL))
	if err != nil || u.Hostname() == "" {
		return
	}
	panelSite, perr := publicsuffix.EffectiveTLDPlusOne(u.Hostname())
	tabSite, terr := publicsuffix.EffectiveTLDPlusOne(suffix)
	if perr != nil || terr != nil {
		return
	}
	if strings.EqualFold(panelSite, tabSite) {
		return
	}
	log.Printf("config: TAB_PROXY_HOST_SUFFIX %q is on a different site than FRONTEND_URL %q (%s vs %s). "+
		"The tab ticket cookie is set on the tab host and read back from it, which the browser only allows "+
		"same-site, so every proxied tab will fail to authorize. Put the suffix under %s.",
		suffix, u.Hostname(), tabSite, panelSite, panelSite)
}

// warnPanelAPIURLIsAnotherOrigin says so when the split-hostname layout is
// configured, because a browser session cannot survive it any more.
//
// The session is a cookie now, host-only to the host that issued it. Point the
// panel's API at a second hostname and the login response sets that cookie for
// THAT host - which the browser will not even store, because a cross-origin
// fetch without credentials:'include' discards Set-Cookie, and Core answers no
// Access-Control-Allow-Credentials that would make including them work.
//
// The failure is quiet and looks like a broken server: the panel loads, the
// login form submits, and every request afterwards is a 401. So it is named
// here, once, at boot.
//
// It is a warning rather than a refusal because the variable still has a
// legitimate use - a non-browser client pointed at a second hostname, and any
// deployment where the two values are the same origin, which is not a split at
// all.
func warnPanelAPIURLIsAnotherOrigin(frontendURL, panelAPIURL string) {
	if !panelAPIURLIsAnotherOrigin(frontendURL, panelAPIURL) {
		return
	}
	log.Printf("config: PANEL_API_URL %q is a different origin than FRONTEND_URL %q. "+
		"The panel session is a host-only cookie, so a browser will not carry it across that split: "+
		"the panel will load and every API call will answer 401. Core serves the panel and the API "+
		"together - leave PANEL_API_URL empty unless nothing but non-browser clients use it.",
		panelAPIURL, frontendURL)
}

// panelAPIURLIsAnotherOrigin is the predicate on its own, so it can be asserted
// without capturing a log line. Unparseable or empty is NOT a split: this
// decides whether to warn, and a warning fired on a value nobody can read would
// point at the wrong thing.
func panelAPIURLIsAnotherOrigin(frontendURL, panelAPIURL string) bool {
	if strings.TrimSpace(panelAPIURL) == "" {
		return false
	}
	api, aerr := url.Parse(strings.TrimSpace(panelAPIURL))
	fe, ferr := url.Parse(strings.TrimSpace(frontendURL))
	if aerr != nil || ferr != nil || api.Host == "" || fe.Host == "" {
		return false
	}
	return !strings.EqualFold(api.Scheme, fe.Scheme) || !strings.EqualFold(api.Host, fe.Host)
}

// normalizeTabProxyHostSuffix cleans TAB_PROXY_HOST_SUFFIX into the bare DNS
// suffix the host matcher compares against: lowercase, no scheme, no port, no
// leading dot, no trailing dot.
//
// Every one of those is a shape an operator plausibly types - "https://share.x",
// ".share.x", "share.x." - and each would make the suffix match nothing at all
// while reading correct in the compose file. A tab that silently never resolves
// is a worse failure than a rejected value, so the input is repaired rather than
// refused. A value with no dot at all IS refused: a bare label cannot be a
// suffix under which per-tab subdomains live.
func normalizeTabProxyHostSuffix(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return ""
	}
	if i := strings.Index(v, "://"); i >= 0 {
		v = v[i+3:]
	}
	if i := strings.IndexByte(v, '/'); i >= 0 {
		v = v[:i]
	}
	if h, _, err := net.SplitHostPort(v); err == nil {
		v = h
	}
	v = strings.Trim(v, ".")
	if !strings.Contains(v, ".") {
		if v != "" {
			log.Printf("config: TAB_PROXY_HOST_SUFFIX %q is a single label, not a domain suffix; proxied custom tabs stay disabled", raw)
		}
		return ""
	}
	return v
}

// IsLocalOrigin reports whether a CORS Origin header points at the local machine
// or a private LAN address: localhost, 127.0.0.0/8, ::1, the RFC1918 ranges
// (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16) and IPv6 ULA (fc00::/7), on ANY
// port. Such origins are always allowed so a self-hoster reaches the panel over
// localhost or a LAN IP without configuring FRONTEND_URL. It never matches a
// public hostname - those must be named explicitly in FRONTEND_URL - so an empty
// configuration never implicitly trusts a public (vendor) origin. An unparseable
// or opaque origin (e.g. "null") returns false.
func IsLocalOrigin(origin string) bool {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}
