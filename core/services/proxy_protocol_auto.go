package services

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-core/pkg/leader"
	"dylaris-core/store"
)

// Real player IP in "auto" mode: forwarding follows what the server shows it
// expects. The node pings each running server with and without a PROXY header
// and writes what answered to dylaris:server:<uuid>:proxy_ready (node
// proxy_ready.go). A header sent to a server that does not expect it locks
// every player out, so "auto" forwards only on a positive answer, and only
// through links that can write the header.

// ServerProxyReadyKey is where the node reports what the server expects.
func ServerProxyReadyKey(uuid string) string { return "dylaris:server:" + uuid + ":proxy_ready" }

// proxyAutoWant decides an auto server's forwarding. ok=false means "change
// nothing": the node has not said (an old node, a stopped server whose answer
// expired), and guessing either way could lock players out or drop the IP.
func proxyAutoWant(ready string, platformOn bool, routes int, routesWithCap int) (want, ok bool) {
	switch ready {
	case "1":
		return platformOn && routes > 0 && routesWithCap == routes, true
	case "0":
		return false, true
	}
	return false, false
}

type proxyAutoStore interface {
	ListProxyAutoServers() ([]store.ProxyAutoServer, error)
	proxyProtocolStore
}

// ReconcileProxyAuto applies proxyAutoWant to every auto server whose applied
// forwarding differs from it. Every change goes through
// ApplyServerProxyProtocol, the one write path.
func ReconcileProxyAuto(ctx context.Context, st proxyAutoStore, rdb *redis.Client, hub proxyProtocolHub, platformOn bool) {
	servers, err := st.ListProxyAutoServers()
	if err != nil || len(servers) == 0 {
		if err != nil {
			log.Printf("proxy-protocol auto: %v", err)
		}
		return
	}
	routes, err := ReadRoutesFromRedis(ctx, rdb)
	if err != nil {
		log.Printf("proxy-protocol auto: routes: %v", err)
		return
	}
	capCache := map[string]bool{}
	hasCap := func(tunnel string) (bool, error) {
		if v, ok := capCache[tunnel]; ok {
			return v, nil
		}
		v, err := LinkAnnouncesCap(ctx, rdb, tunnel, "client-addr")
		if err == nil {
			capCache[tunnel] = v
		}
		return v, err
	}
	for _, srv := range servers {
		ready, err := rdb.Get(ctx, ServerProxyReadyKey(srv.UUID)).Result()
		if err != nil && err != redis.Nil {
			continue
		}
		n, withCap, capErr := 0, 0, false
		for _, rt := range routes {
			if !RouteBelongsToServer(rt, srv.UUID) {
				continue
			}
			n++
			ok, err := hasCap(rt.TunnelID)
			if err != nil {
				capErr = true
				break
			}
			if ok {
				withCap++
			}
		}
		if capErr {
			continue
		}
		want, ok := proxyAutoWant(ready, platformOn, n, withCap)
		if !ok || want == srv.Active {
			continue
		}
		if err := ApplyServerProxyProtocol(ctx, st, rdb, hub, srv.ID, srv.UUID, want); err != nil {
			log.Printf("proxy-protocol auto: server %d -> %v: %v", srv.ID, want, err)
			continue
		}
		log.Printf("proxy-protocol auto: server %d forwarding %v (server reported proxy_ready=%s)", srv.ID, want, ready)
	}
}

// StartProxyAutoReconcile runs ReconcileProxyAuto every 20 s on the leader.
func StartProxyAutoReconcile(ctx context.Context, st proxyAutoStore, rdb *redis.Client, hub proxyProtocolHub, platformOn func(context.Context) bool, l leader.Election) {
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if l != nil && !l.IsLeader() {
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			ReconcileProxyAuto(rctx, st, rdb, hub, platformOn(rctx))
			cancel()
		}
	}()
}
