package storagereach

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"dylaris-core/storage"
)

// probeDir is the directory every round writes under. It is a dotted name so
// it sorts away from real content and is obvious as machine-owned when an
// operator looks at the bucket or the mount.
const probeDir = ".dylaris-reachability"

// MachineOwnedDirs are the top-level directories this package writes at the
// Core storage root. They are not Core data: a migration of that root skips
// them, because the fleet beacons are rewritten under it every few minutes.
func MachineOwnedDirs() []string { return []string{FleetDir, probeDir} }

// ackSuffix names a participant's ack directory. Peers write a marker into it
// to prove they may write into that participant's namespace, which is the only
// way to catch an NFS uid-squash that leaves reads working.
const ackSuffix = ".acks"

// ProbeRoot is where one round's files live. Exported so the coordinator can
// clean up without duplicating the layout.
func ProbeRoot(roundID string) string {
	return path.Join(probeDir, roundID)
}

// ProbeOptions configures one participant's run.
type ProbeOptions struct {
	CoreID      string
	RoundID     string
	Fingerprint string
	// Participants is every Core expected in this round, including CoreID
	// itself. A Core is never its own peer.
	Participants []string
	// Deadline bounds the whole read-all/cross-write loop.
	Deadline time.Duration
	// RetryEvery is the interval between list attempts.
	RetryEvery time.Duration
	// Now is injectable for tests; nil means time.Now.
	Now func() time.Time
}

// Probe runs one participant's side of a round and returns what IT observed.
// It never decides a verdict - Aggregate does that - because a participant can
// only honestly report its own capabilities.
//
// The single write-own phase followed by a retrying read-and-cross-write loop
// is race-free by construction: every path is unique to its writer, so no two
// Cores ever write the same object. Cross-writing only to peers already SEEN
// is what removes the need for a barrier; seeing a peer's beacon proves that
// peer finished its own write-own phase and its ack directory exists.
func Probe(ctx context.Context, prov storage.StorageProvider, opts ProbeOptions) Report {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.RetryEvery <= 0 {
		opts.RetryEvery = time.Second
	}

	root := ProbeRoot(opts.RoundID)
	rep := Report{
		CoreID:      opts.CoreID,
		Fingerprint: opts.Fingerprint,
		At:          now().Unix(),
		// Non-nil so the JSON is [] rather than null; the panel renders these
		// lists directly.
		SeenPeers:        []string{},
		MismatchedPeers:  []string{},
		CrossWroteTo:     []string{},
		CrossWriteDenied: []string{},
	}

	hostname, _ := os.Hostname()
	beacon := BeaconPayload{
		CoreID:      opts.CoreID,
		Hostname:    hostname,
		Token:       opts.RoundID,
		Fingerprint: opts.Fingerprint,
		TS:          rep.At,
	}
	body, err := json.Marshal(beacon)
	if err != nil {
		rep.WriteErr = err.Error()
		return rep
	}

	// Write-own. The ack directory is created next to the beacon so a peer
	// that can see the beacon can immediately write its marker.
	if err := prov.WriteFile(ctx, path.Join(root, opts.CoreID), bytes.NewReader(body)); err != nil {
		rep.WriteErr = err.Error()
	} else {
		rep.Wrote = true
		rep.Reachable = true
		// A failure here is not fatal: S3 has no real directories, and a
		// backend that took the beacon will take the marker too.
		_ = prov.CreateDir(ctx, path.Join(root, opts.CoreID+ackSuffix))
	}

	peers := make(map[string]bool, len(opts.Participants))
	for _, p := range opts.Participants {
		if p != opts.CoreID {
			peers[p] = false // false = not yet seen
		}
	}

	deadline := now().Add(opts.Deadline)
	for {
		names, listErr := listNames(ctx, prov, root)
		if listErr == nil {
			// Listing working at all means the backend is reachable even if
			// the write was denied - that distinction is write-denied vs
			// unreachable, and it decides what the operator has to fix.
			rep.Reachable = true
			for _, name := range names {
				if seen, expected := peers[name]; !expected || seen {
					continue
				}
				ok, mismatch := readBeacon(ctx, prov, path.Join(root, name), opts.RoundID, opts.Fingerprint)
				if mismatch {
					rep.MismatchedPeers = appendOnce(rep.MismatchedPeers, name)
					// Mark as handled: re-reading a stale-config peer every
					// tick would add nothing and cost a request per second.
					peers[name] = true
					continue
				}
				if !ok {
					continue
				}
				peers[name] = true
				rep.SeenPeers = appendOnce(rep.SeenPeers, name)
				crossWrite(ctx, prov, root, name, opts.CoreID, body, &rep)
			}
		}

		if allSeen(peers) || !now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return finishReport(rep)
		case <-time.After(opts.RetryEvery):
		}
	}
	return finishReport(rep)
}

