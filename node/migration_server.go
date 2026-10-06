package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"dylaris-pkg/migration"

	"github.com/redis/go-redis/v9"
)

// StartMigrationServer runs the node-side migration pull endpoint and publishes
// its overlay address to Redis. A target node calls GET /migration with a
// per-node-secret-derived bearer token; on a valid token we ServeFile the staged
// archive resolved via archivePathFor. Auth gates every request, so binding to
// all interfaces (needed so a sibling node can reach us over the overlay) opens
// no new surface — same reasoning as the Beam gRPC server.
//
// Wave 2a wires nothing into main.go yet: the real archivePathFor (which stages
// a zip per server UUID) lands with the command handlers in Wave 2b. This
// function only needs to compile and be ready.
func StartMigrationServer(ctx context.Context, rdb *redis.Client, nodeID string, archivePathFor func(serverUUID string) (string, bool)) {
	port := os.Getenv("MIGRATION_PORT")
	if port == "" {
		port = "25522"
	}
	listenAddr := ":" + port

	mux := http.NewServeMux()
	mux.HandleFunc("GET /migration", func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Mirror Core's keying: always verify with the per-node secret (read
		// through the guarded accessor so this always sees the latest value).
		verifyKey := string(getNodeSecret())
		claims, err := migration.VerifyToken(verifyKey, token)
		if err != nil {
			// Don't echo the underlying reason (expired vs forged) to callers.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if archivePathFor == nil {
			// No stager wired (pre-Wave-2b): treat as nothing to serve.
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		path, ok := archivePathFor(claims.ServerUUID)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// ServeFile handles range requests + chunking. We pass an absolute,
		// fully-resolved path so its own internal redirect/cleaning never
		// reinterprets it, and never surface the path in an error.
		http.ServeFile(w, r, path)
	})

	srv := &http.Server{Addr: listenAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go publishMigrationEndpoint(ctx, rdb, nodeID, port)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("migration-server: listening on %s (reachable via overlay; HMAC-gated)", listenAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("migration-server: serve error: %v", err)
	}
}

// bearerToken reads the pull token from the Authorization: Bearer header,
// falling back to a ?token= query param (matching the Pull side, which sends
// the header). The query fallback keeps the endpoint testable/curl-able.
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	return strings.TrimSpace(r.URL.Query().Get("token"))
}

// publishMigrationEndpoint refreshes Redis key dylaris:migration:endpoint:<nodeID>
// with this node's overlay address every 10s (30s TTL), mirroring
// publishBeamEndpoint so a peer node can discover where to pull from without
// relying on Docker-DNS service names. Best-effort: a Redis outage just leaves
// the key to expire and the orchestrator (Wave 3) can fall back.
func publishMigrationEndpoint(ctx context.Context, rdb *redis.Client, nodeID, port string) {
	if rdb == nil || strings.TrimSpace(nodeID) == "" {
		return
	}
	const (
		key         = "dylaris:migration:endpoint:"
		ttl         = 30 * time.Second
		refreshTick = 10 * time.Second
	)
	var lastLoggedIP string
	publish := func() {
		ip := migrationOverlayIP()
		if ip == "" {
			return
		}
		if err := rdb.Set(ctx, key+nodeID, ip+":"+port, ttl).Err(); err != nil {
			log.Printf("migration-server: endpoint publish failed: %v", err)
			return
		}
		if ip != lastLoggedIP {
			log.Printf("migration-server: endpoint published to Redis (%s:%s for node %q)", ip, port, nodeID)
			lastLoggedIP = ip
		}
	}
	publish()
	ticker := time.NewTicker(refreshTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}

// migrationOverlayIP picks the container's Swarm-overlay IPv4, preferring the
// 10.0.0.0/8 range and falling back to 172.x/192.x. Duplicated from
// beam_server.go's overlayIP rather than refactoring it (per Wave 2a scope) —
// see that function's comment for why the naive net.Dial trick is wrong here.
func migrationOverlayIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	var fallback string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if ip[0] == 10 {
			return ip.String()
		}
		if fallback == "" && (ip[0] == 172 || ip[0] == 192) {
			fallback = ip.String()
		}
	}
	return fallback
}
