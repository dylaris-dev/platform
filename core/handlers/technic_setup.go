package handlers

import (
	"context"

	"dylaris-core/services"
	"dylaris-pkg/release"
)

// technicSince is the node release that shipped the "technic" installer.
// Written as a constant for the reason modReportingSince gives: the running
// Core's own version is empty in a development build.
const technicSince = "2026.09.19.4"

// nodeReleaseOlderThan reports whether the node's last heartbeat names a
// release older than since. An unknown version (no heartbeat, unstamped dev
// build) is NOT older: an empty stamp is never treated as old, and refusing
// every development node would make the feature untestable where it is built.
// A genuinely old node that reports nothing fails the install on the node
// with "unknown installer type", which is where it failed before this check.
func nodeReleaseOlderThan(ctx context.Context, st *AppState, nodeToken, since string) bool {
	if st == nil || st.Redis == nil {
		return false
	}
	hb := services.LoadHeartbeat(ctx, st.Redis, nodeToken)
	if hb == nil {
		return false
	}
	have, err := release.ParseVersion(hb.ReleaseVersion)
	if err != nil || have.IsZero() {
		return false
	}
	want, err := release.ParseVersion(since)
	if err != nil {
		return false
	}
	return have.Compare(want) < 0
}

// nodeReleaseAtLeastKnown is the stricter gate for a feature an old node gets
// wrong SILENTLY rather than with an error: it reports whether the node must be
// refused because it is older than since OR does not report a version at all.
//
// An unknown version passes only while this Core is itself unstamped. That is a
// local or self-built Core, whose nodes are built the same way (the testbed
// builds both from docker-compose.local.yml with no RELEASE_VERSION), so the
// feature stays testable where it is developed. A stamped Core is a released
// image, and a node next to it that reports nothing predates the stamp or was
// built by hand - neither can be assumed to have the feature. A node with no
// heartbeat at all is not refused here: it is offline, and the dispatch to it
// fails on its own.
func nodeReleaseAtLeastKnown(ctx context.Context, st *AppState, nodeToken, since string) (refuse bool) {
	if nodeReleaseOlderThan(ctx, st, nodeToken, since) {
		return true
	}
	if st == nil || st.Redis == nil {
		return false
	}
	if core, err := release.ParseVersion(st.ReleaseVersion); err != nil || core.IsZero() {
		return false
	}
	hb := services.LoadHeartbeat(ctx, st.Redis, nodeToken)
	if hb == nil {
		return false
	}
	have, err := release.ParseVersion(hb.ReleaseVersion)
	return err != nil || have.IsZero()
}
