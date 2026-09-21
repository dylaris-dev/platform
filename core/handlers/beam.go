package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"dylaris-core/authz"
	"dylaris-core/services"
	"dylaris-core/services/redisacl"
	beamauth "dylaris-pkg/beam/auth"
	"dylaris-pkg/fileperms"
	"github.com/redis/go-redis/v9"
)

// Beam relay discovery — matches the heartbeat pattern in
// gateway/beam/relay: each relay writes JSON to beam:registry:<id> with a
// 30s TTL and adds its id to the sys:beams set. The Core resolves "alive"
// relays by intersecting the set with surviving keys.
const beamRegistrySet = "sys:beams"

// BeamRelayInfo describes one discovered relay. Mirrors what the relay's
// heartbeat publishes so we can surface it raw in the admin UI.
type BeamRelayInfo struct {
	BeamID     string `json:"beam_id"`
	IP         string `json:"ip"`
	PrivateIP  string `json:"private_ip,omitempty"`
	PublicHost string `json:"public_host,omitempty"` // e.g. "beam.dylaris.com" — preferred over IP
	// Region groups relays the way EdgeRegion groups edges. Matched against the
	// region of the NODE a transfer targets, because the payload path is
	// client -> relay -> node: a relay near the client but far from the node
	// makes the round trip worse, not better.
	Region       string `json:"region,omitempty"`
	ServicePort  string `json:"service_port"`
	ClientPort   string `json:"client_port,omitempty"`
	DownloadPort string `json:"download_port,omitempty"`
	Timestamp    int64  `json:"timestamp"`
}

// PublicAddress returns the host:port the panel should hand out to clients.
// Prefers the operator-configured public host over the internal overlay IP
// so we never accidentally publish a 172.x address to a browser.
func (i BeamRelayInfo) PublicAddress(port string) string {
	host := strings.TrimSpace(i.PublicHost)
	if host == "" {
		host = i.IP
	}
	if host == "" {
		host = i.BeamID
	}
	if port == "" || strings.Contains(host, ":") {
		return host
	}
	return host + ":" + port
}

// DiscoverBeamRelays reads sys:beams and resolves each entry against the
// corresponding beam:registry:<id> key. Stale entries (TTL expired) are
// skipped silently — the relay's own cleanup loop will eventually prune
// them from sys:beams.
func DiscoverBeamRelays(ctx context.Context, rdb *redis.Client) []BeamRelayInfo {
	if rdb == nil {
		return nil
	}
	ids, err := rdb.SMembers(ctx, beamRegistrySet).Result()
	if err != nil {
		return nil
	}
	out := make([]BeamRelayInfo, 0, len(ids))
	for _, id := range ids {
		raw, err := rdb.Get(ctx, "beam:registry:"+id).Bytes()
		if err != nil {
			continue
		}
		var info BeamRelayInfo
		if json.Unmarshal(raw, &info) != nil {
			continue
		}
		if info.BeamID == "" {
			info.BeamID = id
		}
		out = append(out, info)
	}
	return out
}

// PickBeamRelay returns one live relay, preferring preferredRegion and falling
// back to any relay when that region has none.
//
// The random choice among equals is the load balancing: several relays sharing
// one region (and therefore one DNS name) split traffic and can be rolled one at
// a time. The region is a HARD filter rather than a score because the wrong
// region is not "slightly slower" - it can double a transatlantic hop.
//
// Falling back to another region rather than failing is deliberate: a transfer
// over a distant relay beats no transfer at all, and because clients re-resolve
// on every connect, the next one returns to the right region on its own the
// moment a relay there registers. No re-check loop is needed for that.
func PickBeamRelay(ctx context.Context, rdb *redis.Client, preferredRegion string) (BeamRelayInfo, bool) {
	relays := DiscoverBeamRelays(ctx, rdb)
	if len(relays) == 0 {
		return BeamRelayInfo{}, false
	}
	if region := strings.TrimSpace(preferredRegion); region != "" {
		matching := make([]BeamRelayInfo, 0, len(relays))
		for _, r := range relays {
			if strings.EqualFold(strings.TrimSpace(r.Region), region) {
				matching = append(matching, r)
			}
		}
		if len(matching) > 0 {
			relays = matching
		}
	}
	return relays[rand.Intn(len(relays))], true
}

