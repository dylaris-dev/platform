package services

import (
	"context"
	"dylaris-core/pkg/leader"
	"dylaris-core/store"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type StatusWatcherService struct {
	store  store.Store
	redis  *redis.Client
	leader leader.Election
}

func NewStatusWatcherService(s store.Store, r *redis.Client) *StatusWatcherService {
	return &StatusWatcherService{store: s, redis: r}
}

// SetLeader wires the leader-election gate. Status scan + DB writes only
// run on the elected Core; followers idle through each tick.
func (s *StatusWatcherService) SetLeader(l leader.Election) { s.leader = l }

// statusScanInterval is how often a node-reported status reaches the database
// and the panel. It was 5 s, which was most of the wait a user saw after a stop
// or start had already finished on the node.
//
// ponytail: each tick runs three keyspace SCANs (status, port,
// reconcile_failed), three full-table server reads, one GetServerByUUID per
// port key and a pipeline of about five SETs per server. Cheap at hundreds of
// servers; past that, have the node PUBLISH its status and keep this as a slow
// backstop instead of shortening it further.
const statusScanInterval = 2 * time.Second

func (s *StatusWatcherService) Start() {
	log.Println("Status Watcher Service started")
	ticker := time.NewTicker(statusScanInterval)
	go func() {
		for range ticker.C {
			if s.leader != nil && !s.leader.IsLeader() {
				continue
			}
			s.scan()
		}
	}()
}

func (s *StatusWatcherService) scan() {
	ctx := context.Background()

	// Track whether anything panel-visible changed this tick so we can fire
	// exactly one servers.changed event per scan cycle, regardless of how
	// many servers flipped. Avoids flooding the SSE channel when 10 servers
	// boot at once.
	dirty := false

	// SCAN (not KEYS) so this poll stays O(batch) instead of blocking Redis
	// with an O(N) keyspace walk as the number of servers grows.
	var cursor uint64
	for {
		keys, next, err := s.redis.Scan(ctx, cursor, "dylaris:server:*:status", 100).Result()
		if err != nil {
			break
		}
		for _, key := range keys {
			// Key format: dylaris:server:<uuid>:status
			parts := strings.Split(key, ":")
			if len(parts) != 4 {
				continue
			}
			uuid := parts[2]

			// Read and removed in one step. A GET here and a DEL after the
			// database write lost whatever the node wrote in between: a final
			// "stopped" landing in that gap was deleted unread, and nothing
			// writes "stopped" again, so the server sat in "stopping" for good.
			newStatus, err := s.redis.GetDel(ctx, key).Result()
			if err != nil {
				continue
			}

			srv, err := s.store.GetServerByUUID(uuid)
			if err != nil {
				continue
			}

			// "migrating" is the orchestrator's until it ends the move. The
			// source reports its container going down after Core set it, and
			// writing that "stopped" over it reopened power actions mid-move:
			// a start on the source after staging lost everything written
			// since, and the cleanup then deleted the directory under it.
			if srv.Status == "migrating" {
				continue
			}
			if srv.Status != newStatus {
				log.Printf("Status update for %s: %s -> %s", uuid, srv.Status, newStatus)
				s.store.UpdateServerStatus(srv.ID, newStatus)
				dirty = true
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	// Land the node's give-up signal (see consumeReconcileFailures).
	if s.consumeReconcileFailures(ctx) {
		dirty = true
	}

	// The same idea for a version move the node refused to make: put back the
	// columns Core wrote optimistically before dispatching it.
	if s.consumeVersionUpdateFailures(ctx) {
		dirty = true
	}

	// Publish the durable per-server state keys the node reconciler and the
	// gateway edge read (desired_state + live_status).
	s.publishServerStateKeys(ctx)

	// Publish per-server edge transitional-MOTD config so the gateway edge sees it.
	s.publishEdgeMotd(ctx)
	s.publishRconLogFilter(ctx)

	// Sync host ports: Redis → DB (sets dirty=true if anything moved)
	if s.syncPortsFromRedis(ctx) {
		dirty = true
	}

	if dirty {
		s.publishServersChanged(ctx)
	}
}

// publishServersChanged drops one servers.changed event into the system-events
// channel. Uses the same wire format as services.SystemEventsPublisher but
// inlined so the watcher doesn't need a publisher dependency injected.
func (s *StatusWatcherService) publishServersChanged(ctx context.Context) {
	payload, err := json.Marshal(SystemEvent{Type: "servers.changed"})
	if err != nil {
		return
	}
	if err := s.redis.Publish(ctx, SystemEventsChannel, payload).Err(); err != nil {
		log.Printf("status-watcher: publish servers.changed failed: %v", err)
	}
}

// syncPortsFromRedis reads port allocations written by Nodes and updates DB host_port.
// Key format: dylaris:node:{nodeID}:port:{serverUUID} → port number.
// Returns true when at least one DB row was updated so the caller can drop a
// single servers.changed event per scan instead of one per row.
func (s *StatusWatcherService) syncPortsFromRedis(ctx context.Context) bool {
	changed := false
	tokenOf := map[int]string{} // node id -> token, read once per scan
	var cursor uint64
	for {
		keys, next, err := s.redis.Scan(ctx, cursor, "dylaris:node:*:port:*", 100).Result()
		if err != nil {
			return changed
		}
		for _, key := range keys {
			// Parse: dylaris:node:{nodeID}:port:{uuid}
			parts := strings.SplitN(key, ":port:", 2)
			if len(parts) != 2 {
				continue
			}
			serverUUID := parts[1]
			keyToken := strings.TrimPrefix(parts[0], "dylaris:node:")

			portStr, err := s.redis.Get(ctx, key).Result()
			if err != nil {
				continue
			}
			redisPort, err := strconv.Atoi(portStr)
			if err != nil || redisPort <= 0 {
				continue
			}

			srv, err := s.store.GetServerByUUID(serverUUID)
			if err != nil {
				continue
			}
			// Only the node the server lives on speaks for its port. Every node
			// may write under its own token, and the server was taken from the
			// end of the key alone: one node could rewrite the host port of any
			// server on the platform, every five seconds.
			tok, seen := tokenOf[srv.NodeID]
			if !seen {
				if n, nerr := s.store.GetNodeByID(srv.NodeID); nerr == nil && n != nil {
					tok = n.Token
				}
				tokenOf[srv.NodeID] = tok
			}
			if tok == "" || tok != keyToken {
				continue
			}

			if srv.HostPort != redisPort {
				containerPort := srv.ContainerPort
				if containerPort == 0 {
					containerPort = 25565
				}
				s.store.UpdateServerPorts(srv.ID, redisPort, containerPort)
				changed = true
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return changed
}

// consumeReconcileFailures lands the node's "I have given up" signal.
//
// When a container has crashed past the node reconciler's retry limit it writes
// dylaris:server:<uuid>:reconcile_failed, with the comment "Set failed key so
// core can surface it". Nothing surfaced it: no code outside the node ever read
// that key. The server was left reading "restarting" forever - the last status
// the reconciler published before it stopped trying - so the one state the user
// most needs to act on looked like the one state that resolves itself.
//
// Two writes, and both matter. The status becomes "offline" because that is what
// the server is, and desired_state becomes "stopped" so the platform stops
// intending to run something it has just concluded it cannot run - otherwise the
// next reconciler pass on a fresh tracker starts the whole cycle again.
//
// The reason is not lost by writing a plain status: the log-shipper puts a line
// in the server's console stream naming the exit code before it gives up, and
// that is where an owner looks. Starting the server again clears the key on the
// node side and is the deliberate retry.
func (s *StatusWatcherService) consumeReconcileFailures(ctx context.Context) bool {
	var cursor uint64
	changed := false
	for {
		keys, next, err := s.redis.Scan(ctx, cursor, "dylaris:server:*:reconcile_failed", 100).Result()
		if err != nil {
			return changed
		}
		for _, key := range keys {
			parts := strings.Split(key, ":")
			if len(parts) != 4 {
				continue
			}
			uuid := parts[2]
			reason, rerr := s.redis.Get(ctx, key).Result()
			if rerr != nil {
				continue
			}
			srv, serr := s.store.GetServerByUUID(uuid)
			if serr != nil || srv == nil {
				continue
			}
			if srv.Status == "offline" && srv.DesiredState == "stopped" {
				continue // already landed; leave the key for the node to clear on a restart
			}
			log.Printf("Server %s: node gave up restarting it (%s) — marking offline and clearing the intent to run it", uuid, reason)
			s.store.UpdateServerStatus(srv.ID, "offline")
			s.store.UpdateServerDesiredState(srv.ID, "stopped")
			changed = true
		}
		cursor = next
		if cursor == 0 {
			return changed
		}
	}
}

// publishServerStateKeys syncs the two per-server keys that consumers OUTSIDE
// Core read as durable state, from the DB, every scan tick with a 60s TTL:
//
//   - :desired_state — what the node's reconciler should be running.
//   - :live_status   — what the server actually IS, for the gateway edge.
//
// The second one exists because the edge was reading :status, and :status is not
// state at all: it is a one-shot EVENT key the node writes with a 30s TTL and the
// scan above consumes with a Del, usually within 5 seconds. So for almost the
// whole of a restart the edge MGET returned nil, transitionalMOTD("") was empty,
// and a player got a raw connection drop instead of "Server Restarting" - while
// the crash-loop notice beside it worked, because reconcile_failed IS left in
// place. A durable key under its own name keeps the event channel and the state
// read from being the same thing again.
//
// Both are written from the DB, which is the authority: the node reports into it
// and Core owns it. A node event landing between two ticks is picked up by the
// scan above before this runs, so the value published here is never staler than
// one tick.
// DesiredStateKey is where the node's reconciler reads what a server should be.
func DesiredStateKey(uuid string) string {
	return fmt.Sprintf("dylaris:server:%s:desired_state", uuid)
}

const desiredStateTTL = 60 * time.Second

// PublishDesiredState writes a changed desired state for the node straight
// away. The scan above republishes every server on its own tick, which left up
// to one tick in which the reconciler still read the OLD value: a kill landed,
// the reconciler saw "online" and a stopped container, and started it again.
func PublishDesiredState(ctx context.Context, rdb *redis.Client, uuid, state string) {
	if rdb == nil {
		return
	}
	if err := rdb.Set(ctx, DesiredStateKey(uuid), state, desiredStateTTL).Err(); err != nil {
		log.Printf("publish desired_state for %s: %v (the next scan tick publishes it)", uuid, err)
	}
}

func (s *StatusWatcherService) publishServerStateKeys(ctx context.Context) {
	servers, err := s.store.ListServers("")
	if err != nil {
		return
	}

	pipe := s.redis.Pipeline()
	for _, srv := range servers {
		pipe.Set(ctx, DesiredStateKey(srv.UUID), srv.DesiredState, desiredStateTTL)
		pipe.Set(ctx, fmt.Sprintf("dylaris:server:%s:live_status", srv.UUID), srv.Status, 60*time.Second)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("Failed to publish server state keys: %v", err)
	}
}

// publishEdgeMotd republishes each server's edge transitional-MOTD config to
// Redis so the gateway edge (which reads dylaris:server:<uuid>:edge_motd_* on
// each new player connect) always sees a live value. Same cadence/TTL as
// desired_state: refreshed every scan tick so a Redis flush self-heals and a
// deleted server's keys expire on their own.
func (s *StatusWatcherService) publishEdgeMotd(ctx context.Context) {
	rows, err := s.store.ListServerEdgeMotd()
	if err != nil {
		return
	}
	pipe := s.redis.Pipeline()
	for _, m := range rows {
		pipe.Set(ctx, fmt.Sprintf("dylaris:server:%s:edge_motd_mode", m.UUID), m.Mode, 60*time.Second)
		pipe.Set(ctx, fmt.Sprintf("dylaris:server:%s:edge_motd_text", m.UUID), m.Text, 60*time.Second)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("Failed to publish edge MOTD config: %v", err)
	}
}

// publishRconLogFilter republishes each server's console RCON-noise toggle so the
// log-shipper inside every MC container (which re-reads
// dylaris:server:<uuid>:log_filter_rcon on a timer) always sees a live value.
// Same cadence/TTL as desired_state above, and for the same two reasons: a Redis
// flush self-heals on the next tick, and a deleted server's key expires on its
// own instead of lingering.
func (s *StatusWatcherService) publishRconLogFilter(ctx context.Context) {
	rows, err := s.store.ListServerRconLogFilter()
	if err != nil {
		return
	}
	pipe := s.redis.Pipeline()
	for _, f := range rows {
		v := "false"
		if f.On {
			v = "true"
		}
		pipe.Set(ctx, fmt.Sprintf("dylaris:server:%s:log_filter_rcon", f.UUID), v, 60*time.Second)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("Failed to publish RCON log filter config: %v", err)
	}
}
