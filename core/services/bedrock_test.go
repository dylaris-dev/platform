package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"dylaris-core/store"
)

type bedrockFakeStore struct {
	store.Store
	keys    map[int][]byte
	uuids   map[int]string
	listErr error
	usage   []voiceUsageCall
	useErr  error
	eds     []store.RouteEdition
	links   []store.LinkRouteEdition
}

func (f *bedrockFakeStore) ListLinkRouteEditions() ([]store.LinkRouteEdition, error) {
	return f.links, nil
}

func (f *bedrockFakeStore) ListRouteEditions(int) ([]store.RouteEdition, error) { return f.eds, nil }

func (f *bedrockFakeStore) AddBedrockUsage(serverID int, _ time.Time, region string, rx, tx int64) error {
	f.usage = append(f.usage, voiceUsageCall{serverID, region, rx, tx})
	return f.useErr
}

func (f *bedrockFakeStore) EnableBedrock(id int, key []byte) error {
	if _, ok := f.keys[id]; !ok { // ON CONFLICT DO NOTHING
		f.keys[id] = key
	}
	return nil
}
func (f *bedrockFakeStore) GetBedrockKey(id int) ([]byte, bool, error) {
	k, ok := f.keys[id]
	return k, ok, nil
}
func (f *bedrockFakeStore) DisableBedrock(id int) error { delete(f.keys, id); return nil }
func (f *bedrockFakeStore) ListBedrockServers() ([]store.BedrockServer, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []store.BedrockServer
	for id, k := range f.keys {
		out = append(out, store.BedrockServer{ServerID: id, ServerUUID: f.uuids[id], Key: k})
	}
	return out, nil
}

// ListBedrockServerRefs deliberately ignores listErr: the keyless list must work
// when the keyed one does not (a bad key row).
func (f *bedrockFakeStore) ListBedrockServerRefs() ([]store.BedrockServerRef, error) {
	var out []store.BedrockServerRef
	for id := range f.keys {
		out = append(out, store.BedrockServerRef{ServerID: id, ServerUUID: f.uuids[id]})
	}
	return out, nil
}

func newBedrockFake() *bedrockFakeStore {
	return &bedrockFakeStore{keys: map[int][]byte{}, uuids: map[int]string{7: "uuid-7"}}
}

func TestBedrockEnablePublishesTheStoredKey(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newBedrockFake()
	first := bytes.Repeat([]byte{0x11}, 16)
	fs.keys[7] = first // an earlier enable already won
	if err := NewBedrockService(fs, rdb).Enable(context.Background(), 7, "uuid-7"); err != nil {
		t.Fatal(err)
	}
	got, _ := mr.Get(FloodgateKeyRedisKey("uuid-7"))
	if want := base64.StdEncoding.EncodeToString(first); got != want {
		t.Fatalf("published %q, want the stored key %q", got, want)
	}
	if ttl := mr.TTL(FloodgateKeyRedisKey("uuid-7")); ttl != 0 {
		t.Fatalf("TTL %v, want none", ttl)
	}
}

func TestBedrockEnableGeneratesA16ByteKey(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newBedrockFake()
	if err := NewBedrockService(fs, rdb).Enable(context.Background(), 7, "uuid-7"); err != nil {
		t.Fatal(err)
	}
	got, _ := mr.Get(FloodgateKeyRedisKey("uuid-7"))
	raw, err := base64.StdEncoding.DecodeString(got)
	if err != nil || len(raw) != 16 || !bytes.Equal(raw, fs.keys[7]) {
		t.Fatalf("published %q (%d bytes, err %v), stored %x", got, len(raw), err, fs.keys[7])
	}
}