// resolveRelay returns the effective public address for Beam Desktop
// clients. Discovery supplies which relay is alive and its port; the
// externally reachable host comes from publicHost (DB setting
// beam.public_host) — the relay's own registered IP is an internal
// overlay address a desktop client can't reach. Manual override (DB
// setting beam.relay_address) still wins outright for incident routing.
func resolveRelay(ctx context.Context, rdb *redis.Client, manualOverride, publicHost, preferredRegion string) (string, string) {
	if strings.TrimSpace(manualOverride) != "" {
		return manualOverride, "manual"
	}
	if info, ok := PickBeamRelay(ctx, rdb, preferredRegion); ok {
		// A relay that reports its own public host knows better than one
		// fleet-wide setting can: with several regions there is no single
		// correct hostname. The setting stays the fallback, which is what a
		// relay predating BEAM_PUBLIC_HOST relies on.
		if info.PublicHost == "" {
			info.PublicHost = strings.TrimSpace(publicHost)
		}
		port := info.ClientPort
		if port == "" {
			port = info.ServicePort
		}
		addr := info.PublicAddress(port)
		if addr != "" {
			return addr, "discovered"
		}
	}
	return "", ""
}

type BeamHandler struct {
	state     *AppState
	jwtSecret string
	// clusterSecret unwraps a node's stored per-node secret, which keys the LAN
	// fast-path certificate. Not the same secret as jwtSecret on purpose - see
	// the derivation in GetBeamTicket.
	clusterSecret string

	// Auto min-version cache: in beam.min_version_mode=auto the force-update
	// floor is the signed manifest's minVersion, fetched+verified at most once
	// per beamMinAutoTTL (GetBeamTicket runs on every connect). See
	// effectiveMinVersion / cachedAutoMinVersion in beam_manifest.go.
	minCacheMu  sync.Mutex
	minCacheVal string
	minCacheAt  time.Time
}

func NewBeamHandler(state *AppState, jwtSecret, clusterSecret string) *BeamHandler {
	return &BeamHandler{state: state, jwtSecret: jwtSecret, clusterSecret: clusterSecret}
}

// beamAccessCap is the capability a caller must hold on a server before Core
// will mint a beam ticket for it, or list it to a beam client.
//
// It is sftp.access rather than a files.* cap because a beam ticket is a
// full file-transfer credential: the node validates it (node/beam_server.go
// Authenticate) and then serves list, read, WRITE, delete and rename over the
// whole server directory. BeamClaims carries no capability, so the node has
// nothing to narrow that down with - the decision has to be made here, once,
// and it has to be the write-capable one.
//
// sftp.access is exactly that decision as the catalog already expresses it:
// it gates the sibling full-file-transfer door (GET /api/servers/{id}/
// sftp-credentials, routes.go), and every preset carrying it also carries
// files.read + files.write.
const beamAccessCap = "sftp.access"

// canBeam resolves beamAccessCap for the caller on one server. Admins and owners
// pass through the resolver's own short-circuits; an admin used to pass before
// it, which put a customer's machine inside every admin's beam ticket.
//
// Fail-closed on a missing resolver: this is the only gate between a panel
// member and unrestricted write access to a server's files.
// It also returns WHAT they may do to the files, which travels in the ticket.
// The node has no other way to know: it sees a signed ticket and nothing else,
// so a beam session used to be all-or-nothing and "all" is what it was. The
// comment on the access check below already said as much - "the node has no
// capability in the ticket to check" - about the field this now fills in.
func (h *BeamHandler) canBeam(r *http.Request, serverID int) (bool, fileperms.Perms) {
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	if h.state == nil || h.state.Authz == nil {
		return false, fileperms.Perms{}
	}
	username, _ := r.Context().Value("username").(string)
	userID, _ := r.Context().Value("userID").(string)
	res, err := h.state.Authz.Resolve(
		authz.Identity{UserID: userID, Username: username, IsAdmin: isAdmin}, serverID)
	if err != nil || !res.HasCap(beamAccessCap) {
		return false, fileperms.Perms{}
	}
	// The same three capability ids handlers/file.go passes to getServerUUID,
	// and the same three services/sftp_sync.go publishes to the node for SFTP.
	return true, fileperms.Perms{
		Read:   res.HasCap("files.read"),
		Write:  res.HasCap("files.write"),
		Delete: res.HasCap("files.delete"),
	}
}

