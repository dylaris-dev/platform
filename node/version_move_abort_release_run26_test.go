package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
)

// writeLogHook records every SET and DEL in the order the client sent them.
type writeLogHook struct {
	mu  sync.Mutex
	log []string
}

func (h *writeLogHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *writeLogHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *writeLogHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if name := strings.ToLower(cmd.Name()); name == "set" || name == "del" {
			args := cmd.Args()
			entry := name + " " + fmt.Sprint(args[1])
			if name == "set" {
				entry += " " + fmt.Sprint(args[2])
			}
			h.mu.Lock()
			h.log = append(h.log, entry)
			h.mu.Unlock()
		}
		return next(ctx, cmd)
	}
}

// R26-02: a move refused after it took its busy hold wrote its status beside
// the hold's running refresher and released the hold only on return, so a
// refresh in between put "installing" back for good. The abort's status now
// goes through the release: by the time Core is told the move was abandoned,
// the hold is gone and the last status written is the server's real one.
func TestAVersionMoveAbortAfterTheHoldReportsThroughTheRelease(t *testing.T) {
	root := t.TempDir()
	const uuid = "26262626-0202-0202-0202-262626262626"
	writeTestFile(t, filepath.Join(root, uuid, "main", "server.jar"), "purpur")
	dm, _ := runningDocker(t, uuid, http.StatusNoContent)
	rdb := newMiniRedis(t)
	hook := &writeLogHook{}
	rdb.AddHook(hook)
	payload, _ := json.Marshal(map[string]interface{}{"config": map[string]interface{}{
		"uuid": uuid, "activeSubServer": "main", "targetDir": "plugins",
	}})
	// purpur is refused after the hold: no installer source.
	runUpdateServerVersion(context.Background(), rdb, dm, &StorageManager{paths: []string{root}},
		NodeCommand{Installer: InstallerConfig{Type: "purpur"}}, string(payload))

	hook.mu.Lock()
	defer hook.mu.Unlock()
	statusKey := "dylaris:server:" + uuid + ":status"
	abandoned, released, lastStatus := -1, -1, ""
	for i, e := range hook.log {
		switch {
		case strings.HasPrefix(e, "set dylaris:server:"+uuid+":version_update_failed "):
			abandoned = i
		case e == "del "+nodeBusyKey(uuid) && released < 0:
			released = i
		case strings.HasPrefix(e, "set "+statusKey+" ") && abandoned < 0:
			lastStatus = strings.TrimPrefix(e, "set "+statusKey+" ")
		}
	}
	if abandoned < 0 {
		t.Fatalf("the move was not abandoned; writes: %v", hook.log)
	}
	if released < 0 || released > abandoned {
		t.Errorf("the busy hold was still up when the abort was reported (release at %d, abandon at %d): its refresher could put installing back", released, abandoned)
	}
	if lastStatus != "online" {
		t.Errorf("last status before the abandon = %q, want online", lastStatus)
	}
}
