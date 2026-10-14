package main

import (
	"fmt"
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
