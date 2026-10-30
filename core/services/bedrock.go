package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"dylaris-core/pkg/leader"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Redis contract with the bedrock-bridge (gateway repo compiles its own reader):
//
//	dylaris:server:<uuid>:floodgate   standard base64 of the 16 raw key bytes, no TTL
//	sys:bedrock                       SET of live bridge ids
//	bedrock:registry:<id>             {"id","region","version"}, TTL 10 s
const (
	bedrockSetKey          = "sys:bedrock"
	bedrockRegistryPrefix  = "bedrock:registry:"
	bedrockReconcilePeriod = 60 * time.Second
	floodgateKeyBytes      = 16
)

func FloodgateKeyRedisKey(uuid string) string { return "dylaris:server:" + uuid + ":floodgate" }

// BedrockEntry is one live bridge as it registers itself.
type BedrockEntry struct {
	ID      string `json:"id"`
	Region  string `json:"region"`
	Version string `json:"version"`
}

// LiveBedrockEntries lists the bridges whose registry key is still alive; a set
// member without one stopped heartbeating and is skipped.
func LiveBedrockEntries(ctx context.Context, rdb *redis.Client) ([]BedrockEntry, error) {
	ids, err := rdb.SMembers(ctx, bedrockSetKey).Result()
	if err != nil {
		return nil, fmt.Errorf("list bedrock entries: %w", err)
	}
	var out []BedrockEntry
	for _, id := range ids {
		raw, err := rdb.Get(ctx, bedrockRegistryPrefix+id).Bytes()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read bedrock entry %s: %w", id, err)
		}
		var e BedrockEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			log.Printf("[bedrock] entry %s: unreadable registry entry: %v", id, err)
			continue
		}
		if e.ID == "" {
			e.ID = id
		}
		out = append(out, e)
	}
	return out, nil
}

// BedrockService keeps Redis in step with the bedrock_servers table. Core is
// the source of truth: the leader rewrites every key and deletes keys without a row.
type BedrockService struct {
	store  store.Store
	redis  *redis.Client
	leader leader.Election
	flags  *FeatureFlags
}

func NewBedrockService(st store.Store, rdb *redis.Client) *BedrockService {
	return &BedrockService{store: st, redis: rdb}
}

func (b *BedrockService) SetLeader(l leader.Election) { b.leader = l }

// SetFlags lets Reconcile fold the platform's Bedrock switch into the editions.
func (b *BedrockService) SetFlags(f *FeatureFlags) { b.flags = f }

// Enable stores a fresh key and publishes the STORED one: EnableBedrock keeps
// the first key on a re-enable, so publishing the fresh one would put a key in
// Redis that the table (and every later reconcile) does not have.
func (b *BedrockService) Enable(ctx context.Context, serverID int, uuid string) error {
	key := make([]byte, floodgateKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("generate floodgate key: %w", err)
	}
	if err := b.store.EnableBedrock(serverID, key); err != nil {
		return fmt.Errorf("store floodgate key: %w", err)
	}
	stored, ok, err := b.store.GetBedrockKey(serverID)
	if err != nil {
		return fmt.Errorf("read floodgate key: %w", err)
	}
	if !ok {
		return fmt.Errorf("floodgate key of server %d missing after enable", serverID)
	}
	return b.publish(ctx, uuid, stored)
}

// Disable deletes the row first, then the key. A failed DEL is swept by the reconcile.
func (b *BedrockService) Disable(ctx context.Context, serverID int, uuid string) error {
	if err := b.store.DisableBedrock(serverID); err != nil {
		return fmt.Errorf("delete bedrock row: %w", err)
	}
	if err := b.redis.Del(ctx, FloodgateKeyRedisKey(uuid)).Err(); err != nil {
		return fmt.Errorf("delete floodgate key: %w", err)
	}
	return nil
}

func (b *BedrockService) publish(ctx context.Context, uuid string, key []byte) error {
	if err := b.redis.Set(ctx, FloodgateKeyRedisKey(uuid), base64.StdEncoding.EncodeToString(key), 0).Err(); err != nil {
		return fmt.Errorf("publish floodgate key: %w", err)
	}
	return nil
}