type BeamServerInfo struct {
	ID              int    `json:"id"`
	UUID            string `json:"uuid"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	NodeID          string `json:"node_id"`
	NodeName        string `json:"node_name"`
	ActiveSubServer string `json:"active_sub_server"`
}

// GetBeamServers returns the server list with node_id for Beam clients.
// GET /api/beam/servers
func (h *BeamHandler) GetBeamServers(w http.ResponseWriter, r *http.Request) {
	username := r.Context().Value("username").(string)
	isAdmin := r.Context().Value("isAdmin").(bool)

	// Use ListServersForUser which handles access control
	user, err := h.state.Store.GetUserByUsername(username)
	if err != nil || user == nil {
		sendJSONError(w, "User not found", http.StatusUnauthorized)
		return
	}

	servers, err := h.state.Store.ListServersForUser(user.ID, isAdmin)
	if err != nil {
		sendJSONError(w, "Failed to load servers", http.StatusInternalServerError)
		return
	}

	var result []BeamServerInfo
	for _, s := range servers {
		// This list is the beam client's server picker, so it must show what
		// beam can actually open. ListServersForUser answers "can this account
		// see the server at all", which is a wider set than beamAccessCap -
		// listing the rest offered a member a server whose ticket request is
		// refused, and handed out that server's node discovery token on the way.
		if ok, _ := h.canBeam(r, s.ID); !ok {
			continue
		}
		// Resolve node info — Node.Token is used as the discovery nodeID
		nodeDiscoveryID := ""
		nodeName := s.NodeName
		if s.NodeID > 0 {
			node, err := h.state.Store.GetNodeByID(s.NodeID)
			if err == nil && node != nil {
				nodeDiscoveryID = node.Token // Token = DYLARIS_NODE_ID used in discovery
				nodeName = node.Name
			}
		}

		result = append(result, BeamServerInfo{
			ID:              s.ID,
			UUID:            s.UUID,
			Name:            s.Name,
			Status:          s.Status,
			NodeID:          nodeDiscoveryID,
			NodeName:        nodeName,
			ActiveSubServer: s.ActiveSubServer,
		})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"servers": result,
	})
}

// sendBeamUpdateRequired writes the structured HTTP 426 the Beam app branches
// on (typed, not string-matched) to show its mandatory-update screen. It
// carries reason + min_version in addition to the standard {success,message};
// sendJSONError only emits {success,message}, so the gate needs this instead.
func sendBeamUpdateRequired(w http.ResponseWriter, minVer string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUpgradeRequired) // 426
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     false,
		"reason":      "update_required",
		"min_version": minVer,
		"message":     "Your Beam version is out of date. Update to at least " + minVer + " to connect.",
	})
}

// GetBeamTicket signs a JWT ticket for a specific server.
// POST /api/beam/ticket
func (h *BeamHandler) GetBeamTicket(w http.ResponseWriter, r *http.Request) {
	username := r.Context().Value("username").(string)
	isAdmin := r.Context().Value("isAdmin").(bool)

	// Force-update gate: refuse to mint a ticket for a Beam build below the
	// effective minimum. GetBeamTicket is the single pre-connection choke point
	// (every connect, relay or direct, calls it first), so gating here locks out
	// old clients without touching the node. The floor is the admin-set
	// beam.min_version (manual mode, default) or the signed manifest's minVersion
	// (auto mode); see effectiveMinVersion. Fail-closed: with a floor set, an
	// absent/unparseable X-Beam-Version is treated as below-min (see
	// beamClientBelowMin). An empty effective floor = gating off.
	minVer := h.effectiveMinVersion(r.Context())
	if beamClientBelowMin(r.Header.Get("X-Beam-Version"), minVer) {
		sendBeamUpdateRequired(w, minVer)
		return
	}

	// server_uuid is read from the query string (GET) or a JSON body
	// (POST). The Beam desktop app uses GET — networks in front of Core
	// were observed handing its POSTs an HTML page instead of routing
	// them through, and GET goes straight to the handler.
	serverUUID := r.URL.Query().Get("server_uuid")
	if serverUUID == "" && r.Body != nil {
		var req struct {
			ServerUUID string `json:"server_uuid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		serverUUID = req.ServerUUID
	}
	if serverUUID == "" {
		sendJSONError(w, "server_uuid required", http.StatusBadRequest)
		return
	}

	// Resolve server
	server, err := h.state.Store.GetServerByUUID(serverUUID)
	if err != nil || server == nil {
		sendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}

	// Access: beamAccessCap through the same resolver every other server-scoped
	// route uses.
	//
	// This used to accept any server_invites row - "is this user a member at
	// all" - which made it the one file door in Core that resolved no
	// capability. The Viewer preset is described as "Read-only access to ...
	// files" and holds files.read without files.write; through beam it wrote,
	// deleted and renamed anyway, because the node has no capability in the
	// ticket to check. Operator holds no file capability at all and had the
	// same access.
	//
	// It was wrong in the other direction too: GetInvite matches
	// si.server_id = $1, so an ACCOUNT-wide grant (server_id IS NULL), which
	// the resolver honours on every server of that owner, produced no row and
	// was refused.
	allowed, filePerms := h.canBeam(r, server.ID)
	if !allowed {
		sendJSONError(w, "Access denied", http.StatusForbidden)
		return
	}

	// A ticket is a working channel into the node, so it follows the same rule
	// as everything else a cut-off tenant may not ask the node to do. Measured
	// on production: a suspended account minted a ticket and the relay spliced
	// it through to the node.
	if refuseIfSuspended(w, r, h.state, server) {
		return
	}

	// Resolve node discovery ID (Token field = DYLARIS_NODE_ID) + direct-connect
	// hint inputs. Whether these are actually handed out is decided below by relay
	// presence, not by node ownership.
	nodeDiscoveryID := ""
	var nodePrivateIPs []string
	var nodePublicIP string
	// The node's region decides which relay a transfer should use: the payload
	// runs client -> relay -> node, so the relay belongs near the NODE.
	nodeRegion := ""
	if server.NodeID > 0 {
		node, err := h.state.Store.GetNodeByID(server.NodeID)
		if err == nil && node != nil {
			nodeDiscoveryID = node.Token
			nodePrivateIPs = node.PrivateIPs
			nodePublicIP = node.PublicIP
			nodeRegion = node.Region
		}
	}
	if nodeDiscoveryID == "" {
		sendJSONError(w, "Server has no assigned node", http.StatusBadRequest)
		return
	}

	// Fingerprint of the node's deterministic LAN TLS cert so the app can pin it
	// (encryption + MITM protection on the direct hop, LAN or public). Needed for
	// any direct target, so derive it up front; an error leaves it empty and
	// buildBeamDirectHints then advertises no direct path.
	//
	// Keyed on the PER-NODE secret, not the fleet JWT secret. That secret is the
	// one Core and this node already share, it never crosses the wire, and it is
	// the only one a BYON machine has: the deploy snippet withholds fleet secrets
	// on purpose, so the old derivation left every customer node starting its LAN
	// listener and failing with "cert derive failed: auth: empty secret" - a
	// documented, advertised port that could never work.
	//
	// It is also the better key for platform-owned nodes. JWT_SECRET signs panel
	// sessions, and deriving from it meant any node holding it could reproduce
	// EVERY other node's LAN private key. Per-node, a compromise stops at that
	// machine.
	directFingerprint := ""
	if server.NodeID != 0 {
		if secret, ok, serr := redisacl.LoadNodeSecret(h.state.Store, h.clusterSecret, server.NodeID); serr != nil {
			log.Printf("beam ticket: node %d secret load failed, no direct path advertised: %v", server.NodeID, serr)
		} else if ok {
			if fp, ferr := beamauth.LANCertFingerprint(hex.EncodeToString(secret), nodeDiscoveryID); ferr == nil {
				directFingerprint = fp
			}
		}
	}

	// Presence-driven gate: the node's LAN IPs are handed out regardless of relay
	// presence (a co-located client can take the fast path); the public IP is added
	// only when no relay is registered, since with a relay it would deanonymize the
	// node. resolveRelay reads the same settings GetBeamConfig uses.
	getSetting := func(key string) string {
		val, _ := h.state.Store.GetSetting(key)
		return val
	}
	relayAddr, _ := resolveRelay(r.Context(), h.state.Redis, getSetting("beam.relay_address"), getSetting("beam.public_host"), nodeRegion)
	directHints := buildBeamDirectHints(relayAddr, nodePrivateIPs, nodePublicIP, beamLANPort, directFingerprint)

	// Sign ticket via shared auth package — same format used by gateway
	// beam-relay validators and node-side BeamNodeService.Authenticate.
	claims := beamauth.BeamClaims{
		ServerUUID: server.UUID,
		NodeID:     nodeDiscoveryID,
		Username:   username,
		IsAdmin:    isAdmin,
		// What this ticket may DO, not only which server it may reach. Without
		// it the node had nothing to check and allowed every file operation to
		// anyone holding a valid ticket.
		Perms: &filePerms,
	}
	// The same per-node secret that keys the LAN certificate also stamps a
	// per-node proof into the ticket, so a node holding no fleet secret can
	// still authenticate it. Without this the ticket is unreadable to a BYON
	// machine and beam file access does not exist there at all - the LAN
	// listener comes up, TLS pins, and every request is then refused.
	//
	// Best-effort by design: a node whose secret will not load still gets a
	// plain fleet-signed ticket, which is exactly what it got before. Losing
	// the proof degrades to the old behaviour rather than to no ticket.
	var nodeProofSecret []byte
	if server.NodeID != 0 {
		if secret, ok, serr := redisacl.LoadNodeSecret(h.state.Store, h.clusterSecret, server.NodeID); serr != nil {
			log.Printf("beam ticket: node %d secret load failed, ticket carries no node proof: %v", server.NodeID, serr)
		} else if ok {
			nodeProofSecret = secret
		}
	}
	ticketString, err := beamauth.SignBeamTicketWithNodeProof(
		h.jwtSecret, claims, beamauth.BeamTicketTTL, nodeProofSecret)
	if err != nil {
		sendJSONError(w, fmt.Sprintf("Failed to sign ticket: %v", err), http.StatusInternalServerError)
		return
	}

	// expires is informational. Compute it from the TTL directly - SignBeamTicket
	// takes claims by value and sets ExpiresAt on its own copy, so claims.ExpiresAt
	// here is still nil; dereferencing it panicked the handler (the connection drop
	// surfaced as a 502).
	// Direct-connect hints (lanHints): the node's LAN IPs on the pinned-TLS beam port,
	// plus its public address when no relay is present. The LAN IPs are handed out even
	// when a relay is registered so a co-located client can take the fast path; the
	// public address is omitted with a relay so the node IP stays hidden. The JWT ticket
	// gates auth (node+server bound) on either path. Returned only to a caller already
	// authorized for this server.
	resp := map[string]interface{}{
		"success": true,
		"ticket":  ticketString,
		"expires": time.Now().Add(beamauth.BeamTicketTTL).Unix(),
	}
	if directHints != nil {
		resp["lanHints"] = directHints
	}
	json.NewEncoder(w).Encode(resp)
}

