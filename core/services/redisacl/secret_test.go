package redisacl

import (
	"bytes"
	"sync"
	"testing"
)

// memSecretStore is a tiny in-memory secretStore for round-trip testing. The
// mutex is not decoration: the concurrency test below runs several callers at
// once, and Postgres applies each statement atomically too.
type memSecretStore struct {
	mu  sync.Mutex
	enc map[int]string
}

func newMemSecretStore() *memSecretStore {
	return &memSecretStore{enc: make(map[int]string)}
}

func (m *memSecretStore) GetNodeSecretEnc(id int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enc[id], nil
}

func (m *memSecretStore) SetNodeSecretEnc(id int, enc string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enc[id] = enc
	return nil
}

// SetNodeSecretEncIfUnchanged mirrors what the UPDATE ... WHERE does: one
// statement, applied to whatever the row holds at that instant.
func (m *memSecretStore) SetNodeSecretEncIfUnchanged(id int, prev, next string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enc[id] != prev {
		return false, nil
	}
	m.enc[id] = next
	return true, nil
}

func TestLoadOrCreateNodeSecret_MintThenLoad(t *testing.T) {
	st := newMemSecretStore()
	const cluster = "test-cluster-secret"

	first, err := LoadOrCreateNodeSecret(st, cluster, 7)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(first) != 32 {
		t.Fatalf("expected 32-byte secret, got %d", len(first))
	}
	if st.enc[7] == "" {
		t.Fatal("expected ciphertext to be persisted")
	}

	// Second call must decrypt the stored ciphertext and return the same secret.
	second, err := LoadOrCreateNodeSecret(st, cluster, 7)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("mint-then-load returned a different secret")
	}
}

func TestLoadOrCreateNodeSecret_RemintsOnCorruption(t *testing.T) {
	st := newMemSecretStore()
	const cluster = "test-cluster-secret"

	if _, err := LoadOrCreateNodeSecret(st, cluster, 1); err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Corrupt the stored ciphertext; next call must self-heal by re-minting.
	st.enc[1] = "not-valid-hex-ciphertext"

	got, err := LoadOrCreateNodeSecret(st, cluster, 1)
	if err != nil {
		t.Fatalf("re-mint: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("expected 32-byte secret after re-mint, got %d", len(got))
	}
	if st.enc[1] == "not-valid-hex-ciphertext" {
		t.Fatal("expected corrupted ciphertext to be replaced")
	}
}

// racingStore makes every caller finish its READ before any of them writes.
// That is not an artificial interleaving: a node dials every Core replica in
// the same second, so when the stored secret is absent they all read empty
// before any of them has minted anything.
type racingStore struct {
	*memSecretStore
	readers int

	mu      sync.Mutex
	arrived int
	gate    chan struct{}
}

func newRacingStore(readers int) *racingStore {
	return &racingStore{memSecretStore: newMemSecretStore(), readers: readers, gate: make(chan struct{})}
}

func (r *racingStore) GetNodeSecretEnc(id int) (string, error) {
	v, err := r.memSecretStore.GetNodeSecretEnc(id)
	r.mu.Lock()
	r.arrived++
	if r.arrived == r.readers {
		close(r.gate) // exactly once: later retries only increment past it
	}
	r.mu.Unlock()
	<-r.gate
	return v, err
}

// Every Core replica must hand the node the SAME secret, and it must be the one
// the database ends up holding.
//
// Without the compare-and-set each replica minted its own and the last write
// won, so the node cached one secret while Core stored another. That is not a
// transient failure: the stored secret then EXISTS, so every replica demands a
// challenge the node cannot answer, and Core deliberately never re-issues to a
// bare token holder. Measured in production on 2026-09-08 - node eu-node-00
// cached a secret at 12:56:01 and never authenticated again.
func TestLoadOrCreateNodeSecretAgreesAcrossReplicas(t *testing.T) {
	const replicas = 4
	const nodeID = 42
	const cluster = "cluster-secret"

	st := newRacingStore(replicas)
	got := make([][]byte, replicas)
	errs := make([]error, replicas)

	var wg sync.WaitGroup
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = LoadOrCreateNodeSecret(st, cluster, nodeID)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d: %v", i, err)
		}
	}
	for i := 1; i < replicas; i++ {
		if !bytes.Equal(got[0], got[i]) {
			t.Fatalf("replica 0 and replica %d handed the node different secrets; "+
				"whichever the node caches, the others will reject it forever", i)
		}
	}

	// And the value the node was handed must be the value Core can verify later.
	stored, ok, err := LoadNodeSecret(st, cluster, nodeID)
	if err != nil || !ok {
		t.Fatalf("nothing readable was stored: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(stored, got[0]) {
		t.Fatal("the stored secret is not the one handed to the node: every future challenge fails")
	}
}
