package services

import (
	"encoding/json"
	"testing"
)

// A node's batch named any UUID it liked, as many times as it liked, and every
// one became a history row - a BYON customer's machine could write another
// tenant's CPU, memory and player history.
func TestStatsRowsKeepOnlyTheNodesOwnServersOnce(t *testing.T) {
	batch := statsBatchPayload{TS: 1, Stats: []statsBatchItem{
		{UUID: "mine", CPU: 1},
		{UUID: "someone-elses", CPU: 2},
		{UUID: "mine", CPU: 3},
	}}
	rows := statsRows(batch, map[string]bool{"mine": true})
	if len(rows) != 1 || rows[0].ServerUUID != "mine" || rows[0].CPU != 1 {
		t.Fatalf("rows = %+v, want one row for the node's own server", rows)
	}
	if rows := statsRows(batch, nil); len(rows) != 0 {
		t.Fatalf("rows with no known servers = %+v, want none", rows)
	}
}

// The node sends javaHeapUsed in every batch item; it was dropped. 0 means the
// node has seen no GC yet and must be stored as unknown, not as an empty heap.
func TestStatsRowsCarryJavaHeap(t *testing.T) {
	var batch statsBatchPayload
	if err := json.Unmarshal([]byte(`{"ts":1,"stats":[{"uuid":"a","javaHeapUsed":1536},{"uuid":"b","javaHeapUsed":0}]}`), &batch); err != nil {
		t.Fatal(err)
	}
	rows := statsRows(batch, map[string]bool{"a": true, "b": true})
	if len(rows) != 2 || rows[0].JavaHeapUsed == nil || *rows[0].JavaHeapUsed != 1536 {
		t.Fatalf("rows = %+v, want a with heap 1536", rows)
	}
	if rows[1].JavaHeapUsed != nil {
		t.Errorf("b heap = %d, want nil for an unknown heap", *rows[1].JavaHeapUsed)
	}
}

// players/max_players are INT columns and the whole batch goes in one
// transaction: one out-of-range figure from a tenant's server made Postgres
// refuse every row on that node, retried until the batch was dropped.
func TestStatsRowsBoundPlayersToInt4(t *testing.T) {
	batch := statsBatchPayload{TS: 1, Stats: []statsBatchItem{
		{UUID: "big", Players: 3000000000, MaxPlayers: 1 << 40},
		{UUID: "neg", Players: -1, MaxPlayers: -7},
		{UUID: "ok", Players: 3, MaxPlayers: 20},
	}}
	rows := statsRows(batch, map[string]bool{"big": true, "neg": true, "ok": true})
	want := map[string][2]int{"big": {2147483647, 2147483647}, "neg": {0, 0}, "ok": {3, 20}}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want 3", rows)
	}
	for _, r := range rows {
		if w := want[r.ServerUUID]; r.Players != w[0] || r.MaxPlayers != w[1] {
			t.Errorf("%s: players %d/%d, want %d/%d", r.ServerUUID, r.Players, r.MaxPlayers, w[0], w[1])
		}
	}
}
