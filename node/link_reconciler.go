package main

import (
	"context"
	"log"
	"time"
)

// startLinkReconciler manages the node's own Link sidecar. It (re)spawns Link when
// this node is gateway-routed, self-manages Link, and has its Core-delivered creds;
// it stops Link when those no longer hold. Runs on a 30s tick so late-arriving creds /
// a routing-mode flip / a cred rotation are picked up without a restart.
func startLinkReconciler(ctx context.Context, dm *DockerManager) {
	if !nodeManagesLink {
		// A node that stops managing the Link leaves the one it spawned behind,
		// and nothing else ever removes it: it would run forever beside the Link
		// that replaced it. Once, here, because the flag only changes with a
		// restart. RemoveOwnLinkContainer decides what counts as its own.
		dm.RemoveOwnLinkContainer(linkImage)
		return
	}
	var last string // signature of the last-applied spawn; "" = not running
	var nextImageCheck time.Time
	reconcile := func() {
		secret, proof := getLinkCreds()
		want := linkWanted(getRoutingMode(), secret, proof, linkImage)
		sig := nodeID + "|" + secret + "|" + proof + "|" + linkImage
		if !want {
			if last != "" {
				dm.StopLinkContainer()
				log.Println("link: Link sidecar stopped")
				last = ""
			}
			return
		}
		if sig != last {
			// The signature lives in this variable, so it is empty at every node
			// start and a boot always looks like a change. EnsureLinkContainer
			// is what makes that harmless: it compares the running container and
			// leaves a correct one alone, so the reconciler adopts it instead of
			// rebuilding it under the players on it.
			recreated, err := dm.EnsureLinkContainer(linkImage, nodeID, secret, proof)
			if err != nil {
				log.Printf("link: failed to ensure Link sidecar: %v", err)
				return
			}
			if recreated {
				log.Println("link: Link sidecar (re)started")
			} else {
				log.Println("link: Link sidecar already current, left running")
			}
			last = sig
			// A fresh spawn just pulled, so the next drift check can wait a full
			// interval rather than immediately pulling the same image again.
			nextImageCheck = time.Now().Add(linkImageCheckInterval(getLinkUpdateIntervalMinutes()))
			return
		}
		// Running and configured. The signature cannot notice that a moving tag
		// now points somewhere else, so the image itself is checked on its own,
		// slower cadence.
		if time.Now().Before(nextImageCheck) {
			return
		}
		nextImageCheck = time.Now().Add(linkImageCheckInterval(getLinkUpdateIntervalMinutes()))
		checkLinkImage(dm, secret, proof)
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

// linkWanted reports whether this node should run its Link sidecar. Link carries
// gateway-routed traffic, so it is needed whenever routing is "gateway" OR "both"
// (the mixed mode where some servers still route by ip:port) - i.e. anything but
// pure "ip_port" - provided Core has delivered the Link creds and a Link image is
// configured. Gating on "gateway" alone left a domain route created in "both"
// silently dead: the Link never came up, so its route was never published.
func linkWanted(mode, secret, proof, image string) bool {
	return mode != "ip_port" && secret != "" && proof != "" && image != ""
}
