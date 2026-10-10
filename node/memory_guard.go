package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/redis/go-redis/v9"

	"dylaris-pkg/validate"
)

// Memory guard: a server whose container reaches its memory limit is SIGKILLed
// by the kernel, with no log line and no world save. This watches the
// container's NON-RECLAIMABLE memory (guardMemoryBytes) on every stats tick and
// reports to Core before that happens; Core decides what to do (stop, restart
// or only warn).
//
// The thresholds sit high on purpose. Measured on prod: a healthy Paper server
// four days up holds ~90.5% of its limit in anon memory alone (heap = booking,
// Xms=Xmx touches it all eventually), so anything at or below that would fire
// on servers that are fine.
//
// ponytail: fixed thresholds, not settings. Make them configurable when an
// operator shows a server class they misfire on.
const (
	memWarnPct          = 0.95 // save-all + memory_warning
	memCriticalPct      = 0.98 // memory_critical once held for memCriticalHold
	memRearmPct         = 0.93 // below this, memory_critical may fire again
	memCriticalHold     = 30 * time.Second
	memWarnEvery        = 5 * time.Minute
	memoryEventsMax     = 20
	memoryEventsTTL     = time.Hour
	oomWatchRetryDelay  = 5 * time.Second
	memEventWarning     = "memory_warning"
	memEventCritical    = "memory_critical"
	memEventOOMKilled   = "oom_killed"
	memoryEventsKeyTail = ":memory_events"
)

// memGuard is one running server's guard state. Not safe for concurrent use;
// each container's collector owns its own.
type memGuard struct {
	lastWarn      time.Time
	highSince     time.Time // first sample at/over memCriticalPct; zero when below
	criticalFired bool      // true until usage drops below memRearmPct
}

// step folds one sample in and says what to do now.
func (g *memGuard) step(now time.Time, usedMB, limitMB int64) (warn, critical bool) {
	if limitMB <= 0 {
		g.highSince = time.Time{}
		return false, false
	}
	pct := float64(usedMB) / float64(limitMB)
	if pct < memRearmPct {
		g.criticalFired = false
	}
	if pct >= memWarnPct && (g.lastWarn.IsZero() || now.Sub(g.lastWarn) >= memWarnEvery) {
		warn = true
		g.lastWarn = now
	}
	if pct < memCriticalPct {
		g.highSince = time.Time{}
		return warn, false
	}
	if g.highSince.IsZero() {
		g.highSince = now
	}
	if !g.criticalFired && now.Sub(g.highSince) >= memCriticalHold {
		g.criticalFired = true
		critical = true
	}
	return warn, critical
}

// memoryEvent is what the node hands Core on dylaris:server:<uuid>:memory_events.
// Core only adds fields it reads; a node may add more.
type memoryEvent struct {
	Event   string `json:"event"` // memory_warning | memory_critical | oom_killed
	At      int64  `json:"at"`    // unix seconds
	UsedMB  int64  `json:"usedMB,omitempty"`
	LimitMB int64  `json:"limitMB,omitempty"`
}

func memoryEventsKey(uuid string) string {
	return "dylaris:server:" + uuid + memoryEventsKeyTail
}

// reportMemoryEvent appends to the server's event list. Capped and expiring, so
// a Core that predates the list (and never drains it) leaves nothing behind.
func reportMemoryEvent(ctx context.Context, rdb *redis.Client, uuid string, ev memoryEvent) {
	data, _ := json.Marshal(ev)
	key := memoryEventsKey(uuid)
	pipe := rdb.TxPipeline()
	pipe.RPush(ctx, key, string(data))
	pipe.LTrim(ctx, key, -memoryEventsMax, -1)
	pipe.Expire(ctx, key, memoryEventsTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("memory guard: report %s for %s: %v", ev.Event, uuid, err)
	}
}

