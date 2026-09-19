package handlers

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestNodeReleaseOlderThan(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	st := &AppState{Redis: rdb}
	ctx := context.Background()

	cases := []struct {
		name      string
		heartbeat string // "" = no heartbeat at all
		want      bool
	}{
		{"older release", `{"releaseVersion":"2026.09.19.3"}`, true},
		{"much older", `{"releaseVersion":"2026.08.01"}`, true},
		{"the release itself", `{"releaseVersion":"` + technicSince + `"}`, false},
		{"newer", `{"releaseVersion":"2026.10.01"}`, false},
		{"unstamped dev build", `{"releaseVersion":""}`, false},
		{"unparseable", `{"releaseVersion":"banana"}`, false},
		{"no heartbeat", "", false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token := "node-" + string(rune('a'+i))
			if c.heartbeat != "" {
				mr.Set("dylaris:discovery:"+token, c.heartbeat)
			}
			if got := nodeReleaseOlderThan(ctx, st, token, technicSince); got != c.want {
				t.Errorf("nodeReleaseOlderThan = %v, want %v", got, c.want)
			}
		})
	}
	if nodeReleaseOlderThan(ctx, &AppState{}, "x", technicSince) {
		t.Error("no Redis must not refuse")
	}
}
