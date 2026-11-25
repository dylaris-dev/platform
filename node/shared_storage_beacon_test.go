package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "dylaris-proto/node"
)

func TestEvaluateBeacons(t *testing.T) {
	now := time.Now().UTC()
	fresh := now.Add(-30 * time.Second)
	stale := now.Add(-beaconFreshness - time.Minute)

	tests := []struct {
		name       string
		found      []nodeBeacon
		firstRound bool
		wantKinds  []string
	}{
		{
			name:      "our own beacon alone is not a conflict",
			found:     []nodeBeacon{{NodeID: "node-a", InstanceID: "inst-1", UpdatedAt: fresh}},
			wantKinds: nil,
		},
		{
			// Storage is shared, identities still separate.
			name: "a live peer beacon is a peer conflict",
			found: []nodeBeacon{
				{NodeID: "node-a", InstanceID: "inst-1", UpdatedAt: fresh},
				{NodeID: "node-b", InstanceID: "inst-2", Hostname: "worker-2", UpdatedAt: fresh},
			},
			wantKinds: []string{sharedStoragePeer},
		},
		{
			// The worst case: same node id, different process. Both nodes are
			// overwriting one .node_secret and one .node_id.
			name:      "our own beacon written by another process is an identity conflict",
			found:     []nodeBeacon{{NodeID: "node-a", InstanceID: "someone-else", UpdatedAt: fresh}},
			wantKinds: []string{sharedStorageIdentity},
		},
		{
			// After a restart our own beacon still carries the previous process's
			// instance id, which is indistinguishable from a peer writing it.
			name:       "a foreign instance id is ignored on the first round",
			found:      []nodeBeacon{{NodeID: "node-a", InstanceID: "previous-boot", UpdatedAt: fresh}},
			firstRound: true,
			wantKinds:  nil,
		},
		{
			// A peer conflict is real on the first round: a different node id can
			// never be our own previous boot.
			name:       "a peer is still detected on the first round",
			found:      []nodeBeacon{{NodeID: "node-b", InstanceID: "inst-2", UpdatedAt: fresh}},
			firstRound: true,
			wantKinds:  []string{sharedStoragePeer},
		},
		{
			// A decommissioned node's leftover file must not warn forever.
			name:      "a stale peer beacon is ignored",
			found:     []nodeBeacon{{NodeID: "node-b", InstanceID: "inst-2", UpdatedAt: stale}},
			wantKinds: nil,
		},
		{
			name:      "a stale identity beacon is ignored",
			found:     []nodeBeacon{{NodeID: "node-a", InstanceID: "old-process", UpdatedAt: stale}},
			wantKinds: nil,
		},
		{
			name: "several peers are all reported",
			found: []nodeBeacon{
				{NodeID: "node-b", InstanceID: "i2", UpdatedAt: fresh},
				{NodeID: "node-c", InstanceID: "i3", UpdatedAt: fresh},
			},
			wantKinds: []string{sharedStoragePeer, sharedStoragePeer},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateBeacons("/storage", "node-a", "inst-1", tt.found, now, tt.firstRound)
			if len(got) != len(tt.wantKinds) {
				t.Fatalf("evaluateBeacons = %+v, want %d conflicts", got, len(tt.wantKinds))
			}
			for i, kind := range tt.wantKinds {
				if got[i].Kind != kind {
					t.Errorf("[%d] kind = %q, want %q", i, got[i].Kind, kind)
				}
				if got[i].Path != "/storage" {
					t.Errorf("[%d] path = %q", i, got[i].Path)
				}
			}
		})
	}
}

func TestConflictMessage(t *testing.T) {
	peer := sharedStorageConflict{Path: "/storage", Kind: sharedStoragePeer, PeerNode: "node-b", PeerHost: "worker-2"}
	if msg := peer.Message(); msg == "" {
		t.Fatal("peer conflict has no message")
	}
	identity := sharedStorageConflict{Path: "/storage", Kind: sharedStorageIdentity, PeerNode: "node-a"}
	// The identity case is strictly worse and must not read like the peer case,
	// or an operator will not understand why their node ids are unstable.
	if peer.Message() == identity.Message() {
		t.Error("identity and peer conflicts render identically")
	}
	if !strings.Contains(identity.Message(), "identity") {
		t.Errorf("identity message does not mention identity: %q", identity.Message())
	}
}

