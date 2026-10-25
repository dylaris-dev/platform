package main

import "testing"

// A start must never wait on the reconciler: on an external node none runs,
// and two starts back to back need one round, not two.
func TestKickNetPolicyNeverBlocks(t *testing.T) {
	dm := &DockerManager{netPolicyKick: make(chan struct{}, 1)}
	dm.kickNetPolicy()
	dm.kickNetPolicy()
	if len(dm.netPolicyKick) != 1 {
		t.Fatalf("pending kicks = %d, want 1", len(dm.netPolicyKick))
	}
	(&DockerManager{}).kickNetPolicy() // no channel at all: a no-op
}