// beamLANPort is the node's LAN fast-path TLS port (BEAM_LAN_PORT default). The
// app probes the node's private IPs on this port, dials TLS, and pins the
// fingerprint above. Distinct from the plain overlay port (25521) used by the relay.
const beamLANPort = "25523"

// beamDirectHints is the direct-connect payload on a beam ticket (serialized as the
// JSON field "lanHints"). It carries the node's LAN IPs and, when no relay is present,
// its public address on the pinned-TLS beam port. The LAN IPs are emitted whenever the
// node has a pinnable fingerprint - even with a relay - so a co-located client can take
// the fast path; the public address is emitted ONLY when no relay is present, so a
// relay-fronted node's public IP stays hidden.
type beamDirectHints struct {
	IPs         []string `json:"ips"`
	PublicAddr  string   `json:"publicAddr"`
	Port        string   `json:"port"`
	Fingerprint string   `json:"fingerprint"`
}

// buildBeamDirectHints returns the node's dialable direct targets on the pinned-TLS
// beam port. The LAN IPs are always emitted when a fingerprint exists (a co-located
// client can use them even when a relay is present; a remote client's LAN probe just
// misses and falls back to the relay). The public address is emitted ONLY when no
// relay is present - with a relay it is the deanonymizing target and stays hidden.
// Returns nil when there is no fingerprint to pin (never advertise an unpinnable,
// plaintext-risk path) or when nothing is dialable. Pure (no I/O) so it is unit-tested
// directly.
func buildBeamDirectHints(relayAddr string, lanIPs []string, publicIP, port, fingerprint string) *beamDirectHints {
	if fingerprint == "" {
		return nil // no pin -> refuse to advertise an unpinnable direct path
	}
	h := &beamDirectHints{IPs: lanIPs, Port: port, Fingerprint: fingerprint}
	// The public IP is the deanonymizing target: emit it ONLY with no relay. With a
	// relay the LAN IPs are still handed out (a co-located client uses them; a remote
	// client's LAN probe just misses), but the public address stays hidden.
	if strings.TrimSpace(relayAddr) == "" {
		if ip := strings.TrimSpace(publicIP); ip != "" {
			h.PublicAddr = net.JoinHostPort(ip, port)
		}
	}
	if len(h.IPs) == 0 && h.PublicAddr == "" {
		return nil // nothing to dial
	}
	return h
}

