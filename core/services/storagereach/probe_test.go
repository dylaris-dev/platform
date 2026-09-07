package storagereach

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"dylaris-core/storage"
)

// probeFakeProvider wraps a real LocalProvider rooted at a temp dir so the
// probe runs against genuine filesystem semantics, and layers failure
// injection on top. Embedding storage.StorageProvider is not used here on
// purpose: every method the probe calls is exercised, so overriding all of
// them keeps the failure injection in one obvious place.
type probeFakeProvider struct {
	inner storage.StorageProvider
	// writeErrPrefix fails any WriteFile whose path starts with it.
	writeErrPrefix string
	writeErr       error
	// hideePrefix makes ListFiles pretend paths under it do not exist, which
	// is the fake-shared volume: this Core's own writes are visible, peers'
	// are not.
	hidePrefix string
	listErr    error
}

func (p *probeFakeProvider) ListFiles(ctx context.Context, path string) ([]storage.FileInfo, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	out, err := p.inner.ListFiles(ctx, path)
	if err != nil {
		return nil, err
	}
	if p.hidePrefix == "" {
		return out, nil
	}
	kept := out[:0]
	for _, fi := range out {
		if strings.HasPrefix(fi.Name, p.hidePrefix) {
			continue
		}
		kept = append(kept, fi)
	}
	return kept, nil
}

func (p *probeFakeProvider) GetFile(ctx context.Context, path string) (io.ReadCloser, error) {
	if p.hidePrefix != "" && strings.Contains(path, "/"+p.hidePrefix) {
		return nil, errors.New("no such file")
	}
	return p.inner.GetFile(ctx, path)
}

func (p *probeFakeProvider) DeletePath(ctx context.Context, path string) error {
	return p.inner.DeletePath(ctx, path)
}

func (p *probeFakeProvider) CreateDir(ctx context.Context, path string) error {
	return p.inner.CreateDir(ctx, path)
}

func (p *probeFakeProvider) CopyToLocal(ctx context.Context, src, dst string) error {
	return p.inner.CopyToLocal(ctx, src, dst)
}

func (p *probeFakeProvider) WriteFile(ctx context.Context, path string, content io.Reader) error {
	if p.writeErrPrefix != "" && strings.Contains(path, p.writeErrPrefix) {
		return p.writeErr
	}
	return p.inner.WriteFile(ctx, path, content)
}

func (p *probeFakeProvider) DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "", nil
}

func (p *probeFakeProvider) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

var _ storage.StorageProvider = (*probeFakeProvider)(nil)

// sharedRoot gives every Core in a test the SAME LocalProvider base path,
// which is exactly what a genuinely shared mount looks like.
func sharedRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func newProbeProvider(root string) *probeFakeProvider {
	return &probeFakeProvider{inner: &storage.LocalProvider{BasePath: root}}
}

// probeOpts is the SETTLING class of deadline, in settlingRound()'s sense
// (round_test.go): the peer's beacon is already there, so Probe returns the
// moment it reads it and the deadline is only ever reached when the test is
// already failing. A generous one therefore costs a passing run nothing.
//
// The 60ms deadlines further down are the opposite class and must STAY small:
// those tests expect the clock to run out, so raising them would only make the
// suite slower.
func probeOpts(coreID, roundID string, peers []string) ProbeOptions {
	return ProbeOptions{
		CoreID:       coreID,
		RoundID:      roundID,
		Fingerprint:  "fp-1",
		Participants: peers,
		Deadline:     30 * time.Second,
		RetryEvery:   10 * time.Millisecond,
	}
}

func TestProbe_SingleParticipantWritesAndReadsItself(t *testing.T) {
	root := sharedRoot(t)
	prov := newProbeProvider(root)

	rep := Probe(context.Background(), prov, probeOpts("core-a", "r1", []string{"core-a"}))

	if !rep.Reachable || !rep.Wrote {
		t.Fatalf("report = %+v, want reachable and wrote", rep)
	}
	if len(rep.SeenPeers) != 0 {
		t.Errorf("SeenPeers = %v, want empty (a Core is never its own peer)", rep.SeenPeers)
	}
	if rep.Fingerprint != "fp-1" {
		t.Errorf("Fingerprint = %q, want fp-1", rep.Fingerprint)
	}
}

