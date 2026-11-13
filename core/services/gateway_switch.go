package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"dylaris-core/config"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// ErrGatewayRemote is returned for an operation the managed (remote) gateway
// does not offer, or when remote mode is on but no remote provider is wired.
var ErrGatewayRemote = errors.New("not available with the managed gateway")

// DefaultGatewayProviderURL is the provider used when remote mode is on and
// neither the environment nor the settings name one.
const DefaultGatewayProviderURL = "https://panel.dylaris.com"

const (
	SettingGatewayProvider    = "gateway.provider"
	SettingGatewayProviderURL = "gateway.provider_url"
	SettingGatewayProviderKey = "gateway.provider_key"
)

// ProviderConfig is the resolved gateway provider choice.
type ProviderConfig struct {
	Remote bool
	URL    string
	Key    string
	// KeyFromEnv / URLFromEnv tell the panel a field is pinned by the
	// environment and cannot be edited there.
	KeyFromEnv, URLFromEnv bool
}

// ErrGatewayProviderUnknown is returned while the provider setting has never
// been read successfully. The gateway never guesses: guessing "local" on a
// remote install would let a delete succeed against a Redis that holds nothing
// and leave the real route on the provider.
var ErrGatewayProviderUnknown = errors.New("gateway provider setting could not be read")

// LoadProviderConfig resolves the provider per field: the environment wins over
// the setting. An env var set to the empty string counts as NOT set (config
// hands it over as ""), unlike the usual trap where a set-but-empty variable
// overrides a default. Remote needs mode=remote AND a non-empty key, so a
// platform that never configured a key can never make an outbound call.
//
// A missing settings row is not an error; any other read error is.
// Known gap: a stored provider key that fails to decrypt reads as "" (GetSetting
// logs and returns empty), which is indistinguishable from "no key".
func LoadProviderConfig(st store.Store, cfg *config.Config) (ProviderConfig, error) {
	var pc ProviderConfig
	var firstErr error
	get := func(k string) string {
		v, err := st.GetSetting(k)
		if err != nil && !errors.Is(err, sql.ErrNoRows) && firstErr == nil {
			firstErr = fmt.Errorf("read %s: %w", k, err)
		}
		return strings.TrimSpace(v)
	}

	pc.URL = strings.TrimSpace(cfg.GatewayProviderURL)
	pc.URLFromEnv = pc.URL != ""
	if !pc.URLFromEnv {
		pc.URL = get(SettingGatewayProviderURL)
	}
	if pc.URL == "" {
		pc.URL = DefaultGatewayProviderURL
	}

	pc.Key = strings.TrimSpace(cfg.GatewayProviderKey)
	pc.KeyFromEnv = pc.Key != ""
	if !pc.KeyFromEnv {
		pc.Key = get(SettingGatewayProviderKey)
	}

	pc.Remote = get(SettingGatewayProvider) == "remote" && pc.Key != ""
	if firstErr != nil {
		return ProviderConfig{}, firstErr
	}
	return pc, nil
}

// CachedProviderMode wraps fn so it is evaluated at most once per ttl and is
// safe for concurrent use. On an error it keeps the last good answer
// (known=true) for another ttl; with no good answer yet it reports
// known=false and retries at most once per second. Either way a database stall
// costs one settings read per window, not one per request: every link
// heartbeat and API call asks this.
//
// No lock is held across fn: one caller reads, and while it does every other
// caller gets the last good answer (or unknown) at once. A database that hangs
// instead of failing therefore holds up that one caller, never the rest.
// reset makes the next call re-read, so a saved provider setting applies at
// once; a read already running when reset is called is not stored.
func CachedProviderMode(ttl time.Duration, fn func() (bool, error)) (mode func() (remote, known bool), reset func()) {
	return cachedProviderMode(ttl, time.Now, fn)
}

// providerModeColdRetry spaces the reads while no good answer exists yet.
const providerModeColdRetry = time.Second

