package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/store"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }

func newTestCache() (*linkKeyCache, *fakeClock) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	return &linkKeyCache{now: fc.now}, fc
}

func linkKey(revoked bool) store.WarpAPIKey {
	k := store.WarpAPIKey{ID: 7, NodeID: "link-abc", OwnerID: "u1"}
	if revoked {
		now := time.Now()
		k.RevokedAt = &now
	}
	return k
}

func TestLinkKeyCacheServesWithinTTL(t *testing.T) {
	c, fc := newTestCache()
	reads := 0
	load := func() (store.WarpAPIKey, error) { reads++; return linkKey(false), nil }
	for i := 0; i < 2; i++ {
		if _, err := c.getKey("h", load); err != nil {
			t.Fatal(err)
		}
		fc.t = fc.t.Add(4 * time.Second)
	}
	if reads != 1 {
		t.Fatalf("store reads = %d, want 1", reads)
	}
}

func TestLinkKeyCacheHonoursRevokeWithinTTL(t *testing.T) {
	c, fc := newTestCache()
	revoked := false
	load := func() (store.WarpAPIKey, error) { return linkKey(revoked), nil }
	k, _ := c.getKey("h", load)
	revoked = true // revoked by another replica: this one cannot know
	if k, _ = c.getKey("h", load); k.RevokedAt != nil {
		t.Fatal("stale entry expected before the TTL")
	}
	fc.t = fc.t.Add(linkKeyCacheTTL)
	if k, _ = c.getKey("h", load); k.RevokedAt == nil {
		t.Fatal("entry outlived the TTL")
	}

	// A revoke handled here drops at once.
	revoked = false
	fc.t = fc.t.Add(linkKeyCacheTTL)
	k, _ = c.getKey("h", load)
	revoked = true
	c.dropKeyID(k.ID)
	if k, _ = c.getKey("h", load); k.RevokedAt == nil {
		t.Fatal("local revoke did not drop the entry")
	}
	revoked = false
	c.dropKey("h")
	if k, _ = c.getKey("h", load); k.RevokedAt != nil {
		t.Fatal("dropKey did not drop the entry")
	}
}

func TestLinkKeyCacheHonoursCutOffWithinTTL(t *testing.T) {
	c, fc := newTestCache()
	cut := false
	load := func() (bool, error) { return cut, nil }
	c.cutOff("u1", load)
	cut = true
	if got, _ := c.cutOff("u1", load); got {
		t.Fatal("stale answer expected before the TTL")
	}
	fc.t = fc.t.Add(linkKeyCacheTTL)
	if got, _ := c.cutOff("u1", load); !got {
		t.Fatal("cut-off not seen after the TTL")
	}
	cut = false
	c.dropOwner("u1")
	if got, _ := c.cutOff("u1", load); got {
		t.Fatal("dropOwner did not drop the entry")
	}
}

func TestLinkKeyCacheDoesNotCacheErrors(t *testing.T) {
	c, _ := newTestCache()
	boom := errors.New("db down")
	fail := true
	reads := 0
	load := func() (store.WarpAPIKey, error) {
		reads++
		if fail {
			return store.WarpAPIKey{}, boom
		}
		return linkKey(false), nil
	}
	if _, err := c.getKey("h", load); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	fail = false
	if _, err := c.getKey("h", load); err != nil {
		t.Fatal(err)
	}
	if reads != 2 {
		t.Fatalf("reads = %d, want 2 (error must not be cached)", reads)
	}
	cutFail := true
	cl := func() (bool, error) {
		if cutFail {
			return false, boom
		}
		return true, nil
	}
	c.cutOff("u1", cl)
	cutFail = false
	if got, err := c.cutOff("u1", cl); err != nil || !got {
		t.Fatalf("cut-off error was cached: %v %v", got, err)
	}
}

func TestLinkKeyCacheOnlyCachesLinkKits(t *testing.T) {
	c, _ := newTestCache()
	reads := 0
	load := func() (store.WarpAPIKey, error) {
		reads++
		return store.WarpAPIKey{ID: 1, NodeID: "node-x", OwnerID: "u1"}, nil
	}
	c.getKey("h", load)
	c.getKey("h", load)
	if reads != 2 {
		t.Fatalf("reads = %d, want 2: node keys must not be cached", reads)
	}
}

type countingStore struct {
	store.Store
	keyReads, billReads, revokeReads, settingReads int
}

func (s *countingStore) GetWarpAPIKeyByHash(string) (*store.WarpAPIKey, error) {
	s.keyReads++
	return s.Store.GetWarpAPIKeyByNodeID("link-xyz") // the routeOnlyKit fixture's kit
}

func (s *countingStore) GetUserBilling(id string) (*store.UserBilling, error) {
	s.billReads++
	return s.Store.GetUserBilling(id)
}

func (s *countingStore) GetWarpAPIKeyByNodeID(id string) (*store.WarpAPIKey, error) {
	s.revokeReads++
	return s.Store.GetWarpAPIKeyByNodeID(id)
}

func (s *countingStore) GetSetting(k string) (string, error) {
	s.settingReads++
	return s.Store.GetSetting(k)
}

// A link beats every 5 s. Drives the real middleware + LinkHeartbeat for a
// minute of beats and counts every store read.
func TestHeartbeatDBReadsPerMinute(t *testing.T) {
	h, fs, _, _ := routeOnlyKit(t)
	st := &countingStore{Store: fs}
	h.d.Store = st
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	h.links.now = fc.now
	for i := 0; i < 12; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/warp/link/heartbeat", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer link-abc")
		rec := httptest.NewRecorder()
		h.WarpAPIKeyMiddleware(h.LinkHeartbeat)(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("beat %d: status = %d (%s)", i, rec.Code, rec.Body.String())
		}
		fc.t = fc.t.Add(5 * time.Second)
	}
	t.Logf("key %d billing %d settings %d linkRevoked %d", st.keyReads, st.billReads, st.settingReads, st.revokeReads)
	// 12 beats span 55 s: the 10 s TTL allows a reload every third beat at most.
	if st.keyReads > 6 || st.billReads > 6 || st.settingReads > 6 {
		t.Errorf("cached reads too high: key %d, billing %d, settings %d", st.keyReads, st.billReads, st.settingReads)
	}
	// Intentional: linkRevoked is the pre/post-write revoke re-check and is
	// never cached, two reads per beat.
	if st.revokeReads != 24 {
		t.Errorf("linkRevoked reads = %d, want 24", st.revokeReads)
	}
}