func TestBeaconRoundTrip(t *testing.T) {
	dir := t.TempDir()
	b := nodeBeacon{NodeID: "node-a", InstanceID: "inst-1", Hostname: "worker-1", UpdatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := writeBeacon(dir, b); err != nil {
		t.Fatalf("writeBeacon: %v", err)
	}
	got := readBeacons(dir)
	if len(got) != 1 {
		t.Fatalf("readBeacons = %+v, want one", got)
	}
	if got[0].NodeID != b.NodeID || got[0].InstanceID != b.InstanceID || got[0].Hostname != b.Hostname {
		t.Errorf("round trip lost data: %+v", got[0])
	}
}

func TestReadBeacons_MissingDirIsEmpty(t *testing.T) {
	if got := readBeacons(filepath.Join(t.TempDir(), "nope")); got != nil {
		t.Errorf("readBeacons on a missing path = %+v, want nil", got)
	}
}

// A diagnostic must never take the node down, so unreadable entries are skipped
// rather than propagated.
func TestReadBeacons_SkipsMalformed(t *testing.T) {
	dir := t.TempDir()
	beaconDir := filepath.Join(dir, beaconDirName)
	if err := os.MkdirAll(beaconDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beaconDir, "broken.beacon"), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	// A file with no node id carries no claim and must be ignored too.
	if err := os.WriteFile(filepath.Join(beaconDir, "empty.beacon"), []byte(`{"instanceId":"x"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beaconDir, "notes.txt"), []byte("ignored"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeBeacon(dir, nodeBeacon{NodeID: "node-a", InstanceID: "i1", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	got := readBeacons(dir)
	if len(got) != 1 || got[0].NodeID != "node-a" {
		t.Fatalf("readBeacons = %+v, want only the valid beacon", got)
	}
}

// The end-to-end case this exists for: two node processes writing into one
// directory. The second must see the first.
func TestDetector_FindsPeerOnSharedPath(t *testing.T) {
	shared := t.TempDir()
	paths := func() []string { return []string{shared} }

	nodeA := newSharedStorageDetector("node-a", paths)
	if got := nodeA.Check(); len(got) != 0 {
		t.Fatalf("first node found conflicts on an empty path: %+v", got)
	}

	nodeB := newSharedStorageDetector("node-b", paths)
	got := nodeB.Check()
	if len(got) != 1 || got[0].Kind != sharedStoragePeer {
		t.Fatalf("second node conflicts = %+v, want one peer conflict", got)
	}
	if got[0].PeerNode != "node-a" {
		t.Errorf("peer = %q, want node-a", got[0].PeerNode)
	}
	if got[0].Path != shared {
		t.Errorf("path = %q, want %q", got[0].Path, shared)
	}
}

// Identity collision: both processes believe they are the same node, which is
// what actually happens once .node_id is shared. A beacon keyed only on node id
// would find nothing here.
func TestDetector_FindsIdentityCollision(t *testing.T) {
	shared := t.TempDir()
	paths := func() []string { return []string{shared} }

	first := newSharedStorageDetector("node-a", paths)
	first.Check()

	second := newSharedStorageDetector("node-a", paths)
	// First round is suppressed: a restart of the same node looks identical.
	if got := second.Check(); len(got) != 0 {
		t.Fatalf("first round reported %+v, want nothing", got)
	}
	// The other process refreshes its beacon, and now the difference is real.
	first.Check()
	got := second.Check()
	if len(got) != 1 || got[0].Kind != sharedStorageIdentity {
		t.Fatalf("conflicts = %+v, want one identity conflict", got)
	}
}

// A node restarting alone must not accuse itself: the previous boot's beacon is
// its own, and the suppressed first round is what makes that safe.
func TestDetector_RestartAloneIsClean(t *testing.T) {
	dir := t.TempDir()
	paths := func() []string { return []string{dir} }

	before := newSharedStorageDetector("node-a", paths)
	before.Check()
	before.Check()

	after := newSharedStorageDetector("node-a", paths)
	if got := after.Check(); len(got) != 0 {
		t.Fatalf("restart round 1 = %+v, want nothing", got)
	}
	if got := after.Check(); len(got) != 0 {
		t.Fatalf("restart round 2 = %+v, want nothing - it overwrote its own beacon", got)
	}
}

// Separate paths are the normal topology and must stay silent.
func TestDetector_SeparatePathsAreClean(t *testing.T) {
	a := newSharedStorageDetector("node-a", func() []string { return []string{t.TempDir()} })
	b := newSharedStorageDetector("node-b", func() []string { return []string{t.TempDir()} })
	a.Check()
	if got := b.Check(); len(got) != 0 {
		t.Fatalf("separate paths reported %+v", got)
	}
}

// Only some paths may be shared, so each is evaluated on its own.
func TestDetector_ReportsPerPath(t *testing.T) {
	shared, private := t.TempDir(), t.TempDir()

	other := newSharedStorageDetector("node-b", func() []string { return []string{shared} })
	other.Check()

	mine := newSharedStorageDetector("node-a", func() []string { return []string{private, shared} })
	got := mine.Check()
	if len(got) != 1 {
		t.Fatalf("conflicts = %+v, want exactly one", got)
	}
	if got[0].Path != shared {
		t.Errorf("path = %q, want the shared one", got[0].Path)
	}
}

func TestDetector_NilSafe(t *testing.T) {
	var d *sharedStorageDetector
	if got := d.Check(); got != nil {
		t.Errorf("Check on nil = %+v", got)
	}
	if got := d.Conflicts(); got != nil {
		t.Errorf("Conflicts on nil = %+v", got)
	}
	d.Run(nil) // must not panic
}

func TestNewInstanceID_Unique(t *testing.T) {
	// Two processes must never collide, or the identity case goes undetected.
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newInstanceID()
		if id == "" {
			t.Fatal("empty instance id")
		}
		if seen[id] {
			t.Fatalf("duplicate instance id %q", id)
		}
		seen[id] = true
	}
}

// The heartbeat reads Conflicts() on the discovery goroutine while Check() runs
// on the detector's own ticker. Nothing in production exercises both at once
// from a test, which is exactly why the race between them survived review: a
// plain `go test -race` run finds nothing, because no test ever started the two
// goroutines together. This one does, so the race detector has something to
// bite on. It FAILS under -race against a detector without the mutex.
func TestDetector_ConcurrentCheckAndConflicts(t *testing.T) {
	dir := t.TempDir()
	d := newSharedStorageDetector("node-a", func() []string { return []string{dir} })

	// A second node on the same path, so Conflicts() returns a non-empty slice
	// and a torn read has a length to disagree about.
	other := newSharedStorageDetector("node-b", func() []string { return []string{dir} })
	other.Check()

	stop := make(chan struct{})
	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		for {
			select {
			case <-stop:
				return
			default:
				d.Check()
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			select {
			case <-stop:
				return
			default:
				for _, c := range d.Conflicts() {
					_ = c.Message() // touch the elements, not just the header
				}
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	<-done
	<-done
}

// R35-12: after an identity change the previous process's beacon carries the
// id this node ran as until now. Left there it read as a foreign node: a
// shared-storage alarm for five minutes and the boot sweep skipped for a day.
// A beacon under a former id that is refreshed after this process started is
// a live process, though, and must still be reported.
func TestDetector_FormerOwnIDIsNotAPeer(t *testing.T) {
	tests := []struct {
		name      string
		updated   time.Time
		wantPeer  bool
		wantKept  bool
		wantShare bool
	}{
		{name: "written before this process started", updated: processStarted.Add(-30 * time.Second)},
		{name: "refreshed after this process started", updated: time.Now().UTC().Add(time.Second), wantPeer: true, wantKept: true, wantShare: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			oldFormer, oldID := formerNodeIDs, nodeID
			formerNodeIDs, nodeID = map[string]bool{"node-old": true}, "node-new"
			t.Cleanup(func() { formerNodeIDs, nodeID = oldFormer, oldID })

			if err := writeBeacon(dir, nodeBeacon{NodeID: "node-old", InstanceID: "prev", UpdatedAt: tt.updated}); err != nil {
				t.Fatal(err)
			}
			if got := sharedWithAnotherNode(dir); got != tt.wantShare {
				t.Errorf("sharedWithAnotherNode = %v, want %v", got, tt.wantShare)
			}
			got := newSharedStorageDetector("node-new", func() []string { return []string{dir} }).Check()
			if peer := len(got) == 1 && got[0].Kind == sharedStoragePeer; peer != tt.wantPeer || len(got) > 1 {
				t.Errorf("conflicts = %+v, want peer %v", got, tt.wantPeer)
			}
			_, err := os.Stat(filepath.Join(dir, beaconDirName, "node-old.beacon"))
			if kept := err == nil; kept != tt.wantKept {
				t.Errorf("old beacon kept = %v, want %v", kept, tt.wantKept)
			}
		})
	}
}

// R42-07: a peer whose STORAGE_PATHS lists this node's identity dir as a later
// path writes its own id into that dir's marker. Only ids this node itself
// held count as former, or the peer's live beacon is deleted as a leftover.
func TestFormerIDsOf_IgnoresPeerIDInMarker(t *testing.T) {
	tests := []struct {
		name    string
		bootIDs []string
		marker  string
		want    []string
	}{
		{name: "peer id in marker", marker: "#node node-peer\n", want: []string{"node-new"}},
		{name: "re-keyed this boot", bootIDs: []string{"node-old"}, marker: "#node node-old\n#node node-peer\n", want: []string{"node-new", "node-old"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := nodeSecretDir
			nodeSecretDir = t.TempDir()
			t.Cleanup(func() { nodeSecretDir = prev })
			t.Setenv("NODE_ID", "")
			if err := saveNodeID(nodeSecretDir, "node-new"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(nodeSecretDir, storageMarkerName), []byte(tt.marker), 0o644); err != nil {
				t.Fatal(err)
			}
			got := formerIDsOf(tt.bootIDs)
			if len(got) != len(tt.want) {
				t.Errorf("formerIDsOf = %v, want %v", got, tt.want)
			}
			for _, id := range tt.want {
				if !got[id] {
					t.Errorf("formerIDsOf = %v, missing %s", got, id)
				}
			}
		})
	}
}

// R43-06/R43-07: the id an enrolment replaces lives only in main's bootNodeIDs
// until NewStorageManager. A restart in between (the Redis ping loop has no
// bound) must still read it as this node's former id, or its fresh beacon
// counts as a peer for 24h. A peer's id in the shared marker still never does.
func TestFormerIDsOf_SurvivesARestartAfterAReplacedIdentity(t *testing.T) {
	tests := []struct {
		name     string
		cached   string
		assigned string
		want     []string
	}{
		{name: "identity replaced", cached: "node-old", assigned: "node-new", want: []string{"node-new", "node-old"}},
		{name: "first pairing", assigned: "node-new", want: []string{"node-new"}},
		{name: "re-confirmed", cached: "node-new", assigned: "node-new", want: []string{"node-new"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.cached != "" {
				if err := saveNodeID(dir, tt.cached); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, storageMarkerName), []byte("#node node-peer\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			secret := bytes.Repeat([]byte{0x5a}, 32)
			assign := func(_ *pb.NodeAuth, stream pb.NodeService_NodeConnectServer) error {
				return sendResult(stream, &pb.AuthResult{Ok: true, AssignedId: tt.assigned, NodeSecret: hex.EncodeToString(secret)})
			}
			useFakeCore(t, &fakeCore{script: []func(*pb.NodeAuth, pb.NodeService_NodeConnectServer) error{assign}}, dir)
			t.Setenv("NODE_ID", "")
			nodeID = tt.cached
			if nodeID == "" {
				nodeID = "the-hostname"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := bootstrapSecretViaGRPC(ctx, true); err != nil {
				t.Fatal(err)
			}

			// The restart: the boot ids are read again from disk.
			got := formerIDsOf(nodeIdentities(dir))
			if len(got) != len(tt.want) {
				t.Errorf("formerIDsOf = %v, want %v", got, tt.want)
			}
			for _, id := range tt.want {
				if !got[id] {
					t.Errorf("formerIDsOf = %v, missing %s", got, id)
				}
			}
		})
	}
}
