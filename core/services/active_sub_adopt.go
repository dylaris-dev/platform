package services

// Following the node's .active_server when Core names another sub-server.

import (
	"context"
	"log"
	"time"

	"dylaris-pkg/validate"
)

// Core writes active_sub_server before a switch, setup or reinstall goes out,
// and each undo path puts it back from a record Core kept. Interleavings of
// those records kept leaving Core on a sub-server the node never switched to
// (R34-04, R35-01, R35-03), with nothing to correct it. The node's
// .active_server is what runs, so once the two disagree with no command in
// flight, Core follows the node.

// activeSubSettle is how long a disagreement must hold, with no switch_undo or
// setup_before record pending, before Core follows the node. The node writes
// .active_server before it deletes its record, and the heartbeat key lives 15
// seconds, so any heartbeat read this long after the records were seen gone
// was taken after they cleared.
const activeSubSettle = 20 * time.Second

type activeSubSighting struct {
	core, node string
	since      time.Time
}

// activeSubStep decides one server's tick: whether to follow the node now, and
// the sighting to keep for the next tick (track false: keep none).
func activeSubStep(core, node string, pending bool, prev activeSubSighting, seen bool, now time.Time) (adopt bool, next activeSubSighting, track bool) {
	// An older node sends no name, and a server with none active in Core is a
	// delete of the active slot the node may not have made yet.
	if node == "" || core == "" || core == node || pending || !validate.IsSubServerName(node) {
		return false, activeSubSighting{}, false
	}
	if seen && prev.core == core && prev.node == node {
		if now.Sub(prev.since) >= activeSubSettle {
			return true, activeSubSighting{}, false
		}
		return false, prev, true
	}
	return false, activeSubSighting{core: core, node: node, since: now}, true
}

// adoptNodeActiveSubServers moves active_sub_server, and the loader columns
// from that sub-server's install record, onto what each node reports it runs.
// Returns true when anything changed.
//
// ponytail: one node and one server list per tick while any node reports the
// field; fold into publishServerStateKeys' list if the tick gets heavy.
func (s *StatusWatcherService) adoptNodeActiveSubServers(ctx context.Context, now time.Time) bool {
	hbs := LoadHeartbeats(ctx, s.redis)
	reported := false
	for _, hb := range hbs {
		if len(hb.ActiveSubServers) > 0 {
			reported = true
			break
		}
	}
	if !reported {
		s.activeSubSeen = nil
		return false
	}
	nodes, err := s.store.ListNodes()
	if err != nil {
		return false
	}
	tokens := make(map[int]string, len(nodes))
	for _, n := range nodes {
		tokens[n.ID] = n.Token
	}
	servers, err := s.store.ListAllServers()
	if err != nil {
		return false
	}
	seen := map[string]activeSubSighting{}
	changed := false
	for _, srv := range servers {
		// Only the node the server is on: a moved server's old copy stays on
		// the source until its cleanup.
		hb := hbs[tokens[srv.NodeID]]
		if hb == nil || srv.Status == "migrating" {
			continue
		}
		node := hb.ActiveSubServers[srv.UUID]
		pending := false
		if node != "" && srv.ActiveSubServer != "" && node != srv.ActiveSubServer {
			n, err := s.redis.Exists(ctx, SwitchUndoKey(srv.UUID), setupBeforeKey(srv.UUID)).Result()
			pending = err != nil || n > 0
		}
		prev, ok := s.activeSubSeen[srv.UUID]
		adopt, next, track := activeSubStep(srv.ActiveSubServer, node, pending, prev, ok, now)
		if track {
			seen[srv.UUID] = next
		}
		if !adopt {
			continue
		}
		// No record: a sub-server Core does not know, or one just deleted
		// whose delete the node has not made yet. Looked at again after
		// another settle.
		in, err := s.store.GetSubServerInstall(srv.ID, node)
		if err != nil || in == nil {
			continue
		}
		mv := RecordedMinecraftVersion(in.InstallerType, in.McVersion, in.BuildVersion, in.Loader)
		ok, err = s.store.AdoptServerActiveSubServer(srv.ID, srv.ActiveSubServer, node, in.InstallerType, mv, in.BuildVersion)
		if err != nil {
			log.Printf("Server %s: could not follow the node onto sub-server %q: %v", srv.UUID, node, err)
			continue
		}
		if ok {
			log.Printf("Server %s: the node runs sub-server %q while Core named %q; following the node", srv.UUID, node, srv.ActiveSubServer)
			changed = true
		}
	}
	s.activeSubSeen = seen
	return changed
}
