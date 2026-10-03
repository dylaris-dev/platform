package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// CoreHeartbeat is written to Redis so Nodes can discover this Core instance.
// Region is the operator-configured DYLARIS_REGION env. Nodes can
// use it for region-affinity decisions later; the panel uses it for the
// "Connected to <region> Core" chip.
type CoreHeartbeat struct {
	ID       string  `json:"id"`
	Region   string  `json:"region"`
	GRPCAddr string  `json:"grpc_addr"` // host:port for gRPC connections
	IPs      CoreIPs `json:"ips"`

	// Version is the release this Core image was built from, so the updates
	// view can report every Core rather than only the one answering the
	// request. Omitted when unstamped, which reads as "not reporting" and never
	// as old - a Core built before this field existed must not be flagged.
	Version string `json:"version,omitempty"`
}

type CoreIPs struct {
	Public  string   `json:"public"`
	Private []string `json:"private"`
}

type CoreHeartbeatService struct {
	redis    *redis.Client
	coreID   string
	region   string
	grpcPort int
	version  string

	started atomic.Bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

func NewCoreHeartbeatService(r *redis.Client, coreID, region, version string, grpcPort int) *CoreHeartbeatService {
	if region == "" {
		region = "default"
	}
	return &CoreHeartbeatService{
		redis:    r,
		coreID:   coreID,
		region:   region,
		version:  version,
		grpcPort: grpcPort,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// key is the single definition of where this Core's heartbeat lives. Start and
// Stop must agree on it: a Stop that deleted a different key would look
// successful and leave the instance counted until its TTL ran out.
func (s *CoreHeartbeatService) key() string {
	return "dylaris:core:" + s.coreID
}

// CoreIndexKey lists the Core ids that heartbeat, so a node finds the Cores by
// reading one set instead of walking the keyspace. A node may not SCAN: SCAN
// lists every key NAME regardless of ACL key patterns, and on this platform
// the names - link tokens above all - are the secret. Inside dylaris:core:*
// on purpose, so the node's existing read grant covers it.
const CoreIndexKey = "dylaris:core:index"

// Start begins writing heartbeats every 10 seconds. Calling it twice is a
// no-op, so a second call cannot leave an unstoppable goroutine behind.
func (s *CoreHeartbeatService) Start() {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	log.Printf("Core Heartbeat started (id=%s, grpc_port=%d)", s.coreID, s.grpcPort)

	s.writeHeartbeat()

	ticker := time.NewTicker(10 * time.Second)
	go func() {
		defer close(s.doneCh)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.writeHeartbeat()
			}
		}
	}()
}

// Stop halts the heartbeat loop and removes this Core's key, then blocks until
// the loop has exited. Deleting the key is the useful half: the key carries a
// 30s TTL, so without this a Core that shut down cleanly would still be counted
// as online for up to half a minute - long enough to make the host-path storage
// backend unsavable right after scaling a deployment back down to one Core.
//
// A Core that dies without running this (SIGKILL, OOM, hardware) still falls
// back to the TTL, which is why the TTL stays.
//
// Safe to call without a preceding Start: it returns immediately rather than
// blocking on a goroutine that was never launched.
func (s *CoreHeartbeatService) Stop(ctx context.Context) {
	if !s.started.Load() {
		return
	}
	select {
	case <-s.stopCh:
		// already stopped
	default:
		close(s.stopCh)
	}

	// The join honours ctx too. The loop can be inside a Redis write when the
	// stop arrives, and that write does not observe this context, so without
	// the second case the caller's deadline bounded only the Del below while
	// the wait above ran for however long go-redis took. A shutdown budget
	// that is not enforced is the thing this branch kept finding.
	select {
	case <-s.doneCh:
	case <-ctx.Done():
		log.Printf("Core Heartbeat: the write loop did not stop within the shutdown budget; leaving %s to expire with its TTL", s.key())
		return
	}

	if err := s.redis.Del(ctx, s.key()).Err(); err != nil {
		log.Printf("Core Heartbeat: could not remove %s on shutdown, it will expire with its TTL instead: %v", s.key(), err)
	}
	s.redis.SRem(ctx, CoreIndexKey, s.coreID)
}

func (s *CoreHeartbeatService) writeHeartbeat() {
	ctx := context.Background()

	privateIPs := getPrivateIPs()
	publicIP := getOutboundIP()

	// Build gRPC address: prefer first private IP, fallback to public
	host := publicIP
	if len(privateIPs) > 0 {
		host = privateIPs[0]
	}
	grpcAddr := fmt.Sprintf("%s:%d", host, s.grpcPort)

	hb := CoreHeartbeat{
		ID:       s.coreID,
		Region:   s.region,
		Version:  s.version,
		GRPCAddr: grpcAddr,
		IPs: CoreIPs{
			Public:  publicIP,
			Private: privateIPs,
		},
	}

	data, err := json.Marshal(hb)
	if err != nil {
		log.Printf("Core Heartbeat marshal error: %v", err)
		return
	}

	if err := s.redis.Set(ctx, s.key(), string(data), 30*time.Second).Err(); err != nil {
		log.Printf("Core Heartbeat Redis error: %v", err)
	}
	if err := s.redis.SAdd(ctx, CoreIndexKey, s.coreID).Err(); err != nil {
		log.Printf("Core Heartbeat: index write failed: %v", err)
	}
	pruneCoreIndex(ctx, s.redis)
}

// pruneCoreIndex drops ids whose heartbeat has expired. A Core killed without
// its Stop never removes itself, and nodes cannot write the index, so every
// live Core sweeps it. Readers skip a dead id anyway; this keeps the set short.
func pruneCoreIndex(ctx context.Context, rdb *redis.Client) {
	ids, err := rdb.SMembers(ctx, CoreIndexKey).Result()
	if err != nil {
		return
	}
	for _, id := range ids {
		if n, err := rdb.Exists(ctx, "dylaris:core:"+id).Result(); err == nil && n == 0 {
			rdb.SRem(ctx, CoreIndexKey, id)
		}
	}
}

// getPrivateIPs returns all private/internal IP addresses.
func getPrivateIPs() []string {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		ip := ipNet.IP.To4()
		if ip == nil {
			continue
		}
		if ip[0] == 10 || (ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) || (ip[0] == 192 && ip[1] == 168) {
			ips = append(ips, ip.String())
		}
	}
	return ips
}

// getOutboundIP returns the preferred outbound IP (same approach as Node).
func getOutboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		name, _ := os.Hostname()
		return name
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