// GetBeamConfig returns the Beam relay address and branding info.
// GET /api/beam/config
func (h *BeamHandler) GetBeamConfig(w http.ResponseWriter, r *http.Request) {
	getSetting := func(key string) string {
		val, _ := h.state.Store.GetSetting(key)
		return val
	}

	manualOverride := getSetting("beam.relay_address")
	publicHost := getSetting("beam.public_host")
	// No server context here, so no region to prefer: this endpoint answers
	// "which relay can I reach at all", and the ticket path applies the region.
	relayAddress, _ := resolveRelay(r.Context(), h.state.Redis, manualOverride, publicHost, "")
	enabled := getSetting("beam.enabled")
	if enabled == "" {
		enabled = "true"
	}

	// Branding
	brandName := getSetting("branding.name")
	if brandName == "" {
		brandName = "Dylaris"
	}
	brandLogoURL := getSetting("branding.logo_url")

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"relay_address": relayAddress,
		"enabled":       enabled == "true",
		// min_version is the advertised force-update floor (empty = gating off).
		// The Beam app reads it for its proactive startup gate; the actual
		// enforcement lives in GetBeamTicket, not here. It always comes from the
		// signed release manifest - see effectiveMinVersion.
		"min_version": h.effectiveMinVersion(r.Context()),
		// update_channel is deliberately absent: the prerelease channel was
		// removed, and the app normalizes a missing value to "stable".
		"branding": map[string]string{
			"name":     brandName,
			"logo_url": brandLogoURL,
		},
	})
}

