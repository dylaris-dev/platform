package main

// Where this node's Redis address comes from, and how it moves.
//
// The node used to be CONFIGURED with it (REDIS_ADDR), so moving Redis meant
// editing every node's spec and restarting each one. It is now Core's answer:
// every successful auth carries Core's own REDIS_ADDR, the node caches what it
// was told in .redis_addr beside .node_secret, and it boots from that file when
// Core cannot be reached. The credentials were never part of this - they are
// derived from the per-node secret - so only the address is new.
//
// Core wins whenever it answers, but never by dropping the old address first. A
// different answer is VALIDATED with the node's own credentials before it is
// promoted, and the file is written only after that succeeded. An address Core
// names and this node cannot use leaves the node where it is, which is the
// difference between Core being wrong and every node on the platform going dark
// at once.
//
// Only a node holding CLUSTER_SECRET takes part (followsCoreRedisAddr). Any
// other node resolves exactly as it did before: REDIS_ADDR if set, else the
// local warp proxy, whose loopback is warp's and behind which warp follows the
// real address. It neither reads nor writes the file and ignores Core's field.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dylaris-pkg/retry"

	"github.com/redis/go-redis/v9"
)

const (
	redisAddrFileName    = ".redis_addr"
	redisAddrFileVersion = 1
	// redisProbeTimeout bounds one validation. It runs on the reconnect path, and
	// a candidate that swallows packets must not hold that for long.
	redisProbeTimeout = 5 * time.Second
)

type redisAddrFile struct {
	V         int    `json:"v"`
	Addr      string `json:"addr"`
	WrittenAt string `json:"written_at"`
}

// loadRedisAddr reads the cached address. ok=false when there is none or it
// cannot be trusted. An unreadable file or an unknown version is ignored with
// one log line rather than refusing to boot: REDIS_ADDR and Core are both still
// there to answer.
func loadRedisAddr(dir string) (string, bool) {
	path := filepath.Join(dir, redisAddrFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("redisaddr: ignoring %s: %v", path, err)
		}
		return "", false
	}
	var f redisAddrFile
	if err := json.Unmarshal(b, &f); err != nil || f.V != redisAddrFileVersion || strings.TrimSpace(f.Addr) == "" {
		log.Printf("redisaddr: ignoring %s: not a version-%d address file", path, redisAddrFileVersion)
		return "", false
	}
	return strings.TrimSpace(f.Addr), true
}

// saveRedisAddr writes the address atomically: a temp file in the same
// directory, fsynced, then renamed over the old one. A crash leaves the old file
// or the new one and never half of either - and a half-written address is the
// one thing this file must not hold, because the next boot trusts it first.
func saveRedisAddr(dir, addr string, now time.Time) error {
	if err := ensureSecretDir(dir); err != nil {
		return fmt.Errorf("redis address dir: %w", err)
	}
	b, err := json.Marshal(redisAddrFile{V: redisAddrFileVersion, Addr: addr, WrittenAt: now.UTC().Format(time.RFC3339)})
	if err != nil {
		return fmt.Errorf("encode redis address: %w", err)
	}
	// CreateTemp opens the file 0600, the mode of .node_secret beside it.
	tmp, err := os.CreateTemp(dir, redisAddrFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp redis address file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmpName, filepath.Join(dir, redisAddrFileName))
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write redis address file: %w", err)
	}
	return nil
}

// redisAddrSource says where the boot address came from.
type redisAddrSource string

const (
	redisAddrFromProxy redisAddrSource = "the local warp proxy"
	redisAddrFromFile  redisAddrSource = "the cached " + redisAddrFileName
	redisAddrFromEnv   redisAddrSource = "REDIS_ADDR"
	redisAddrFromCore  redisAddrSource = "Core"
)

// resolveBootRedisAddr picks the address the node boots on before anything has
// been asked. nodeAddr and viaProxy are resolveNodeAddr's answer for REDIS_ADDR.
//
// The cached file beats REDIS_ADDR because it holds what Core last said, and
// Core wins once it has answered. The variable stays as the transition for a
// node that has never cached anything and as the escape hatch for one that
// cannot reach Core. An empty address with redisAddrFromCore means nothing is
// known yet and Core has to be asked.
func resolveBootRedisAddr(nodeAddr string, viaProxy bool, cached string) (string, redisAddrSource) {
	switch {
	case viaProxy:
		return nodeAddr, redisAddrFromProxy
	case cached != "":
		return cached, redisAddrFromFile
	case nodeAddr != "":
		return nodeAddr, redisAddrFromEnv
	}
	return "", redisAddrFromCore
}

