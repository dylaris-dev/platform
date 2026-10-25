package services

import (
	"context"
	"crypto/rand"
	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/store"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Voice chat ports. The public pool is per region; the in-container port is
// the same for Simple Voice Chat and Plasmo Voice.
const (
	VoicePortMin      = 26000
	VoicePortMax      = 26999
	VoiceInternalPort = 24454
)

// Redis keys of the voice mapping. Core writes them, the voice relay reads the
// port keys and the index, the node reads its server key. Both other sides
// compile their own reading of these shapes, so a field may be added but never
// renamed.
//
//	voice:port:<region>:<port>     {"v":1,"server_uuid":..,"key":..}
//	sys:index:voice:<region>       SET of <port> (decimal)
//	dylaris:server:<uuid>:voice    {"v":1,"region":..,"port":..,"internal_port":..,"key":..,"host":..}
func VoicePortKey(region string, port int) string {
	return "voice:port:" + region + ":" + strconv.Itoa(port)
}

func VoiceIndexKey(region string) string { return "sys:index:voice:" + region }

func ServerVoiceKey(serverUUID string) string { return "dylaris:server:" + serverUUID + ":voice" }

const (
	voiceIndexPrefix     = "sys:index:voice:"
	voicePortPrefix      = "voice:port:"
	voiceRelaySetKey     = "sys:voices"
	voiceRegistryPrefix  = "voice:registry:"
	voiceReconcilePeriod = 60 * time.Second
)

type voicePortEntry struct {
	V          int    `json:"v"`
	ServerUUID string `json:"server_uuid"`
	Key        string `json:"key"`
}

type serverVoiceEntry struct {
	V            int    `json:"v"`
	Region       string `json:"region"`
	Port         int    `json:"port"`
	InternalPort int    `json:"internal_port"`
	Key          string `json:"key"`
	Host         string `json:"host"`
}

// PublishVoice writes the three keys of one voice port. No TTL: the relay keeps
// serving the last map it loaded, and the reconcile is what removes them.
func PublishVoice(ctx context.Context, rdb *redis.Client, p store.VoicePort) error {
	key := hex.EncodeToString(p.UplinkKey)
	portJSON, err := json.Marshal(voicePortEntry{V: 1, ServerUUID: p.ServerUUID, Key: key})
	if err != nil {
		return err
	}
	serverJSON, err := json.Marshal(serverVoiceEntry{V: 1, Region: p.Region, Port: p.Port, InternalPort: p.InternalPort, Key: key, Host: p.Host})
	if err != nil {
		return err
	}
	pipe := rdb.TxPipeline()
	pipe.Set(ctx, VoicePortKey(p.Region, p.Port), portJSON, 0)
	pipe.SAdd(ctx, VoiceIndexKey(p.Region), strconv.Itoa(p.Port))
	pipe.Set(ctx, ServerVoiceKey(p.ServerUUID), serverJSON, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("publish voice port %s:%d: %w", p.Region, p.Port, err)
	}
	return nil
}

// RotateVoiceKey gives a server's voice port a new uplink key and publishes it.
// For a server that moved: the machine it left could read the old key (its
// node ACL covered the server's keys), and a REG signed with it would steer the
// server's voice to that machine.
func RotateVoiceKey(ctx context.Context, st store.Store, rdb *redis.Client, serverID int) error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	p, err := st.RotateVoiceUplinkKey(serverID, key)
	if err != nil || p == nil {
		return err
	}
	return PublishVoice(ctx, rdb, *p)
}

// UnpublishVoice removes the three keys of one voice port.
func UnpublishVoice(ctx context.Context, rdb *redis.Client, p store.VoicePort) error {
	pipe := rdb.TxPipeline()
	pipe.Del(ctx, VoicePortKey(p.Region, p.Port))
	pipe.SRem(ctx, VoiceIndexKey(p.Region), strconv.Itoa(p.Port))
	pipe.Del(ctx, ServerVoiceKey(p.ServerUUID))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("unpublish voice port %s:%d: %w", p.Region, p.Port, err)
	}
	return nil
}

// VoiceRelay is one live voice relay as it registers itself.
type VoiceRelay struct {
	ID     string `json:"voice_id"`
	Region string `json:"region"`
	IP     string `json:"ip"`
}