// validBeamPlatforms is the set of platform slugs Core will serve.
//
// It used to say it mirrored gateway/beam/relay/binaries.go. That file was
// deleted in eeff445 (the app was decoupled from the relay and now releases via
// GitHub), so the pointer sent anyone adding a platform to a file that no
// longer exists. The two places that must actually agree today are BOTH in this
// repo: detectBeamPlatform in core/main.go, and the BeamPlatform union in
// panel/src/app/(authed)/servers/[id]/files/page.tsx - the panel's is the one
// that decides what the browser asks for.
var validBeamPlatforms = map[string]bool{
	"windows-amd64": true,
	"linux-amd64":   true,
	"linux-arm64":   true,
	"darwin-amd64":  true,
	"darwin-arm64":  true,
}

// beamBinaryClient fetches the Beam executable that Core then streams to the
// browser.
//
// It VERIFIES TLS. It used to set InsecureSkipVerify, and the reason was sound
// at the time: the upstream was a beam-relay on the internal overlay holding a
// self-signed cert. That upstream is gone - eeff445 decoupled the app from the
// relay and deleted the relay's binaries.go, so the only upstreams left are the
// GitHub release asset from the signed manifest and an operator-set
// beam.download_link (documented as a CDN/mirror). Skipping verification on a
// public-internet fetch of an EXECUTABLE meant anyone able to intercept Core's
// egress could substitute the binary, and the browser would still see it
// arriving over Core's own trusted TLS with no signal that anything was wrong.
//
// An operator pointing beam.download_link at an internal host with a
// self-signed cert now needs a real certificate there.
//
// It also dials through services.SafeDialContext, which refuses any address
// that resolves to a loopback, private, CGNAT or link-local range - the
// cloud metadata endpoint included. That is load-bearing, not tidiness:
// beam.download_link is a settings.write-writable string this client GETs and
// GetBeamDownload then streams straight to the caller, and that route is
// deliberately unauthenticated (you fetch the client before you have an
// account). Without the guard a settings.write holder - a delegatable panel
// capability, not admin - could point Core at an internal address and have
// anyone on the internet read the response back out of it. The check runs on
// the resolved IP at connect time, so every redirect hop is covered too.
//
// Tight connect + response-header timeouts so an unreachable upstream surfaces
// as a 502 in a few seconds instead of leaving the browser spinning.
var beamBinaryClient = &http.Client{
	Timeout: 30 * time.Minute, // total — leaves room for slow networks on large binaries
	Transport: &http.Transport{
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		DialContext:           beamDialContext,
		TLSHandshakeTimeout:   5 * time.Second,
	},
}

// beamDial is the dial both beam upstream fetches (the binary and the signed
// manifest) go through. It is a variable ONLY so a test can point an httptest
// server at it: httptest binds 127.0.0.1, which the guard correctly refuses.
// Nothing in production reassigns it, and a test that does must restore it.
var beamDial = services.SafeDialContext

// beamDialContext is the indirection the transports hold, so swapping beamDial
// takes effect on clients built at package init.
func beamDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return beamDial(ctx, network, addr)
}

