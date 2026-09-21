package handlers

// ProxyHandler serves the custom-tab reverse proxy: it streams a server
// container's HTTP/WebSocket through Core over the existing gRPC mesh, so the
// browser talks to Core and never to the container.
//
// This file is the shared plumbing - serve/serveHTTP/serveWS, the share-token
// lookup, and the ticket check. The entry points live in tab_proxy_host.go,
// because a tab is served on its OWN host now rather than under a path on the
// panel's.
//
// Auth on the content plane is cookie-only, and deliberately nothing else: the
// dyl_tabproxy ticket, and never a session by header or query. Two consequences
// worth keeping in mind while reading:
//
//   - The ticket is not a hint about access, it IS the access. Nothing
//     downstream re-checks who the viewer is.
//   - Exactly one function parses one, ParseTabProxyTicket, so there is one
//     place where a ticket's signature and claims are judged.
//
// Where a ticket comes from is in tab_proxy_host.go: MintTicket decides on the
// panel's origin, HostMint stores it on the tab's.

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	pb "dylaris-proto/node"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const proxyCookieName = "dyl_tabproxy"

// tabsReadCap is the capability that means "may see this server's tabs, and
// what is inside them". Both mint endpoints hold to it: the in-dashboard one
// through the route table (routes.go), the share-link one in MintPublicProxyAuth
// below. It used to be tabs.read there and overview.read here, which reproduced
// the exact split the route table's own comment describes - a member refused
// when asking WHICH tabs exist could still mint a ticket for one and read it,
// just through the other door. Measured live: 403 listing the tabs, 403 for a
// dashboard ticket, 204 + 200 for the same tab's content via a share link.
const tabsReadCap = "tabs.read"
const proxyMaxRequestBody = 1 << 20 // 1 MB inline request-body cap

// coreHopByHop lists headers dropped in both directions (RFC 7230 6.1).
var coreHopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// coreResponseStrip lists headers dropped ONLY from the container's response
// on its way back to the browser (WS5 final-review hardening) - never
// applied to forwardRequestHeaders' outbound direction. The proxied response
// is served under the panel's own origin, so a container that emitted
// Set-Cookie/Set-Cookie2 could set or clobber a cookie in the visitor's
// browser on that origin (cookie-bomb/fixation surface, worse on the public
// share path - Public can serve a tab anonymously). The map/plugin web UIs
// this proxy serves (BlueMap/squaremap/Dynmap) use localStorage, not
// cookies, so nothing legitimate depends on this passing through.
//
// Cache-Control/Expires/Pragma are stripped for a second reason: serveHTTP
// and Public both stamp their own authoritative "Cache-Control: no-store" on
// this per-user/per-ticket response via Set, but relaying a container's own
// Cache-Control (e.g. "public, max-age=3600") through afterwards via Add
// would put TWO Cache-Control values on the wire - a lenient shared cache
// could honor the container's permissive one and cache per-user content on
// the shared panel origin. Stripping them here (the single chokepoint every
// container header passes through) guarantees "no-store" stays the sole,
// authoritative value.
var coreResponseStrip = map[string]bool{
	"set-cookie":    true,
	"set-cookie2":   true,
	"cache-control": true,
	"expires":       true,
	"pragma":        true,
}

type ProxyHandler struct {
	state *AppState
	auth  *AuthHandler
}

func NewProxyHandler(state *AppState, auth *AuthHandler) *ProxyHandler {
	return &ProxyHandler{state: state, auth: auth}
}

type proxyTab struct {
	ID           int
	ServerID     int
	ServerUUID   string
	NodeID       int
	Mode         string
	TargetPort   int
	TargetPath   string
	Surface      string
	Visibility   string
	ShareExpires sql.NullTime
	Enabled      bool
	// ShareToken is present only once the tab has been published as a link. In
	// host mode it is what separates "public" from "public and actually shared",
	// which is not the same question now that every proxied tab has a host.
	ShareToken sql.NullString
	// ProxyHostLabel is the subdomain this tab is served on. NULL for a direct
	// tab, which has no content host and needs none.
	ProxyHostLabel sql.NullString
	// SubServerName pins the tab to one sub-server; "" is every sub-server.
	// ActiveSubServer is what the container is running right now. They are read
	// together because the tab addresses a PORT, and the port belongs to
	// whichever sub-server is up - serving a tab pinned to one world while
	// another is running would proxy into the wrong thing under the right name.
	SubServerName   string
	ActiveSubServer string
}

