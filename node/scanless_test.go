package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The node's Redis login is losing SCAN: it lists every key NAME on the
// platform whatever the ACL's key patterns say, and the names are the secret.
// Each of its three keyspace walks reads an index instead.

// denyScan makes the client answer SCAN the way Valkey answers a login without
// it, which is what every node has once the follow-up release lands.
type denyScan struct{}

func (denyScan) DialHook(next redis.DialHook) redis.DialHook { return next }
func (denyScan) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "scan" {
			err := errors.New("NOPERM User has no permissions to run the 'scan' command")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}
func (denyScan) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// Once the node may not walk, the index is the answer.
func TestCoresAreFoundFromCoresIndexWithoutAWalk(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rdb.AddHook(denyScan{})
	mr.SAdd(coreIndexKey, "core-a")
	mr.Set("dylaris:core:core-a", `{"id":"core-a"}`)

	keys, err := coreHeartbeatKeys(context.Background(), rdb)
	if err != nil || len(keys) != 1 || keys[0] != "dylaris:core:core-a" {
		t.Fatalf("keys = %v, %v; want the indexed Core", keys, err)
	}
}

// While it still may, index and walk are unioned. An index holding only dead
// ids - a rollback to a Core that does not keep it - and a rolling update that
// has written only the first new Core's id both hid live Cores, and a node that
// sees no Core disconnects from every one it had.
func TestAStaleOrPartialIndexDoesNotHideALiveCore(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.SAdd(coreIndexKey, "core-dead")
	mr.Set("dylaris:core:core-old", `{"id":"core-old"}`)

	keys, err := coreHeartbeatKeys(context.Background(), rdb)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range keys {
		if k == "dylaris:core:core-old" {
			found = true
		}
	}
	if !found {
		t.Fatalf("keys = %v: the live, unindexed Core was hidden", keys)
	}
}

// A Core too old to keep the index: the walk, so upgrading the node first
// does not cut it off from Core.
func TestCoresAreFoundWithoutAnIndexToo(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Set("dylaris:core:core-a", `{"id":"core-a"}`)
	keys, err := coreHeartbeatKeys(context.Background(), rdb)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %v, %v", keys, err)
	}
}

// Without a walk the ledger loads from the hash, and every write keeps it.
func TestPortsLoadFromTheHashAndStayMirrored(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rdb.AddHook(denyScan{})
	mr.HSet("dylaris:node:node-1:ports", "alpha", "25600")

	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	if pm.GetPort("alpha") != 25600 {
		t.Fatalf("loaded alpha=%d, want 25600 from the hash", pm.GetPort("alpha"))
	}

	port, err := pm.AllocatePort("beta")
	if err != nil {
		t.Fatal(err)
	}
	if v := mr.HGet("dylaris:node:node-1:ports", "beta"); v == "" {
		t.Fatalf("allocation of %d not mirrored into the hash", port)
	}
	pm.ReleasePort("beta")
	if v := mr.HGet("dylaris:node:node-1:ports", "beta"); v != "" {
		t.Fatal("release left the hash entry behind")
	}
	if err := pm.SetPort("gamma", 25650); err != nil {
		t.Fatal(err)
	}
	if v := mr.HGet("dylaris:node:node-1:ports", "gamma"); v != "25650" {
		t.Fatalf("SetPort mirrored %q, want 25650", v)
	}
}

// While a walk is possible the per-server keys are the truth: a node rolled
// back to a binary without the hash kept changing them, and loading the hash it
// left behind would hold ports for servers that gave them up.
func TestAStaleHashIsRebuiltFromTheKeys(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.HSet("dylaris:node:node-1:ports", "gone", "25600")
	mr.Set("dylaris:node:node-1:port:alpha", "25601")

	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	if pm.GetPort("gone") != 0 || pm.GetPort("alpha") != 25601 {
		t.Fatalf("gone=%d alpha=%d, want 0 and 25601", pm.GetPort("gone"), pm.GetPort("alpha"))
	}
	if mr.HGet("dylaris:node:node-1:ports", "gone") != "" || mr.HGet("dylaris:node:node-1:ports", "alpha") != "25601" {
		t.Fatal("the hash was not rebuilt from the keys")
	}
}

// A node upgraded with allocations and no hash yet builds it once, while it
// still holds SCAN.
func TestAnUpgradedNodeBuildsThePortHashOnce(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Set("dylaris:node:node-1:port:alpha", "25600")

	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	if pm.GetPort("alpha") != 25600 {
		t.Fatalf("GetPort(alpha) = %d", pm.GetPort("alpha"))
	}
	if v := mr.HGet("dylaris:node:node-1:ports", "alpha"); v != "25600" {
		t.Fatalf("hash not built: %q", v)
	}
}

// The release sweep finds a held server by its directory on this node, with
// the id the panel actually mints, and no keyspace walk.
func TestAResolvedDiskHoldIsReleasedWithoutAWalk(t *testing.T) {
	prevNodeID, prevMgr := nodeID, globalStorageMgr
	t.Cleanup(func() { nodeID, globalStorageMgr = prevNodeID, prevMgr })
	nodeID = "node-1"
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	root := t.TempDir()
	globalStorageMgr = NewStorageManager(root, nil)
	const held = "cccccccc-1111-2222-3333-444444444444_232qs8ryxy3"
	if err := os.MkdirAll(filepath.Join(root, held), 0o755); err != nil {
		t.Fatal(err)
	}
	recordDiskLimit(ctx, rdb, held, 1) // 1 MB, the directory is empty
	mr.Set(diskFullKey(held), "1")

	releaseResolvedDiskHolds(ctx, rdb, NewQuotaSet(nil))
	if mr.Exists(diskFullKey(held)) {
		t.Fatal("a hold whose server is back under its limit was not released")
	}
	if got, _ := mr.Get("dylaris:server:" + held + ":status"); got != "stopped" {
		t.Fatalf("status = %q, want stopped", got)
	}
}

func TestLocalServersAreThePanelsIDs(t *testing.T) {
	prevMgr := globalStorageMgr
	t.Cleanup(func() { globalStorageMgr = prevMgr })
	root := t.TempDir()
	globalStorageMgr = NewStorageManager(root, nil)
	for _, d := range []string{
		"aaaaaaaa-1111-2222-3333-444444444444",
		"cccccccc-1111-2222-3333-444444444444_232qs8ryxy3",
		"not-a-server", "backups",
	} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	got := localServerUUIDs()
	sort.Strings(got)
	want := []string{"aaaaaaaa-1111-2222-3333-444444444444", "cccccccc-1111-2222-3333-444444444444_232qs8ryxy3"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("local servers = %v, want %v", got, want)
	}
}
