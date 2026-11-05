package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/docker/docker/client"
	"github.com/redis/go-redis/v9"
)

// PowerAction("delete") used to release the port only when ContainerRemove
// succeeded, which stranded the ledger entry in the one case that needs it:
// the container was already gone. Nothing else ever reclaims a port
// (AdoptExistingBindings only adds) and the default range is about a hundred
// wide, so every such delete narrowed the node permanently.
//
// The dead daemon address makes ContainerRemove fail the way "No such
// container" would, without needing Docker.
func TestDeleteReleasesThePortEvenWhenTheContainerIsAlreadyGone(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://127.0.0.1:1"),
		client.WithVersion("1.44"),
	)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	dm := &DockerManager{cli: cli, ctx: t.Context()}
	dm.portMgr = NewPortManager(rdb, "node-1", 25600, 25699, seqMode)

	if _, err := dm.portMgr.AllocatePort("alpha"); err != nil {
		t.Fatalf("AllocatePort: %v", err)
	}
	if got := dm.portMgr.GetPort("alpha"); got != 25600 {
		t.Fatalf("GetPort(alpha) = %d, want 25600", got)
	}

	// The removal is expected to fail; the release is not conditional on it.
	if err := dm.PowerAction("alpha", "delete"); err == nil {
		t.Fatal("ContainerRemove unexpectedly succeeded; the test is not exercising the failure path")
	}

	if got := dm.portMgr.GetPort("alpha"); got != 0 {
		t.Errorf("port %d is still held for a deleted server", got)
	}
	if _, err := mr.Get("dylaris:node:node-1:port:alpha"); err == nil {
		t.Error("the Redis ledger entry survived the delete")
	}

	// The freed port must be handed out again, which is the whole point.
	got, err := dm.portMgr.AllocatePort("beta")
	if err != nil {
		t.Fatalf("AllocatePort(beta): %v", err)
	}
	if got != 25600 {
		t.Errorf("beta got %d, want the reclaimed 25600", got)
	}
}

// The migration path deliberately calls ContainerRemove directly rather than
// PowerAction("delete"), because a moved server keeps its port. Pin that, so
// the unconditional release above cannot be reached from there by a later
// "simplification".
func TestMigrateStorageDoesNotGoThroughPowerActionDelete(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	body := string(src)
	i := strings.Index(body, `case "migrate_storage":`)
	j := strings.Index(body, `case "migrate_out":`)
	if i < 0 || j < 0 || j <= i {
		t.Fatal("cannot locate the migrate_storage handler")
	}
	if strings.Contains(body[i:j], `PowerAction(cmd.Config.UUID, "delete")`) {
		t.Error("migrate_storage removes the container via PowerAction(\"delete\"), which now also frees the port a moved server keeps")
	}
}

// A release during a Redis blip removed the server from RAM but left its keys
// in Redis, with no TTL, and nothing ever retried: the next restart loaded the
// deleted server's port again. The release is retried until it lands.
func TestReleasePortRetriesWhenRedisFails(t *testing.T) {
	prev := forgetRetryEvery
	forgetRetryEvery = 10 * time.Millisecond
	t.Cleanup(func() { forgetRetryEvery = prev })

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	pm := NewPortManager(rdb, "node-1", 25600, 25699, seqMode)
	if _, err := pm.AllocatePort("alpha"); err != nil {
		t.Fatal(err)
	}

	mr.SetError("LOADING Redis is loading the dataset in memory")
	pm.ReleasePort("alpha")
	mr.SetError("")

	deadline := time.Now().Add(2 * time.Second)
	for mr.Exists("dylaris:node:node-1:port:alpha") || mr.HGet("dylaris:node:node-1:ports", "alpha") != "" {
		if time.Now().After(deadline) {
			t.Fatal("the deleted server's port entry is still in Redis")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
