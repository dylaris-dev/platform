package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Detection for the one topology this node cannot work in: two nodes whose
// STORAGE_PATHS are backed by the same filesystem (a hoster putting five nodes
// on one NAS share).
//
// It is not merely fragile there, it cannot work at all. `.node_secret` and
// `.node_id` both live in the first storage path, so two nodes overwrite each
// other's identity; and the scheduler counts one 10 TB share once per node. The migration guard already refuses the
// data-destroying case (see migration_commands.go), but a refused migration is a
// symptom. This says what is actually wrong, before anything is migrated.
//
// The detection has to survive the very corruption it looks for. Node identity
// itself collides on a shared mount, so two nodes can genuinely believe they are
// the same node - which is why a beacon keyed only on node id would find nothing
// in the worst case. Every beacon therefore carries a PROCESS-LOCAL instance id,
// generated fresh at startup and never persisted. Two running processes cannot
// share one, whatever their configuration says.

// beaconDirName sits inside each storage path. Dot-prefixed like the other node
// state files so it never shows up in a user's file browser.
const beaconDirName = ".dylaris-node"

// beaconWriteInterval is how often each node refreshes its beacons, and
// beaconFreshness is how long one counts as live. The gap between them absorbs a
// slow NFS write without a node briefly declaring its peer dead.
const (
	beaconWriteInterval = 60 * time.Second
	beaconFreshness     = 5 * time.Minute
)