func (h *ProxyHandler) rawDB() *sql.DB {
	if h.state.Store == nil {
		return nil
	}
	provider, ok := h.state.Store.(sparkDBProvider)
	if !ok {
		return nil
	}
	return provider.RawDB()
}

// proxyUpstreamError answers a failed proxy hop without repeating what the node
// said.
//
// The node's message names its own internals. Measured on production: an
// anonymous visitor with a PUBLIC share link, on a tab whose server happened to
// be stopped, was handed
// "mc_<server-uuid> has no network IP (is it running?)" - so the link told a
// stranger the server's UUID, its container name, and whether it was running.
// Nothing about a proxied third-party page needs any of that, and this is a
// content host: everything that reaches it is either a share visitor or a
// browser rendering someone's page, never an operator reading a console.
//
// The real message goes to the log with the tab and server that produced it,
// which is where an operator looks anyway.
func proxyUpstreamError(w http.ResponseWriter, tab *proxyTab, code int, detail string) {
	// An upstream that answers something that is not an HTTP status must not
	// become one: WriteHeader panics below 100 and above 999.
	if code < 400 || code > 599 {
		code = http.StatusBadGateway
	}
	if tab != nil {
		log.Printf("tab proxy: tab %d (server %s, node %d) failed with %d: %s",
			tab.ID, tab.ServerUUID, tab.NodeID, code, detail)
	}
	http.Error(w, "The page could not be loaded.", code)
}

// serve branches to the WS bridge (Task 10) or the HTTP path.
func (h *ProxyHandler) serve(w http.ResponseWriter, r *http.Request, tab *proxyTab, subPath string) {
	if isWebSocketUpgrade(r) {
		h.serveWS(w, r, tab)
		return
	}
	h.serveHTTP(w, r, tab, subPath)
}

// serveHTTP dispatches one HTTP request over the mesh and streams the response.
//
// Everything streams, including text/html. It did not use to: html was buffered
// whole so a <base href> could be injected, capped at 10 MB to keep one large
// page from eating memory, with a degraded no-injection path past the cap. All
// of that existed to make relative urls resolve under a path prefix. There is no
// prefix any more - a tab is served at the root of its own host - so the buffer,
// the cap and the branch it guarded are gone with it.
func (h *ProxyHandler) serveHTTP(w http.ResponseWriter, r *http.Request, tab *proxyTab, subPath string) {
	target := singleJoin(tab.TargetPath, subPath)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	// Security invariant #2: normalize to a safe origin-form path before it
	// crosses the mesh boundary. tab.TargetPath is DB-validated at tab
	// creation/update (must start with "/"), but subPath is the raw,
	// client-controlled request path off the content host - sanitize the JOINED result
	// so a request like .../proxy/@evil.com/x or .../proxy//evil.com/x can
	// never be turned into something that re-targets the host on the node
	// side, however the node ends up building/parsing the outbound URL.
	target = sanitizeProxyPath(target)
	var body []byte
	if r.Body != nil {
		b, _ := io.ReadAll(io.LimitReader(r.Body, proxyMaxRequestBody+1))
		if len(b) > proxyMaxRequestBody {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		body = b
	}
	reqID := uuid.NewString()
	msg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: tab.ServerUUID,
		Payload: &pb.NodeMessage_HttpProxyReq{HttpProxyReq: &pb.HttpProxyReq{
			// Security invariant #1: the port sent to the node is ALWAYS the
			// DB-stored value fetched via getTabByID - never anything derived
			// from the request or the client. This is what closes the SSRF
			// containment: the node dials whatever port Core asks for, so
			// Core must never let a client choose it.
			TargetPort: int32(tab.TargetPort),
			Method:     r.Method,
			Path:       target,
			Headers:    h.forwardRequestHeaders(r),
			Body:       body,
		}},
	}
	ch, err := h.state.GRPCRegistry.SendRequestStreaming(tab.NodeID, msg)
	if err != nil {
		proxyUpstreamError(w, tab, http.StatusBadGateway, "dispatch: "+err.Error())
		return
	}
	defer h.state.GRPCRegistry.CleanupRequest(tab.NodeID, reqID)

	headerWritten := false
	flusher, canFlush := w.(http.Flusher)

	for resp := range ch {
		if e := resp.GetError(); e != nil {
			if !headerWritten {
				proxyUpstreamError(w, tab, int(e.Code), e.Message)
			}
			return
		}
		if hd := resp.GetHttpProxyRespHead(); hd != nil {
			// The proxied page is per-user/per-ticket - never let a shared
			// cache in front of Core (or the browser's disk cache) keep it.
			// This stays the SOLE Cache-Control value: coreResponseStrip
			// drops any container-supplied Cache-Control/Expires/Pragma
			// before writeProxyHeaders relays the rest, so a later Add can
			// never append a second, more permissive value.
			w.Header().Set("Cache-Control", "no-store")
			// Content-Length is relayed unchanged now that nothing rewrites the
			// body. That is not a detail: a container streaming a chunked
			// response and one declaring a length both arrive intact, and the
			// browser gets the same bytes the container sent.
			writeProxyHeaders(w, hd.Headers, false)
			w.WriteHeader(int(hd.StatusCode))
			headerWritten = true
			continue
		}
		chunk := resp.GetChunk()
		if chunk == nil {
			continue
		}
		if !headerWritten {
			w.WriteHeader(http.StatusOK)
			headerWritten = true
		}
		w.Write(chunk.Data)
		if canFlush {
			flusher.Flush()
		}
	}
}

