package main

import (
	"context"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/redis/go-redis/v9"
)

// A server's network traffic as the container's interfaces count it: every
// byte, the game port and any extra port together. Read for the admin's
// extra-ports overview, where a server moving far more than its players
// explain stands out. Per-port numbers would need NET_ADMIN somewhere, which
// the node does not hold on a self-hosted machine.
//
// dylaris:server:<uuid>:net = hash {rx, tx (bytes since the container
// started), rx_bps, tx_bps (over the last interval), ts}. The field names are
// read by core/handlers/servers/server_ports_admin.go.

const netTrafficInterval = 10 * time.Second

func netTrafficKey(uuid string) string { return "dylaris:server:" + uuid + ":net" }

// sumNetworks adds up every interface of the container.
func sumNetworks(nets map[string]container.NetworkStats) (rx, tx uint64) {
	for _, n := range nets {
		rx += n.RxBytes
		tx += n.TxBytes
	}
	return rx, tx
}

// netRate is bytes per second between two readings of the SAME container. A
// counter that went down belongs to a new container and gives no rate.
func netRate(prev, cur uint64, d time.Duration) uint64 {
	if cur < prev || d <= 0 {
		return 0
	}
	return uint64(float64(cur-prev) / d.Seconds())
}

type netSample struct {
	containerID string
	rx, tx      uint64
	at          time.Time
}

// publishNetTraffic writes the hash at most every netTrafficInterval and
// returns the sample to compare the next reading with.
func publishNetTraffic(ctx context.Context, rdb *redis.Client, uuid string, last netSample, cur netSample) netSample {
	if !last.at.IsZero() && cur.at.Sub(last.at) < netTrafficInterval {
		return last
	}
	var rxBps, txBps uint64
	if last.containerID == cur.containerID && !last.at.IsZero() {
		d := cur.at.Sub(last.at)
		rxBps, txBps = netRate(last.rx, cur.rx, d), netRate(last.tx, cur.tx, d)
	}
	key := netTrafficKey(uuid)
	pipe := rdb.Pipeline()
	pipe.HSet(ctx, key, map[string]interface{}{
		"rx": strconv.FormatUint(cur.rx, 10), "tx": strconv.FormatUint(cur.tx, 10),
		"rx_bps": strconv.FormatUint(rxBps, 10), "tx_bps": strconv.FormatUint(txBps, 10),
		"ts": strconv.FormatInt(cur.at.Unix(), 10),
	})
	// A stopped server's numbers go stale, not wrong forever.
	pipe.Expire(ctx, key, 5*time.Minute)
	pipe.Exec(ctx)
	return cur
}