func cachedProviderMode(ttl time.Duration, now func() time.Time, fn func() (bool, error)) (func() (remote, known bool), func()) {
	var (
		mu         sync.Mutex
		val        bool
		have       bool
		expires    time.Time
		refreshing bool
		gen        int
	)
	reset := func() {
		mu.Lock()
		expires = time.Time{}
		gen++
		mu.Unlock()
	}
	read := func() (v bool, err error) {
		// A panic must not leave refreshing set, or the mode would never be
		// read again.
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("read gateway provider mode: panic: %v", r)
			}
		}()
		return fn()
	}
	return func() (bool, bool) {
		mu.Lock()
		if refreshing || now().Before(expires) {
			v, h := val, have
			mu.Unlock()
			return v, h
		}
		refreshing = true
		started := gen
		mu.Unlock()

		v, err := read()

		mu.Lock()
		defer mu.Unlock()
		refreshing = false
		if started != gen {
			// Reset while reading: this answer may predate the saved setting.
			return val, have
		}
		t := now()
		switch {
		case err == nil:
			val, have = v, true
			expires = t.Add(ttl)
		case have:
			expires = t.Add(ttl)
		default:
			expires = t.Add(providerModeColdRetry)
		}
		return val, have
	}, reset
}

// LocalGatewayErr is nil when local gateway state (Redis, Hub) may be used:
// the mode is known and local. A nil mode is local (tests, bare state).
// Otherwise it is ErrGatewayRemote or ErrGatewayProviderUnknown; an unknown
// mode is never read as local.
func LocalGatewayErr(mode func() (remote, known bool)) error {
	if mode == nil {
		return nil
	}
	remote, known := mode()
	if !known {
		return ErrGatewayProviderUnknown
	}
	if remote {
		return ErrGatewayRemote
	}
	return nil
}

// ActiveRemoteGateway is rg while the mode is known and remote. Otherwise it is
// nil with LocalGatewayErr(mode): nil for local, so the caller runs its local
// path, or the sentinel for an unknown mode (and for remote with no rg wired),
// which gate.GatewayModeError answers.
func ActiveRemoteGateway(mode func() (remote, known bool), rg *RemoteGateway) (*RemoteGateway, error) {
	err := LocalGatewayErr(mode)
	if errors.Is(err, ErrGatewayRemote) && rg != nil {
		return rg, nil
	}
	return nil, err
}

// LocalGatewayRedis is rdb while local gateway state may be read, nil
// otherwise. Readers that treat a nil client as "nothing registered" (beam
// relay discovery, the DNS check's edge list) get the empty answer without a
// branch of their own.
func LocalGatewayRedis(rdb *redis.Client, mode func() (remote, known bool)) *redis.Client {
	if LocalGatewayErr(mode) != nil {
		return nil
	}
	return rdb
}

// SwitchingGateway implements GatewayProvider by handing every call to the
// local Redis gateway or to the remote one, decided per call. Flipping the
// provider therefore needs no restart.
type SwitchingGateway struct {
	local    *RedisGateway
	remote   GatewayProvider
	remoteOn func() (remote, known bool)
}

// NewSwitchingGateway: remote may be nil (remote mode configured, provider not
// wired); every call then fails with ErrGatewayRemote instead of reaching the
// local gateway, so a remote platform never writes to a Hub it does not have.
func NewSwitchingGateway(local *RedisGateway, remote GatewayProvider, remoteOn func() (remote, known bool)) *SwitchingGateway {
	return &SwitchingGateway{local: local, remote: remote, remoteOn: remoteOn}
}

// pick returns the provider for this call. err is ErrGatewayProviderUnknown when
// the mode is not known, ErrGatewayRemote when remote is on but not wired.
func (g *SwitchingGateway) pick() (GatewayProvider, error) {
	if g.remoteOn == nil {
		return g.local, nil
	}
	remote, known := g.remoteOn()
	if !known {
		return nil, ErrGatewayProviderUnknown
	}
	if !remote {
		return g.local, nil
	}
	if g.remote == nil {
		return nil, ErrGatewayRemote
	}
	return g.remote, nil
}

