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
	turn chan struct{} // capacity 1: whoever holds it runs
	held sync.Mutex    // guards active and waiting
	// active is true while an action runs.
	active bool
	// waiting holds one waiter per slot (waitSlot).
	waiting map[string]powerWaiter
	// abandon is the holder's startGuard, nil when it has none.
	abandon func() bool
}

type powerWaiter struct {
	kick chan struct{} // closed to send the waiter away
	cfg  *ServerConfig // its payload, nil when not given
}

// waitSlot names the one waiting place an action may hold. Start, stop and
// restart share one: the last click is the whole intent. Each config change
// has its own, because its payload is the full state of its kind only - except
// update_resources, which the newer waiter merges (inheritResourceChanges).
func waitSlot(action string) string {
	if powerActions[action] {
		return "power"
	}
	return action
}

// serialisedActions are the commands that stop, start or recreate a server's
// container.
var serialisedActions = map[string]bool{
	"start": true, "stop": true, "restart": true,
	"switch_server": true, "update_resources": true, "reconfigure": true,
}

func powerLockFor(uuid string) *serverPowerLock {
	l, _ := serverPowerLocks.LoadOrStore(uuid, &serverPowerLock{turn: make(chan struct{}, 1)})
	return l.(*serverPowerLock)
}

// lockServerPower takes the server's lock and returns its release, or reports
// false when the action is not to run. A waiter sits in one of the node's
// command slots, so without a bound a burst of clicks on one server could fill
// them all and stall every other server's commands, kill included.
//
// The bound keeps one waiter per slot (waitSlot), and a newer action REPLACES
// the waiter in its slot. Refusing the newcomer instead dropped and acked the
// newest intent: a stop behind a waiting start, or the third settings save,
// whose payload held the newest state while the older waiter ran and stayed.
// At most four waiters per server, so kill (its own express pool) and other
// servers still get a slot.
//
// cfg is the action's payload, which it may still be changed through before it
// runs: a newer update_resources takes over what the one it replaces changed.
func lockServerPower(uuid, action string, cfg *ServerConfig) (func(), bool) {
	l := powerLockFor(uuid)
	slot := waitSlot(action)
	kick := make(chan struct{})
	l.held.Lock()
	if l.waiting == nil {
		l.waiting = map[string]powerWaiter{}
	}
	if old, ok := l.waiting[slot]; ok {
		if action == "update_resources" && cfg != nil && old.cfg != nil {
			inheritResourceChanges(&cfg.Docker, old.cfg.Docker)
		}
		close(old.kick)
	}
	l.waiting[slot] = powerWaiter{kick: kick, cfg: cfg}
	l.held.Unlock()

	select {
	case l.turn <- struct{}{}:
	case <-kick:
		return nil, false
	}
	l.held.Lock()
	select {
	case <-kick: // replaced in the moment the turn came free
		l.held.Unlock()
		<-l.turn
		return nil, false
	default:
	}
	delete(l.waiting, slot)
	l.active = true
	l.held.Unlock()
	return func() {
		l.setActive(false)
		<-l.turn
	}, true
}

// inheritResourceChanges gives a newer update_resources what the waiting one
// it replaces changed and it does not. Core sends the padding and the ports
// only when they change, and the node reads them missing as "keep as is", so
// replacing the waiter outright dropped a headroom or port change and acked it.
// Everything else is in every payload, so the newer one already wins there.
func inheritResourceChanges(newer *DockerConfig, older DockerConfig) {
	if newer.RAMPaddingMB == nil {
		newer.RAMPaddingMB = older.RAMPaddingMB
	}
	if newer.HostPort == 0 {
		newer.HostPort = older.HostPort
	}
	if newer.ContainerPort == 0 {
		newer.ContainerPort = older.ContainerPort
	}
}

// tryLockServerPower takes the server's lock only if it is free, for the
// reconciler: it must not queue behind a command, and a command must not find
// the reconciler's recreate running beside it unlocked.
func tryLockServerPower(uuid string) (func(), bool) {
	l := powerLockFor(uuid)
	select {
	case l.turn <- struct{}{}:
	default:
		return nil, false
	}
	l.setActive(true)
	return func() {
		l.setActive(false)
		<-l.turn
	}, true
}

// startGuard sets the check the lock holder's bring-up makes right before
// ContainerStart (startAbandoned), and returns its removal. Kill takes no lock,
// so one pressed during a recreate - an image pull, an ownership walk - hit a
// container that was gone or not yet started, failed, and the recreate then
// started the server against the user's last click.
func startGuard(uuid string, abandon func() bool) func() {
	l := powerLockFor(uuid)
	l.held.Lock()
	l.abandon = abandon
	l.held.Unlock()
	return func() {
		l.held.Lock()
		l.abandon = nil
		l.held.Unlock()
	}
}

// startAbandoned is the last check before a server's container is started.
func startAbandoned(uuid string) bool {
	v, ok := serverPowerLocks.Load(uuid)
	if !ok {
		return false
	}
	l := v.(*serverPowerLock)
	l.held.Lock()
	abandon := l.abandon
	l.held.Unlock()
	return abandon != nil && abandon()
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
