package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/api/types/container"
	"github.com/redis/go-redis/v9"
)

func TestSumNetworksAddsEveryInterface(t *testing.T) {
	rx, tx := sumNetworks(map[string]container.NetworkStats{
		"eth0": {RxBytes: 1000, TxBytes: 50},
		"eth1": {RxBytes: 24, TxBytes: 6},
	})
	if rx != 1024 || tx != 56 {
		t.Fatalf("rx %d tx %d, want 1024 56", rx, tx)
	}
	if rx, tx := sumNetworks(nil); rx != 0 || tx != 0 {
		t.Fatal("no interfaces, no bytes")
	}
}

func TestPublishNetTraffic(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	t0 := time.Unix(1_000_000, 0)
	key := netTrafficKey("u-1")

	last := publishNetTraffic(ctx, rdb, "u-1", netSample{}, netSample{containerID: "c1", rx: 1000, tx: 100, at: t0})
	if got := mr.HGet(key, "rx"); got != "1000" || mr.HGet(key, "rx_bps") != "0" {
		t.Fatalf("first sample: rx %q rx_bps %q, want 1000 and no rate yet", got, mr.HGet(key, "rx_bps"))
	}
	// Inside the interval: not written again.
	last = publishNetTraffic(ctx, rdb, "u-1", last, netSample{containerID: "c1", rx: 5000, tx: 100, at: t0.Add(2 * time.Second)})
	if mr.HGet(key, "rx") != "1000" {
		t.Fatal("wrote again inside the interval")
	}
	// 10 s later: 11000 more bytes in = 1100 B/s.
	last = publishNetTraffic(ctx, rdb, "u-1", last, netSample{containerID: "c1", rx: 12000, tx: 2100, at: t0.Add(10 * time.Second)})
	if mr.HGet(key, "rx") != "12000" || mr.HGet(key, "rx_bps") != "1100" || mr.HGet(key, "tx_bps") != "200" {
		t.Fatalf("rx %s rx_bps %s tx_bps %s", mr.HGet(key, "rx"), mr.HGet(key, "rx_bps"), mr.HGet(key, "tx_bps"))
	}
	// A recreated container restarts its counters: no rate from the old one.
	publishNetTraffic(ctx, rdb, "u-1", last, netSample{containerID: "c2", rx: 300, tx: 30, at: t0.Add(20 * time.Second)})
	if mr.HGet(key, "rx") != "300" || mr.HGet(key, "rx_bps") != "0" {
		t.Fatalf("new container: rx %s rx_bps %s", mr.HGet(key, "rx"), mr.HGet(key, "rx_bps"))
	}
	if ttl := mr.TTL(key); ttl <= 0 || ttl > 5*time.Minute {
		t.Fatalf("ttl %v, want the key to expire", ttl)
	}
}

func TestNetRateNeverGoesNegative(t *testing.T) {
	if netRate(500, 100, time.Second) != 0 {
		t.Fatal("a counter that went down gave a rate")
	}
	if netRate(0, 100, 0) != 0 {
		t.Fatal("no time, no rate")
	}
}
