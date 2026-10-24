package handlers

import (
	"context"
	"encoding/json"
	"math/rand"
	"strings"

	"github.com/redis/go-redis/v9"
)

// BeamRelayInfo describes one discovered relay. Mirrors what the relay's
// heartbeat publishes so we can surface it raw in the admin UI.
type BeamRelayInfo struct {
	BeamID     string `json:"beam_id"`
	IP         string `json:"ip"`
	PrivateIP  string `json:"private_ip,omitempty"`
	PublicHost string `json:"public_host,omitempty"` // e.g. "beam.dylaris.com" — preferred over IP
	// Region groups relays the way EdgeRegion groups edges. Matched against the
	// region of the NODE a transfer targets, because the payload path is
	// client -> relay -> node: a relay near the client but far from the node
	// makes the round trip worse, not better.
	Region       string `json:"region,omitempty"`
	ServicePort  string `json:"service_port"`
	ClientPort   string `json:"client_port,omitempty"`
	DownloadPort string `json:"download_port,omitempty"`
	Timestamp    int64  `json:"timestamp"`
}

// DiscoverBeamRelays reads sys:beams and resolves each entry against the
// corresponding beam:registry:<id> key. Stale entries (TTL expired) are
// skipped silently — the relay's own cleanup loop will eventually prune
// them from sys:beams.
func DiscoverBeamRelays(ctx context.Context, rdb *redis.Client) []BeamRelayInfo {
	if rdb == nil {
		return nil
	}
	ids, err := rdb.SMembers(ctx, beamRegistrySet).Result()
	if err != nil {
		return nil
	}
	out := make([]BeamRelayInfo, 0, len(ids))
	for _, id := range ids {
		raw, err := rdb.Get(ctx, "beam:registry:"+id).Bytes()
		if err != nil {
			continue
		}
		var info BeamRelayInfo
		if json.Unmarshal(raw, &info) != nil {
			continue
		}
		if info.BeamID == "" {
			info.BeamID = id
		}
		out = append(out, info)
	}
	return out
}

// resolveRelay returns the effective public address for Beam Desktop
// clients. Discovery supplies which relay is alive and its port; the
// externally reachable host comes from publicHost (DB setting
// beam.public_host) — the relay's own registered IP is an internal
// overlay address a desktop client can't reach. Manual override (DB
// setting beam.relay_address) still wins outright for incident routing.
func resolveRelay(ctx context.Context, rdb *redis.Client, manualOverride, publicHost, preferredRegion string) (string, string) {
	if strings.TrimSpace(manualOverride) != "" {
		return manualOverride, "manual"
	}
	if info, ok := PickBeamRelay(ctx, rdb, preferredRegion); ok {
		// A relay that reports its own public host knows better than one
		// fleet-wide setting can: with several regions there is no single
		// correct hostname. The setting stays the fallback, which is what a
		// relay predating BEAM_PUBLIC_HOST relies on.
		if info.PublicHost == "" {
			info.PublicHost = strings.TrimSpace(publicHost)
		}
		port := info.ClientPort
		if port == "" {
			port = info.ServicePort
		}
		addr := info.PublicAddress(port)
		if addr != "" {
			return addr, "discovered"
		}
	}
	return "", ""
}

// Beam relay discovery — matches the heartbeat pattern in
// gateway/beam/relay: each relay writes JSON to beam:registry:<id> with a
// 30s TTL and adds its id to the sys:beams set. The Core resolves "alive"
// relays by intersecting the set with surviving keys.
const beamRegistrySet = "sys:beams"

// PickBeamRelay returns one live relay, preferring preferredRegion and falling
// back to any relay when that region has none.
//
// The random choice among equals is the load balancing: several relays sharing
// one region (and therefore one DNS name) split traffic and can be rolled one at
// a time. The region is a HARD filter rather than a score because the wrong
// region is not "slightly slower" - it can double a transatlantic hop.
//
// Falling back to another region rather than failing is deliberate: a transfer
// over a distant relay beats no transfer at all, and because clients re-resolve
// on every connect, the next one returns to the right region on its own the
// moment a relay there registers. No re-check loop is needed for that.
func PickBeamRelay(ctx context.Context, rdb *redis.Client, preferredRegion string) (BeamRelayInfo, bool) {
	relays := DiscoverBeamRelays(ctx, rdb)
	if len(relays) == 0 {
		return BeamRelayInfo{}, false
	}
	if region := strings.TrimSpace(preferredRegion); region != "" {
		matching := make([]BeamRelayInfo, 0, len(relays))
		for _, r := range relays {
			if strings.EqualFold(strings.TrimSpace(r.Region), region) {
				matching = append(matching, r)
			}
		}
		if len(matching) > 0 {
			relays = matching
		}
	}
	return relays[rand.Intn(len(relays))], true
}

// PublicAddress returns the host:port the panel should hand out to clients.
// Prefers the operator-configured public host over the internal overlay IP
// so we never accidentally publish a 172.x address to a browser.
func (i BeamRelayInfo) PublicAddress(port string) string {
	host := strings.TrimSpace(i.PublicHost)
	if host == "" {
		host = i.IP
	}
	if host == "" {
		host = i.BeamID
	}
	if port == "" || strings.Contains(host, ":") {
		return host
	}
	return host + ":" + port
}
