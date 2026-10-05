package services

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/store"
)

// fakeWarpStore implements the warpStore surface the service uses. It seeds a
// single region "leader-01" (10.0.99.0/24) with one leader so enroll behaves like
// the pre-multi-hub single-hub deployment.
type fakeWarpStore struct {
	peers   map[string]store.WarpPeer // pubkey -> peer
	byKey   map[int][]store.WarpPeer
	regions []store.WarpRegion
	leaders []store.WarpLeader
	nextID  int
}

func newFakeWarpStore() *fakeWarpStore {
	return &fakeWarpStore{
		peers: map[string]store.WarpPeer{},
		byKey: map[int][]store.WarpPeer{},
		regions: []store.WarpRegion{
			{Region: "leader-01", Subnet: "10.0.99.0/24", Enabled: true},
		},
		leaders: []store.WarpLeader{
			{LeaderID: "leader-01", Region: "leader-01", Endpoint: "vpn.example.com:25599", Enabled: true},
		},
	}
}

func (f *fakeWarpStore) InsertWarpPeer(p store.WarpPeer) (int, error) {
	f.nextID++
	p.ID = f.nextID
	f.peers[p.Pubkey] = p
	f.byKey[p.APIKeyID] = append(f.byKey[p.APIKeyID], p)
	return p.ID, nil
}
func (f *fakeWarpStore) GetWarpPeerByPubkey(pk string) (*store.WarpPeer, error) {
	if p, ok := f.peers[pk]; ok {
		return &p, nil
	}
	return nil, errNotFound
}
func (f *fakeWarpStore) ListWarpPeersByRegion(region string) ([]store.WarpPeer, error) {
	var out []store.WarpPeer
	for _, p := range f.peers {
		if p.Region == region {
			out = append(out, p)
		}
	}
	return out, nil
}
func (f *fakeWarpStore) ListWarpPeersByKey(apiKeyID int) ([]store.WarpPeer, error) {
	var out []store.WarpPeer
	for _, p := range f.peers {
		if p.APIKeyID == apiKeyID {
			out = append(out, p)
		}
	}
	return out, nil
}
func (f *fakeWarpStore) DeleteWarpPeerByPubkey(pubkey string) error {
	delete(f.peers, pubkey)
	return nil
}
func (f *fakeWarpStore) CountWarpPeersByRegion() (map[string]int, error) {
	out := map[string]int{}
	for _, p := range f.peers {
		out[p.Region]++
	}
	return out, nil
}
func (f *fakeWarpStore) ListWarpRegions() ([]store.WarpRegion, error) { return f.regions, nil }
func (f *fakeWarpStore) GetWarpRegion(region string) (*store.WarpRegion, error) {
	for _, r := range f.regions {
		if r.Region == region {
			rr := r
			return &rr, nil
		}
	}
	return nil, errNotFound
}
func (f *fakeWarpStore) ListWarpLeaders() ([]store.WarpLeader, error) { return f.leaders, nil }

func (f *fakeWarpStore) SetWarpPeerAssignedLeader(pubkey, leaderID string) error {
	p, ok := f.peers[pubkey]
	if !ok {
		return errNotFound
	}
	p.AssignedLeader = leaderID
	f.peers[pubkey] = p
	return nil
}

func (f *fakeWarpStore) deletePeer(pk string) {
	if p, ok := f.peers[pk]; ok {
		delete(f.peers, pk)
		rest := f.byKey[p.APIKeyID][:0]
		for _, q := range f.byKey[p.APIKeyID] {
			if q.Pubkey != pk {
				rest = append(rest, q)
			}
		}
		f.byKey[p.APIKeyID] = rest
	}
}

func (f *fakeWarpStore) EnrollPeerTx(keyID, limit int, onNewConn, pubkey, fixedIP, region string, allocIP func(taken map[string]bool) (string, error)) (string, string, error) {
	var evicted string
	if len(f.byKey[keyID]) >= limit {
		if onNewConn != "kill_old" {
			return "", "", errLimitReached
		}
		old := f.byKey[keyID][0]
		evicted = old.Pubkey
		f.deletePeer(old.Pubkey)
	}
	wgIP := fixedIP
	if wgIP == "" {
		taken := map[string]bool{}
		for _, p := range f.peers {
			taken[p.WGIP] = true
		}
		ip, err := allocIP(taken)
		if err != nil {
			return "", "", err
		}
		wgIP = ip
	}
	_, _ = f.InsertWarpPeer(store.WarpPeer{APIKeyID: keyID, Pubkey: pubkey, WGIP: wgIP, Region: region})
	return wgIP, evicted, nil
}

// errLimitReached lets the fake return the same sentinel the real store does.
var errLimitReached = store.ErrWarpLimitReached

var errNotFound = errString("not found")

type errString string

func (e errString) Error() string { return string(e) }

func enrollTestService(t *testing.T) (*WarpService, *fakeWarpStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	fs := newFakeWarpStore()
	svc := &WarpService{warp: fs, redis: rdb, clusterSecret: "cluster-secret"}
	return svc, fs, mr
}

