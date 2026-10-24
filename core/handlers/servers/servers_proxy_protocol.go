package servers

import (
	"context"
	"log"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"
)

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
	in.NodeWritesConfig = handlers.NodeReleaseAtLeast(ctx, h.state, node.Token, handlers.ProxyProtocolDefaultSince)

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
		ok, err := handlers.LinkAnnouncesCap(ctx, h.state.Redis, tok, handlers.LinkCapClientAddr)
		if err != nil {
			log.Printf("proxy-protocol default: server %d: link check: %v", srv.ID, err)
		}
		in.LinkClientAddr = err == nil && ok
	}
	return services.ProxyProtocolDefaultOn(in)
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
