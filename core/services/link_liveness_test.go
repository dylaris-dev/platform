package services

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The one thing this answer must never do is turn "Core could not ask" into
// "your machine is not connected". Everything else here is about asking once
// for a whole page rather than once per row.

func livenessRedis(t *testing.T, live ...string) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	for _, tok := range live {
		mr.Set("online_link:"+tok, "1")
		mr.SetTTL("online_link:"+tok, 15*time.Second)
	}
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { c.Close() })
	return c, mr
}

func TestLinkOnline_AnswersPerToken(t *testing.T) {
	rdb, _ := livenessRedis(t, "live-one", "live-two")

	got := LinkOnline(context.Background(), rdb, []string{"live-one", "dead", "live-two"})

	for tok, want := range map[string]bool{"live-one": true, "live-two": true, "dead": false} {
		if v, ok := got[tok]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v", tok, v, ok, want)
		}
	}
}

// A key that expired is exactly what a Link that stopped looks like: the answer
// has to flip without anyone deleting anything.
func TestLinkOnline_AnExpiredKeyIsOffline(t *testing.T) {
	rdb, mr := livenessRedis(t, "gone")

	mr.FastForward(20 * time.Second)

	if got := LinkOnline(context.Background(), rdb, []string{"gone"}); got["gone"] {
		t.Fatalf("a link whose key expired reads as online: %v", got)
	}
}

// nil is the contract for "not known". A caller that renders it as offline would
// tell a customer their machine is down because OUR Redis is.
func TestLinkOnline_UnknownRatherThanOfflineWhenItCannotAsk(t *testing.T) {
	rdb, mr := livenessRedis(t, "live")
	mr.Close() // the server is gone, so every read fails

	if got := LinkOnline(context.Background(), rdb, []string{"live", "other"}); got != nil {
		t.Fatalf("got %v, want nil for a Redis that cannot answer", got)
	}
	if got := LinkOnline(context.Background(), nil, []string{"live"}); got != nil {
		t.Fatalf("got %v, want nil without a Redis at all", got)
	}
	if got := LinkOnline(context.Background(), rdb, nil); got != nil {
		t.Fatalf("got %v, want nil for no tokens", got)
	}
}

// One round trip for a page, and a token repeated across rows asked once.
func TestLinkOnline_AsksEachTokenOnce(t *testing.T) {
	rdb, mr := livenessRedis(t, "shared")

	got := LinkOnline(context.Background(), rdb, []string{"shared", "shared", "", "other"})

	if !got["shared"] || got["other"] {
		t.Fatalf("got %v, want shared online and other offline", got)
	}
	if _, ok := got[""]; ok {
		t.Errorf("an empty token was asked about: %v", got)
	}
	if mr.Addr() == "" {
		t.Fatal("miniredis went away mid-test")
	}
}