// Reconcile republishes every enabled key and removes keys that have no row.
// A list error skips the tick and deletes nothing. The keys are collected
// BEFORE the rows are read, so a key published in between is in the rows or
// not yet in Redis, never swept as an orphan. A follower does nothing.
func (b *BedrockService) Reconcile(ctx context.Context) error {
	if b.leader != nil && !b.leader.IsLeader() {
		return nil
	}
	// Editions first: a server that gained a Bedrock route gets its key here,
	// so the sweep below already sees the row.
	if err := b.PublishEditions(ctx, BedrockActive(ctx, b.flags)); err != nil {
		return err
	}
	var existing []string
	iter := b.redis.Scan(ctx, 0, "dylaris:server:*:floodgate", 200).Iterator()
	for iter.Next(ctx) {
		existing = append(existing, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("scan floodgate keys: %w", err)
	}
	rows, err := b.store.ListBedrockServers()
	if err != nil {
		return fmt.Errorf("list bedrock servers: %w", err)
	}
	keep := map[string]bool{}
	for _, r := range rows {
		keep[FloodgateKeyRedisKey(r.ServerUUID)] = true
		if err := b.publish(ctx, r.ServerUUID, r.Key); err != nil {
			return err
		}
	}
	// Route-only keys were published by PublishEditions above; they only
	// have to survive the sweep. Read after the scan, like the servers.
	links, err := b.store.ListLinkRouteEditions()
	if err != nil {
		return fmt.Errorf("list link route editions: %w", err)
	}
	for _, r := range links {
		keep[FloodgateKeyRedisKey(LinkRouteBedrockSubject(r.OwnerID, r.Domain))] = true
	}
	for _, k := range existing {
		if !keep[k] && strings.HasSuffix(k, ":floodgate") {
			if err := b.redis.Del(ctx, k).Err(); err != nil {
				return fmt.Errorf("sweep floodgate orphan: %w", err)
			}
		}
	}
	return nil
}

func (b *BedrockService) Start(ctx context.Context) {
	if b.redis == nil {
		return
	}
	ticker := time.NewTicker(bedrockReconcilePeriod)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := b.Reconcile(ctx); err != nil {
					log.Printf("[bedrock] reconcile: %v", err)
				}
				b.Meter(ctx)
			}
		}
	}()
}

// bedrockSeenPrefix holds, per server, the brx:/btx: values Meter has already
// written to bedrock_usage, under the same field names as the counter.
const bedrockSeenPrefix = "dylaris:bedrock:agg:seen:"

// Meter adds each Bedrock server's new brx:/btx: bytes to bedrock_usage. A
// follower does nothing. Observability, not billing: the bill gets the same
// bytes through the TrafficAggregator, inside player traffic. Mirrors
// VoiceService.Meter: DB first, seen hash after, so a failed write is retried;
// the seen hash is renewed on every read so it cannot expire under a counter
// that outlives it and re-add the lifetime bytes.
func (b *BedrockService) Meter(ctx context.Context) {
	if b.leader != nil && !b.leader.IsLeader() {
		return
	}
	rows, err := b.store.ListBedrockServerRefs()
	if err != nil {
		log.Printf("[bedrock] meter: list servers: %v", err)
		return
	}
	period := monthStartUTC(time.Now())
	for _, p := range rows {
		fields, err := b.redis.HGetAll(ctx, trafficEdgePrefix+p.ServerUUID).Result()
		if err != nil {
			log.Printf("[bedrock] meter %s: read counter: %v", p.ServerUUID, err)
			continue
		}
		seenKey := bedrockSeenPrefix + p.ServerUUID
		seen, err := b.redis.HGetAll(ctx, seenKey).Result()
		if err != nil {
			log.Printf("[bedrock] meter %s: read seen: %v", p.ServerUUID, err)
			continue
		}
		b.redis.Expire(ctx, seenKey, trafficSeenTTL)
		for region, cur := range dirBytesFor(fields, "brx", "btx") {
			rxField, txField := "brx:"+region, "btx:"+region
			rx := voiceDelta(cur[0], seen[rxField])
			tx := voiceDelta(cur[1], seen[txField])
			if rx == 0 && tx == 0 {
				if seen[rxField] == strconv.FormatInt(cur[0], 10) && seen[txField] == strconv.FormatInt(cur[1], 10) {
					continue
				}
			} else if err := b.store.AddBedrockUsage(p.ServerID, period, region, rx, tx); err != nil {
				log.Printf("[bedrock] meter %s/%s: add usage: %v", p.ServerUUID, region, err)
				continue
			}
			pipe := b.redis.TxPipeline()
			pipe.HSet(ctx, seenKey, rxField, cur[0], txField, cur[1])
			pipe.Expire(ctx, seenKey, trafficSeenTTL)
			if _, err := pipe.Exec(ctx); err != nil {
				log.Printf("[bedrock] meter %s/%s: write seen: %v", p.ServerUUID, region, err)
			}
		}
	}
}