// LiveVoiceRelays lists the relays whose registry key is still alive. A live
// relay's registrar prunes members whose key expired, but only while some relay
// runs: a member without a registry key is a relay that stopped heartbeating
// and is skipped.
func LiveVoiceRelays(ctx context.Context, rdb *redis.Client) ([]VoiceRelay, error) {
	ids, err := rdb.SMembers(ctx, voiceRelaySetKey).Result()
	if err != nil {
		return nil, fmt.Errorf("list voice relays: %w", err)
	}
	sort.Strings(ids)
	var out []VoiceRelay
	for _, id := range ids {
		raw, err := rdb.Get(ctx, voiceRegistryPrefix+id).Bytes()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read voice relay %s: %w", id, err)
		}
		var r VoiceRelay
		if err := json.Unmarshal(raw, &r); err != nil {
			log.Printf("[voice] relay %s: unreadable registry entry: %v", id, err)
			continue
		}
		if r.ID == "" {
			r.ID = id
		}
		out = append(out, r)
	}
	return out, nil
}

// VoiceRouteFor picks the address players type for voice, and the relay region
// behind it. The region comes from the DOMAIN, not from servers.region: a move
// keeps the domain and only re-points the tunnel, so the relay must not change.
//
// Only a domain exactly one label under an online edge's wildcard qualifies,
// because that is the only name the wildcard A record resolves. A custom domain
// has no voice address. Among several qualifying routes the lexically first wins,
// so the answer does not depend on Redis ordering.
func VoiceRouteFor(routes []GatewayRoute, edges []GatewayEdgeInfo, serverUUID string) (host, region string, ok bool) {
	var domains []string
	for _, rt := range routes {
		if RouteBelongsToServer(rt, serverUUID) {
			domains = append(domains, strings.ToLower(rt.Domain))
		}
	}
	sort.Strings(domains)
	for _, d := range domains {
		if region, ok := VoiceRegionFor(d, edges); ok {
			return d, region, true
		}
	}
	return "", "", false
}

// VoiceRegionFor is the relay region behind one domain, by the rule above.
func VoiceRegionFor(domain string, edges []GatewayEdgeInfo) (region string, ok bool) {
	d := strings.ToLower(domain)
	for _, e := range edges {
		if e.Status != "online" || e.Region == "" || !strings.HasPrefix(e.Wildcard, "*.") {
			continue
		}
		suffix := strings.ToLower(e.Wildcard[1:]) // ".eu.dylaris.com"
		label, found := strings.CutSuffix(d, suffix)
		if found && label != "" && !strings.Contains(label, ".") {
			return e.Region, true
		}
	}
	return "", false
}

// VoiceService keeps Redis in step with the voice_ports table. Core is the
// source of truth: the leader rewrites every row's keys and deletes keys that
// have no row, which covers every delete path including the ones that do not
// know voice exists (the FK cascade on servers).
type VoiceService struct {
	store  store.Store
	redis  *redis.Client
	leader leader.Election
	// revoke drops the row and keys and must not wait on the node: the config
	// write-back runs in the background. It lives in handlers, which can reach
	// the node; nil skips revocation.
	revoke func(ctx context.Context, srv *models.Server) error
}

func NewVoiceService(st store.Store, rdb *redis.Client) *VoiceService {
	return &VoiceService{store: st, redis: rdb}
}

// SetLeader wires the leader-election gate. Call once at boot.
func (v *VoiceService) SetLeader(l leader.Election) { v.leader = l }

// SetRevoke wires the disable path the reconcile runs for a server that is no
// longer allowed voice. Call once at boot.
func (v *VoiceService) SetRevoke(f func(ctx context.Context, srv *models.Server) error) { v.revoke = f }

func (v *VoiceService) Start(ctx context.Context) {
	if v.redis == nil {
		return
	}
	ticker := time.NewTicker(voiceReconcilePeriod)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := v.Reconcile(ctx); err != nil {
					log.Printf("[voice] reconcile: %v", err)
				}
				v.Meter(ctx)
			}
		}
	}()
}