func (g *SwitchingGateway) CreateServerRoute(serverID uint, ownerID string, domain string, port int) error {
	p, err := g.pick()
	if err != nil {
		return err
	}
	return p.CreateServerRoute(serverID, ownerID, domain, port)
}

func (g *SwitchingGateway) SetServerProxyProtocol(serverUUID string, enabled bool) error {
	p, err := g.pick()
	if err != nil {
		return err
	}
	return p.SetServerProxyProtocol(serverUUID, enabled)
}

func (g *SwitchingGateway) SetCoreOwnedRouteProxyProtocol(domain string, enabled bool) error {
	p, err := g.pick()
	if err != nil {
		return err
	}
	return p.SetCoreOwnedRouteProxyProtocol(domain, enabled)
}

func (g *SwitchingGateway) CreateRouteViaLink(ownerID string, domain string, linkToken string, targetHost string, port int) error {
	p, err := g.pick()
	if err != nil {
		return err
	}
	return p.CreateRouteViaLink(ownerID, domain, linkToken, targetHost, port)
}

func (g *SwitchingGateway) DeleteCoreOwnedRoute(domain string) error {
	p, err := g.pick()
	if err != nil {
		return err
	}
	return p.DeleteCoreOwnedRoute(domain)
}

func (g *SwitchingGateway) SuspendCoreOwnedRoute(domain, reason string) (string, error) {
	p, err := g.pick()
	if err != nil {
		return "", err
	}
	return p.SuspendCoreOwnedRoute(domain, reason)
}

func (g *SwitchingGateway) UnsuspendCoreOwnedRoute(domain string) (string, error) {
	p, err := g.pick()
	if err != nil {
		return "", err
	}
	return p.UnsuspendCoreOwnedRoute(domain)
}

func (g *SwitchingGateway) DeleteRoute(domain string) error {
	p, err := g.pick()
	if err != nil {
		return err
	}
	return p.DeleteRoute(domain)
}

func (g *SwitchingGateway) DeleteServerRoutes(serverUUID string) error {
	p, err := g.pick()
	if err != nil {
		return err
	}
	return p.DeleteServerRoutes(serverUUID)
}

func (g *SwitchingGateway) MigrateServerRoutes(serverID uint, newNodeID uint) error {
	p, err := g.pick()
	if err != nil {
		return err
	}
	return p.MigrateServerRoutes(serverID, newNodeID)
}

// ForgetNode drops a deleted node's managed-gateway kit and route rows while
// the managed gateway is the provider; on a local gateway there is nothing to
// do. While the mode is unknown nothing is guessed: a kit left behind is
// reported and the sentinel returned.
func (g *SwitchingGateway) ForgetNode(ctx context.Context, nodeID uint) error {
	p, err := g.pick()
	rg, _ := g.remote.(*RemoteGateway)
	if rg == nil {
		if errors.Is(err, ErrGatewayRemote) {
			return nil // remote on, nothing wired: no kit was ever minted here
		}
		return err
	}
	if errors.Is(err, ErrGatewayProviderUnknown) {
		rg.ReportKitLeft(nodeID)
		return err
	}
	if p == GatewayProvider(rg) {
		rg.ForgetNode(ctx, nodeID)
	}
	return nil
}

func (g *SwitchingGateway) LinkToken(nodeID string) string {
	p, err := g.pick()
	if err != nil {
		return ""
	}
	return p.LinkToken(nodeID)
}

func (g *SwitchingGateway) DiscoveryProof(nodeID string) string {
	p, err := g.pick()
	if err != nil {
		return ""
	}
	return p.DiscoveryProof(nodeID)
}

func (g *SwitchingGateway) SetNodeLinkEnabled(linkToken string, enabled bool) error {
	p, err := g.pick()
	if err != nil {
		return err
	}
	return p.SetNodeLinkEnabled(linkToken, enabled)
}
