package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// LimitUser counts per signed-in user: one user's budget is not another's,
// and the same IP serves both.
func TestLimitUserCountsPerUser(t *testing.T) {
	l := NewIPRateLimiter()
	h := l.LimitUser(2, func(w http.ResponseWriter, r *http.Request) {})
	call := func(user string) int {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		r = r.WithContext(context.WithValue(r.Context(), "userID", user))
		rec := httptest.NewRecorder()
		h(rec, r)
		return rec.Code
	}
	for i := 0; i < 2; i++ {
		if c := call("u1"); c != http.StatusOK {
			t.Fatalf("u1 call %d = %d", i, c)
		}
	}
	if c := call("u1"); c != http.StatusTooManyRequests {
		t.Fatalf("u1 over budget = %d, want 429", c)
	}
	if c := call("u2"); c != http.StatusOK {
		t.Fatalf("u2 = %d, want its own budget", c)
	}
}