// guardMemoryBytes is the part of a container's memory the kernel cannot
// reclaim, from the Docker stats memory_stats.stats map: what actually drives
// it into the OOM killer. The displayed "used" figure still counts active file
// cache, which the kernel drops under pressure, so it reads high on a server
// that is in no danger.
//
// cgroup v2: anon + shmem + kernel - slab_reclaimable (kernel counts the
// reclaimable slab, dentries and inodes the kernel drops under pressure).
// cgroup v1: rss + shmem (the hierarchical total_* keys when present).
// Neither: fallback (the displayed figure).
func guardMemoryBytes(st map[string]uint64, fallback uint64) uint64 {
	if anon, ok := st["anon"]; ok {
		kernel := st["kernel"]
		if r := st["slab_reclaimable"]; r <= kernel {
			kernel -= r
		} else {
			kernel = 0
		}
		return anon + st["shmem"] + kernel
	}
	if rss, ok := st["total_rss"]; ok {
		return rss + st["total_shmem"]
	}
	if rss, ok := st["rss"]; ok {
		return rss + st["shmem"]
	}
	return fallback
}

// The node's backups are tracked in backupsInFlight under "server:<uuid>"
// (backup_worker.go); a backup turns world saving off for its archive.
func (s *inflightSet) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[id]
}

// backupRunning reports whether this node is archiving the server right now.
// A save-all then would write region files under the running tar.
func backupRunning(uuid string) bool { return backupsInFlight.has("server:" + uuid) }

// applyMemGuard runs one sample through the guard and acts on it.
func applyMemGuard(ctx context.Context, rdb *redis.Client, uuid string, g *memGuard, usedMB, limitMB int64) {
	now := time.Now()
	warn, critical := g.step(now, usedMB, limitMB)
	if warn {
		if backupRunning(uuid) {
			log.Printf("memory guard: %s at %d/%d MB, backup running so no save-all", uuid, usedMB, limitMB)
		} else {
			log.Printf("memory guard: %s at %d/%d MB, sending save-all", uuid, usedMB, limitMB)
			rdb.RPush(ctx, fmt.Sprintf("dylaris:server:%s:input", uuid), "save-all")
		}
		reportMemoryEvent(ctx, rdb, uuid, memoryEvent{Event: memEventWarning, At: now.Unix(), UsedMB: usedMB, LimitMB: limitMB})
	}
	if critical {
		log.Printf("memory guard: %s held at %d/%d MB for %s, reporting memory_critical", uuid, usedMB, limitMB, memCriticalHold)
		reportMemoryEvent(ctx, rdb, uuid, memoryEvent{Event: memEventCritical, At: now.Unix(), UsedMB: usedMB, LimitMB: limitMB})
	}
}

// oomEventServerUUID maps a Docker "oom" event to the server it hit, if it is
// one of this node's MC containers.
//
// Docker emits "oom" the moment the kernel kills a process in the container's
// cgroup, while the container may still be running: the log-shipper is PID 1
// and restarts Java after a crash, so the container itself often never exits
// and State.OOMKilled at exit would miss the first kills. The event does not.
func oomEventServerUUID(msg events.Message, self []string) (string, bool) {
	if msg.Type != events.ContainerEventType || msg.Action != events.ActionOOM {
		return "", false
	}
	name := strings.TrimPrefix(msg.Actor.Attributes["name"], "/")
	if !strings.HasPrefix(name, "mc_") {
		return "", false
	}
	uuid := strings.TrimPrefix(name, "mc_")
	if !validate.IsServerUUID(uuid) || !ownsContainer(msg.Actor.Attributes, self) {
		return "", false
	}
	return uuid, true
}

// watchOOMEvents reports every OOM kill in one of this node's MC containers.
// An event missed while the stream is down (node restart, daemon restart) is
// lost; that is a missing notification, never a wrong action.
func watchOOMEvents(ctx context.Context, rdb *redis.Client, dm *DockerManager) {
	opts := events.ListOptions{Filters: filters.NewArgs(
		filters.Arg("type", string(events.ContainerEventType)),
		filters.Arg("event", string(events.ActionOOM)),
	)}
	for ctx.Err() == nil {
		msgs, errs := dm.cli.Events(ctx, opts)
	recv:
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-errs:
				if ctx.Err() == nil {
					log.Printf("memory guard: docker event stream: %v (retrying)", err)
				}
				break recv
			case msg := <-msgs:
				uuid, ok := oomEventServerUUID(msg, nodeIdentities(nodeSecretDir))
				if !ok {
					continue
				}
				at := msg.Time
				if at == 0 {
					at = time.Now().Unix()
				}
				log.Printf("memory guard: %s was OOM-killed", uuid)
				reportMemoryEvent(ctx, rdb, uuid, memoryEvent{Event: memEventOOMKilled, At: at})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(oomWatchRetryDelay):
		}
	}
}
