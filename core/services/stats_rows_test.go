package services

import "testing"

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
