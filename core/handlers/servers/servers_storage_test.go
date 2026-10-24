package servers

import (
	"encoding/json"
	"testing"
)

// TestStripStorageServerUUIDs proves the co-tenant identifiers are removed from
// what /servers/{id}/storage-path returns, while everything the picker renders
// (path, capacity, server_count) survives. The input is the real heartbeat
// shape: node/storage.go's StorageInfo, one entry per configured path.
func TestStripStorageServerUUIDs(t *testing.T) {
	raw := `[
		{"path":"/mnt/ssd","total_bytes":100,"free_bytes":40,"used_bytes":60,"server_count":2,
		 "server_uuids":["aaaa-1111","bbbb-2222"],"quota_enforceable":true},
		{"path":"/mnt/hdd","total_bytes":900,"free_bytes":800,"used_bytes":100,"server_count":1,
		 "server_uuids":["cccc-3333"],"quota_enforceable":false}
	]`
	var decoded interface{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("fixture does not decode: %v", err)
	}

	out, err := json.Marshal(stripStorageServerUUIDs(decoded))
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	var entries []map[string]interface{}
	if err := json.Unmarshal(out, &entries); err != nil {
		t.Fatalf("result is not a list of objects: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 - stripping must not drop paths", len(entries))
	}
	for _, e := range entries {
		if _, present := e["server_uuids"]; present {
			t.Errorf("entry %v still carries server_uuids", e["path"])
		}
		// The fields the migrate picker actually reads must survive.
		for _, k := range []string{"path", "total_bytes", "free_bytes", "used_bytes", "server_count"} {
			if _, present := e[k]; !present {
				t.Errorf("entry %v lost %q", e["path"], k)
			}
		}
	}
}

// TestStripStorageServerUUIDsPassesThroughUnexpectedShapes: the payload is
// whatever the node sent, so a shape this function does not recognize must come
// back unchanged rather than becoming nil - the endpoint would otherwise report
// "no storage paths" for a node whose heartbeat format drifted.
func TestStripStorageServerUUIDsPassesThroughUnexpectedShapes(t *testing.T) {
	for _, in := range []interface{}{
		nil,
		"a string",
		map[string]interface{}{"path": "/mnt/ssd"},
		[]interface{}{"not an object", 42.0},
	} {
		got := stripStorageServerUUIDs(in)
		a, _ := json.Marshal(in)
		b, _ := json.Marshal(got)
		if string(a) != string(b) {
			t.Errorf("input %s came back as %s", a, b)
		}
	}
}
