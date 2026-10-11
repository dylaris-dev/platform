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