// forwardRequestHeaders converts the browser request headers into the wire
// slice, dropping hop-by-hop and the panel session cookie/Authorization (never
// forwarded to the container - security invariant #6).
//
// Accept-Encoding is still dropped. It no longer HAS to be - nothing rewrites
// the body since the <base href> injection went away - but asking the container
// for an identity encoding keeps the relay a byte pipe with no compressed
// framing to get wrong, and a tab is a local hop, not a bandwidth problem.
func (h *ProxyHandler) forwardRequestHeaders(r *http.Request) []*pb.HttpHeader {
	out := []*pb.HttpHeader{}
	for k, vals := range r.Header {
		lk := strings.ToLower(k)
		if coreHopByHop[lk] || lk == "authorization" || lk == "cookie" || lk == "accept-encoding" {
			continue
		}
		for _, v := range vals {
			out = append(out, &pb.HttpHeader{Key: k, Value: v})
		}
	}
	return out
}

// --- pure helpers ---

// coreStripHopByHop drops both the true hop-by-hop headers (coreHopByHop) and
// the response-only strip set (coreResponseStrip, e.g. Set-Cookie) - the
// single chokepoint writeProxyHeaders relies on before anything from a
// container response reaches the browser on the panel origin.
func coreStripHopByHop(hs []*pb.HttpHeader) []*pb.HttpHeader {
	out := make([]*pb.HttpHeader, 0, len(hs))
	for _, h := range hs {
		lk := strings.ToLower(h.Key)
		if coreHopByHop[lk] || coreResponseStrip[lk] {
			continue
		}
		out = append(out, h)
	}
	return out
}