// nodeBeacon is one node's claim on a storage path.
type nodeBeacon struct {
	NodeID string `json:"nodeId"`
	// InstanceID identifies the PROCESS. It is regenerated on every start and
	// never written to persistent config, so it cannot be shared by a mount.
	InstanceID string    `json:"instanceId"`
	Hostname   string    `json:"hostname"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Conflict kinds, ordered by how bad they are.
const (
	// sharedStoragePeer: another node's beacon is on our disk. Storage is shared;
	// the two still have separate identities.
	sharedStoragePeer = "peer"
	// sharedStorageIdentity: OUR beacon is being written by another process.
	// Storage is shared AND node identity has already collided, so the two nodes
	// are overwriting each other's secret and id.
	sharedStorageIdentity = "identity"
)

// sharedStorageConflict is one detected overlap, shaped for the heartbeat.
type sharedStorageConflict struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	PeerNode string `json:"peerNode,omitempty"`
	PeerHost string `json:"peerHost,omitempty"`
}

// Message renders the conflict for a log line and for the panel.
func (c sharedStorageConflict) Message() string {
	peer := c.PeerHost
	if peer == "" {
		peer = c.PeerNode
	}
	if peer == "" {
		peer = "another node"
	}
	if c.Kind == sharedStorageIdentity {
		return fmt.Sprintf(
			"%s is shared with %s AND node identity has collided there - both nodes are overwriting the same .node_secret/.node_id",
			c.Path, peer)
	}
	return fmt.Sprintf("%s is shared with node %s - a storage path must not be mounted into two nodes", c.Path, peer)
}

// newInstanceID returns a random per-process identifier. Falls back to a
// timestamp only if the system RNG fails, which keeps detection working rather
// than aborting the node over a diagnostic.
func newInstanceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// evaluateBeacons decides which of the beacons found on one path are conflicts.
// Pure, so the rules can be tested without a filesystem.
//
// firstRound suppresses the identity check on the very first pass: after a
// restart our own beacon still carries the PREVIOUS process's instance id, which
// is indistinguishable from a peer writing it. From the second pass on, our own
// write has landed, so a foreign instance id in our own beacon can only be
// another process.
func evaluateBeacons(path, ownNodeID, ownInstanceID string, found []nodeBeacon, now time.Time, firstRound bool) []sharedStorageConflict {
	var out []sharedStorageConflict
	for _, b := range found {
		if now.Sub(b.UpdatedAt) > beaconFreshness {
			continue // a decommissioned node's leftover, not a live peer
		}
		switch {
		case b.NodeID != ownNodeID:
			out = append(out, sharedStorageConflict{
				Path: path, Kind: sharedStoragePeer, PeerNode: b.NodeID, PeerHost: b.Hostname,
			})
		case b.InstanceID != ownInstanceID && !firstRound:
			// Same node id, different process: identity has already collided.
			out = append(out, sharedStorageConflict{
				Path: path, Kind: sharedStorageIdentity, PeerNode: b.NodeID, PeerHost: b.Hostname,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].PeerNode < out[j].PeerNode
	})
	return out
}

// readBeacons loads every beacon file in one storage path. Unreadable or
// malformed entries are skipped: a diagnostic must never take down the node.
func readBeacons(path string) []nodeBeacon {
	dir := filepath.Join(path, beaconDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []nodeBeacon
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".beacon") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var b nodeBeacon
		if json.Unmarshal(raw, &b) != nil || b.NodeID == "" {
			continue
		}
		out = append(out, b)
	}
	return out
}

// writeBeacon refreshes this node's claim on one path.
func writeBeacon(path string, b nodeBeacon) error {
	dir := filepath.Join(path, beaconDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, b.NodeID+".beacon"), raw, 0644)
}

// sharedStorageDetector holds the state the check needs across rounds.
type sharedStorageDetector struct {
	nodeID     string
	instanceID string
	hostname   string
	paths      func() []string

	// mu guards the fields Check writes. Check runs on its own ticker while
	// the heartbeat calls Conflicts every 5 seconds, so the two genuinely
	// race - a torn slice header would hand the heartbeat a bogus length.
	mu         sync.Mutex
	firstRound bool
	conflicts  []sharedStorageConflict
}

func newSharedStorageDetector(nodeID string, paths func() []string) *sharedStorageDetector {
	host, _ := os.Hostname()
	return &sharedStorageDetector{
		nodeID:     nodeID,
		instanceID: newInstanceID(),
		hostname:   host,
		paths:      paths,
		firstRound: true,
	}
}

// Check reads the beacons on every storage path, records what it finds, then
// refreshes this node's own beacons. Read-before-write is what makes the
// identity case detectable: our own file still holds whatever the other process
// last wrote.
func (d *sharedStorageDetector) Check() []sharedStorageConflict {
	if d == nil || d.paths == nil {
		return nil
	}
	var found []sharedStorageConflict
	now := time.Now().UTC()

	// Snapshot the round flag and publish the result under the lock, but do the
	// disk work outside it: writeBeacon can block for minutes on a hung NFS
	// mount, and holding the mutex across that would stall the heartbeat - the
	// very thing that has to keep reporting when storage misbehaves.
	d.mu.Lock()
	firstRound := d.firstRound
	d.mu.Unlock()

	for _, path := range d.paths() {
		if strings.TrimSpace(path) == "" {
			continue
		}
		found = append(found, evaluateBeacons(
			path, d.nodeID, d.instanceID, readBeacons(path), now, firstRound,
		)...)

		if err := writeBeacon(path, nodeBeacon{
			NodeID:     d.nodeID,
			InstanceID: d.instanceID,
			Hostname:   d.hostname,
			UpdatedAt:  now,
		}); err != nil {
			log.Printf("shared-storage check: could not write beacon in %s: %v", path, err)
		}
	}

	d.mu.Lock()
	d.firstRound = false
	d.conflicts = found
	d.mu.Unlock()
	return found
}

// Conflicts returns the last result, for the heartbeat. Called from the
// discovery goroutine while Check runs on its own ticker, hence the lock.
func (d *sharedStorageDetector) Conflicts() []sharedStorageConflict {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conflicts
}

// Run starts the periodic check and logs every conflict on every round.
// Deliberately not rate-limited: this is a misconfiguration that silently
// destroys servers on the next migration, and it stays fixed only by being
// impossible to overlook.
func (d *sharedStorageDetector) Run(stop <-chan struct{}) {
	if d == nil {
		return
	}
	report := func() {
		for _, c := range d.Check() {
			log.Printf("SHARED STORAGE DETECTED: %s", c.Message())
		}
	}
	report()
	go func() {
		ticker := time.NewTicker(beaconWriteInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				report()
			}
		}
	}()
}
