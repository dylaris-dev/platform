package services

// Removing what Core wrote for a sub-server copy the node could not make.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-pkg/validate"
)

// CopySubServer writes the target's mod rows, install record and modpack
// snapshot when it queues the copy, because the node reports nothing on
// success. A copy that then failed on the node - most often the server's disk
// limit, since a copy doubles the sub-server - left those rows for a directory
// that does not exist: the panel listed a sub-server with mods, a switch to it
// was accepted, and the operator had only ever been told "Copy queued".
//
// The node writes this key on such a failure (node/copy_sub_server.go). Same
// per-server prefix the node's Redis ACL already grants, like
// version_update_failed; an older node never writes it, which is the old
// behaviour.
func CopySubFailedKey(serverUUID string) string {
	return "dylaris:server:" + serverUUID + ":copy_sub_failed"
}

const copySubFailedPattern = "dylaris:server:*:copy_sub_failed"

// CopyPendingKey holds the target of a copy Core has queued and not yet seen
// land. The node takes node_busy only once it STARTS the copy, so a copy still
// waiting in the stream left nothing a version move could see, and the two ran
// on one tree at once. Core writes it and Core clears it: when a move finds
// the copy in place, or when the node reports the copy failed. An older node
// needs nothing new for that; the TTL is the backstop.
func CopyPendingKey(serverUUID string) string {
	return "dylaris:server:" + serverUUID + ":copy_pending"
}

// CopyPendingTTL outlasts any copy that is actually going to happen.
const CopyPendingTTL = time.Hour

type copySubFailure struct {
	Target string `json:"target"`
	Reason string `json:"reason"`
}

// consumeCopySubFailures removes the rows of each failed copy's target and
// tells the owner why. Returns true when anything panel-visible changed.
func (s *StatusWatcherService) consumeCopySubFailures(ctx context.Context) bool {
	var cursor uint64
	changed := false
	for {
		keys, next, err := s.redis.Scan(ctx, cursor, copySubFailedPattern, 100).Result()
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
			// No longer pending: a move asked to follow it now hears it failed.
			s.redis.Del(ctx, CopyPendingKey(parts[2]))
			var rep copySubFailure
			if json.Unmarshal([]byte(raw), &rep) != nil || !validate.IsSubServerName(rep.Target) {
				continue
			}
			srv, serr := s.store.GetServerByUUID(parts[2])
			if serr != nil || srv == nil {
				continue
			}
			// Never the sub-server that runs: a copy's target is a new name, and
			// a report naming the active one is not about a copy.
			if strings.EqualFold(rep.Target, srv.ActiveSubServer) {
				log.Printf("Server %s: ignoring a copy failure report that names the active sub-server %q", srv.UUID, rep.Target)
				continue
			}
			log.Printf("Server %s: the copy to sub-server %q failed on the node (%s), removing its rows", srv.UUID, rep.Target, rep.Reason)
			if err := s.store.ReplaceServerMods(srv.ID, rep.Target, nil); err != nil {
				log.Printf("Server %s: could not remove the mod rows of %q: %v", srv.UUID, rep.Target, err)
			}
			if err := s.store.DeleteSubServerInstall(srv.ID, rep.Target); err != nil {
				log.Printf("Server %s: could not remove the install record of %q: %v", srv.UUID, rep.Target, err)
			}
			if err := s.store.ReplaceServerModpackContents(srv.ID, rep.Target, nil); err != nil {
				log.Printf("Server %s: could not remove the modpack snapshot of %q: %v", srv.UUID, rep.Target, err)
			}
			reason := rep.Reason
			if reason == "" {
				reason = "the node could not copy the files"
			}
			notifyServerOwner(s.store, srv, "Copy failed",
				fmt.Sprintf("Copying %s to the sub-server %s did not finish: %s. Nothing was created.", srv.Name, rep.Target, reason))
			changed = true
		}
		cursor = next
		if cursor == 0 {
			return changed
		}
	}
}

// notifyServerOwner puts a node-side failure in front of the server's owner.
func notifyServerOwner(st installFailureStore, srv *models.Server, title, body string) {
	if srv.OwnerID == "" {
		return
	}
	if _, err := st.InsertNotification(&models.Notification{
		UserID: srv.OwnerID,
		Type:   "server.install_failed",
		Title:  title,
		Body:   body,
		Link:   fmt.Sprintf("/servers/%d", srv.ID),
	}); err != nil {
		log.Printf("could not record a notification for server %q: %v", srv.UUID, err)
	}
}
