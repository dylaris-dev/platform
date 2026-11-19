package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

// R37-03: the undo record Core parks for a version move outlived every move
// that landed, and Core's adoption reads it as a command still pending, so it
// stood down for the record's hour. The move deletes it once it has landed,
// and only the record it started with.
func TestALandedVersionMoveForgetsItsUndoRecord(t *testing.T) {
	const key = "dylaris:server:u1:version_update_undo"
	cases := []struct {
		name    string
		start   string // the record when the move began, "" for none
		now     string // the record when it landed, "" for none
		wantDel bool
	}{
		{name: "the record the move started with", start: `{"a":1}`, now: `{"a":1}`, wantDel: true},
		{name: "a newer dispatch's record", start: `{"a":1}`, now: `{"a":2}`},
		{name: "no record at the start", now: `{"a":2}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rdb := newMiniRedis(t)
			ctx := context.Background()
			if tc.now != "" {
				rdb.Set(ctx, key, tc.now, 0)
			}
			forgetValueIf(ctx, rdb, key, tc.start)
			n, _ := rdb.Exists(ctx, key).Result()
			if gone := n == 0; gone != (tc.wantDel || tc.now == "") {
				t.Errorf("record gone = %v, want %v", gone, tc.wantDel)
			}
		})
	}

	src, err := os.ReadFile("update_server_version.go")
	if err != nil {
		t.Fatal(err)
	}
	landed := between(t, string(src), "stash.discard()", "if err := recreate(cmd.Config)")
	if !strings.Contains(landed, "forgetValueIf(ctx, rdb, undoKey, undoAtStart)") {
		t.Error("a landed version move does not forget its undo record")
	}
}