// GetBeamDownload streams a Beam binary through Core. We deliberately do
// NOT 302-redirect any more — that exposes the relay's internal IP /
// hostname to the browser, which usually means a 172.x address on the
// overlay network or a hostname with a self-signed cert. By reverse-
// proxying through Core the browser only ever sees a single trusted
// Core-served URL.
// GET /api/beam/download?platform={os}-{arch}
// Returns the platform index when no platform query is given.
func (h *BeamHandler) GetBeamDownload(w http.ResponseWriter, r *http.Request) {
	platform := r.URL.Query().Get("platform")
	if platform == "" {
		platforms := make([]string, 0, len(validBeamPlatforms))
		for p := range validBeamPlatforms {
			platforms = append(platforms, p)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"platforms": platforms,
		})
		return
	}

	if !validBeamPlatforms[platform] {
		sendJSONError(w, "Invalid platform", http.StatusBadRequest)
		return
	}

	getSetting := func(key string) string {
		val, _ := h.state.Store.GetSetting(key)
		return val
	}

	// The upstream is the signed release manifest's URL for this platform, or
	// the beam.download_link override. Relays have not served binaries since
	// eeff445; the loop below survives because the override may still name
	// several hosts in future, and one attempt is the normal case.
	candidates, expectedSHA, resolveErr := resolveDownloadCandidates(r.Context(), h.state.Redis, getSetting, platform)
	if len(candidates) == 0 {
		// This route is UNAUTHENTICATED and browser-facing (/api/tools/beam
		// redirects into it off the User-Agent), so the two reasons a download
		// cannot start are answered differently. "We do not build for your
		// platform" is a permanent, ordinary fact the visitor can act on; a
		// manifest that is missing, unreachable or unverifiable is an operator
		// problem the visitor can only wait out, and its detail belongs in the
		// log rather than in a stranger's browser.
		if errors.Is(resolveErr, errBeamPlatformNotInManifest) {
			sendJSONError(w, "Beam is not available for "+platform+" yet.", http.StatusNotFound)
			return
		}
		log.Printf("beam-download: cannot resolve %s from the signed release manifest "+
			"(check beam.release_manifest, or set beam.download_link): %v", platform, resolveErr)
		sendJSONError(w, "The Beam download is temporarily unavailable. Please try again shortly.",
			http.StatusServiceUnavailable)
		return
	}

	var resp *http.Response
	var lastErr error
	var winningURL string
	for _, upstream := range candidates {
		log.Printf("beam-download: trying upstream %s for platform=%s", upstream, platform)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream, nil)
		if err != nil {
			lastErr = fmt.Errorf("build request: %w", err)
			continue
		}
		if ua := r.Header.Get("User-Agent"); ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		r2, doErr := beamBinaryClient.Do(req)
		if doErr != nil {
			log.Printf("beam-download: %s failed: %v", upstream, doErr)
			lastErr = doErr
			continue
		}
		if r2.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(r2.Body, 512))
			r2.Body.Close()
			log.Printf("beam-download: %s returned %d: %s", upstream, r2.StatusCode, strings.TrimSpace(string(body)))
			lastErr = fmt.Errorf("upstream returned %d", r2.StatusCode)
			continue
		}
		resp = r2
		winningURL = upstream
		break
	}
	if resp == nil {
		msg := "Could not fetch the Beam binary from its release URL"
		if lastErr != nil {
			msg += ": " + lastErr.Error()
		}
		sendJSONError(w, msg, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	log.Printf("beam-download: streaming %s (%s) for platform=%s", winningURL, resp.Header.Get("Content-Length"), platform)

	// Buffer and verify BEFORE a single byte reaches the browser.
	//
	// Streaming straight through and hashing on the way would only let us abort
	// mid-file: the user would already hold most of an executable we have just
	// discovered we cannot vouch for, and a truncated download reads as a flaky
	// network, not as a rejected artifact. A Beam binary is tens of megabytes, and
	// this runs once per user per release, so a temp file is the cheap side of
	// that trade.
	verified, size, vErr := verifiedBeamBody(resp.Body, expectedSHA)
	if vErr != nil {
		log.Printf("beam-download: REFUSING %s for platform=%s: %v", winningURL, platform, vErr)
		sendJSONError(w, "The Beam binary served for "+platform+
			" does not match the signed release manifest, so it was not delivered."+
			" If beam.download_link is set, that mirror is serving something else.",
			http.StatusBadGateway)
		return
	}
	defer verified.Close()

	// Mirror the relay's content headers so the browser saves the file
	// with the right filename and gets a real Content-Length progress bar.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	} else {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="beam-%s"`, platform))
	}
	// The verified size, not the upstream's header: they are the same for an
	// honest upstream, and where they differ the header is the one we cannot
	// trust.
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, verified); err != nil {
		log.Printf("beam-download: send to client failed for platform=%s: %v", platform, err)
	}
}

