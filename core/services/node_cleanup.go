package services

import (
	"context"
	"dylaris-core/pkg/leader"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type NodeCleanupService struct {
	store  store.Store
	redis  *redis.Client
	ttl    time.Duration
	leader leader.Election
}

func NewNodeCleanupService(s store.Store, rdb *redis.Client, ttl time.Duration) *NodeCleanupService {
	return &NodeCleanupService{store: s, redis: rdb, ttl: ttl}
}

// SetLeader wires the leader-election gate so only one Core deletes per tick.
func (s *NodeCleanupService) SetLeader(l leader.Election) { s.leader = l }

func (s *NodeCleanupService) Start() {
	log.Printf("Node Cleanup Service started (TTL: %s)", s.ttl)
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		for range ticker.C {
			if s.leader != nil && !s.leader.IsLeader() {
				continue
			}
			s.sweep(context.Background())
		}
	}()
	go func() {
		time.Sleep(2 * time.Minute)
		for {
			if s.leader == nil || s.leader.IsLeader() {
				s.sweepOrphanKeys(context.Background())
			}
			time.Sleep(15 * time.Minute)
		}
	}()
}

// orphanNodeKeySuffixes are the NodeRedisKeys a Core publisher writes from a
// node list. A writer that listed a node before its row went can SET one again
// after the teardown's DEL, with no TTL, and no later tick lists that node to
// clear it. Keys the node writes itself are not here: its ACL is gone with it.
var orphanNodeKeySuffixes = []string{":netpolicy", ":netpolicy_tenancy", ":storage_placement", ":backup_policy"}

// sweepOrphanKeys DELs publisher keys whose token has no node row. One sweep
// instead of a WATCH on every writer, present and future. The scan runs BEFORE
// ListNodes, so a node registered in between is listed and its keys stay.
func (s *NodeCleanupService) sweepOrphanKeys(ctx context.Context) {
	orphans := map[string][]string{}
	for _, suffix := range orphanNodeKeySuffixes {
		iter := s.redis.Scan(ctx, 0, "dylaris:node:*"+suffix, 500).Iterator()
		for iter.Next(ctx) {
			key := iter.Val()
			token := strings.TrimSuffix(strings.TrimPrefix(key, "dylaris:node:"), suffix)
			// The glob's * also matches colons; such a key is not a node's own.
			if token == "" || strings.Contains(token, ":") {
				continue
			}
			orphans[token] = append(orphans[token], key)
		}
		if err := iter.Err(); err != nil {
			log.Printf("Node cleanup: orphan key scan failed: %v", err)
			return
		}
	}
	if len(orphans) == 0 {
		return
	}
	nodes, err := s.store.ListNodes()
	if err != nil {
		log.Printf("Node cleanup: orphan key sweep could not list nodes: %v", err)
		return
	}
	for _, n := range nodes {
		delete(orphans, n.Token)
	}
	var keys []string
	for _, k := range orphans {
		keys = append(keys, k...)
	}
	if len(keys) == 0 {
		return
	}
	if err := s.redis.Del(ctx, keys...).Err(); err != nil {
		log.Printf("Node cleanup: %d orphan node key(s) could not be removed: %v", len(keys), err)
		return
	}
	log.Printf("Node cleanup: removed %d orphan key(s) of %d deleted node(s)", len(keys), len(orphans))
}

// sweep deletes the stale rows and then the Redis state each one owned, the
// same teardown every other node delete runs: none of those keys expire.
func (s *NodeCleanupService) sweep(ctx context.Context) {
	tokens, err := s.store.DeleteStaleOfflineNodes(time.Now().Add(-s.ttl))
	if err != nil {
		log.Printf("Node cleanup error: %v", err)
		return
	}
	for _, t := range tokens {
		RemoveNodeRedisState(ctx, s.redis, redisacl.NewProvisioner(s.redis), t)
	}
	if len(tokens) > 0 {
		log.Printf("Cleaned up %d stale offline node(s)", len(tokens))
	}
}