// Reconcile rewrites the keys of every row and removes the keys without one.
// A follower does nothing.
//
// The keys are collected BEFORE the rows are read. A port allocated in between
// commits its row before it publishes, so it is either in the rows or not yet in
// Redis - never swept as an orphan. A row deleted in between gets its keys
// rewritten once and removed on the next tick.
func (v *VoiceService) Reconcile(ctx context.Context) error {
	if v.leader != nil && !v.leader.IsLeader() {
		return nil
	}
	portKeys, err := v.scan(ctx, voicePortPrefix+"*")
	if err != nil {
		return err
	}
	indexKeys, err := v.scan(ctx, voiceIndexPrefix+"*")
	if err != nil {
		return err
	}
	serverKeys, err := v.scan(ctx, "dylaris:server:*:voice")
	if err != nil {
		return err
	}
	indexMembers := map[string][]string{}
	for _, k := range indexKeys {
		m, err := v.redis.SMembers(ctx, k).Result()
		if err != nil {
			return fmt.Errorf("read %s: %w", k, err)
		}
		indexMembers[k] = m
	}

	rows, err := v.store.ListVoicePorts()
	if err != nil {
		return fmt.Errorf("list voice ports: %w", err)
	}
	keep := map[string]bool{}
	for _, p := range rows {
		if v.revoked(ctx, p) {
			// Its keys are not kept: the disable removed them, and the sweep
			// below takes whatever a failed unpublish left.
			continue
		}
		// The row is read again right before the write: the revocations above
		// can take a while, and a row the API deleted or moved meanwhile must
		// not be republished from the stale list.
		cur, err := v.store.GetVoicePort(p.ServerID)
		if err != nil {
			log.Printf("[voice] reconcile: server %d: re-read before publish: %v", p.ServerID, err)
			keep[VoicePortKey(p.Region, p.Port)] = true
			keep[ServerVoiceKey(p.ServerUUID)] = true
			continue
		}
		if cur == nil {
			continue
		}
		keep[VoicePortKey(cur.Region, cur.Port)] = true
		keep[ServerVoiceKey(cur.ServerUUID)] = true
		if cur.Region != p.Region || cur.Port != p.Port {
			// Moved: whoever moved it published the new keys.
			continue
		}
		// cur, not p: same port, but whatever else the API changed since the
		// list was read (host, uplink key) is what the relay must get.
		if err := PublishVoice(ctx, v.redis, *cur); err != nil {
			return err
		}
	}

	if err := v.reconcileLinkRoutes(ctx, keep); err != nil {
		return err
	}

	pipe := v.redis.Pipeline()
	for _, k := range append(portKeys, serverKeys...) {
		if !keep[k] {
			pipe.Del(ctx, k)
		}
	}
	for k, members := range indexMembers {
		region := strings.TrimPrefix(k, voiceIndexPrefix)
		for _, m := range members {
			port, err := strconv.Atoi(m)
			if err != nil || !keep[VoicePortKey(region, port)] {
				pipe.SRem(ctx, k, m)
			}
		}
	}
	if pipe.Len() > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("sweep voice orphans: %w", err)
		}
	}
	return nil
}

// reconcileLinkRoutes is the server loop above for route-only ports: revoke
// what is no longer allowed, re-read, republish, and mark what to keep.
func (v *VoiceService) reconcileLinkRoutes(ctx context.Context, keep map[string]bool) error {
	rows, err := v.store.ListLinkRouteVoice()
	if err != nil {
		return fmt.Errorf("list route voice ports: %w", err)
	}
	for _, rv := range rows {
		// Same gate as the servers' revoked: no revoke wired, no revocation.
		allowed, by, err := true, "", error(nil)
		if v.revoke != nil {
			allowed, by, err = ResolveVoiceAllowed(v.store, &models.Server{OwnerID: rv.OwnerID})
		}
		if err == nil && !allowed {
			if _, err := v.store.DeleteLinkRouteVoice(rv.Domain); err != nil {
				log.Printf("[voice] reconcile: route %s: disable failed: %v", rv.Domain, err)
			} else {
				log.Printf("voice: route %s disabled, not allowed (%s)", rv.Domain, by)
				continue
			}
		} else if err != nil {
			log.Printf("[voice] reconcile: route %s: permission check: %v", rv.Domain, err)
		}
		cur, err := v.store.GetLinkRouteVoice(rv.Domain)
		if err != nil {
			log.Printf("[voice] reconcile: route %s: re-read before publish: %v", rv.Domain, err)
			keep[VoicePortKey(rv.Region, rv.Port)] = true
			continue
		}
		if cur == nil {
			continue
		}
		keep[VoicePortKey(cur.Region, cur.Port)] = true
		if cur.Region != rv.Region || cur.Port != rv.Port {
			continue
		}
		if err := PublishLinkRouteVoice(ctx, v.redis, *cur); err != nil {
			return err
		}
	}
	return nil
}