// CleanupRound removes a round's probe files. Best-effort by contract: an
// orphaned root costs a few bytes and is ignored by later rounds, which key
// off the round id in every beacon.
func CleanupRound(ctx context.Context, prov storage.StorageProvider, roundID string) error {
	return prov.DeletePath(ctx, ProbeRoot(roundID))
}

// crossWrite writes this Core's marker into a peer's ack namespace and records
// the outcome. The write error IS the evidence of cross-write-denied; no
// confirmation from the peer is needed, because the peer's visibility was
// already proven by reading its beacon.
func crossWrite(ctx context.Context, prov storage.StorageProvider, root, peer, coreID string, body []byte, rep *Report) {
	target := path.Join(root, peer+ackSuffix, coreID)
	if err := prov.WriteFile(ctx, target, bytes.NewReader(body)); err != nil {
		rep.CrossWriteDenied = appendOnce(rep.CrossWriteDenied, peer)
		return
	}
	rep.CrossWroteTo = appendOnce(rep.CrossWroteTo, peer)
}

// readBeacon reports whether the file at p is a beacon for this round with the
// expected backend fingerprint. The second return distinguishes "readable but
// pointed elsewhere" (a stale-config peer) from "not readable / not this
// round", because those need different fixes.
func readBeacon(ctx context.Context, prov storage.StorageProvider, p, roundID, wantFingerprint string) (ok bool, mismatch bool) {
	rc, err := prov.GetFile(ctx, p)
	if err != nil {
		return false, false
	}
	defer rc.Close()
	data, err := readLimited(rc)
	if err != nil {
		return false, false
	}
	var b BeaconPayload
	if err := json.Unmarshal(data, &b); err != nil {
		return false, false
	}
	if b.Token != roundID {
		return false, false
	}
	if b.Fingerprint != wantFingerprint {
		return false, true
	}
	return true, false
}

// readLimited bounds a beacon read: a beacon is a few hundred bytes, and this
// reads a path any participant can write to. Stated once here so the probe's
// round beacons and the fleet beacon share the same bound.
func readLimited(rc io.ReadCloser) ([]byte, error) {
	return io.ReadAll(io.LimitReader(rc, 4096))
}

// listNames returns the immediate child names under root, with ack
// directories filtered out so they are never mistaken for a participant.
func listNames(ctx context.Context, prov storage.StorageProvider, root string) ([]string, error) {
	infos, err := prov.ListFiles(ctx, root)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(infos))
	for _, fi := range infos {
		if strings.HasSuffix(fi.Name, ackSuffix) {
			continue
		}
		out = append(out, fi.Name)
	}
	return out, nil
}

func allSeen(peers map[string]bool) bool {
	for _, seen := range peers {
		if !seen {
			return false
		}
	}
	return true
}

func appendOnce(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// finishReport sorts every list so two runs with the same outcome produce
// byte-identical JSON, which keeps the panel from re-rendering on noise.
func finishReport(rep Report) Report {
	sort.Strings(rep.SeenPeers)
	sort.Strings(rep.MismatchedPeers)
	sort.Strings(rep.CrossWroteTo)
	sort.Strings(rep.CrossWriteDenied)
	return rep
}
