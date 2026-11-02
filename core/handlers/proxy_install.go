package handlers

import (
	"context"
	"dylaris-core/handlers/nodeops"
	"net/http"
)

// proxyInstallSince is the node release that installs velocity, waterfall and
// bungeecord. An older node answers them with "unknown installer type" after
// the container already exists.
const proxyInstallSince = "2026.10.16.3"

// proxyInstallers are the installer types that make a server a Minecraft proxy.
var proxyInstallers = map[string]bool{"velocity": true, "waterfall": true, "bungeecord": true}

// proxyInstallRefusal reports why a proxy install must not go out: on a game
// server, or to a node that predates the installer. Zero means go ahead, and
// every non-proxy type is zero.
//
// A proxy on a game server would run unmarked: no backend linking, no network
// policy letting it reach them, and its ping counted twice in the players
// online. The server's type is chosen at creation, so that is where the choice
// belongs.
func proxyInstallRefusal(ctx context.Context, st *AppState, serverType, nodeToken, installerType string) (int, string) {
	if !proxyInstallers[installerType] {
		return 0, ""
	}
	if serverType != "proxy" {
		return http.StatusBadRequest, "A proxy can only be installed on a server created as a proxy"
	}
	if nodeops.NodeReleaseOlderThan(ctx, st.Redis, nodeToken, proxyInstallSince) {
		return http.StatusConflict, "This node runs an older release that cannot install a proxy. Update the node, then try again."
	}
	return 0, ""
}
