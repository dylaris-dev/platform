package main

import (
	"os"
	"strings"
	"testing"
)

// One connect per Core at a time: the connection is registered only after
// auth, and a Core hanging in auth got a new attempt from every scan.
func TestMeshConnectsToACoreOnceAtATime(t *testing.T) {
	m := NewMeshManager("tok", nil, nil)
	if !m.claimConnect("core-1") {
		t.Fatal("the first connect was refused")
	}
	if m.claimConnect("core-1") {
		t.Fatal("a second connect started while the first was running")
	}
	if !m.claimConnect("core-2") {
		t.Fatal("another Core was blocked")
	}
	m.mu.Lock()
	delete(m.connecting, "core-1")
	m.connections["core-1"] = &coreConnection{}
	m.mu.Unlock()
	if m.claimConnect("core-1") {
		t.Fatal("a connect started next to a registered connection")
	}
}

// A connection that ended removes only itself; it removed its successor's
// entry, and the next scan opened yet another stream.
func TestMeshDropsOnlyItsOwnConnection(t *testing.T) {
	m := NewMeshManager("tok", nil, nil)
	old, cur := &coreConnection{}, &coreConnection{}
	m.connections["core-1"] = cur
	m.dropConnection("core-1", old)
	if m.connections["core-1"] != cur {
		t.Fatal("an ended connection removed its successor")
	}
	m.dropConnection("core-1", cur)
	if _, ok := m.connections["core-1"]; ok {
		t.Fatal("a connection did not remove itself")
	}
}

// Pinned in the source: the handshake needs a live Core to hang.
func TestMeshAuthHasADeadline(t *testing.T) {
	b, err := os.ReadFile("grpc_mesh.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"time.AfterFunc(coreAuthTimeout, cancel)", "if !authDeadline.Stop() {", "if m.claimConnect(info.ID) {", "m.dropConnection(info.ID, cc)"} {
		if !strings.Contains(s, want) {
			t.Errorf("grpc_mesh.go no longer has %q", want)
		}
	}
}
