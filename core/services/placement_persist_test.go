package services

import (
	"context"
	"testing"

	"dylaris-core/models"

	"github.com/redis/go-redis/v9"
)

// Storage placement was written to Redis alone, and Redis here runs without
// persistence: a restart put every node back on auto placement, new servers
// landed on the disk with the most free space (the system disk included), and
// the panel showed the setting as never saved. The database owns it now and the
// publisher re-asserts it.
func TestStoragePlacementSurvivesARedisRestart(t *testing.T) {
	ctx := context.Background()
	node := models.Node{ID: 7, Token: "tok-7"}
	manual := NodePlacement{Mode: "manual", Order: []string{"/mnt/fast"}}

	tests := []struct {
		name      string
		save      func(st *settingsStub, rdb *redis.Client) error
		key       string
		seedRedis string // a value an older Core left in Redis only
		want      string
	}{
		{
			name: "node policy saved through Core",
			save: func(st *settingsStub, rdb *redis.Client) error { return SaveNodePlacement(ctx, st, rdb, node, manual) },
			key:  NodePlacementKey(node.Token),
			want: `{"mode":"manual","order":["/mnt/fast"]}`,
		},
		{
			name: "fleet default saved through Core",
			save: func(st *settingsStub, rdb *redis.Client) error { return SaveFleetPlacement(ctx, st, rdb, manual) },
			key:  FleetPlacementKey,
			want: `{"mode":"manual","order":["/mnt/fast"]}`,
		},
		{
			name:      "node policy an older Core kept in Redis is adopted",
			key:       NodePlacementKey(node.Token),
			seedRedis: `{"mode":"manual","order":["/old"]}`,
			want:      `{"mode":"manual","order":["/old"]}`,
		},
		{
			name:      "fleet default an older Core kept in Redis is adopted",
			key:       FleetPlacementKey,
			seedRedis: `{"mode":"manual","order":["/old"]}`,
			want:      `{"mode":"manual","order":["/old"]}`,
		},
		{
			name: "nothing saved publishes nothing",
			key:  NodePlacementKey(node.Token),
		},
	}
	for _, tt := range tests {
		rdb := newQueueTestRedis(t)
		st := &settingsStub{values: map[string]string{}, nodes: []models.Node{node}}
		if tt.save != nil {
			if err := tt.save(st, rdb); err != nil {
				t.Fatalf("%s: save: %v", tt.name, err)
			}
		}
		if tt.seedRedis != "" {
			rdb.Set(ctx, tt.key, tt.seedRedis, 0)
			PublishStoragePlacements(ctx, st, rdb) // the first tick after the upgrade
		}
		if err := rdb.FlushAll(ctx).Err(); err != nil { // what a Redis restart does
			t.Fatalf("flush: %v", err)
		}
		PublishStoragePlacements(ctx, st, rdb)

		got, _ := rdb.Get(ctx, tt.key).Result()
		if got != tt.want {
			t.Errorf("%s: %s after a Redis restart = %q, want %q", tt.name, tt.key, got, tt.want)
		}
	}
}
