package main

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// R38-01: since setup, reinstall and version move hold the server's power lock
// for their whole run, the boot-time ReconcileRedisEnv waited behind one such
// install while every server after it kept its dead Redis credentials. A busy
// server is now revisited after the rest of the list.
func TestEachUnderPowerLockRevisitsABusyServerLast(t *testing.T) {
	for _, tc := range []struct {
		name string
		busy string
		want []string
	}{
		{"first busy", "r38-a", []string{"r38-b", "r38-c", "r38-a"}},
		{"middle busy", "r38-b", []string{"r38-a", "r38-c", "r38-b"}},
		{"none busy", "", []string{"r38-a", "r38-b", "r38-c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var release func()
			if tc.busy != "" {
				r, ok := tryLockServerPower(tc.busy)
				if !ok {
					t.Fatal("power lock already held")
				}
				release = r
			}
			var mu sync.Mutex
			var got []string
			othersDone := make(chan struct{})
			done := make(chan struct{})
			list := []MCContainer{{UUID: "r38-a"}, {UUID: "r38-b"}, {UUID: "r38-c"}}
			go func() {
				eachUnderPowerLock(context.Background(), list, func(mc MCContainer) {
					mu.Lock()
					got = append(got, mc.UUID)
					if len(got) == len(list)-1 {
						close(othersDone)
					}
					mu.Unlock()
				})
				close(done)
			}()
			if release != nil {
				select {
				case <-othersDone:
				case <-time.After(5 * time.Second):
					release()
					t.Fatal("the other servers waited behind the busy one")
				}
				release()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("did not finish after the lock came free")
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("order %v, want %v", got, tc.want)
			}
		})
	}
}
