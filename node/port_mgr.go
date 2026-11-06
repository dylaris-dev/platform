package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// PortManager handles host-port allocation for servers in ip_port / both
// routing modes (i.e. when servers are reachable by a published host port).
// Ports are persisted in Redis under dylaris:node:{nodeID}:port:{serverUUID} so
// allocations survive node restarts and are visible to Core.
type PortManager struct {
	rdb        *redis.Client
	nodeID     string
	rangeStart int
	rangeEnd   int
	// portMode is asked at allocation time, not copied at construction.
	//
	// It used to be a plain string field, and the manager is built during
	// startup BEFORE the first loadModesFromRedis - so it always captured the
	// compiled-in "sequential" default. The 30s refresh then kept updating a
	// global that this, its only consumer, had already copied away from.
	// Setting the allocation strategy to "random" in the panel therefore did
	// nothing at all, and looked like it had worked.
	portMode func() string

	mu        sync.Mutex
	usedPorts map[int]string // port → serverUUID (in-RAM index for fast allocation)
}

func NewPortManager(rdb *redis.Client, nodeID string, rangeStart, rangeEnd int, portMode func() string) *PortManager {
	pm := &PortManager{
		rdb:        rdb,
		nodeID:     nodeID,
		rangeStart: rangeStart,
		rangeEnd:   rangeEnd,
		portMode:   portMode,
		usedPorts:  make(map[int]string),
	}
	pm.loadFromRedis()
	return pm
}

// AdoptExistingBindings rebuilds the ledger from the host's own containers,
// which are its only durable record.
//
// Redis is in-memory ONLY by design (the compose Valkey runs with --save "" and
// --appendonly no), so a Valkey restart - a plain host reboot is enough - erases
// every port allocation while the containers holding those ports survive.
// loadFromRedis then reports 0 allocations and the whole range looks free, so
// the next server is handed a port a live container already binds: Docker
// refuses it ("Bind for 0.0.0.0:25600 failed: port is already allocated"), the
// reconciler starts the container anyway WITHOUT a published port, and Core
// still reports the server online. Reproduced end to end on the testbed.
//
// bindings maps serverUUID -> published host port, taken from the containers.
func (pm *PortManager) AdoptExistingBindings(bindings map[string]int) {
	adopted := 0
	for uuid, port := range bindings {
		if port <= 0 {
			continue
		}
		if pm.GetPort(uuid) == port {
			continue // ledger already agrees
		}
		if err := pm.SetPort(uuid, port); err != nil {
			// Two containers configured for the same host port. Only one of
			// them can actually hold it, so this is corruption to report, not
			// to silently paper over.
			log.Printf("PortManager: cannot adopt port %d for %s: %v", port, uuid, err)
			continue
		}
		adopted++
	}
	if adopted > 0 {
		log.Printf("PortManager: adopted %d port binding(s)", adopted)
	}
}

// AdoptContainerBindings is AdoptExistingBindings for the bindings of the
// host's containers, which hold their ports for real: a ledger entry loaded
// from Redis for a server that binds no such port gives way. Such an entry is
// what a release that failed in Redis leaves behind, and it used to keep the
// live server out of the ledger, so its next recreate failed or moved it.
func (pm *PortManager) AdoptContainerBindings(bindings map[string]int) {
	pm.mu.Lock()
	for uuid, port := range bindings {
		owner, ok := pm.usedPorts[port]
		if !ok || owner == uuid || bindings[owner] == port {
			continue
		}
		delete(pm.usedPorts, port)
		// An owner with a container elsewhere is moved by its own adoption,
		// which rewrites its keys; one with none is gone.
		if bindings[owner] == 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := pm.forget(ctx, owner, fmt.Sprintf("dylaris:node:%s:port:%s", pm.nodeID, owner)); err != nil {
				log.Printf("PortManager: could not clear stale port %d of %s: %v", port, owner, err)
			}
			cancel()
		}
		log.Printf("PortManager: port %d was recorded for %s, but %s's container binds it", port, owner, uuid)
	}
	pm.mu.Unlock()
	pm.AdoptExistingBindings(bindings)
}

