package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// powerActions are the commands that only express what a server should be
// doing now.
var powerActions = map[string]bool{"start": true, "stop": true, "kill": true, "restart": true}

// staleCommandAge is how old a power action must be before it is held against
// the current intent. A fresh one is never second-guessed: Core may queue it a
// few seconds before the matching desired state reaches Redis, and a node clock
// a little off must not make a fresh command look old.
const staleCommandAge = 10 * time.Minute

func desiredStateKey(uuid string) string {
	return fmt.Sprintf("dylaris:server:%s:desired_state", uuid)
}

// stalePowerAction reports a power action queued long enough ago that it
// contradicts what the server should be now. Core queues commands for a node
// that is away, and they all ran on its return: a start the user had given up
// on hours ago, then the stop that replaced it, in whatever order the workers
// took them. Only a contradiction is dropped - a stop still wanted is carried
// out however late, since nothing else would stop the server - and with no
// desired state to compare against (Core away too) everything runs as before.
func stalePowerAction(action string, issuedAt int64, now time.Time, desired string, desiredErr error) bool {
	if issuedAt == 0 || desiredErr != nil || now.Sub(time.Unix(issuedAt, 0)) < staleCommandAge {
		return false
	}
	switch action {
	case "start", "restart":
		return desired != "online"
	case "stop", "kill":
		return desired == "online"
	}
	return false
}

// isExpressCommand picks the commands the queue runs on their own workers.
func isExpressCommand(payload []byte) bool {
	var c struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return false
	}
	return c.Action == "stop" || c.Action == "kill"
}

// lastStopIssued holds, per server, when the newest stop or kill the node has
// begun was queued. Stop and kill run on workers of their own, so one can run
// before a start that was queued earlier and is still waiting for a worker; the
// start then brought the server back up against the user's last click.
var lastStopIssued sync.Map // uuid -> int64

// noteStop records a stop or kill that is being carried out.
func noteStop(uuid string, issuedAt int64) {
	if issuedAt == 0 {
		return
	}
	for {
		old, loaded := lastStopIssued.LoadOrStore(uuid, issuedAt)
		if !loaded || old.(int64) >= issuedAt {
			return
		}
		if lastStopIssued.CompareAndSwap(uuid, old, issuedAt) {
			return
		}
	}
}

// overtakenByStop reports a start, restart or switch queued before a stop or
// kill that has already run. The same second is left alone: which came first
// cannot be told.
func overtakenByStop(action, uuid string, issuedAt int64) bool {
	switch action {
	case "start", "restart", "switch_server":
	default:
		return false
	}
	if issuedAt == 0 {
		return false
	}
	v, ok := lastStopIssued.Load(uuid)
	return ok && v.(int64) > issuedAt
}
