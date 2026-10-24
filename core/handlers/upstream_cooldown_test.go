package handlers

import (
	"net/http"
	"testing"
	"time"
)

func TestUpstreamCooldownTrip(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{"retry-after", http.Header{"Retry-After": {"12"}}, 12 * time.Second},
		{"modrinth reset", http.Header{"X-Ratelimit-Reset": {"7"}}, 7 * time.Second},
		{"nothing said", http.Header{}, upstreamCooldownDefault},
		{"capped", http.Header{"Retry-After": {"86400"}}, upstreamCooldownMax},
		{"http-date is not seconds", http.Header{"Retry-After": {"Wed, 21 Oct 2026 07:28:00 GMT"}}, upstreamCooldownDefault},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cd upstreamCooldown
			if got := cd.trip(c.header, now); got != c.want {
				t.Errorf("trip = %v, want %v", got, c.want)
			}
			if left, ok := cd.blocked(now.Add(c.want - time.Second)); !ok || left != time.Second {
				t.Errorf("blocked just before the end = %v %v", left, ok)
			}
			if _, ok := cd.blocked(now.Add(c.want)); ok {
				t.Error("still blocked at the end")
			}
		})
	}
}
