package services

import (
	"context"
	"dylaris-core/pkg/leader"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
	"log"
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
