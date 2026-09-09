package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Link sidecar image updates.
//
// The reconciler spawns the Link once and keys its work on a signature built
// from the node id, the creds and the image REFERENCE. A tag like ":latest"
// never changes, so once the container existed the node stopped looking - which
// pinned every Link to whatever ":latest" meant on the day it first started.
// Version skew between a Link and the edges it talks to does not fail loudly; it
// misbehaves subtly, and it cost a long live debugging session to find. Hence:
// compare the resolved IMAGE, not the tag string.

const (
	linkUpdateNotify   = "notify"
	linkUpdateAutoIdle = "auto_idle"
	linkUpdateAuto     = "auto"

	// linkMgmtPort is the Link's management server (link/link.go, LINK_PORT
	// default :25540). buildLinkEnv does not override it, so the node-managed
	// Link always listens here.
	linkMgmtPort = 25540

	linkImageCheckIntervalDefault = 15 * time.Minute
	linkImageCheckIntervalMin     = 1 * time.Minute
)

// resolveLinkUpdatePolicy decides how this node applies a Link image update.
//
// An external node (BYON / home / customer hardware) is ALWAYS "auto", and that
// is deliberately not configurable. There is no operator on the other end to
// press a button, and the failure mode of a stale Link is not an outage but
// subtly wrong behaviour that only the platform owner can diagnose. A switch
// here would let the one person who cannot debug it turn it off. The cost of
// being wrong is bounded and small: replacing the container interrupts that
// node's players for roughly 10-30 seconds.
func resolveLinkUpdatePolicy(setting string, external bool) string {
	if external {
		return linkUpdateAuto
	}
	switch setting {
	case linkUpdateNotify, linkUpdateAuto, linkUpdateAutoIdle:
		return setting
	default:
		return linkUpdateAutoIdle
	}
}

// linkUpdateDecision reports whether to replace the Link container now, plus a
// reason for the log. sessionsKnown is false when the Link's management server
// could not be reached.
//
// An unknown session count applies the update. The alternative - refuse until
// the count is known - means a Link whose management server is wedged can never
// be replaced, which is precisely the state an update is most likely to fix. The
// downside is bounded: at worst it interrupts players who were connected through
// a Link that was not answering anyway.
func linkUpdateDecision(policy string, drifted bool, sessions int, sessionsKnown bool) (apply bool, reason string) {
	if !drifted {
		return false, ""
	}
	switch policy {
	case linkUpdateNotify:
		return false, "a newer Link image is available; policy is notify, so it will not be applied automatically"
	case linkUpdateAuto:
		return true, "applying the newer Link image now (policy: auto)"
	default: // auto_idle
		if !sessionsKnown {
			return true, "applying the newer Link image: the Link did not report its session count, so it is not serving anyone reliably"
		}
		if sessions == 0 {
			return true, "applying the newer Link image: no player sessions are active"
		}
		return false, fmt.Sprintf("a newer Link image is available but %d player session(s) are active; waiting for idle", sessions)
	}
}

// linkImageCheckInterval clamps the configured check interval. Below a minute
// this is just registry traffic; the manifest request is cheap but not free, and
// nothing about a Link update is urgent to the second.
func linkImageCheckInterval(minutes int) time.Duration {
	if minutes <= 0 {
		return linkImageCheckIntervalDefault
	}
	d := time.Duration(minutes) * time.Minute
	if d < linkImageCheckIntervalMin {
		return linkImageCheckIntervalMin
	}
	return d
}

