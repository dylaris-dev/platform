package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// nodeStartedAt tells a container that stopped while this node was watching
// from one that was already down when it started: a host reboot stops every
// container with a clean exit too, and those must come back.
var nodeStartedAt = time.Now()

// Commands are consumed eight at a time, with nothing per server, so a stop
// and a start for one server ran side by side: the start's recreate removed
// the container while the stop was still saving the world, and the stop then
// wrote "stopped" over a server that was starting. These actions now take
// their server's lock and run one after another. Kill does not wait: it is
// what someone presses when a stop hangs.
var serverPowerLocks sync.Map // uuid -> *serverPowerLock

type serverPowerLock struct {
	mu      sync.Mutex
	held    sync.Mutex // guards active and waiting
	active  bool
	waiting int
}

// serialisedActions are the commands that stop, start or recreate a server's
// container.
var serialisedActions = map[string]bool{
	"start": true, "stop": true, "restart": true,
	"switch_server": true, "update_resources": true, "reconfigure": true,
}

func powerLockFor(uuid string) *serverPowerLock {
	l, _ := serverPowerLocks.LoadOrStore(uuid, &serverPowerLock{})
	return l.(*serverPowerLock)
}

// lockServerPower takes the server's lock and returns its release, or reports
// false when an action is already running and another already waits. A waiter
// sits in one of the node's eight command slots, so without a bound a burst of
// clicks on one server could fill them all and stall every other server's
// commands, kill included. The dropped action is a repeat of what is queued;
// the panel shows the outcome and it can be sent again.
func lockServerPower(uuid string) (func(), bool) {
	l := powerLockFor(uuid)
	l.held.Lock()
	if l.active && l.waiting > 0 {
		l.held.Unlock()
		return nil, false
	}
	l.waiting++
	l.held.Unlock()

	l.mu.Lock()
	l.held.Lock()
	l.waiting--
	l.active = true
	l.held.Unlock()
	return func() {
		l.setActive(false)
		l.mu.Unlock()
	}, true
}

func (l *serverPowerLock) setActive(v bool) {
	l.held.Lock()
	l.active = v
	l.held.Unlock()
}

// serverPowerActive reports whether a power action is running for the server
// right now. The reconciler leaves such a server alone: between a restart's
// stop and its start the container is down on purpose.
func serverPowerActive(uuid string) bool {
	l, ok := serverPowerLocks.Load(uuid)
	if !ok {
		return false
	}
	pl := l.(*serverPowerLock)
	pl.held.Lock()
	defer pl.held.Unlock()
	return pl.active
}

// stoppedByServerKey marks a server that shut itself down cleanly: "stop" in
// the console or in game. It used to be restarted at once as a crash, and three
// of those in a quarter of an hour read as a crash loop. Core lands it as
// stopped (status_watcher), and the reconciler does not restart it; a start,
// restart, setup, reinstall or switch clears it, like reconcile_failed.
func stoppedByServerKey(uuid string) string {
	return fmt.Sprintf("dylaris:server:%s:stopped_by_server", uuid)
}

// clearRunBlockers drops the markers that hold a server down. Every deliberate
// start goes through here.
func clearRunBlockers(ctx context.Context, rdb *redis.Client, uuid string) {
	// :input too: a "stop" left there by a stop the container died during
	// reached the next start at once, and that start then read as a stop.
	rdb.Del(ctx, fmt.Sprintf("dylaris:server:%s:reconcile_failed", uuid), stoppedByServerKey(uuid),
		fmt.Sprintf("dylaris:server:%s:input", uuid))
}