func TestBedrockDisableRemovesRowAndKey(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newBedrockFake()
	svc := NewBedrockService(fs, rdb)
	ctx := context.Background()
	if err := svc.Enable(ctx, 7, "uuid-7"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Disable(ctx, 7, "uuid-7"); err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.keys[7]; ok || mr.Exists(FloodgateKeyRedisKey("uuid-7")) {
		t.Fatal("row or key survived disable")
	}
}

func TestBedrockReconcileRestoresAndSweeps(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newBedrockFake()
	fs.keys[7] = bytes.Repeat([]byte{0x22}, 16)
	mr.Set(FloodgateKeyRedisKey("orphan"), "AAAA")
	if err := NewBedrockService(fs, rdb).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := mr.Get(FloodgateKeyRedisKey("uuid-7")); got != base64.StdEncoding.EncodeToString(fs.keys[7]) {
		t.Fatalf("deleted key not restored, got %q", got)
	}
	if mr.Exists(FloodgateKeyRedisKey("orphan")) {
		t.Fatal("orphan key not removed")
	}
}

func TestBedrockReconcileListErrorDeletesNothing(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newBedrockFake()
	fs.listErr = errors.New("db down")
	mr.Set(FloodgateKeyRedisKey("uuid-7"), "AAAA")
	if err := NewBedrockService(fs, rdb).Reconcile(context.Background()); err == nil {
		t.Fatal("want the list error returned")
	}
	if !mr.Exists(FloodgateKeyRedisKey("uuid-7")) {
		t.Fatal("a failed list deleted a key")
	}
}

func TestBedrockMeterAccumulatesWithoutDoubleCounting(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newBedrockFake()
	fs.keys[7] = bytes.Repeat([]byte{1}, 16)
	svc := NewBedrockService(fs, rdb)
	mr.HSet("dylaris:traffic:edge:uuid-7", "rx:eu", "1000", "vrx:eu", "9", "brx:eu", "40", "btx:eu", "60")

	svc.Meter(context.Background())
	if len(fs.usage) != 1 || fs.usage[0] != (voiceUsageCall{7, "eu", 40, 60}) {
		t.Fatalf("usage = %+v, want server 7 eu rx 40 tx 60 (no java, no voice)", fs.usage)
	}
	fs.usage = nil
	svc.Meter(context.Background())
	if len(fs.usage) != 0 {
		t.Fatalf("an unchanged counter wrote usage: %+v", fs.usage)
	}
	mr.HSet("dylaris:traffic:edge:uuid-7", "brx:eu", "45")
	svc.Meter(context.Background())
	if len(fs.usage) != 1 || fs.usage[0] != (voiceUsageCall{7, "eu", 5, 0}) {
		t.Fatalf("usage = %+v, want only the 5 new rx bytes", fs.usage)
	}
}

func TestBedrockMeterLeavesSeenOnAFailedWrite(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newBedrockFake()
	fs.keys[7] = bytes.Repeat([]byte{1}, 16)
	fs.useErr = errors.New("db down")
	mr.HSet("dylaris:traffic:edge:uuid-7", "brx:eu", "40")
	NewBedrockService(fs, rdb).Meter(context.Background())
	if mr.Exists(bedrockSeenPrefix + "uuid-7") {
		t.Fatal("seen advanced past a failed write")
	}
}

// A keyed-list failure (one undecryptable key) must not stop metering.
func TestBedrockMeterSurvivesAKeyedListFailure(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newBedrockFake()
	fs.keys[7] = bytes.Repeat([]byte{1}, 16)
	fs.listErr = errors.New("bad key row")
	mr.HSet("dylaris:traffic:edge:uuid-7", "brx:eu", "40")
	NewBedrockService(fs, rdb).Meter(context.Background())
	if len(fs.usage) != 1 {
		t.Fatalf("usage = %+v, want metering to go on", fs.usage)
	}
}

// A flat Bedrock counter still renews the seen hash: the counter outlives it,
// and an expired seen hash would re-add the lifetime bytes.
func TestBedrockMeterRenewsSeenOnAFlatCounter(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newBedrockFake()
	fs.keys[7] = bytes.Repeat([]byte{1}, 16)
	mr.HSet("dylaris:traffic:edge:uuid-7", "brx:eu", "40", "btx:eu", "60")
	mr.HSet(bedrockSeenPrefix+"uuid-7", "brx:eu", "40", "btx:eu", "60")
	mr.SetTTL(bedrockSeenPrefix+"uuid-7", time.Hour)

	NewBedrockService(fs, rdb).Meter(context.Background())
	if len(fs.usage) != 0 {
		t.Fatalf("a flat counter wrote usage: %+v", fs.usage)
	}
	if ttl := mr.TTL(bedrockSeenPrefix + "uuid-7"); ttl <= time.Hour {
		t.Fatalf("seen TTL = %v, want renewed to %v", ttl, trafficSeenTTL)
	}
}

// A route-only address has no server row: its key is published under the
// owner subject the bridge derives, survives the orphan sweep, and its
// edition lands in the same hash as a server route's.
func TestBedrockReconcileKeepsRouteOnlyKeys(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newBedrockFake()
	key := bytes.Repeat([]byte{0x33}, 16)
	fs.links = []store.LinkRouteEdition{{Domain: "r.eu.dylaris.com", OwnerID: "owner-1", Java: true, Bedrock: true, Key: key}}
	name := FloodgateKeyRedisKey("owner:owner-1:bedrock:r.eu.dylaris.com")
	if name != FloodgateKeyRedisKey(LinkRouteBedrockSubject("owner-1", "r.eu.dylaris.com")) {
		t.Fatal("subject drifted from the bridge's copy (gateway floodgateSubject)")
	}
	mr.Set(FloodgateKeyRedisKey("orphan"), "AAAA")
	svc := NewBedrockService(fs, rdb)
	svc.SetFlags(nil) // off: the key is still kept, the edition pauses
	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := mr.Get(name); got != base64.StdEncoding.EncodeToString(key) {
		t.Fatalf("route-only key %q, want the stored one", got)
	}
	if mr.Exists(FloodgateKeyRedisKey("orphan")) {
		t.Fatal("orphan key not removed")
	}
	if err := svc.PublishEditions(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet(RouteEditionsKey, "r.eu.dylaris.com"); got != "both" {
		t.Fatalf("edition %q, want both", got)
	}
}

// A route-only row whose key could not be read (Key nil) must not stop the
// publish for everyone else: its edition is still written, server routes too,
// and no empty key is put in Redis.
func TestPublishEditionsSurvivesAnUnreadableRouteOnlyKey(t *testing.T) {
	mr, rdb := newVoiceRedis(t)
	fs := newBedrockFake()
	fs.keys[7] = bytes.Repeat([]byte{1}, 16)
	fs.eds = []store.RouteEdition{{Domain: "s.eu.dylaris.com", ServerID: 7, ServerUUID: "uuid-7", Java: true, Bedrock: true}}
	fs.links = []store.LinkRouteEdition{{Domain: "r.eu.dylaris.com", OwnerID: "owner-1", Java: false, Bedrock: true}}
	if err := NewBedrockService(fs, rdb).PublishEditions(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if mr.HGet(RouteEditionsKey, "s.eu.dylaris.com") != "both" || mr.HGet(RouteEditionsKey, "r.eu.dylaris.com") != "bedrock" {
		t.Fatalf("editions: s=%q r=%q", mr.HGet(RouteEditionsKey, "s.eu.dylaris.com"), mr.HGet(RouteEditionsKey, "r.eu.dylaris.com"))
	}
	if mr.Exists(FloodgateKeyRedisKey(LinkRouteBedrockSubject("owner-1", "r.eu.dylaris.com"))) {
		t.Fatal("an unreadable key was published")
	}
}