// followsCoreRedisAddr decides whether this node's Redis address is Core's to
// give: cached in .redis_addr and replaced by Core's answer.
//
// Only a node holding CLUSTER_SECRET - the same "in the cluster" test
// linkPrefersPublicEdge uses. Core's answer is its own REDIS_ADDR, a name on the
// overlay such a node sits on. A node without the secret (BYON by invariant, or a
// secret-free external admin node) runs on a machine where Core sends nothing,
// so a cached value there would never be replaced and would silently outrank the
// operator's own REDIS_ADDR - the documented escape hatch for a port collision.
// The proxy is outside it for every node: its loopback is not an answer.
func followsCoreRedisAddr(clusterSecret string, viaProxy bool) bool {
	return clusterSecret != "" && !viaProxy
}

// redisAddrState is the address the node's own Redis client dials, read at
// every dial by redisDialer.
type redisAddrState struct {
	mu     sync.Mutex
	addr   string
	onDisk bool // addr is what .redis_addr holds, so there is nothing to cache

	// promoteMu serialises validation, promotion and every write of the file.
	// Two Core replicas answer the same node, usually with the same address; the
	// second answer waits here, finds it already current and does nothing,
	// instead of racing the first through a second validation.
	promoteMu sync.Mutex

	probe func(ctx context.Context, addr string, secret []byte) error
	save  func(addr string) error
}

var nodeRedis = &redisAddrState{
	probe: probeRedisAddr,
	save:  func(addr string) error { return saveRedisAddr(nodeSecretDir, addr, time.Now()) },
}

func (s *redisAddrState) current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

func (s *redisAddrState) set(addr string, onDisk bool) {
	s.mu.Lock()
	s.addr, s.onDisk = addr, onDisk
	s.mu.Unlock()
}

// persistIfNew caches the current address unless it came from the file. Called
// after the address's first successful use, so one that never worked is never
// written - and so a node upgraded while still running on REDIS_ADDR caches it,
// after which the variable can leave its spec.
func (s *redisAddrState) persistIfNew() {
	s.promoteMu.Lock()
	defer s.promoteMu.Unlock()
	s.mu.Lock()
	addr, done := s.addr, s.onDisk
	s.mu.Unlock()
	if done || addr == "" {
		return
	}
	if err := s.save(addr); err != nil {
		log.Printf("redisaddr: WARN could not cache %s in %s: %v", addr, redisAddrFileName, err)
		return
	}
	s.set(addr, true)
	log.Printf("redisaddr: cached %s in %s", addr, redisAddrFileName)
}

// offer is Core's answer arriving. Empty is Core naming none, and the same
// address is nothing to do; anything else is validated before it replaces the
// current one, which stays in use until then and after a failure.
func (s *redisAddrState) offer(ctx context.Context, candidate string, secret []byte) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || candidate == s.current() {
		return
	}
	s.promoteMu.Lock()
	defer s.promoteMu.Unlock()
	cur := s.current()
	if candidate == cur {
		return
	}
	if cur == "" {
		// The boot that knew no address at all. There is nothing to keep, so
		// nothing to validate against: the boot ping is the proof, and the file
		// waits for it (persistIfNew).
		s.set(candidate, false)
		log.Printf("redisaddr: Core names %s; booting on it", candidate)
		return
	}
	pctx, cancel := context.WithTimeout(ctx, redisProbeTimeout)
	err := s.probe(pctx, candidate, secret)
	cancel()
	if err != nil {
		log.Printf("redisaddr: Core names %s but it failed validation; staying on %s: %v", candidate, cur, err)
		return
	}
	s.set(candidate, false)
	if werr := s.save(candidate); werr != nil {
		log.Printf("redisaddr: switched from %s to %s (validated) but could not cache it: %v", cur, candidate, werr)
		return
	}
	s.set(candidate, true)
	log.Printf("redisaddr: switched from %s to %s (validated, cached); connections move as they are redialled", cur, candidate)
	// Said out loud because it is the part with a cost: nothing running is
	// touched now, and the next restart of this node is when it happens.
	log.Printf("redisaddr: running MC containers and the Link keep %s until this node restarts; "+
		"that restart recreates them on %s, disconnecting their players - plan it", cur, candidate)
}