func TestProbe_TwoParticipantsOnASharedRootSeeEachOther(t *testing.T) {
	root := sharedRoot(t)
	peers := []string{"core-a", "core-b"}

	// core-a goes first and cannot see core-b yet; it must NOT report having
	// seen it. This is the case a naive "list once" probe gets wrong.
	early := Probe(context.Background(), newProbeProvider(root), ProbeOptions{
		CoreID: "core-a", RoundID: "r1", Fingerprint: "fp-1", Participants: peers,
		Deadline: 50 * time.Millisecond, RetryEvery: 10 * time.Millisecond,
	})
	if len(early.SeenPeers) != 0 {
		t.Fatalf("core-a SeenPeers = %v before core-b wrote, want empty", early.SeenPeers)
	}
	if !early.Wrote {
		t.Fatal("core-a did not write its own beacon")
	}

	// core-b now runs with core-a's beacon already present.
	b := Probe(context.Background(), newProbeProvider(root), probeOpts("core-b", "r1", peers))
	if len(b.SeenPeers) != 1 || b.SeenPeers[0] != "core-a" {
		t.Fatalf("core-b SeenPeers = %v, want [core-a]", b.SeenPeers)
	}
	if len(b.CrossWroteTo) != 1 || b.CrossWroteTo[0] != "core-a" {
		t.Fatalf("core-b CrossWroteTo = %v, want [core-a]", b.CrossWroteTo)
	}

	// core-a re-runs and now sees core-b: the retry loop is what makes a
	// genuinely shared but slow backend pass instead of failing on first look.
	a := Probe(context.Background(), newProbeProvider(root), probeOpts("core-a", "r1", peers))
	if len(a.SeenPeers) != 1 || a.SeenPeers[0] != "core-b" {
		t.Fatalf("core-a SeenPeers = %v, want [core-b]", a.SeenPeers)
	}
}

func TestProbe_SucceedsOnRetryAfterAPropagationDelay(t *testing.T) {
	root := sharedRoot(t)
	peers := []string{"core-a", "core-b"}

	// core-b's beacon lands only after core-a has already looked once.
	go func() {
		time.Sleep(60 * time.Millisecond)
		Probe(context.Background(), newProbeProvider(root), ProbeOptions{
			CoreID: "core-b", RoundID: "r1", Fingerprint: "fp-1", Participants: peers,
			Deadline: 10 * time.Millisecond, RetryEvery: 5 * time.Millisecond,
		})
	}()

	// 30s, not 3s. This bounds how long core-a waits for the GOROUTINE ABOVE to
	// be scheduled, which on a runner executing the whole job matrix at once has
	// little to do with how long the work takes - the same latent bug a181094c
	// fixed in six copies of the round-test loop, still here in this one. It ran
	// out on CI while core-a was fine, on a commit that touched neither this
	// package nor anything near it.
	//
	// It only runs out on the FAILING path: Probe returns the moment it sees
	// core-b's beacon, so a passing run never waits on this number at all.
	rep := Probe(context.Background(), newProbeProvider(root), ProbeOptions{
		CoreID: "core-a", RoundID: "r1", Fingerprint: "fp-1", Participants: peers,
		Deadline: 30 * time.Second, RetryEvery: 10 * time.Millisecond,
	})
	if len(rep.SeenPeers) != 1 || rep.SeenPeers[0] != "core-b" {
		t.Fatalf("SeenPeers = %v, want [core-b] after the retry window", rep.SeenPeers)
	}
}

func TestProbe_WriteDenied(t *testing.T) {
	prov := newProbeProvider(sharedRoot(t))
	prov.writeErrPrefix = "core-a"
	prov.writeErr = errors.New("read-only file system")

	rep := Probe(context.Background(), prov, ProbeOptions{
		CoreID: "core-a", RoundID: "r1", Fingerprint: "fp-1", Participants: []string{"core-a", "core-b"},
		Deadline: 60 * time.Millisecond, RetryEvery: 10 * time.Millisecond,
	})

	if rep.Wrote {
		t.Fatal("Wrote = true despite a failing write")
	}
	if !strings.Contains(rep.WriteErr, "read-only") {
		t.Errorf("WriteErr = %q, want the backend error text", rep.WriteErr)
	}
}

func TestProbe_NotSharedWhenPeerBeaconsAreInvisible(t *testing.T) {
	root := sharedRoot(t)
	peers := []string{"core-a", "core-b"}

	// core-b writes for real...
	Probe(context.Background(), newProbeProvider(root), ProbeOptions{
		CoreID: "core-b", RoundID: "r1", Fingerprint: "fp-1", Participants: peers,
		Deadline: 60 * time.Millisecond, RetryEvery: 10 * time.Millisecond,
	})

	// ...but core-a's view hides everything belonging to core-b, which is
	// what a per-host volume that only looks shared does.
	blind := newProbeProvider(root)
	blind.hidePrefix = "core-b"
	rep := Probe(context.Background(), blind, ProbeOptions{
		CoreID: "core-a", RoundID: "r1", Fingerprint: "fp-1", Participants: peers,
		Deadline: 60 * time.Millisecond, RetryEvery: 10 * time.Millisecond,
	})

	if !rep.Wrote {
		t.Fatal("core-a should still have written its own beacon")
	}
	if len(rep.SeenPeers) != 0 {
		t.Fatalf("SeenPeers = %v, want empty on a fake-shared volume", rep.SeenPeers)
	}
}

