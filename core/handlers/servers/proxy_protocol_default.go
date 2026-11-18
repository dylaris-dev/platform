package servers

import (
	"context"
	"net/http"

	"dylaris-core/handlers/httpx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// proxyProtocolDefaultSince is the node release that writes the real-IP
// setting into a server's config and reports whether it did.
const proxyProtocolDefaultSince = "2026.10.21.2"

// proxyProtocolInstallGate is asked by every install on an existing server.
// While the switch is on, the software installed has to expect the header
// too: an install the node can configure is sent with the request (keep=true),
// so the node writes or confirms the setting, including after a config wipe;
// anything else is refused with 409 and nothing is written. Answers all false
// while the switch is off.
//
// In auto mode nothing is refused: switchOff asks the caller to switch
// forwarding off before an install that cannot be set up for it, and the auto
// reconcile turns it on again once the new software's status ping shows it
// expects the header. The gate only decides: the caller switches it off with
// applyInstallProxyOff right before it dispatches, after every refusal of its
// own, or a refused install would still have stopped the header the running
// software expects.
func proxyProtocolInstallGate(w http.ResponseWriter, st Deps, srv *models.Server, installerType, version string) (keep, switchOff, refused bool) {
	if !srv.ProxyProtocol {
		return false, false, false
	}
	if services.ProxyProtocolDefaultSoftware(installerType, version) {
		return true, false, false
	}
	if st.Store != nil {
		if mode, err := st.Store.GetServerProxyMode(srv.ID); err == nil && mode == store.ProxyModeAuto {
			return false, true, false
		}
	}
	refuseInstallWhileProxyOn(w)
	return false, false, true
}

// applyInstallProxyOff carries out the gate's switchOff. False means it could
// not, and the install was refused with the gate's 409.
func applyInstallProxyOff(w http.ResponseWriter, st Deps, srv *models.Server) bool {
	if err := services.ApplyServerProxyProtocol(context.Background(), st.Store, st.Redis, st.Gateway, srv.ID, srv.UUID, false); err != nil {
		refuseInstallWhileProxyOn(w)
		return false
	}
	srv.ProxyProtocol = false
	return true
}

func refuseInstallWhileProxyOn(w http.ResponseWriter) {
	httpx.SendJSONErrorCode(w, "Real player IP is on for this server, and this install cannot be set up to expect it, so nobody could join. Turn off Real player IP (Routes) first, then install.",
		"proxy_protocol_on", http.StatusConflict)
}