// noteCoreRedisAddr hands Core's answer to the node's address state. A node
// that does not follow Core ignores it (followsCoreRedisAddr).
func noteCoreRedisAddr(ctx context.Context, addr string, secret []byte) {
	if !redisAddrFollowsCore {
		return
	}
	nodeRedis.offer(ctx, addr, secret)
}

// probeRedisAddr is the one test a candidate gets: a short-lived client with
// this node's own ACL login, running one command the node genuinely needs and
// its ACL grants. The routing mode is read every 30s by loadModesFromRedis and
// granted read-only to every node, so success proves the candidate is the Redis
// Core provisioned this node on, not merely a Redis. An absent key is an answer.
//
// A plain client without redisDialer, deliberately: it must dial the
// candidate, not the current address.
func probeRedisAddr(ctx context.Context, addr string, secret []byte) error {
	c := redis.NewClient(&redis.Options{
		Addr:       addr,
		Username:   aclNodeUsername(nodeID),
		Password:   aclNodePassword(secret, nodeID),
		DB:         redisDB,
		PoolSize:   1,
		MaxRetries: -1,
	})
	defer c.Close()
	if err := c.Get(ctx, "dylaris:routing_mode").Err(); err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	return nil
}

// redisDialer ignores the address go-redis passes in - Options.Addr, frozen when
// the client was built - and dials the node's CURRENT address at the moment of
// each dial. That is what lets a promoted address take effect without rebuilding
// a client the whole node holds. Connections already open stay where they are
// until they close or fail, so the move happens as the pool turns over: the
// right trade when the old address still works, which validation guarantees the
// new one does too.
//
// There is no TLS to Redis today. If it is ever added: go-redis wraps TLS inside
// the dialer it builds for itself and installs that one only when
// Options.Dialer is nil, so supplying this one REPLACES the TLS - silently, the
// client still connects and the password crosses the wire in the clear. The TLS
// config would have to be carried in here by hand.
func redisDialer(addr func() string) func(ctx context.Context, network, _ string) (net.Conn, error) {
	// go-redis' own defaults (NewDialer), so moving to a custom dialer changes
	// where the node dials and nothing else.
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 5 * time.Minute}
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr())
	}
}

// newNodeRedisClient builds the node agent's own Redis client.
func newNodeRedisClient(secret []byte) *redis.Client {
	return redis.NewClient(&redis.Options{
		// Only read back by logs and error strings; the Dialer decides.
		Addr:     nodeRedis.current(),
		Dialer:   redisDialer(nodeRedis.current),
		Username: aclNodeUsername(nodeID),
		Password: aclNodePassword(secret, nodeID),
		DB:       redisDB,
	})
}

// askCoreForRedisAddr is the last resort at boot: no cached address and no
// REDIS_ADDR. One gRPC auth round trip - the one the secret bootstrap makes -
// repeated only while Core cannot be reached. Core ANSWERING without an address
// is final: nothing else could name one, so the node refuses to start and says
// what would. Returns the secret Core confirmed, nil on shutdown.
func askCoreForRedisAddr(ctx context.Context) []byte {
	// Not a transient failure: with no Core address the round trip can never
	// happen, and retrying would hide the misconfiguration behind a quiet loop.
	if coreGRPCAddr == "" {
		log.Fatal("FATAL: no Redis address. There is no cached " + redisAddrFileName +
			", REDIS_ADDR is unset, and CORE_GRPC_ADDR is unset, so Core cannot be asked.\n" +
			"  Set CORE_GRPC_ADDR (Core tells the node its address), or REDIS_ADDR on this node.")
	}
	var bo retry.Backoff
	for {
		s, err := bootstrapSecretViaGRPC(ctx, false)
		if err == nil && len(s) == 32 {
			if nodeRedis.current() == "" {
				log.Fatal("FATAL: no Redis address. There is no cached " + redisAddrFileName +
					", REDIS_ADDR is unset, and Core named none.\n" +
					"  A node is told Core's own REDIS_ADDR, so set it on Core; or set REDIS_ADDR on this node as an override.")
			}
			return s
		}
		wait := bo.Next()
		log.Printf("redisaddr: no Redis address known yet; asking Core failed (retry in %s): %v", wait, err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}
