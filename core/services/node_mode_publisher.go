package services

import (
	"context"
	"log"
	"time"

	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// nodeModePublishInterval bounds how long a node can run on the wrong settings
// after Redis has lost them. It matches the node's own loadModesFromRedis
// cadence, so a node picks the values back up on its next read either way.
const nodeModePublishInterval = 30 * time.Second

// NodeModePublisher keeps the settings nodes consume present in Redis.
//
// The database owns these values; Redis only distributes them. They used to be
// written exactly twice: the two mode keys once at Core startup, the placement
// keys once per admin save - both with no TTL and nothing re-asserting them.
// Redis runs without persistence (save "", appendonly no), so a restart drops
// them.
//
// The node's 30s re-read is not the safety net it looks like. loadModesFromRedis
// overwrites a mode only on a successful NON-EMPTY get, so a missing key leaves
// the previous value standing - fail-safe for a node that is already running,
// useless for one that restarts, which then falls back to its compiled
// defaults. A node coming up after a Redis restart would route ip_port on a
// gateway-routed platform and serve files over SFTP with beam configured, with
// nothing in either log saying so.
//
// Publishing on a ticker makes the values re-derivable from their source of
// truth, which is what every other piece of state in Redis already is.
type NodeModePublisher struct {
	store store.Store
	redis *redis.Client
}

func NewNodeModePublisher(st store.Store, rdb *redis.Client) *NodeModePublisher {
	return &NodeModePublisher{store: st, redis: rdb}
}

// Start publishes once, then every nodeModePublishInterval until ctx ends.
//
// Deliberately NOT leader-gated: every Core reads these from the same database
// and writes the same values, so any Core republishing is correct, and recovery
// keeps working across a leadership gap.
func (p *NodeModePublisher) Start(ctx context.Context) {
	if p.redis == nil || p.store == nil {
		return
	}
	p.Publish(ctx)
	go func() {
		t := time.NewTicker(nodeModePublishInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.Publish(context.Background())
			}
		}
	}()
}

// Publish writes the current database values to Redis.
func (p *NodeModePublisher) Publish(ctx context.Context) {
	// Written every time, defaults included: a stale value left in Redis by a
	// previous install must not survive. That is why the original startup write
	// wrote the defaults too, and it still holds.
	mode, _ := p.store.GetSetting("routing_mode")
	if mode == "" {
		mode = "ip_port"
	}
	fileMode, _ := p.store.GetSetting("file_access_mode")
	if fileMode == "" {
		fileMode = "sftp"
	}
	values := map[string]string{
		"dylaris:routing_mode":     mode,
		"dylaris:file_access_mode": fileMode,
	}

	// These only exist once an admin has saved them. An absent setting means
	// the node's own compiled default is the intended value - placement agrees
	// on sequential / 25565 / 0 / 0, the Link pair on auto_idle / 15 - so
	// publishing nothing is correct, and encoding the defaults here would be a
	// second copy free to drift from the handlers that own them.
	//
	// The Link pair was mirrored on save like the placement keys and then left
	// out of this loop, which is the only reason it is called out: the two are
	// the whole of Settings -> Link updates, so losing them turned an operator's
	// explicit "notify" - do not touch my Link, just tell me - back into
	// auto_idle on the next node start, while the panel went on reading the
	// database and showing "notify". Nothing re-asserted them, so that gap did
	// not close on its own; only saving the page again did.
	for setting, key := range map[string]string{
		"placement.port_mode":      "dylaris:placement:port_mode",
		"placement.container_port": "dylaris:placement:container_port",
		"placement.pids_limit":     "dylaris:placement:pids_limit",
		"placement.io_weight":      "dylaris:placement:io_weight",
	} {
		if v, err := p.store.GetSetting(setting); err == nil && v != "" {
			values[key] = v
		}
	}

	for k, v := range values {
		if err := p.redis.Set(ctx, k, v, 0).Err(); err != nil {
			log.Printf("node mode publisher: set %s: %v", k, err)
		}
	}
}
