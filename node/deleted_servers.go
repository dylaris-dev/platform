package main

import (
	"sync"
	"time"
)

// deletedServers remembers which servers were deleted on this node, so work
// that was already running for one cannot bring it back.
//
// The node runs commands in parallel, eight at a time, and nothing orders a
// delete against a setup, reinstall or move of the same server. A delete that
// arrived during a minutes-long Forge install removed the container and the
// directory; the install then recreated the directory and ended by creating and
// STARTING a container for a server Core no longer had. Nothing removes a
// container without a database row, so it kept its memory and its port.
//
// A UUID is never reused, so a tombstone is final; it is only aged out so the
// map stays small. Nothing that was already running lasts a day.
var deletedServers = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

const deletedServerMemory = 24 * time.Hour

func markServerDeleted(uuid string) {
	deletedServers.Lock()
	defer deletedServers.Unlock()
	now := time.Now()
	for u, t := range deletedServers.at {
		if now.Sub(t) > deletedServerMemory {
			delete(deletedServers.at, u)
		}
	}
	deletedServers.at[uuid] = now
}

func serverDeleted(uuid string) bool {
	deletedServers.Lock()
	defer deletedServers.Unlock()
	t, ok := deletedServers.at[uuid]
	return ok && time.Since(t) <= deletedServerMemory
}