// linkSessionCount asks the Link how many player sessions it is carrying.
// Returns ok=false when the Link could not be reached or did not answer with a
// count, which the caller must not read as "zero".
func linkSessionCount(endpoint string) (int, bool) {
	if endpoint == "" {
		return 0, false
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + endpoint + "/health")
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	// A 503 still carries a body: the Link reports itself unhealthy when Redis is
	// down, but it can still be carrying sessions, so the count is read either way.
	var body struct {
		Sessions *int `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Sessions == nil {
		// An older Link has no "sessions" field. Unknown, not zero - treating a
		// missing field as idle would restart it mid-game.
		return 0, false
	}
	return *body.Sessions, true
}

// Platform-level Link update settings, mirrored from Redis by loadModesFromRedis.
// They are advisory only: an external node ignores the policy entirely (see
// resolveLinkUpdatePolicy), so a node stays correct even when it cannot read them.
var (
	linkUpdateSettingsMu    sync.RWMutex
	linkUpdatePolicySetting string
	linkUpdateIntervalMin   int
)

func setLinkUpdateSettings(policy string, intervalMinutes int) {
	linkUpdateSettingsMu.Lock()
	linkUpdatePolicySetting = policy
	linkUpdateIntervalMin = intervalMinutes
	linkUpdateSettingsMu.Unlock()
}

func getLinkUpdatePolicySetting() string {
	linkUpdateSettingsMu.RLock()
	defer linkUpdateSettingsMu.RUnlock()
	return linkUpdatePolicySetting
}

func getLinkUpdateIntervalMinutes() int {
	linkUpdateSettingsMu.RLock()
	defer linkUpdateSettingsMu.RUnlock()
	return linkUpdateIntervalMin
}

// linkUpdatePending records the last observed drift so the heartbeat can report
// it and the panel can offer a manual trigger.
var (
	linkUpdatePendingMu sync.RWMutex
	linkImageRunning    string
	linkImageAvailable  string
)

func setLinkImageState(running, available string) {
	linkUpdatePendingMu.Lock()
	linkImageRunning, linkImageAvailable = running, available
	linkUpdatePendingMu.Unlock()
}

// GetLinkImageState reports the running image, the available image, and whether
// an update is pending. Unknown on either side is NOT drift.
func GetLinkImageState() (running, available string, updateAvailable bool) {
	linkUpdatePendingMu.RLock()
	defer linkUpdatePendingMu.RUnlock()
	return linkImageRunning, linkImageAvailable,
		linkImageRunning != "" && linkImageAvailable != "" && linkImageRunning != linkImageAvailable
}

// checkLinkImage refreshes the image reference, records the result, and applies
// the update if the resolved policy allows it right now.
func checkLinkImage(dm *DockerManager, secret, proof string) {
	running, available := dm.LinkImageStatus(linkImage)
	setLinkImageState(running, available)
	if running == "" || available == "" || running == available {
		return
	}

	policy := resolveLinkUpdatePolicy(getLinkUpdatePolicySetting(), nodeExternal)
	sessions, known := linkSessionCount(dm.LinkEndpoint())
	apply, reason := linkUpdateDecision(policy, true, sessions, known)
	if reason != "" {
		log.Printf("link: %s", reason)
	}
	if apply {
		applyLinkImageUpdate(dm, nodeID, secret, proof)
	}
}

// TriggerLinkImageUpdate applies the current Link image regardless of policy.
// This is the panel's manual button: an operator asking for it has already been
// told it interrupts active sessions.
func TriggerLinkImageUpdate(dm *DockerManager) {
	secret, proof := getLinkCreds()
	if !linkWanted(getRoutingMode(), secret, proof, linkImage) {
		log.Println("link: manual update requested but this node does not run a Link sidecar")
		return
	}
	log.Println("link: manual Link image update requested")
	applyLinkImageUpdate(dm, nodeID, secret, proof)
	running, available := dm.LinkImageStatus(linkImage)
	setLinkImageState(running, available)
}

// applyLinkImageUpdate replaces the Link container with the current image.
//
// Replace, not Ensure: this path has already decided. The drift check got here
// because the image genuinely moved, and the manual button got here because an
// operator asked for it having been told it interrupts sessions. Making it
// idempotent would turn "apply now" into a silent no-op.
func applyLinkImageUpdate(dm *DockerManager, nodeID, secret, proof string) {
	if err := dm.ReplaceLinkContainer(linkImage, nodeID, secret, proof); err != nil {
		log.Printf("link: image update failed: %v", err)
		return
	}
	log.Println("link: Link sidecar replaced with the current image")
}
