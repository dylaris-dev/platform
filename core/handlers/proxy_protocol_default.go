package handlers

import (
	"context"
	"log"
	"net/http"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-pkg/release"
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

// proxyProtocolDefault decides whether this setup turns real player IP
// forwarding on by itself. It must run BEFORE the setup writes its install
// record, which is one of the things that tells a new server from an old one.
// Every read that fails answers "no": the default is a convenience, opt-in is
// always safe.
func (h *ServerHandler) proxyProtocolDefault(ctx context.Context, srv *models.Server, node *models.Node, installerType, version string, routeRequested bool) bool {
	// Cheap, local checks first: most setups end here without touching Redis.
	if srv.Status != "pending_setup" || srv.InstallerType != "" || node == nil ||
		!services.ProxyProtocolDefaultSoftware(installerType, version) {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	in := services.ProxyProtocolDefaultInput{
		InstallerType:  installerType,
		Version:        version,
		RouteRequested: routeRequested,
		BehindProxy:    srv.ProxyID != nil,
	}
	in.RoutingMode, _ = h.state.Store.GetSetting("routing_mode")
	in.PlatformEnabled = h.state.FeatureFlags != nil && h.state.FeatureFlags.IsProxyProtocolEnabled(ctx)
	in.NodeWritesConfig = nodeReleaseAtLeast(ctx, h.state, node.Token, proxyProtocolDefaultSince)

	installs, ierr := h.state.Store.ListSubServerInstalls(srv.ID)
	routes, rerr := services.ReadRoutesFromRedis(ctx, h.state.Redis)
	if ierr != nil || rerr != nil {
		log.Printf("proxy-protocol default: server %d: cannot tell whether it is new (installs: %v, routes: %v)", srv.ID, ierr, rerr)
		return false
	}
	hasRoute := false
	for _, rt := range routes {
		if services.RouteBelongsToServer(rt, srv.UUID) {
			hasRoute = true
			break
		}
	}
	in.NewServer = len(installs) == 0 && !hasRoute

	// The link the server's routes WILL use: none exists yet, the panel adds
	// the route after setup answers. Same token CreateServerRoute picks.
	if h.state.Gateway != nil {
		tok := node.LinkToken
		if tok == "" {
			tok = h.state.Gateway.LinkToken(node.Token)
		}
		ok, err := linkAnnouncesCap(ctx, h.state.Redis, tok, linkCapClientAddr)
		if err != nil {
			log.Printf("proxy-protocol default: server %d: link check: %v", srv.ID, err)
		}
		in.LinkClientAddr = err == nil && ok
	}
	return services.ProxyProtocolDefaultOn(in)
}

// proxyProtocolInstallGate is asked by every install on an existing server.
// While the switch is on, the software installed has to expect the header
// too: an install the node can configure is sent with the request (keep=true),
// so the node writes or confirms the setting, including after a config wipe;
// anything else is refused with 409 and nothing is written. Answers false,
// false while the switch is off.
func proxyProtocolInstallGate(w http.ResponseWriter, srv *models.Server, installerType, version string) (keep, refused bool) {
	if !srv.ProxyProtocol {
		return false, false
	}
	if services.ProxyProtocolDefaultSoftware(installerType, version) {
		return true, false
	}
	sendJSONErrorCode(w, "Real player IP is on for this server, and this install cannot be set up to expect it, so nobody could join. Turn off Real player IP (Routes) first, then install.",
		"proxy_protocol_on", http.StatusConflict)
	return false, true
}

// trackProxyProtocolInstall remembers that the install about to be sent asks
// the node to configure the setting. Without the record a lost answer could
// never be acted on, so a failure here sends the install without the request.
func (h *ServerHandler) trackProxyProtocolInstall(ctx context.Context, srv *models.Server, sub string, isDefault bool) bool {
	if err := services.TrackProxyProtocolInstall(ctx, h.state.Redis, srv.UUID, sub, isDefault); err != nil {
		log.Printf("proxy-protocol: server %d: cannot record the request: %v", srv.ID, err)
		return false
	}
	return true
}

// switchProxyProtocolOn turns the switch on ahead of the install that writes
// the matching config, and reports whether all of it took. A partial write is
// rolled back so the server is never left half on.
func (h *ServerHandler) switchProxyProtocolOn(ctx context.Context, srv *models.Server) bool {
	if err := services.ApplyServerProxyProtocol(ctx, h.state.Store, h.state.Redis, h.state.Gateway, srv.ID, srv.UUID, true); err != nil {
		log.Printf("proxy-protocol default: server %d: could not switch on, staying opt-in: %v", srv.ID, err)
		h.switchProxyProtocolOff(ctx, srv)
		return false
	}
	log.Printf("proxy-protocol default: server %d: switched on for its first install", srv.ID)
	return true
}

// switchProxyProtocolOff undoes the default when the install that should have
// configured the server will not run. Detached from the request: a client
// that hangs up must not leave the switch on for a server nothing configures.
func (h *ServerHandler) switchProxyProtocolOff(ctx context.Context, srv *models.Server) {
	ctx = context.WithoutCancel(ctx)
	if err := services.ApplyServerProxyProtocol(ctx, h.state.Store, h.state.Redis, h.state.Gateway, srv.ID, srv.UUID, false); err != nil {
		log.Printf("proxy-protocol default: server %d: could not switch back off: %v", srv.ID, err)
	}
}
