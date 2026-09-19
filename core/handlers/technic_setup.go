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
