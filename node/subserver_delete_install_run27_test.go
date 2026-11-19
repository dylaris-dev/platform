package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// R27-01: a sub-server delete ran beside a setup, reinstall or version move of
// the same server, renamed the slot away under the install or had it installed
// again afterwards. It now waits for both before it touches the tree.
func TestASubServerDeleteWaitsForTheInstall(t *testing.T) {
	prevPoll, prevMax := treeChangePoll, treeChangeWaitMax
	t.Cleanup(func() { treeChangePoll, treeChangeWaitMax = prevPoll, prevMax })
	treeChangePoll = 5 * time.Millisecond

	for _, tc := range []struct {
		name       string
		install    bool // holds lockServerInstall (create, setup, reinstall)
		software   bool // holds softwareKey (version move)
		busyFor    time.Duration
		waitMax    time.Duration
		wantLeaves bool
	}{
		{"idle server", false, false, 0, time.Second, true},
		{"install ends", true, false, 50 * time.Millisecond, 5 * time.Second, true},
		{"version move ends within the bound", false, true, 50 * time.Millisecond, 5 * time.Second, true},
		{"version move outlasts the bound", false, true, time.Hour, 30 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uuid := "r27-01-" + strings.ReplaceAll(tc.name, " ", "-")
			treeChangeWaitMax = tc.waitMax
			released := make(chan struct{})
			var stop func() bool
			if tc.install {
				unlock := lockServerInstall(uuid)
				timer := time.AfterFunc(tc.busyFor, func() { close(released); unlock() })
				stop = timer.Stop
			}
			if tc.software {
				if !commandsInFlight.enter(softwareKey(uuid)) {
					t.Fatal("software key taken")
				}
				timer := time.AfterFunc(tc.busyFor, func() { close(released); commandsInFlight.leave(softwareKey(uuid)) })
				stop = timer.Stop
				t.Cleanup(func() {
					if timer.Stop() {
						commandsInFlight.leave(softwareKey(uuid))
					}
				})
			}
			unlock, reason := waitInstallOfServer(context.Background(), uuid)
			if got := reason == ""; got != tc.wantLeaves {
				t.Fatalf("entered = %v (reason %q), want %v", got, reason, tc.wantLeaves)
			}
			if tc.wantLeaves && stop != nil {
				select {
				case <-released:
				default:
					t.Fatal("the delete went ahead while the install still held the server")
				}
			}
			if unlock != nil {
				unlock()
			}
		})
	}
}

func TestDeleteSubServerWaitsForTheInstall(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	i := strings.Index(s, "\tcase \"delete_sub_server\":")
	j := strings.Index(s[i:], "\tcase \"reinstall\":")
	if i < 0 || j < 0 {
		t.Fatal("the delete_sub_server case moved; move this assertion with it")
	}
	body := s[i : i+j]
	tree := strings.Index(body, "waitTreeChangeOfServer(ctx, cmd.Config.UUID)")
	inst := strings.Index(body, "waitInstallOfServer(ctx, cmd.Config.UUID)")
	if inst < 0 || inst < tree {
		t.Error("delete_sub_server does not wait for the install of its server after the tree wait")
	}
}
