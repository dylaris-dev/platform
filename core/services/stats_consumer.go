package services

import (
	"context"
	"database/sql"
	"dylaris-core/models"
	"dylaris-core/store"
	"encoding/json"
	"errors"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type StatsConsumerService struct {
	store  store.Store
	redis  *redis.Client
	coreID string

	mu       sync.Mutex
	nodeKeys map[string]bool // tracked node batch stream keys
}

type statsBatchPayload struct {
	TS    int64            `json:"ts"`
	Stats []statsBatchItem `json:"stats"`
}

type statsBatchItem struct {
	UUID       string  `json:"uuid"`
	CPU        float64 `json:"cpu"`
	CPULimit   float64 `json:"cpuLimit"`
	MemUsed    int64   `json:"memUsed"`
	MemLimit   int64   `json:"memLimit"`
	Players    int     `json:"players"`
	MaxPlayers int     `json:"maxPlayers"`
	// The node sends 0 when it has seen no GC yet; stored as NULL.
	JavaHeapUsed int64 `json:"javaHeapUsed"`
}

// nodeServerUUIDs is the set of servers placed on the node with this token.
func (s *StatsConsumerService) nodeServerUUIDs(token string) (map[string]bool, error) {
	node, err := s.store.GetNodeByToken(token)
	if err != nil || node == nil {
		return nil, err
	}
	servers, err := s.store.ListServersByNode(node.ID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(servers))
	for _, srv := range servers {
		out[srv.UUID] = true
	}
	return out, nil
}

// statsRows turns one node batch into history rows, keeping only servers that
// live on that node, once each.
//
// The UUID in a batch is whatever the node wrote, and the node writes its own
// batch stream - a customer's machine, for a BYON node. It could write history
// for any server whose UUID it knew, another tenant's or ours, and as many rows
// per batch as it liked.
//
// The row time is the stream entry id's, Redis's clock, not the batch's ts:
// that is the node's clock, and a BYON machine running hours off filed every
// row outside the charts' window or in the future. Whole seconds, as before.
func statsRows(batch statsBatchPayload, msgID string, allowed map[string]bool) []models.ServerStatRow {
	ts := time.Unix(batch.TS, 0)
	if at, ok := streamIDTime(msgID); ok {
		ts = at.Truncate(time.Second)
	}
	seen := make(map[string]bool, len(batch.Stats))
	var rows []models.ServerStatRow
	for _, item := range batch.Stats {
		if !allowed[item.UUID] || seen[item.UUID] {
			continue
		}
		seen[item.UUID] = true
		var heap *int64
		if item.JavaHeapUsed > 0 {
			heap = &item.JavaHeapUsed
		}
		rows = append(rows, models.ServerStatRow{
			Time:         ts,
			ServerUUID:   item.UUID,
			CPU:          item.CPU,
			CPULimit:     item.CPULimit,
			MemUsed:      item.MemUsed,
			MemLimit:     item.MemLimit,
			Players:      boundInt4(item.Players),
			MaxPlayers:   boundInt4(item.MaxPlayers),
			JavaHeapUsed: heap,
		})
	}
	return rows
}

// boundInt4 keeps a node-sent count inside an INT column. The batch is
// inserted in one transaction, so one out-of-range value - a tenant's SLP
// answer, or a BYON node's own write - refused every row on that node.
func boundInt4(n int) int {
	return min(max(n, 0), math.MaxInt32)
}

func NewStatsConsumerService(s store.Store, r *redis.Client, coreID string) *StatsConsumerService {
	return &StatsConsumerService{
		store:    s,
		redis:    r,
		coreID:   coreID,
		nodeKeys: make(map[string]bool),
	}
}

func (s *StatsConsumerService) Start() {
	log.Println("Stats Consumer Service started")

	// Scan for node batch streams every 30s
	go func() {
		s.scanNodes()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			s.scanNodes()
		}
	}()
}

func (s *StatsConsumerService) scanNodes() {
	ctx := context.Background()
	keys, err := s.redis.Keys(ctx, "dylaris:discovery:*").Result()
	if err != nil {
		return
	}

	for _, key := range keys {
		val, err := s.redis.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		// The id must be the key's own: a node that wrote a fresh id every scan
		// started one consumer, and one stream, per id it ever named.
		hb := heartbeatUnderKey(key, val)
		if hb == nil {
			continue
		}

		batchKey := "dylaris:node:" + hb.ID + ":stats:batch"

		s.mu.Lock()
		already := s.nodeKeys[batchKey]
		if !already {
			s.nodeKeys[batchKey] = true
		}
		s.mu.Unlock()

		if !already {
			go s.consumeStream(batchKey)
		}
	}
}

func (s *StatsConsumerService) consumeStream(streamKey string) {
	ctx := context.Background()
	r := &statsStreamReader{
		key:      streamKey,
		group:    "dylaris-core-stats",
		consumer: s.coreID,
		// The servers this node may report for, refreshed each minute.
		nodeToken: strings.TrimSuffix(strings.TrimPrefix(streamKey, "dylaris:node:"), ":stats:batch"),
		block:     10 * time.Second,
		// Its own leftovers first: a Core that stopped mid-batch or failed one.
		retryAt: time.Now(),
	}

	// A consumer outlived its node: it polled for ever, and creating the
	// group would bring back the stream the node teardown removed. Dropping
	// the entry lets scanNodes start a fresh one if the node is seen again.
	defer func() {
		s.mu.Lock()
		delete(s.nodeKeys, streamKey)
		s.mu.Unlock()
	}()
	if s.nodeGone(r.nodeToken) {
		return
	}

	// XGroupCreateMkStream is idempotent — error on re-create is expected and ignored.
	s.redis.XGroupCreateMkStream(ctx, r.key, r.group, "0").Err()

	log.Printf("Stats consumer started for stream %s", streamKey)

	for s.readStats(ctx, r) {
	}
	log.Printf("Stats consumer stopped for stream %s: the node is gone", streamKey)
}