// revoked turns voice off for a row whose server is no longer allowed it, and
// reports whether it did. A read error or a failed disable keeps the row: the
// next tick asks again.
func (v *VoiceService) revoked(ctx context.Context, p store.VoicePort) bool {
	if v.revoke == nil {
		return false
	}
	srv, err := v.store.GetServerByID(p.ServerID)
	if err != nil || srv == nil {
		log.Printf("[voice] reconcile: server %d: read for the permission check: %v", p.ServerID, err)
		return false
	}
	allowed, by, err := ResolveVoiceAllowed(v.store, srv)
	if err != nil {
		log.Printf("[voice] reconcile: server %d: permission check: %v", p.ServerID, err)
		return false
	}
	if allowed {
		return false
	}
	if err := v.revoke(ctx, srv); err != nil {
		log.Printf("[voice] reconcile: server %d: disable failed: %v", p.ServerID, err)
		return false
	}
	log.Printf("voice: server %d disabled, not allowed (%s)", p.ServerID, by)
	return true
}

// voiceSeenPrefix holds, per server, the vrx:/vtx: values Meter has already
// written to voice_usage, under the same field names as the counter.
const voiceSeenPrefix = "dylaris:voice:agg:seen:"

// Meter adds each voice server's new vrx:/vtx: bytes to voice_usage. A follower
// does nothing.
//
// This is observability, not billing: the bill gets the same bytes through the
// TrafficAggregator, inside player traffic. So it covers every server with a
// voice port, including platform servers no tenant pays for. The DB is written
// first and the seen hash after, so a failed write is retried next tick. A
// missing seen field bills in full: there is no voice history before Core.
//
// A server whose port was released between flushes loses its last unmetered
// bytes here (not in the bill).
func (v *VoiceService) Meter(ctx context.Context) {
	if v.leader != nil && !v.leader.IsLeader() {
		return
	}
	ports, err := v.store.ListVoicePorts()
	if err != nil {
		log.Printf("[voice] meter: list voice ports: %v", err)
		return
	}
	period := monthStartUTC(time.Now())
	for _, p := range ports {
		fields, err := v.redis.HGetAll(ctx, trafficEdgePrefix+p.ServerUUID).Result()
		if err != nil {
			log.Printf("[voice] meter %s: read counter: %v", p.ServerUUID, err)
			continue
		}
		seenKey := voiceSeenPrefix + p.ServerUUID
		seen, err := v.redis.HGetAll(ctx, seenKey).Result()
		if err != nil {
			log.Printf("[voice] meter %s: read seen: %v", p.ServerUUID, err)
			continue
		}
		// Renewed on every read, not only when a value changes: the counter
		// outlives an idle seen hash (the edge refreshes it on every flush), and
		// a seen hash that expired would re-add the lifetime voice bytes.
		v.redis.Expire(ctx, seenKey, trafficSeenTTL)
		for region, cur := range voiceDirBytes(fields) {
			rxField, txField := "vrx:"+region, "vtx:"+region
			rx := voiceDelta(cur[0], seen[rxField])
			tx := voiceDelta(cur[1], seen[txField])
			if rx == 0 && tx == 0 {
				if seen[rxField] == strconv.FormatInt(cur[0], 10) && seen[txField] == strconv.FormatInt(cur[1], 10) {
					continue
				}
				// Nothing to add, but a seen value that does not match (unset
				// at zero, or not a number) is brought up to date.
			} else if err := v.store.AddVoiceUsage(p.ServerID, period, region, rx, tx); err != nil {
				log.Printf("[voice] meter %s/%s: add usage: %v", p.ServerUUID, region, err)
				continue
			}
			pipe := v.redis.TxPipeline()
			pipe.HSet(ctx, seenKey, rxField, cur[0], txField, cur[1])
			pipe.Expire(ctx, seenKey, trafficSeenTTL)
			if _, err := pipe.Exec(ctx); err != nil {
				log.Printf("[voice] meter %s/%s: write seen: %v", p.ServerUUID, region, err)
			}
		}
	}
}

// voiceDelta is current minus the seen value. A missing seen value is zero; a
// current below it is a counter that was reset and counts as current; a seen
// value that is not a number cannot say what was written and starts over.
func voiceDelta(current int64, seenRaw string) int64 {
	if seenRaw == "" {
		return current
	}
	last, err := strconv.ParseInt(seenRaw, 10, 64)
	if err != nil {
		return 0
	}
	if current < last {
		return current
	}
	return current - last
}

func (v *VoiceService) scan(ctx context.Context, match string) ([]string, error) {
	var out []string
	iter := v.redis.Scan(ctx, 0, match, 200).Iterator()
	for iter.Next(ctx) {
		out = append(out, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", match, err)
	}
	return out, nil
}