// writeProxyHeaders is the single chokepoint both InDashboard and Public
// relay a container's response headers through (via serve -> serveHTTP), so
// every response-side header rule lives here exactly once.
func writeProxyHeaders(w http.ResponseWriter, hs []*pb.HttpHeader, dropContentLength bool) {
	for _, h := range coreStripHopByHop(hs) {
		if dropContentLength && strings.EqualFold(h.Key, "Content-Length") {
			continue
		}
		w.Header().Add(h.Key, h.Value)
	}
	// WS5 final-review hardening: Set (not Add) so this always wins over any
	// container-supplied value - defense-in-depth against MIME-sniffing of
	// content relayed onto the panel origin.
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// isWebSocketUpgrade reports whether the request is a WebSocket handshake.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// singleJoin joins a base path and a forwarded sub-path without doubling or
// dropping the separating slash.
func singleJoin(base, sub string) string {
	if sub == "" {
		return base
	}
	if strings.HasSuffix(base, "/") && strings.HasPrefix(sub, "/") {
		return base + sub[1:]
	}
	if !strings.HasSuffix(base, "/") && !strings.HasPrefix(sub, "/") {
		return base + "/" + sub
	}
	return base + sub
}

// sanitizeProxyPath hardens the origin-form path (+ query) Core forwards to
// the node in HttpProxyReq.Path (security invariant #2, carried forward from
// earlier task reviews).
//
// This is load-bearing, not defence in depth. The node builds the outbound URL
// by plain string concatenation ("http://"+addr+path, see
// node/grpc_tabproxy.go), and concatenation does NOT keep the host fixed the
// way this comment used to claim: net/url reads "http://10.0.0.5:8080@evil.com/x"
// as host evil.com with the container address demoted to USERINFO. Guaranteeing
// the leading "/" below is what makes the concatenation safe, so the node's
// private-IP guard (resolveMCAddr) is never simply bypassed. Verified against
// http.NewRequest, which returns host="evil.com" for exactly that input.
//
// Three rules, applied to the JOINED path so the client-controlled {rest:.*}
// capture is covered as well as the DB-stored target_path:
//   - strips raw control characters (CR/LF, etc.) that could otherwise enable
//     request-line/header injection when concatenated into a raw URL string.
//   - collapses a leading run of slashes to exactly one, so a value like
//     "//evil.com/x" (scheme-relative in some URL parsers/consumers) can
//     never be mistaken for a different authority.
//   - guarantees the result starts with "/" (origin-form).
func sanitizeProxyPath(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	p = b.String()
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	for len(p) > 1 && p[1] == '/' {
		p = p[1:]
	}
	return p
}

// --- Task 9: public share-token endpoint ---

// getTabByShareToken resolves a share-token slug to its proxied tab + owning
// server, mirroring getTabByID's query shape but keyed on the public slug
// the standalone page has instead of a server+tab id pair.
func (h *ProxyHandler) getTabByShareToken(token string) (*proxyTab, error) {
	db := h.rawDB()
	if db == nil {
		return nil, fmt.Errorf("db unavailable")
	}
	var t proxyTab
	err := db.QueryRow(`SELECT t.id, t.server_id, s.uuid, s.node_id, t.mode,
		COALESCE(t.target_port,0), t.target_path, t.surface, t.visibility, t.share_expires_at,
		t.enabled, t.share_token, t.proxy_host_label,
		COALESCE(t.sub_server_name,''), COALESCE(s.active_sub_server,'')
		FROM server_tabs t JOIN servers s ON s.id = t.server_id
		WHERE t.share_token=$1`, token).Scan(
		&t.ID, &t.ServerID, &t.ServerUUID, &t.NodeID, &t.Mode,
		&t.TargetPort, &t.TargetPath, &t.Surface, &t.Visibility, &t.ShareExpires,
		&t.Enabled, &t.ShareToken, &t.ProxyHostLabel,
		&t.SubServerName, &t.ActiveSubServer)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// decideProxyAccess resolves the visibility gate for a share-token request.
// public + allowed serves anyone (even anon); public + disabled hides (404);
// private requires a valid tab-proxy ticket (401 if none/invalid) scoped to
// this exact tab (403 if the ticket is valid but minted for a different tab).
func decideProxyAccess(visibility string, allowPublic, authed, hasAccess bool) (allow bool, status int) {
	if visibility == "public" {
		if allowPublic {
			return true, http.StatusOK
		}
		return false, http.StatusNotFound
	}
	if !authed {
		return false, http.StatusUnauthorized
	}
	if !hasAccess {
		return false, http.StatusForbidden
	}
	return true, http.StatusOK
}

// shareTokenExpired reports whether an optional expiry is in the past (an
// expiry is "used up" the instant it's reached, not the instant after -
// hence !now.Before(expires.Time) rather than a strict After).
func shareTokenExpired(expires sql.NullTime, now time.Time) bool {
	return expires.Valid && !now.Before(expires.Time)
}

// resolvePublicTicket validates the dyl_tabproxy cookie against an
// already-resolved share-token tab, mirroring resolveProxyTicket for
// InDashboard. authed reports whether a valid (signature + Purpose ==
// "tab_proxy" + unexpired) ticket was present at all; hasAccess additionally
// requires the ticket's server-id + tab-id claims to match THIS tab - a
// ticket that is valid but was minted for a different tab is
// authed-but-no-access, which decideProxyAccess turns into 403 rather than
// the 401 a missing/invalid cookie gets. readOnly carries the ticket's own
// ReadOnly claim (set by MintPublicProxyAuth from the demo-account check) so
// Public can apply the same read-only method gate InDashboard does - it is
// meaningless (and always false) when authed is false.
func (h *ProxyHandler) resolvePublicTicket(r *http.Request, tab *proxyTab) (authed, hasAccess, readOnly bool) {
	c, err := r.Cookie(proxyCookieName)
	if err != nil || c.Value == "" {
		return false, false, false
	}
	claims, err := h.auth.ParseTabProxyTicket(c.Value)
	if err != nil {
		return false, false, false
	}
	return true, claims.ServerID == tab.ServerID && claims.TabID == tab.ID, claims.ReadOnly
}

const coreWSFragmentSize = 60 * 1024

// maxWSMessageBytes caps one reassembled container->browser application
// message (see the container->browser loop in serveWS below). Without it, a
// container/node pair streaming endless Fin=false fragments (or one huge
// message) grows rxBuf here until Core OOMs, which takes the mesh-facing
// side down for EVERY tab-proxy session, not just this one - the same DoS
// shape the node side already closes for its own Core->container
// reassembly. This value MUST mirror the node's own maxWSMessageBytes
// (node/grpc_tabproxy_ws.go) since both sides are bounding the same
// conceptual message size, just in opposite directions.
const maxWSMessageBytes = 8 * 1024 * 1024 // 8 MiB

// wsReassemblyExceeded reports whether appending add bytes to a reassembly
// buffer that already holds cur bytes would push it past max. Pulled out as
// a pure helper (mirroring the shape of the node's wsBridge.appendInbound
// check) so the cap logic is table-testable without standing up a full
// WS/gRPC bridge.
func wsReassemblyExceeded(cur, add, max int) bool {
	return cur+add > max
}

// splitProxyBytes splits data into <=max fragments (reassembled on the far
// side); an empty message yields one empty fragment. Keeps a WsFrame under
// Core's 128KB gRPC receive cap.
func splitProxyBytes(data []byte, max int) [][]byte {
	if max <= 0 {
		max = coreWSFragmentSize
	}
	if len(data) == 0 {
		return [][]byte{{}}
	}
	var out [][]byte
	for off := 0; off < len(data); off += max {
		end := off + max
		if end > len(data) {
			end = len(data)
		}
		out = append(out, data[off:end])
	}
	return out
}

// upgrader is the tab proxy's browser-facing WebSocket upgrader. It used to
// live in handlers/node_grpc.go (the legacy, unauthenticated /node/connect
// endpoint, now deleted) and was shared with it, which is why its old comment
// argued about node-to-node connections. This is its only caller.
//
// The origin check is what stops cross-site WebSocket hijacking here, and it
// matters more than it would elsewhere because this endpoint authenticates with
// a COOKIE (dyl_tabproxy), which a browser attaches to a cross-site WS handshake
// automatically. A page on another origin gets ITS origin sent and is rejected.
// The empty case is allowed because a browser always sends Origin on a WS
// handshake, so "" means a non-browser client - which has no victim cookie to
// carry. Both schemes are compared because Core is reached over plain http on a
// LAN or localhost as often as over https behind a proxy, and r.Host is the host
// the client actually addressed - which for a tab is its OWN content host.
//
// That last part got stronger when each tab moved to its own hostname. All tabs
// used to share one origin, so a page inside tab A could open a WebSocket to
// tab B's proxy and the check would pass. Now they are different origins and it
// does not.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return origin == "" || origin == "http://"+r.Host || origin == "https://"+r.Host
	},
}

