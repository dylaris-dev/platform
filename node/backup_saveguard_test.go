package main

import "testing"

// The guard's whole job is that saving comes BACK on. These pin the two ways
// that can fail quietly: resuming twice (the deferred call plus an explicit
// one) sending the command twice, and a guard that was never armed sending it
// to a server that was not running - whose console queue is drained by the
// log-shipper inside the container, so a stray command would be read by the
// NEXT start rather than now.
func TestWorldSaveGuardResumesExactlyOnce(t *testing.T) {
	var sent []string
	g := &worldSaveGuard{uuid: "srv-1", send: func(c string) { sent = append(sent, c) }}

	g.resume()
	g.resume()

	if len(sent) != 1 || sent[0] != "save-on" {
		t.Fatalf("sent %v, want exactly one save-on", sent)
	}
}

func TestWorldSaveGuardSendsNothingWhenItNeverPaused(t *testing.T) {
	// What pauseWorldSaves returns for a server that is not running: no send,
	// so nothing to undo.
	g := &worldSaveGuard{uuid: "srv-1"}
	g.resume()
	if g.send != nil {
		t.Error("an unarmed guard should stay unarmed")
	}
}

func TestWorldSaveGuardNilIsSafe(t *testing.T) {
	// RunBackup defers resume() on whatever pauseWorldSaves returned; a nil
	// guard must not take the node down with it.
	var g *worldSaveGuard
	g.resume()
}