func TestProbe_CrossWriteDenied(t *testing.T) {
	root := sharedRoot(t)
	peers := []string{"core-a", "core-b"}
	Probe(context.Background(), newProbeProvider(root), ProbeOptions{
		CoreID: "core-b", RoundID: "r1", Fingerprint: "fp-1", Participants: peers,
		Deadline: 60 * time.Millisecond, RetryEvery: 10 * time.Millisecond,
	})

	// core-a may write its own beacon but not into core-b's ack namespace:
	// the NFS uid-squash shape.
	squashed := newProbeProvider(root)
	squashed.writeErrPrefix = "core-b.acks"
	squashed.writeErr = errors.New("permission denied")

	rep := Probe(context.Background(), squashed, probeOpts("core-a", "r1", peers))

	if !rep.Wrote {
		t.Fatal("core-a's own write must still succeed")
	}
	if len(rep.SeenPeers) != 1 {
		t.Fatalf("SeenPeers = %v, want [core-b] - visibility is unaffected", rep.SeenPeers)
	}
	if len(rep.CrossWriteDenied) != 1 || rep.CrossWriteDenied[0] != "core-b" {
		t.Fatalf("CrossWriteDenied = %v, want [core-b]", rep.CrossWriteDenied)
	}
	if len(rep.CrossWroteTo) != 0 {
		t.Errorf("CrossWroteTo = %v, want empty", rep.CrossWroteTo)
	}
}

func TestProbe_UnreachableWhenListingFails(t *testing.T) {
	prov := newProbeProvider(sharedRoot(t))
	prov.writeErrPrefix = "core-a"
	prov.writeErr = errors.New("connection refused")
	prov.listErr = errors.New("connection refused")

	rep := Probe(context.Background(), prov, ProbeOptions{
		CoreID: "core-a", RoundID: "r1", Fingerprint: "fp-1", Participants: []string{"core-a", "core-b"},
		Deadline: 60 * time.Millisecond, RetryEvery: 10 * time.Millisecond,
	})

	if rep.Reachable {
		t.Fatal("Reachable = true although both write and list failed")
	}
}

func TestProbe_IgnoresBeaconsFromAnotherRound(t *testing.T) {
	// A stale beacon left by an earlier round must never count as a peer
	// confirming THIS round, or a cleanup that failed once would make every
	// later round pass on evidence that proves nothing.
	root := sharedRoot(t)
	peers := []string{"core-a", "core-b"}
	Probe(context.Background(), newProbeProvider(root), ProbeOptions{
		CoreID: "core-b", RoundID: "OLD", Fingerprint: "fp-1", Participants: peers,
		Deadline: 60 * time.Millisecond, RetryEvery: 10 * time.Millisecond,
	})

	rep := Probe(context.Background(), newProbeProvider(root), ProbeOptions{
		CoreID: "core-a", RoundID: "NEW", Fingerprint: "fp-1", Participants: peers,
		Deadline: 60 * time.Millisecond, RetryEvery: 10 * time.Millisecond,
	})
	if len(rep.SeenPeers) != 0 {
		t.Fatalf("SeenPeers = %v, want empty - the beacon belongs to round OLD", rep.SeenPeers)
	}
}

func TestProbe_ReportsAPeerPointedAtADifferentBackend(t *testing.T) {
	root := sharedRoot(t)
	peers := []string{"core-a", "core-b"}
	// core-b is on the same mount but configured for a different backend, so
	// its beacon carries a different fingerprint.
	Probe(context.Background(), newProbeProvider(root), ProbeOptions{
		CoreID: "core-b", RoundID: "r1", Fingerprint: "fp-OTHER", Participants: peers,
		Deadline: 50 * time.Millisecond, RetryEvery: 10 * time.Millisecond,
	})

	rep := Probe(context.Background(), newProbeProvider(root), probeOpts("core-a", "r1", peers))
	if len(rep.SeenPeers) != 0 {
		t.Fatalf("SeenPeers = %v, want empty - fp-OTHER is not this round's backend", rep.SeenPeers)
	}
	if len(rep.MismatchedPeers) != 1 || rep.MismatchedPeers[0] != "core-b" {
		t.Fatalf("MismatchedPeers = %v, want [core-b]", rep.MismatchedPeers)
	}
}

func TestCleanupRound_RemovesTheProbeRoot(t *testing.T) {
	root := sharedRoot(t)
	prov := newProbeProvider(root)
	Probe(context.Background(), prov, probeOpts("core-a", "r1", []string{"core-a"}))

	if err := CleanupRound(context.Background(), prov, "r1"); err != nil {
		t.Fatalf("CleanupRound: %v", err)
	}
	// LocalProvider.ListFiles deliberately treats a missing directory as an
	// empty listing rather than an error (see storage.LocalProvider.ListFiles),
	// so an empty result - not an error return - is what proves the round's
	// contents are gone.
	names, err := prov.ListFiles(context.Background(), ProbeRoot("r1"))
	if err != nil {
		t.Fatalf("ListFiles after cleanup: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("probe root still lists %v after cleanup", names)
	}
}
