package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A routing-mode switch redeploys every server, and Core queues that redeploy
// immediately after publishing the new mode - long before the node's 30s mode
// ticker would fire. The node therefore re-reads the modes when it pulls a
// command. Switching gateway -> ip_port on a stale node skipped the host port
// binding, leaving every server running, "online", and unreachable.
func TestLoadModesFromRedis_PicksUpASwitchImmediately(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	origRouting, origFile, origPort, origCPort, origIO, origPids := getModes()
	origExternal := nodeExternal
	t.Cleanup(func() {
		setModes(origRouting, origFile, origPort, origCPort, origIO, origPids)
		nodeExternal = origExternal
	})
	nodeExternal = false

	mr.Set("dylaris:routing_mode", "gateway")
	loadModesFromRedis(ctx, rdb)
	if got := getRoutingMode(); got != "gateway" {
		t.Fatalf("routing mode after switch to gateway = %q, want gateway", got)
	}

	// The direction that broke: back to direct ports. A node that kept
	// "gateway" here creates containers with no published port.
	mr.Set("dylaris:routing_mode", "ip_port")
	loadModesFromRedis(ctx, rdb)
	if got := getRoutingMode(); got != "ip_port" {
		t.Fatalf("routing mode after switch back = %q, want ip_port", got)
	}
}

// The fix itself: pulling a command refreshes the modes, so a redeploy queued
// seconds after a mode switch acts on the new mode. Driven through an unknown
// action, whose only effect is the default branch's log line - everything
// before the switch (the refresh) still runs.
func TestProcessCommand_RefreshesModesBeforeActing(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	origRouting, origFile, origPort, origCPort, origIO, origPids := getModes()
	origExternal := nodeExternal
	t.Cleanup(func() {
		setModes(origRouting, origFile, origPort, origCPort, origIO, origPids)
		nodeExternal = origExternal
	})
	nodeExternal = false

	// The node's view is the pre-switch mode; Redis already carries the new one.
	setModes("gateway", origFile, origPort, origCPort, origIO, origPids)
	mr.Set("dylaris:routing_mode", "ip_port")

	processCommand(ctx, NodeCommand{Action: "__test_unknown__"}, "", rdb, nil, "node-1", nil, nil)

	if got := getRoutingMode(); got != "ip_port" {
		t.Fatalf("routing mode after a pulled command = %q, want ip_port (stale mode was not refreshed)", got)
	}
}

// The refresh must not become a way for Core to un-gateway a BYON node: an
// external node forces gateway locally so it never publishes a host port and
// never exposes the owner's home address.
func TestLoadModesFromRedis_ExternalNodeStaysOnGateway(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	origRouting, origFile, origPort, origCPort, origIO, origPids := getModes()
	origExternal := nodeExternal
	t.Cleanup(func() {
		setModes(origRouting, origFile, origPort, origCPort, origIO, origPids)
		nodeExternal = origExternal
	})
	nodeExternal = true

	mr.Set("dylaris:routing_mode", "ip_port")
	mr.Set("dylaris:file_access_mode", "sftp")
	loadModesFromRedis(ctx, rdb)

	if got := getRoutingMode(); got != "gateway" {
		t.Fatalf("external node routing mode = %q, want gateway", got)
	}
	if got := getFileAccessMode(); got != "beam" {
		t.Fatalf("external node file access mode = %q, want beam", got)
	}
}

// pauseAfterRoutingRead holds a client's round right after it read the routing
// mode, until release is closed.
type pauseAfterRoutingRead struct{ reached, release chan struct{} }

func (pauseAfterRoutingRead) DialHook(next redis.DialHook) redis.DialHook { return next }
func (pauseAfterRoutingRead) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h pauseAfterRoutingRead) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if a := cmd.Args(); len(a) == 2 && a[0] == "get" && a[1] == "dylaris:routing_mode" {
			close(h.reached)
			<-h.release
		}
		return err
	}
}

// Rounds ran side by side, each a read-modify-write: the ticker's round read
// the old mode, a command's round read and installed the new one, and the
// ticker's then finished and installed the old one again.
func TestAnOlderModeRoundCannotOverwriteANewerOne(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()
	origRouting, origFile, origPort, origCPort, origIO, origPids := getModes()
	origExternal := nodeExternal
	t.Cleanup(func() {
		setModes(origRouting, origFile, origPort, origCPort, origIO, origPids)
		nodeExternal = origExternal
	})
	nodeExternal = false

	mr.Set("dylaris:routing_mode", "ip_port")
	stale := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	h := pauseAfterRoutingRead{reached: make(chan struct{}), release: make(chan struct{})}
	stale.AddHook(h)
	staleDone := make(chan struct{})
	go func() { loadModesFromRedis(ctx, stale); close(staleDone) }()
	<-h.reached

	mr.Set("dylaris:routing_mode", "gateway") // Core switches
	freshDone := make(chan struct{})
	go func() { loadModesFromRedis(ctx, redis.NewClient(&redis.Options{Addr: mr.Addr()})); close(freshDone) }()
	select {
	case <-freshDone:
	case <-time.After(200 * time.Millisecond):
	}
	close(h.release)
	<-staleDone
	<-freshDone
	if got := getRoutingMode(); got != "gateway" {
		t.Fatalf("routing mode = %q after the switch, want gateway", got)
	}
}
