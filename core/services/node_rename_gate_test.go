package services

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A node before 2026.10.11.7 strips characters from a rename's new name, so
// "b c.txt" lands on "bc.txt" and replaces it. Only a node that keeps the name
// may be sent one; an unknown or unparseable version counts as old.
func TestNodeRenamesFreely(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	for _, tc := range []struct {
		hb   string
		want bool
	}{
		{`{"releaseVersion":"2026.10.11.7"}`, true},
		{`{"releaseVersion":"2026.10.12"}`, true},
		{`{"releaseVersion":"2026.10.11.6"}`, false},
		{`{"releaseVersion":"garbage"}`, false},
		{"", false},
	} {
		mr.FlushAll()
		if tc.hb != "" {
			mr.Set("dylaris:discovery:tok", tc.hb)
		}
		if got := NodeRenamesFreely(ctx, rdb, "tok"); got != tc.want {
			t.Errorf("heartbeat %q: got %v, want %v", tc.hb, got, tc.want)
		}
	}
}
