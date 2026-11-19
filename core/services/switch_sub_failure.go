package services

// Putting Core back on the running sub-server when the node refused a switch.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"dylaris-pkg/validate"
)

// SwitchSubServer writes active_sub_server (and the target's loader columns)
// before it dispatches, and the node used to only log a switch it could not
// make - a target directory with no runnable server in it, most often. Core
// then named a sub-server the node was not running, and the console, mod
// installs and the Content tab all went to the wrong directory.
//
// The node writes SwitchSubFailedKey on such a failure (node/main.go
// reportSwitchFailed), under the per-server prefix its Redis ACL already
// grants; an older node never writes it, which is the old behaviour.
func SwitchSubFailedKey(serverUUID string) string {
	return "dylaris:server:" + serverUUID + ":switch_failed"
}

const switchSubFailedPattern = "dylaris:server:*:switch_failed"

// SwitchUndoKey holds the columns a switch overwrote, written by Core at
// dispatch. The node only knows the previous sub-server's name.
func SwitchUndoKey(serverUUID string) string {
	return "dylaris:server:" + serverUUID + ":switch_undo"
}

// SwitchUndoTTL outlasts the node's report.
const SwitchUndoTTL = 10 * time.Minute

// SwitchUndo is the state a switch to Target is about to overwrite.
type SwitchUndo struct {
	Target           string `json:"target"`
	Previous         string `json:"previous"`
	InstallerType    string `json:"installerType"`
	MinecraftVersion string `json:"minecraftVersion"`
	BuildNumber      string `json:"buildNumber"`
}

type switchSubFailure struct {
	Target   string `json:"target"`
	Previous string `json:"previous"`
	Reason   string `json:"reason"`
}

// consumeSwitchSubFailures puts each failed switch back and tells the owner.
// Returns true when anything panel-visible changed.
func (s *StatusWatcherService) consumeSwitchSubFailures(ctx context.Context) bool {
	var cursor uint64
	changed := false
	for {
		keys, next, err := s.redis.Scan(ctx, cursor, switchSubFailedPattern, 100).Result()
		if err != nil {
			return changed
		}
		for _, key := range keys {
			parts := strings.Split(key, ":")
			if len(parts) != 4 {
				continue
			}
			raw, rerr := s.redis.Get(ctx, key).Result()
			if rerr != nil {
				continue
			}
			s.redis.Del(ctx, key)
			var rep switchSubFailure
			if json.Unmarshal([]byte(raw), &rep) != nil || !validate.IsSubServerName(rep.Target) {
				continue
			}
			srv, serr := s.store.GetServerByUUID(parts[2])
			if serr != nil || srv == nil {
				continue
			}
			// A switch since then moved Core on; this report is not about the
			// active sub-server any more, and its undo record is that switch's.
			if !strings.EqualFold(rep.Target, srv.ActiveSubServer) {
				continue
			}
			var undo SwitchUndo
			if u, uerr := s.redis.Get(ctx, SwitchUndoKey(srv.UUID)).Result(); uerr == nil {
				if json.Unmarshal([]byte(u), &undo) != nil || !strings.EqualFold(undo.Target, rep.Target) {
					undo = SwitchUndo{}
				}
			}
			// The node's .active_server is what still runs; Core's record of
			// the same name only adds the loader columns.
			previous := rep.Previous
			if previous == "" {
				previous = undo.Previous
			}
			if !validate.IsSubServerName(previous) {
				log.Printf("Server %s: the switch to %q failed on the node (%s) and the sub-server it runs is unknown", srv.UUID, rep.Target, rep.Reason)
				continue
			}
			s.redis.Del(ctx, SwitchUndoKey(srv.UUID))
			log.Printf("Server %s: the switch to sub-server %q failed on the node (%s), back on %q", srv.UUID, rep.Target, rep.Reason, previous)
			if err := s.store.UpdateServerActiveSubServer(srv.ID, previous); err != nil {
				log.Printf("Server %s: could not restore the active sub-server: %v", srv.UUID, err)
				continue
			}
			if undo.Target != "" && strings.EqualFold(undo.Previous, previous) {
				if err := s.store.UpdateServerLoaderMetadata(srv.ID, undo.InstallerType, undo.MinecraftVersion, undo.BuildNumber); err != nil {
					log.Printf("Server %s: could not restore the loader metadata: %v", srv.UUID, err)
				}
			}
			reason := rep.Reason
			if reason == "" {
				reason = "the node could not start it"
			}
			notifyServerOwner(s.store, srv, "Switch failed",
				fmt.Sprintf("Switching %s to the sub-server %s did not happen: %s. It is still on %s.", srv.Name, rep.Target, reason, previous))
			changed = true
		}
		cursor = next
		if cursor == 0 {
			return changed
		}
	}
}
