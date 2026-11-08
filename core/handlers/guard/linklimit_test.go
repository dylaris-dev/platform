package guard

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func linkID(r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.Header.Get("X-Test-Key"))
	return id, err == nil
}

func linkCall(h http.HandlerFunc, keyID int) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("X-Test-Key", strconv.Itoa(keyID))
	r.RemoteAddr = "203.0.113.7:1234" // every link behind one NAT
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func TestManyLinksBehindOneIPAreNotThrottled(t *testing.T) {
	_, rdb := keyLimitSetup(t)
	h := NewKeyClassLimiter(rdb).LimitLink(linkID, ok)
	for k := 1; k <= 40; k++ {
		for i := 0; i < 12; i++ {
			if w := linkCall(h, k); w.Code != http.StatusNoContent {
				t.Fatalf("key %d call %d = %d, want 204", k, i+1, w.Code)
			}
		}
	}
}

func TestOneLinkKeyIsLimited(t *testing.T) {
	_, rdb := keyLimitSetup(t)
	h := NewKeyClassLimiter(rdb).LimitLink(linkID, ok)
	for i := 0; i < LinkMaxPerMinute; i++ {
		if w := linkCall(h, 1); w.Code != http.StatusNoContent {
			t.Fatalf("call %d = %d, want 204", i+1, w.Code)
		}
	}
	w := linkCall(h, 1)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("301st call = %d, want 429", w.Code)
	}
	if s, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || s < 1 || s > 60 {
		t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
	}
	if w := linkCall(h, 2); w.Code != http.StatusNoContent {
		t.Fatalf("another key = %d, want 204", w.Code)
	}
}

func TestLinkLimitResetsNextMinute(t *testing.T) {
	_, rdb := keyLimitSetup(t)
	l := NewKeyClassLimiter(rdb)
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	h := l.LimitLink(linkID, ok)
	for i := 0; i <= LinkMaxPerMinute; i++ {
		linkCall(h, 1)
	}
	now = now.Add(time.Minute)
	if w := linkCall(h, 1); w.Code != http.StatusNoContent {
		t.Fatalf("next minute = %d, want 204", w.Code)
	}
}

func TestLinkLimitSharedAcrossReplicas(t *testing.T) {
	_, rdb := keyLimitSetup(t)
	a := NewKeyClassLimiter(rdb).LimitLink(linkID, ok)
	b := NewKeyClassLimiter(rdb).LimitLink(linkID, ok)
	for i := 0; i < LinkMaxPerMinute; i++ {
		h := a
		if i%2 == 1 {
			h = b
		}
		if w := linkCall(h, 1); w.Code != http.StatusNoContent {
			t.Fatalf("call %d = %d, want 204", i+1, w.Code)
		}
	}
	if w := linkCall(a, 1); w.Code != http.StatusTooManyRequests {
		t.Fatalf("301st call = %d, want 429", w.Code)
	}
}

func TestLinkLimitRedisDownFailsOpen(t *testing.T) {
	mr, rdb := keyLimitSetup(t)
	h := NewKeyClassLimiter(rdb).LimitLink(linkID, ok)
	mr.Close()
	for i := 0; i < 3; i++ {
		if w := linkCall(h, 1); w.Code != http.StatusNoContent {
			t.Fatalf("redis down call %d = %d, want 204", i+1, w.Code)
		}
	}
}

func TestLinkLimitWithoutKeyIsRefused(t *testing.T) {
	_, rdb := keyLimitSetup(t)
	h := NewKeyClassLimiter(rdb).LimitLink(linkID, ok)
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", w.Code)
	}
}