// maxBeamBinaryBytes caps what Core will buffer for one download. Well above any
// real Beam build, low enough that a hostile or misconfigured mirror cannot fill
// the disk by answering with an endless body.
const maxBeamBinaryBytes = 512 << 20 // 512 MiB

// verifiedBeamBody spools src to a temp file while hashing it, and returns a
// reader positioned at the start ONLY if the digest matches expectedSHA.
//
// The returned Close removes the file, so a caller that defers it never leaves
// one behind, on the success path or the failure one.
func verifiedBeamBody(src io.Reader, expectedSHA string) (io.ReadCloser, int64, error) {
	if expectedSHA == "" {
		// Fail closed. An empty digest means the manifest carried no sha256 for
		// this platform, and serving an unverifiable executable is the exact
		// thing this function exists to stop.
		return nil, 0, errors.New("signed manifest carries no sha256 for this platform")
	}
	f, err := os.CreateTemp("", "beam-dl-*")
	if err != nil {
		return nil, 0, fmt.Errorf("create temp: %w", err)
	}
	cleanup := func() {
		f.Close()
		os.Remove(f.Name())
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(src, maxBeamBinaryBytes+1))
	if err != nil {
		cleanup()
		return nil, 0, fmt.Errorf("read upstream: %w", err)
	}
	if n > maxBeamBinaryBytes {
		cleanup()
		return nil, 0, fmt.Errorf("upstream body exceeds %d bytes", int64(maxBeamBinaryBytes))
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, expectedSHA) {
		cleanup()
		return nil, 0, fmt.Errorf("sha256 mismatch: got %s, manifest says %s", got, expectedSHA)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, 0, fmt.Errorf("rewind temp: %w", err)
	}
	return &tempFileReader{File: f}, n, nil
}

// tempFileReader deletes the backing file on Close.
type tempFileReader struct{ *os.File }

func (t *tempFileReader) Close() error {
	name := t.Name()
	err := t.File.Close()
	os.Remove(name)
	return err
}

// resolveDownloadCandidates returns an ordered list of upstream URLs the
// download proxy should try, best-first.
//
// This is a Core→Relay server-to-server fetch. It uses the relays
// registered in Redis (beam:registry:*) directly — strictly internal
// coordinates (overlay IP, private IP, BEAM_ID via Docker-DNS). The
// client-facing beam.relay_address / beam.public_host settings are
// deliberately NOT considered here: they pin where Beam.exe connects
// from outside, where the download port (25552) is typically closed.
//
// The Beam app is published to GitHub Releases (no longer served by the relay).
// Resolution order:
//  1. beam.download_link - explicit full-URL override (CDN/mirror).
//  2. The platform URL from the GitHub manifest (beam.release_manifest, default
//     https://github.com/Bartis-Dev/dylaris-platform/releases/latest/download/latest.json).
//     Public once the repo is public, so Core fetches it without auth.
func resolveDownloadCandidates(ctx context.Context, rdb *redis.Client, getSetting func(string) string, platform string) ([]string, string, error) {
	manifestURL := strings.TrimSpace(getSetting("beam.release_manifest"))
	if manifestURL == "" {
		manifestURL = defaultBeamManifestURL
	}
	// The signed manifest is consulted FIRST and always, even when an override
	// exists, because it is the only thing that says what these bytes are
	// allowed to be.
	//
	// beam.download_link used to return before this ran, which meant the
	// override served an executable with no signature check anywhere in the
	// path. It is a settings.write string - a delegatable panel capability, not
	// admin - and this route is deliberately unauthenticated, so a holder of
	// that capability could have had Core hand every downloading user an
	// arbitrary binary over Core's own trusted TLS.
	//
	// Now the override only moves the LOCATION. The digest below still has to
	// match, so a mirror serves the same artifact or it serves nothing.
	u, sha, err := fetchVerifiedBeamPlatformArtifact(ctx, manifestURL, beamUpdatePublicKeyB64, platform)
	if err != nil {
		// The caller decides what the visitor is told; it needs the error to tell
		// "no build for this platform" apart from "the feed is down".
		return nil, "", err
	}

	if link := strings.TrimSpace(getSetting("beam.download_link")); link != "" {
		base := strings.TrimRight(link, "/")
		if !strings.Contains(base, "/download/") {
			base = base + "/download/" + platform
		}
		return []string{base}, sha, nil
	}
	return []string{u}, sha, nil
}