func storePeer(keyID int, pub, ip string) store.WarpPeer {
	return store.WarpPeer{APIKeyID: keyID, Pubkey: pub, WGIP: ip, Region: "leader-01"}
}

func TestEnroll_GeneralKey_AllocatesAndPushesAddPeer(t *testing.T) {
	svc, _, mr := enrollTestService(t)
	key := store.WarpAPIKey{ID: 1, Policy: "general", MaxConns: 5, OnNewConn: "block"}
	resp, err := svc.Enroll(context.Background(), key, "pubkeyA", nil)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if resp.WGIP == "" || resp.WGSubnet != "10.0.99.0/24" {
		t.Fatalf("bad resp %+v", resp)
	}
	if resp.Region != "leader-01" {
		t.Fatalf("expected region leader-01, got %q", resp.Region)
	}
	vals, _ := mr.List("dylaris:warp:leader-01:queue")
	if len(vals) != 1 {
		t.Fatalf("expected 1 queued command, got %d", len(vals))
	}
}

func TestEnroll_GeneralKey_BlocksAtLimit(t *testing.T) {
	svc, _, _ := enrollTestService(t)
	key := store.WarpAPIKey{ID: 1, Policy: "general", MaxConns: 1, OnNewConn: "block"}
	if _, err := svc.Enroll(context.Background(), key, "pubA", nil); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	if _, err := svc.Enroll(context.Background(), key, "pubB", nil); err == nil {
		t.Fatal("expected block at max_conns=1")
	}
}

func TestEnroll_Idempotent_SamePubkeySameIP(t *testing.T) {
	svc, _, _ := enrollTestService(t)
	key := store.WarpAPIKey{ID: 1, Policy: "general", MaxConns: 5, OnNewConn: "block"}
	r1, _ := svc.Enroll(context.Background(), key, "pubA", nil)
	r2, err := svc.Enroll(context.Background(), key, "pubA", nil)
	if err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	if r1.WGIP != r2.WGIP {
		t.Fatalf("re-enroll changed IP: %s -> %s", r1.WGIP, r2.WGIP)
	}
}

func TestEnroll_FixedWGIP_Invalid_Rejected(t *testing.T) {
	svc, _, mr := enrollTestService(t)
	// .1 is the leader-reserved first host of 10.0.99.0/24 - must be refused, and no
	// leader command must be queued on rejection.
	key := store.WarpAPIKey{ID: 1, Policy: "fixed", MaxConns: 1, OnNewConn: "block", FixedWGIP: "10.0.99.1"}
	_, err := svc.Enroll(context.Background(), key, "pubA", nil)
	if err == nil {
		t.Fatal("expected enroll to reject the leader-reserved fixed IP")
	}
	if !errors.Is(err, ErrInvalidFixedWGIP) {
		t.Fatalf("error %v does not wrap ErrInvalidFixedWGIP", err)
	}
	if vals, _ := mr.List("dylaris:warp:leader-01:queue"); len(vals) != 0 {
		t.Fatalf("expected no leader command on rejection, got %d", len(vals))
	}
}

func TestEnroll_FixedWGIP_Valid_UsesIt(t *testing.T) {
	svc, _, _ := enrollTestService(t)
	key := store.WarpAPIKey{ID: 1, Policy: "fixed", MaxConns: 1, OnNewConn: "block", FixedWGIP: "10.0.99.50"}
	resp, err := svc.Enroll(context.Background(), key, "pubA", nil)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if resp.WGIP != "10.0.99.50" {
		t.Fatalf("expected WGIP 10.0.99.50, got %q", resp.WGIP)
	}
}

func TestEnroll_FixedKey_KillOldEvictsPrevious(t *testing.T) {
	svc, fs, mr := enrollTestService(t)
	key := store.WarpAPIKey{ID: 1, Policy: "fixed", MaxConns: 1, OnNewConn: "kill_old"}
	if _, err := svc.Enroll(context.Background(), key, "pubOld", nil); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := svc.Enroll(context.Background(), key, "pubNew", nil); err != nil {
		t.Fatalf("second (kill_old): %v", err)
	}
	if _, err := fs.GetWarpPeerByPubkey("pubOld"); err == nil {
		t.Fatal("old peer should have been evicted")
	}
	vals, _ := mr.List("dylaris:warp:leader-01:queue")
	if len(vals) != 3 {
		t.Fatalf("expected 3 commands (add, remove, add), got %d", len(vals))
	}
}

// Re-enrolling a public key answers only the key that enrolled it: anyone with
// a key and another peer's public key read that peer's overlay IP, region and
// leader endpoints.
func TestEnroll_AnotherKeysPubkeyIsRefused(t *testing.T) {
	svc, _, _ := enrollTestService(t)
	owner := store.WarpAPIKey{ID: 1, Policy: "general", MaxConns: 5, OnNewConn: "block"}
	other := store.WarpAPIKey{ID: 2, Policy: "general", MaxConns: 5, OnNewConn: "block"}
	if _, err := svc.Enroll(context.Background(), owner, "pubA", nil); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if r, err := svc.Enroll(context.Background(), other, "pubA", nil); err == nil {
		t.Fatalf("another key was answered with the peer's config: %+v", r)
	}
}
