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

	"github.com/redis/go-redis/v9"
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

// SwitchUndoTTL outlasts a switch queued for a node that is away or behind:
// the command waits in the stream for as long as that takes, and a refusal
// after the record expired put back only active_sub_server. A node deletes the
// record once the switch lands (node/main.go), so this only bounds a command
// that is never delivered.
const SwitchUndoTTL = 24 * time.Hour

// SwitchUndo is the state a switch to Target is about to overwrite.
type SwitchUndo struct {
	Target           string `json:"target"`
	Previous         string `json:"previous"`
	InstallerType    string `json:"installerType"`
	MinecraftVersion string `json:"minecraftVersion"`
	BuildNumber      string `json:"buildNumber"`
	// The run intent before IntendOnline; empty from an older Core.
	DesiredState string `json:"desiredState,omitempty"`
	// Set by DeleteSubServer: Previous is gone, so it is no place to go back to.
	PreviousDeleted bool `json:"previousDeleted,omitempty"`
	// The dispatch time, unix nanoseconds.
	At int64 `json:"at,omitempty"`
}

// ForgetSwitchUndo deletes the undo record only when it is the switch to
// target's: the key is overwritten per switch, and a newer one's record is
// what its own refusal is put back from.
func ForgetSwitchUndo(ctx context.Context, rdb *redis.Client, serverUUID, target string) {
	if rdb == nil {
		return
	}
	key := SwitchUndoKey(serverUUID)
	err := rdb.Watch(ctx, func(tx *redis.Tx) error {
		var undo SwitchUndo
		raw, err := tx.Get(ctx, key).Result()
		if err != nil || json.Unmarshal([]byte(raw), &undo) != nil || !strings.EqualFold(undo.Target, target) {
			return nil
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.Del(ctx, key)
			return nil
		})
		return err
	}, key)
	if err != nil {
		log.Printf("Server %s: could not forget the undo record of the switch to %q: %v", serverUUID, target, err)
	}
}

// MarkSwitchPreviousDeleted records that the sub-server a pending switch would
// fall back to was deleted. The delete is allowed because Core already names
// the target, and the node's report still names the deleted one.
func MarkSwitchPreviousDeleted(ctx context.Context, rdb *redis.Client, serverUUID, name string) {
	if rdb == nil {
		return
	}
	raw, err := rdb.Get(ctx, SwitchUndoKey(serverUUID)).Result()
	if err != nil {
		return
	}
	var undo SwitchUndo
	if json.Unmarshal([]byte(raw), &undo) != nil || !strings.EqualFold(undo.Previous, name) {
		return
	}
	undo.PreviousDeleted = true
	if rec, merr := json.Marshal(undo); merr == nil {
		rdb.SetArgs(ctx, SwitchUndoKey(serverUUID), rec, redis.SetArgs{KeepTTL: true})
	}
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
			// Each report dropped below takes its own switch's undo record
			// with it, so a later refusal cannot be put back from it.
			srv, serr := s.store.GetServerByUUID(parts[2])
			if serr != nil || srv == nil {
				ForgetSwitchUndo(ctx, s.redis, parts[2], rep.Target)
				continue
			}
			// A switch, setup or delete since then moved Core on; this report
			// is not about the active sub-server any more.
			if !strings.EqualFold(rep.Target, srv.ActiveSubServer) {
				ForgetSwitchUndo(ctx, s.redis, srv.UUID, rep.Target)
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
			// The undo record of this switch naming no previous is a server
			// whose active sub-server had been deleted: none was active.
			noneBefore := previous == "" && undo.Target != ""
			if !noneBefore && !validate.IsSubServerName(previous) {
				log.Printf("Server %s: the switch to %q failed on the node (%s) and the sub-server it runs is unknown", srv.UUID, rep.Target, rep.Reason)
				ForgetSwitchUndo(ctx, s.redis, srv.UUID, rep.Target)
				continue
			}
			matched := undo.Target != "" && strings.EqualFold(undo.Previous, previous)
			if noneBefore || (matched && undo.PreviousDeleted) {
				// What DeleteSubServer does for a deleted active slot.
				log.Printf("Server %s: the switch to sub-server %q failed on the node (%s) and no sub-server is active (%q was deleted)", srv.UUID, rep.Target, rep.Reason, previous)
				s.store.UpdateServerStatus(srv.ID, "pending_setup")
				if err := s.store.UpdateServerActiveSubServer(srv.ID, ""); err != nil {
					log.Printf("Server %s: could not clear the active sub-server: %v", srv.UUID, err)
				}
				s.store.UpdateServerDesiredState(srv.ID, "stopped")
				PublishDesiredState(ctx, s.redis, srv.UUID, "stopped")
				s.redis.Del(ctx, SwitchUndoKey(srv.UUID))
				body := fmt.Sprintf("Switching %s to the sub-server %s did not happen, and its previous sub-server %s was deleted, so no sub-server is active. Switch to one to run the server again.", srv.Name, rep.Target, previous)
				if noneBefore {
					body = fmt.Sprintf("Switching %s to the sub-server %s did not happen, so no sub-server is active. Switch to one that is set up to run the server again.", srv.Name, rep.Target)
				}
				notifyServerOwner(s.store, srv, "Switch failed", body)
				changed = true
				continue
			}
			log.Printf("Server %s: the switch to sub-server %q failed on the node (%s), back on %q", srv.UUID, rep.Target, rep.Reason, previous)
			if err := s.store.UpdateServerActiveSubServer(srv.ID, previous); err != nil {
				log.Printf("Server %s: could not restore the active sub-server: %v", srv.UUID, err)
				s.redis.Del(ctx, SwitchUndoKey(srv.UUID))
				continue
			}
			if matched {
				if err := s.store.UpdateServerLoaderMetadata(srv.ID, undo.InstallerType, undo.MinecraftVersion, undo.BuildNumber); err != nil {
					log.Printf("Server %s: could not restore the loader metadata: %v", srv.UUID, err)
				}
			}
			// As RestoreSetupBefore: the switch ran nothing new, so the online
			// the dispatch wrote must not have the reconciler start the old
			// sub-server an owner had stopped. A stop since is kept.
			if undo.Target != "" && undo.DesiredState != "" && undo.DesiredState != "online" && srv.DesiredState == "online" {
				if err := s.store.UpdateServerDesiredState(srv.ID, undo.DesiredState); err != nil {
					log.Printf("Server %s: could not put the run intent back: %v", srv.UUID, err)
				} else {
					PublishDesiredState(ctx, s.redis, srv.UUID, undo.DesiredState)
				}
			}
			s.redis.Del(ctx, SwitchUndoKey(srv.UUID))
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