// portsKey mirrors every dylaris:node:<id>:port:<uuid> key of this node into
// one hash, so the allocations load with one read instead of a keyspace walk.
// The node is losing SCAN: it lists every key NAME on the platform whatever
// the ACL's patterns say. The per-server keys stay; Core reads them.
func (pm *PortManager) portsKey() string {
	return fmt.Sprintf("dylaris:node:%s:ports", pm.nodeID)
}

// loadFromRedis reads existing port assignments from Redis into the in-RAM index.
func (pm *PortManager) loadFromRedis() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The per-server keys are the truth while they can still be listed: a
	// node rolled back to a binary without the hash kept changing them, and a
	// hash read on the way forward again would load what it had left behind.
	if !pm.rebuildFromScan(ctx) {
		all, err := pm.rdb.HGetAll(ctx, pm.portsKey()).Result()
		if err != nil {
			log.Printf("PortManager: Redis read error: %v", err)
		}
		for uuid, portStr := range all {
			if port, perr := strconv.Atoi(portStr); perr == nil {
				pm.usedPorts[port] = uuid
			}
		}
	}
	log.Printf("PortManager: loaded %d existing port allocations (range %d-%d)", len(pm.usedPorts), pm.rangeStart, pm.rangeEnd)
}

// rebuildFromScan loads the ledger from the per-server keys and rewrites the
// hash to match, reporting whether it could. It needs SCAN, which a node holds
// only until every node runs this version; after that the hash is read, and
// adopting the running containers' bindings recovers a wiped Redis.
func (pm *PortManager) rebuildFromScan(ctx context.Context) bool {
	prefix := fmt.Sprintf("dylaris:node:%s:port:", pm.nodeID)
	found := map[string]string{}
	var cursor uint64
	for {
		keys, next, err := pm.rdb.Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			return false
		}
		for _, key := range keys {
			if portStr, err := pm.rdb.Get(ctx, key).Result(); err == nil {
				found[key[len(prefix):]] = portStr
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	pipe := pm.rdb.TxPipeline()
	pipe.Del(ctx, pm.portsKey())
	for uuid, portStr := range found {
		if port, err := strconv.Atoi(portStr); err == nil {
			pm.usedPorts[port] = uuid
			pipe.HSet(ctx, pm.portsKey(), uuid, portStr)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("PortManager: could not rebuild the port hash: %v", err)
	}
	return true
}

// AllocatePort finds the next free port in the configured range and reserves it.
// Returns the port number or an error if the range is exhausted.
func (pm *PortManager) AllocatePort(serverUUID string) (int, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Check if already allocated for this server
	for port, uuid := range pm.usedPorts {
		if uuid == serverUUID {
			return port, nil
		}
	}

	// Build candidate list based on mode
	rangeSize := pm.rangeEnd - pm.rangeStart + 1
	candidates := make([]int, rangeSize)
	for i := range candidates {
		candidates[i] = pm.rangeStart + i
	}
	if pm.portMode() == "random" {
		rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	}

	for _, port := range candidates {
		// PORT_RANGE is not checked against the node's own listeners, and a
		// port Docker cannot bind would come back on every retry.
		if hostPortProblem(port) != "" {
			continue
		}
		if _, used := pm.usedPorts[port]; !used {
			key := fmt.Sprintf("dylaris:node:%s:port:%s", pm.nodeID, serverUUID)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := pm.persist(ctx, serverUUID, key, port); err != nil {
				return 0, fmt.Errorf("failed to persist port allocation: %w", err)
			}
			pm.usedPorts[port] = serverUUID
			log.Printf("PortManager: allocated port %d for server %s", port, serverUUID)
			return port, nil
		}
	}

	return 0, fmt.Errorf("port range %d-%d exhausted", pm.rangeStart, pm.rangeEnd)
}

// ReleasePort frees the port assigned to a server.
func (pm *PortManager) ReleasePort(serverUUID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for port, uuid := range pm.usedPorts {
		if uuid == serverUUID {
			key := fmt.Sprintf("dylaris:node:%s:port:%s", pm.nodeID, serverUUID)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := pm.forget(ctx, serverUUID, key); err != nil {
				// The keys have no TTL and nothing else removes them: left
				// there, the next restart hands the port back to this server.
				log.Printf("PortManager: release of port %d (server %s) not stored, retrying: %v", port, serverUUID, err)
				go pm.retryForget(serverUUID, key)
			}
			delete(pm.usedPorts, port)
			log.Printf("PortManager: released port %d (server %s)", port, serverUUID)
			return
		}
	}
}

// SetPort forces a specific port for a server, releasing any previous allocation.
// Returns an error if the port is already used by another server.
func (pm *PortManager) SetPort(serverUUID string, port int) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Checked before the release below: a refused port used to leave the
	// server holding no port at all.
	if existingUUID, ok := pm.usedPorts[port]; ok && existingUUID != serverUUID {
		return fmt.Errorf("port %d is already allocated to server %s", port, existingUUID)
	}

	key := fmt.Sprintf("dylaris:node:%s:port:%s", pm.nodeID, serverUUID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := pm.persist(ctx, serverUUID, key, port); err != nil {
		return fmt.Errorf("failed to persist port: %w", err)
	}
	// The old allocation goes only once the new one is stored: the caller keeps
	// the container running on it when this fails. persist overwrote the same
	// key and hash field, so Redis needs no separate forget.
	for p, uuid := range pm.usedPorts {
		if uuid == serverUUID && p != port {
			delete(pm.usedPorts, p)
			break
		}
	}
	pm.usedPorts[port] = serverUUID
	log.Printf("PortManager: set port %d for server %s", port, serverUUID)
	return nil
}

// persist writes a server's per-server key and its hash entry in one
// transaction, so neither can exist without the other.
func (pm *PortManager) persist(ctx context.Context, serverUUID, key string, port int) error {
	pipe := pm.rdb.TxPipeline()
	pipe.Set(ctx, key, strconv.Itoa(port), 0)
	pipe.HSet(ctx, pm.portsKey(), serverUUID, strconv.Itoa(port))
	_, err := pipe.Exec(ctx)
	return err
}

// forget removes both, together.
func (pm *PortManager) forget(ctx context.Context, serverUUID, key string) error {
	pipe := pm.rdb.TxPipeline()
	pipe.Del(ctx, key)
	pipe.HDel(ctx, pm.portsKey(), serverUUID)
	_, err := pipe.Exec(ctx)
	return err
}

var forgetRetryEvery = 10 * time.Second

// retryForget repeats a failed release for a few minutes. A server given a
// port again in the meantime owns its keys again and stops it. Past that, the
// stale entry is released at the next start (releaseGonePorts), or gives way to
// a live container that binds its port (AdoptContainerBindings).
func (pm *PortManager) retryForget(serverUUID, key string) {
	for range 30 {
		time.Sleep(forgetRetryEvery)
		pm.mu.Lock()
		held := false
		for _, uuid := range pm.usedPorts {
			if uuid == serverUUID {
				held = true
				break
			}
		}
		var err error
		if !held {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err = pm.forget(ctx, serverUUID, key)
			cancel()
		}
		pm.mu.Unlock()
		if held || err == nil {
			return
		}
	}
	log.Printf("PortManager: gave up releasing the port of %s in Redis", serverUUID)
}

// Snapshot returns serverUUID -> port for every allocation.
func (pm *PortManager) Snapshot() map[string]int {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	out := make(map[string]int, len(pm.usedPorts))
	for port, uuid := range pm.usedPorts {
		out[uuid] = port
	}
	return out
}

// GetPort returns the port assigned to a server, or 0 if none.
func (pm *PortManager) GetPort(serverUUID string) int {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for port, uuid := range pm.usedPorts {
		if uuid == serverUUID {
			return port
		}
	}
	return 0
}

// hostPortProblem reports why a host port Core asked for cannot be published,
// or "". The port is bound on every address of the node, so a privileged port
// (SSH among them) or one of the node's own listeners would be taken from the
// service that should hold it. 25565 stays allowed: without the gateway it is
// the port players expect.
func hostPortProblem(port int) string {
	if port < 1024 || port > 65535 {
		return fmt.Sprintf("host port %d is outside 1024-65535", port)
	}
	for _, l := range []struct{ env, def string }{
		{"SFTP_PORT", "25520"}, {"BEAM_GRPC_PORT", "25521"}, {"MIGRATION_PORT", "25522"}, {"BEAM_LAN_PORT", "25523"},
	} {
		v := os.Getenv(l.env)
		if v == "" {
			v = l.def
		}
		if strconv.Itoa(port) == v {
			return fmt.Sprintf("host port %d is the node's own %s", port, l.env)
		}
	}
	return ""
}
