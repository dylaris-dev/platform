package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"dylaris-core/store"
)

// The settings a node reads out of Redis are owned by the database and only
// distributed through Redis. They were written once at Core startup (the mode
// keys) and once per admin save (the placement keys), with no TTL and nothing
// re-asserting them, in a Redis that runs without persistence.
//
// The node's 30s re-read does not cover that: loadModesFromRedis overwrites a
// mode only on a successful non-empty GET, so a lost key leaves a running node
// on its last value and a restarting node on its compiled defaults - ip_port
// routing on a gateway-routed platform, with nothing in the logs.

// settingsStub serves GetSetting from a map; every other Store method is
// unreachable in these tests.
type settingsStub struct {
	store.Store
	values map[string]string
}

func (s *settingsStub) GetSetting(key string) (string, error) {
	v, ok := s.values[key]
	if !ok {
		return "", errors.New("no such setting")
	}
	return v, nil
}

func TestNodeModePublisherRepublishesAfterRedisLostTheKeys(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	st := &settingsStub{values: map[string]string{
		"routing_mode":             "gateway",
		"file_access_mode":         "beam",
		"placement.port_mode":      "random",
		"placement.container_port": "25577",
	}}
	p := NewNodeModePublisher(st, rdb)

	p.Publish(ctx)
	if err := rdb.FlushAll(ctx).Err(); err != nil { // what a Redis restart does
		t.Fatalf("flush: %v", err)
	}
	p.Publish(ctx) // the ticker's next tick

	want := map[string]string{
		"dylaris:routing_mode":             "gateway",
		"dylaris:file_access_mode":         "beam",
		"dylaris:placement:port_mode":      "random",
		"dylaris:placement:container_port": "25577",
	}
	for key, exp := range want {
		got, err := rdb.Get(ctx, key).Result()
		if err != nil {
			t.Errorf("%s absent after republish (%v): a node restarting now would use its compiled default", key, err)
			continue
		}
		if got != exp {
			t.Errorf("%s: got %q, want %q", key, got, exp)
		}
	}
}

// The two mode keys are written even when the database has nothing, so a value
// left behind by a previous install cannot survive. That was the reason the
// original startup write gave, and it has to keep holding.
func TestNodeModePublisherWritesModeDefaultsOverStaleValues(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()

	if err := rdb.Set(ctx, "dylaris:routing_mode", "gateway", 0).Err(); err != nil {
		t.Fatalf("seed stale value: %v", err)
	}
	NewNodeModePublisher(&settingsStub{values: map[string]string{}}, rdb).Publish(ctx)

	if v, _ := rdb.Get(ctx, "dylaris:routing_mode").Result(); v != "ip_port" {
		t.Errorf("stale routing_mode survived: got %q, want ip_port", v)
	}
	if v, _ := rdb.Get(ctx, "dylaris:file_access_mode").Result(); v != "sftp" {
		t.Errorf("file_access_mode: got %q, want sftp", v)
	}
}

// An unsaved placement setting must NOT be published: the node's compiled
// default is the intended value, and writing one here would be a second copy of
// the defaults, free to drift from handlers.defaultPlacementSettings.
func TestNodeModePublisherLeavesUnsetPlacementSettingsAlone(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()

	NewNodeModePublisher(&settingsStub{values: map[string]string{}}, rdb).Publish(ctx)

	for _, key := range []string{
		"dylaris:placement:port_mode",
		"dylaris:placement:container_port",
		"dylaris:placement:pids_limit",
		"dylaris:placement:io_weight",
	} {
		if n, _ := rdb.Exists(ctx, key).Result(); n != 0 {
			v, _ := rdb.Get(ctx, key).Result()
			t.Errorf("%s published as %q although no admin ever saved it", key, v)
		}
	}
}

// Start has to publish before its first tick, or a Core restart leaves the
// modes missing for a whole interval.
func TestNodeModePublisherStartPublishesImmediately(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := &settingsStub{values: map[string]string{"routing_mode": "gateway"}}
	NewNodeModePublisher(st, rdb).Start(ctx)

	v, err := rdb.Get(ctx, "dylaris:routing_mode").Result()
	if err != nil {
		t.Fatalf("routing_mode absent right after Start (%v): nodes would wait a full interval", err)
	}
	if v != "gateway" {
		t.Fatalf("routing_mode: got %q, want gateway", v)
	}
}

// The republish cadence must not be slower than the node's own read loop, or a
// node can sit on a stale value longer than the interval suggests.
func TestNodeModePublishIntervalMatchesTheNodeReadLoop(t *testing.T) {
	if nodeModePublishInterval > 30*time.Second {
		t.Fatalf("publish interval %v is slower than the node's 30s loadModesFromRedis loop", nodeModePublishInterval)
	}
}

// An unsaved Link setting must not be published either, for the same reason the
// placement keys are not: the node compiles in auto_idle / 15, which is what
// handlers.GetLinkUpdateSettings reports for an unset value.
func TestNodeModePublisherLeavesUnsetLinkUpdateSettingsAlone(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()

	NewNodeModePublisher(&settingsStub{values: map[string]string{}}, rdb).Publish(ctx)

	for _, key := range []string{"dylaris:link_update_policy", "dylaris:link_update_interval_min"} {
		if n, _ := rdb.Exists(ctx, key).Result(); n != 0 {
			v, _ := rdb.Get(ctx, key).Result()
			t.Errorf("%s published as %q although no admin ever saved it", key, v)
		}
	}
}

// alwaysSetStore answers every GetSetting, so Publish writes every key it knows
// about and the assertion below is about the SET of keys, not their values.
type alwaysSetStore struct{ store.Store }

func (alwaysSetStore) GetSetting(string) (string, error) { return "x", nil }

// mirroredKeyPattern finds the Redis keys a handler writes on an admin save.
// Only string literals: a key built from a helper (the storage-placement pair)
// is deliberately Redis-only with no database row behind it, so there is
// nothing for this publisher to re-derive it from.
var mirroredKeyPattern = regexp.MustCompile(`Redis\.Set\([^,]+, "(dylaris:[^"]+)"`)

// Every setting a handler mirrors into Redis has to be one this publisher
// re-asserts, or it survives only until Redis restarts - and Redis here runs
// with save "" and appendonly no, so that is a matter of when.
//
// The check reads the handlers rather than a list kept next to it, because a
// list is the thing that was already out of date: six settings were mirrored
// and re-asserted, two more were added to the save path alone, and nothing
// anywhere reported the difference.
func TestEverySettingMirroredIntoRedisIsRepublished(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "handlers", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob handlers: %v (%d files)", err, len(files))
	}

	mirrored := map[string]string{} // key -> file that writes it
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range mirroredKeyPattern.FindAllStringSubmatch(string(src), -1) {
			mirrored[m[1]] = filepath.Base(f)
		}
	}
	if len(mirrored) == 0 {
		t.Fatal("found no mirrored keys at all - the pattern stopped matching, so this test is no longer checking anything")
	}

	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	NewNodeModePublisher(alwaysSetStore{}, rdb).Publish(ctx)

	var missing []string
	for key, file := range mirrored {
		if n, _ := rdb.Exists(ctx, key).Result(); n == 0 {
			missing = append(missing, key+" (written by "+file+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("mirrored into Redis on save but never re-asserted, so lost for good on a Redis restart:\n  %s",
			strings.Join(missing, "\n  "))
	}
}
