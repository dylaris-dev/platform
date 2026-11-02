package nodeops

import (
	"context"
	"encoding/json"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// seedHeartbeat writes the discovery key the node writes, so the version comes
// from the same place the What's New view reads it from rather than from a
// second notion of "how old is this node".
func seedHeartbeat(t *testing.T, m *miniredis.Miniredis, token, version string) {
	t.Helper()
	hb := services.NodeHeartbeat{ID: token, ReleaseVersion: version}
	raw, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	m.Set("dylaris:discovery:"+token, string(raw))
}

// Core and the node are deployed separately, so "new Core, old node" is a
// window every operator passes through - not an edge case. In it the node
// installs the mod correctly and reports nothing, and a row written as pending
// would stay pending for good, with the panel showing every mod on that server
// as mid-install.
func TestInstallStatusWaitsOnlyForANodeThatAnswers(t *testing.T) {
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})

	tests := []struct {
		name    string
		version string
		want    string
		why     string
	}{
		{
			name: "a node from this release reports", version: ModReportingSince,
			want: models.ServerModInstalling,
			why:  "the release that introduced reporting must itself be treated as reporting",
		},
		{
			name: "a newer node reports", version: "2027.01.01",
			want: models.ServerModInstalling,
		},
		{
			name: "an older node does not", version: "2026.08.28",
			want: models.ServerModInstalled,
			why:  "it installs correctly and says nothing; waiting for it waits forever",
		},
		{
			// A development build carries no RELEASE_VERSION. Recorded as
			// installed, which is recoverable: a report that does arrive still
			// names the attempt and corrects the row. The other direction
			// cannot be corrected by anything.
			name: "an unreported version counts as old", version: "",
			want: models.ServerModInstalled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := "node-" + tt.name
			seedHeartbeat(t, m, token, tt.version)
			if got := InstallStatusFor(context.Background(), rdb, token); got != tt.want {
				t.Errorf("InstallStatusFor = %q, want %q: %s", got, tt.want, tt.why)
			}
		})
	}
}

// A node with no heartbeat at all is not evidence of a new node.
func TestInstallStatusWithNoHeartbeatOrNoRedis(t *testing.T) {
	m := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	if got := InstallStatusFor(context.Background(), rdb, "unknown"); got != models.ServerModInstalled {
		t.Errorf("a node with no heartbeat = %q, want installed", got)
	}
	if got := InstallStatusFor(context.Background(), nil, "any"); got != models.ServerModInstalled {
		t.Errorf("no redis = %q, want installed", got)
	}
}
