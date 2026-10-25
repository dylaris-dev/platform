package handlers

import (
	"context"
	"net/http"

	"dylaris-pkg/release"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// proxyProtocolDefaultSince is the node release that writes the real-IP
// setting into a server's config and reports whether it did.
const proxyProtocolDefaultSince = "2026.10.21.2"

// nodeReleaseAtLeast is the strict twin of nodeReleaseOlderThan: a node whose
// release is unknown (no heartbeat, a dev build) does NOT qualify. The default
// turns a switch on, and on for a server the node never configured locks every
// player out, so "unknown" has to read as "no" here.
func nodeReleaseAtLeast(ctx context.Context, st *AppState, nodeToken, since string) bool {
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
	return have.Compare(want) >= 0
}

// proxyProtocolInstallGate is asked by every install on an existing server.
// While the switch is on, the software installed has to expect the header
// too: an install the node can configure is sent with the request (keep=true),
// so the node writes or confirms the setting, including after a config wipe;
// anything else is refused with 409 and nothing is written. Answers false,
// false while the switch is off.
//
// In auto mode nothing is refused: forwarding is switched off before an
// install that cannot be set up for it, and the auto reconcile turns it on
// again once the new software's status ping shows it expects the header.
func proxyProtocolInstallGate(w http.ResponseWriter, st *AppState, srv *models.Server, installerType, version string) (keep, refused bool) {
	if !srv.ProxyProtocol {
		return false, false
	}
	if services.ProxyProtocolDefaultSoftware(installerType, version) {
		return true, false
	}
	if st != nil && st.Store != nil {
		if mode, err := st.Store.GetServerProxyMode(srv.ID); err == nil && mode == store.ProxyModeAuto {
			if err := services.ApplyServerProxyProtocol(context.Background(), st.Store, st.Redis, st.Gateway, srv.ID, srv.UUID, false); err == nil {
				srv.ProxyProtocol = false
				return false, false
			}
		}
	}
	sendJSONErrorCode(w, "Real player IP is on for this server, and this install cannot be set up to expect it, so nobody could join. Turn off Real player IP (Routes) first, then install.",
		"proxy_protocol_on", http.StatusConflict)
	return false, true
}