// serveWS upgrades the browser connection and bridges it to the container WS
// over the mesh: WsOpen, then WsFrame/WsClose in both directions. Application
// messages are fragmented at coreWSFragmentSize and reassembled on the far side
// via the WsFrame.fin flag. V1 does not append the forwarded sub-path (the
// live-WS endpoint sits at the app base path in target_path).
//
// Concurrency/teardown mirrors the node-side bridge (node/grpc_tabproxy_ws.go):
// exactly one reader goroutine (browser->container, below) and exactly one
// writer (the container->browser loop, which is this function's own
// goroutine - the http.Handler call itself), so gorilla's single-reader/
// single-writer requirement on bconn is never violated. Teardown is
// exactly-once via closeOnce/done regardless of which side triggers it: the
// browser closing/erroring, the container sending WsClose/an error, or the
// node connection dying (registry.Unregister closes every pending channel,
// so ch is closed and the `!ok` branch fires here). CleanupRequest is
// deferred so the registry's pending-request map never leaks an entry past
// this handler's return, matching the SendRequestStreaming contract used by
// every other mesh caller (see serveHTTP above).
func (h *ProxyHandler) serveWS(w http.ResponseWriter, r *http.Request, tab *proxyTab) {
	bconn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer bconn.Close()

	target := tab.TargetPath
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	// Security invariant #2 (see serveHTTP): normalize to a safe origin-form
	// path/query the same way before it crosses the mesh boundary, so the WS
	// path is never less sanitized than the HTTP one.
	target = sanitizeProxyPath(target)
	// A fresh, unique request_id per WS open (never reused) - the registry
	// keys the node-side bridge and pending-response channel by this id, so a
	// collision with any other in-flight request (a concurrent WS, or an
	// HTTP proxy call) would let one overwrite the other's bridge/channel.
	reqID := uuid.NewString()
	openMsg := &pb.NodeMessage{
		RequestId:  reqID,
		ServerUuid: tab.ServerUUID,
		Payload: &pb.NodeMessage_WsOpen{WsOpen: &pb.WsOpen{
			TargetPort: int32(tab.TargetPort),
			Path:       target,
			Headers:    h.forwardRequestHeaders(r),
		}},
	}
	ch, err := h.state.GRPCRegistry.SendRequestStreaming(tab.NodeID, openMsg)
	if err != nil {
		return
	}
	defer h.state.GRPCRegistry.CleanupRequest(tab.NodeID, reqID)

	done := make(chan struct{})
	var closeOnce sync.Once
	closeAll := func() {
		closeOnce.Do(func() {
			// Best-effort: if the node/connection is already gone this send
			// simply fails and is ignored - there is nothing left to notify.
			h.state.GRPCRegistry.SendOnStream(tab.NodeID, &pb.NodeMessage{
				RequestId:  reqID,
				ServerUuid: tab.ServerUUID,
				Payload:    &pb.NodeMessage_WsClose{WsClose: &pb.WsClose{Code: 1000}},
			})
			close(done)
			bconn.Close()
		})
	}

	// browser -> container (sole reader of bconn)
	go func() {
		defer closeAll()
		for {
			mt, data, rerr := bconn.ReadMessage()
			if rerr != nil {
				return
			}
			frags := splitProxyBytes(data, coreWSFragmentSize)
			for i, f := range frags {
				if serr := h.state.GRPCRegistry.SendOnStream(tab.NodeID, &pb.NodeMessage{
					RequestId:  reqID,
					ServerUuid: tab.ServerUUID,
					Payload: &pb.NodeMessage_WsFrame{WsFrame: &pb.WsFrame{
						Opcode: int32(mt), Data: f, Fin: i == len(frags)-1,
					}},
				}); serr != nil {
					return
				}
			}
		}
	}()

	// container -> browser (sole writer of bconn)
	var rxBuf []byte
	rxOpcode := websocket.TextMessage
	for {
		select {
		case <-done:
			return
		case resp, ok := <-ch:
			if !ok {
				// Node/connection died: registry.Unregister closed ch.
				closeAll()
				return
			}
			if resp.GetWsClose() != nil || resp.GetError() != nil {
				closeAll()
				return
			}
			fr := resp.GetWsFrame()
			if fr == nil {
				continue
			}
			// DoS-mirror fix: cap the reassembled message the same way the
			// node side caps its own Core->container reassembly. Past the
			// cap, stop reassembling and tear the bridge down via the
			// existing teardown path instead of growing rxBuf unboundedly.
			if wsReassemblyExceeded(len(rxBuf), len(fr.Data), maxWSMessageBytes) {
				closeAll()
				return
			}
			rxBuf = append(rxBuf, fr.Data...)
			if fr.Opcode != 0 {
				rxOpcode = int(fr.Opcode)
			}
			if !fr.Fin {
				continue
			}
			if werr := bconn.WriteMessage(rxOpcode, rxBuf); werr != nil {
				closeAll()
				return
			}
			rxBuf = nil
		}
	}
}