// nodeGone reports whether the node's row is definitely gone. A database
// error is not an answer, so it keeps the consumer running.
func (s *StatsConsumerService) nodeGone(token string) bool {
	_, err := s.store.GetNodeByToken(token)
	return errors.Is(err, sql.ErrNoRows)
}

// statsStreamReader is one node batch stream's consumer state.
type statsStreamReader struct {
	key, group, consumer, nodeToken string
	block                           time.Duration

	allowed   map[string]bool
	allowedAt time.Time

	// goneCheckedAt is when an idle read last asked whether the node still
	// exists; an empty stream is all a deleted node's consumer ever sees.
	goneCheckedAt time.Time

	// retryAt, when set and due, reads this consumer's own un-ACKed entries
	// ("0") instead of new ones (">"). ">" never hands an entry out twice, so
	// a batch whose insert failed was lost for good although it was never
	// ACKed. Retried on a timer rather than before every new read, so a batch
	// that keeps failing cannot hold back the ones after it.
	retryAt time.Time

	// claimedAt is when this consumer last took over entries another consumer
	// of the group left pending. The consumer name is the Core's hostname, a
	// new one on every redeploy, and "0" reads only this consumer's own list.
	claimedAt time.Time
}

// statsRetryEvery is how long a failed batch waits for its next attempt.
const statsRetryEvery = time.Minute

// statsPendingMaxAge is how long a failed batch is retried before it is
// dropped: well past a database blip, and inside the ~3 h the node keeps.
const statsPendingMaxAge = time.Hour

// readStats runs one read of the stream and stores what it got. It returns
// false once the node is gone and the consumer should stop.
func (s *StatsConsumerService) readStats(ctx context.Context, r *statsStreamReader) bool {
	if time.Since(r.claimedAt) >= statsRetryEvery {
		r.claimedAt = time.Now()
		// Idle past a retry interval: a Core that went away, or one of ours
		// that failed (claiming that from ourselves changes nothing).
		ids, _, err := s.redis.XAutoClaimJustID(ctx, &redis.XAutoClaimArgs{
			Stream: r.key, Group: r.group, Consumer: r.consumer,
			MinIdle: statsRetryEvery, Start: "0-0", Count: 100,
		}).Result()
		if err == nil && len(ids) > 0 {
			r.retryAt = time.Now()
		}
	}
	from := ">"
	retrying := !r.retryAt.IsZero() && !time.Now().Before(r.retryAt)
	if retrying {
		from = "0"
	}
	const count = 50
	results, err := s.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    r.group,
		Consumer: r.consumer,
		Streams:  []string{r.key, from},
		Count:    count,
		Block:    r.block,
	}).Result()
	if err == redis.Nil {
		if time.Since(r.goneCheckedAt) < time.Minute {
			return true
		}
		r.goneCheckedAt = time.Now()
		return !s.nodeGone(r.nodeToken)
	}
	if err != nil {
		// NOGROUP is also what the node teardown leaves behind.
		if s.nodeGone(r.nodeToken) {
			return false
		}
		// The group may have vanished (Redis restarted with no persistence).
		// Recreating is idempotent - BUSYGROUP when it still exists - so this
		// self-heals a NOGROUP loop instead of spinning until the Core restarts.
		s.redis.XGroupCreateMkStream(ctx, r.key, r.group, "0")
		time.Sleep(5 * time.Second)
		return true
	}

	read := 0
	failed := false
	for _, stream := range results {
		read += len(stream.Messages)
		var rows []models.ServerStatRow
		var ackIDs []string

		for _, msg := range stream.Messages {
			// A pending entry the node's MAXLEN trim already removed comes back
			// without values, and so does nothing else.
			data, ok := msg.Values["data"].(string)
			if !ok {
				s.redis.XAck(ctx, r.key, r.group, msg.ID) // undeliverable: ack now
				continue
			}
			if at, ok := streamIDTime(msg.ID); ok && time.Since(at) > statsPendingMaxAge {
				log.Printf("Stats batch %s on %s failed for %s, dropping it", msg.ID, r.key, statsPendingMaxAge)
				s.redis.XAck(ctx, r.key, r.group, msg.ID)
				continue
			}

			var batch statsBatchPayload
			if err := json.Unmarshal([]byte(data), &batch); err != nil {
				s.redis.XAck(ctx, r.key, r.group, msg.ID) // undeliverable: ack now
				continue
			}

			if time.Since(r.allowedAt) > time.Minute && !failed {
				if a, err := s.nodeServerUUIDs(r.nodeToken); err == nil {
					r.allowed, r.allowedAt = a, time.Now()
				} else if r.allowed == nil {
					failed = true
				}
			}
			// Never loaded (the database was down from the start): no row
			// would pass, and ACKing the batch would drop it for good.
			if failed && r.allowed == nil {
				continue // not ACKed: retried from the pending list
			}
			rows = append(rows, statsRows(batch, msg.ID, r.allowed)...)
			ackIDs = append(ackIDs, msg.ID)
		}

		if len(rows) > 0 {
			if err := s.store.InsertStatsBatch(rows); err != nil {
				log.Printf("Stats batch insert error: %v", err)
				failed = true
				continue // not ACKed: retried from the pending list
			}
		}

		if len(ackIDs) > 0 {
			s.redis.XAck(ctx, r.key, r.group, ackIDs...)
		}
	}
	switch {
	case failed:
		r.retryAt = time.Now().Add(statsRetryEvery)
	case retrying && read < count:
		r.retryAt = time.Time{} // the pending list is empty now
	}
	return true
}
