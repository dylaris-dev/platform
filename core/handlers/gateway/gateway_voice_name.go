package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/handlers/gate"
	"dylaris-core/handlers/guard"
	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/nodeops"
	"dylaris-core/handlers/validate"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/voicecfg"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// The white-label voice name: a name on the customer's own domain, written
// into the voice mod instead of our regional name once Core has checked it
// points at the voice port's region. The node, the link and the relays never
// see it: they keep resolving our name, so a broken record can only cost the
// customer's own players the voice connection, never anyone's join.

// validVoiceName is name lower cased, and whether it may be checked at all:
// a host name of two labels or more, not an IP, not equal to or under one of
// reserved (the hoster domains and edge wildcard suffixes), and equal to or
// under one of routeDomains, the caller's own admitted addresses. Nothing is
// resolved for a name this refuses (A4).
func validVoiceName(name string, routeDomains []string, reserved []services.HosterDomain) (string, bool) {
	n := validate.NormHost(name)
	if !validate.IsHostname(n, 2) {
		return n, false
	}
	if _, err := netip.ParseAddr(n); err == nil {
		return n, false
	}
	if reservedName(n, reserved) {
		return n, false
	}
	return n, slices.ContainsFunc(routeDomains, func(d string) bool { return nameUnder(n, d) })
}

// nameUnder is n equal to d or under it at a label boundary.
func nameUnder(n, d string) bool {
	d = validate.NormHost(d)
	return d != "" && (n == d || strings.HasSuffix(n, "."+d))
}

func reservedName(n string, reserved []services.HosterDomain) bool {
	return slices.ContainsFunc(reserved, func(hd services.HosterDomain) bool { return nameUnder(n, hd.Domain) })
}

// voiceNameReserved is the hoster domains plus every edge wildcard suffix:
// names there are ours, never a customer's white-label name.
func voiceNameReserved(hosters []services.HosterDomain, edges []services.GatewayEdgeInfo) []services.HosterDomain {
	out := slices.Clone(hosters)
	for _, e := range edges {
		if suffix, ok := strings.CutPrefix(e.Wildcard, "*."); ok {
			out = append(out, services.HosterDomain{Domain: suffix})
		}
	}
	return out
}

// voiceNameInvalid answers 400 voice_host_invalid naming where the name has
// to live.
func voiceNameInvalid(w http.ResponseWriter, name string, routeDomains []string) {
	msg := "Add an address on your own domain first."
	if len(routeDomains) > 0 {
		where := routeDomains[0]
		if len(routeDomains) > 1 {
			where = "one of " + strings.Join(routeDomains, ", ")
		}
		if !validate.IsHostname(name, 1) {
			name = "The name"
		}
		msg = fmt.Sprintf("%s has to be %s or a name under it.", name, where)
	}
	httpx.SendJSONErrorCode(w, msg, "voice_host_invalid", http.StatusBadRequest)
}

// voiceNameVerdict is the pointing check of a white-label name for a voice
// row in region. state is the row's new state (active or pending), or ""
// when nothing may change: "could not check" (503 dns_lookup_failed, 429
// rate_limited) stores and changes nothing. code "" means active. A verdict
// is stored with RecordAddressDNSCheck exactly as the checker answered it:
// the row is shared with the Bedrock check of the same name, so another
// region is an ok there and only decides pending here. whose names the row
// in the wrong-region sentence ("this server's").
//
// A user's set or Check again may be answered from the checker's cache (up
// to 30 s for a miss); the stored check is display state for those.
func (h *GatewayHandler) voiceNameVerdict(ctx context.Context, r *http.Request, owner, name, region, whose string, edges []services.GatewayEdgeInfo) (state, code string, status int, msg string) {
	// A nil checker answers lookup_failed.
	p := h.d.Pointing.CheckFor(ctx, owner, name, services.PurposeVoiceName, h.pointingAllow(r))
	switch {
	case p.Status == services.PointingRateLimited:
		return "", "rate_limited", http.StatusTooManyRequests, services.PointingUnknownText(p, name)
	case !p.Known():
		return "", "dns_lookup_failed", http.StatusServiceUnavailable, services.PointingUnknownText(p, name)
	}
	if _, err := h.d.Store.RecordAddressDNSCheck(name, string(p.Status), p.Region, p.Status == services.PointingOK); err != nil {
		log.Printf("voice: %s: store dns check: %v", name, err) // display state only
	}
	if p.Status == services.PointingOK && p.Region == region {
		return store.VoicePublicActive, "", http.StatusOK, ""
	}
	targets := h.pointingCNAMETargets()
	to := "the address shown under Your own domains"
	if ours, found := services.VoiceHostFor(region, edges); found {
		targets, to = []string{ours}, ours
	} else if len(targets) > 0 {
		to = strings.Join(targets, " or ")
	}
	if p.Status == services.PointingOK {
		return store.VoicePublicPending, "voice_host_not_pointing", http.StatusConflict,
			fmt.Sprintf("%s points at the gateway's region %s, but %s voice chat runs in %s. Point it at %s.", name, p.Region, whose, region, to)
	}
	return store.VoicePublicPending, "voice_host_not_pointing", http.StatusConflict,
		services.PointingRefusal(p, name, targets, services.PurposeVoiceName)
}

// keepActive is whether a verdict that is not active leaves the row alone:
// while a name is active only the background re-check (three misses over 15
// minutes) may take it out of the mod, so a miss on Check again of it, or a
// different name that does not point yet, changes nothing and only answers
// the sentence.
func keepActive(state, priorState string) bool {
	return state != store.VoicePublicActive && priorState == store.VoicePublicActive
}

// keepFallenBack is state, except that a fallen back name checked again and
// still missing stays fallen_back: the mod already carries our name, and the
// restart warning must not turn into "waiting for your record".
func keepFallenBack(state, name string, priorHost, priorState string) string {
	if state == store.VoicePublicPending && priorState == store.VoicePublicFallenBack && priorHost == name {
		return store.VoicePublicFallenBack
	}
	return state
}

// writeVoiceModHost points the server's voice mod at host:port, the same
// write as the enable. No sub-server: nothing to write, the start sync does.
func writeVoiceModHost(reg *nodegrpc.Registry, srv *models.Server, host string, port int) error {
	if srv.ActiveSubServer == "" {
		return nil
	}
	mod, rel, content, err := nodeops.FindVoiceConfig(reg, srv.NodeID, srv.UUID, srv.ActiveSubServer)
	if err != nil {
		return err
	}
	merged := voicecfg.SvcEnable(content, host, port)
	if mod == "plasmo" {
		merged = voicecfg.PlasmoEnable(content, host, port)
	}
	if merged == content {
		return nil
	}
	return nodeops.WriteNodeFileString(reg, srv.NodeID, srv.UUID, srv.ActiveSubServer+"/"+rel, merged)
}

// SetServerVoiceName PUT /api/servers/{id}/voice/name - body {"name": "..."},
// the server's white-label voice name: equal to or under one of its own
// domains. Once it points at the voice port's region the mod is told it
// (restartRequired); until then it is pending and the mod keeps our name. ""
// cancels it and puts our name back. Check again is the same call. While a
// name is active a miss changes nothing (keepActive). Voice must be on (409
// voice_off). In local mode cancelling is never refused. On the managed
// gateway the provider checks the name (setRemoteVoiceName).
func (h *GatewayHandler) SetServerVoiceName(w http.ResponseWriter, r *http.Request) {
	srv, err := h.d.Store.GetServerByID(httpx.MustAtoi(mux.Vars(r)["id"]))
	if err != nil || srv == nil {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req struct {
		Name *string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Name == nil {
		httpx.SendJSONError(w, `Invalid request: expected {"name": "<host name>"}`, http.StatusBadRequest)
		return
	}
	rg, merr := services.ActiveRemoteGateway(h.d.GatewayRemote, h.d.RemoteGateway)
	if gate.GatewayModeError(w, merr) {
		return
	}
	if rg != nil {
		h.setRemoteVoiceName(w, r, rg, srv, validate.NormHost(*req.Name))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	vp, err := h.d.Store.GetVoicePort(srv.ID)
	if err != nil {
		log.Printf("voice: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to read the voice chat setting", http.StatusInternalServerError)
		return
	}
	if vp == nil {
		httpx.SendJSONErrorCode(w, "Turn voice chat on first.", "voice_off", http.StatusConflict)
		return
	}
	name := validate.NormHost(*req.Name)

	if name == "" {
		restart := vp.PublicState == store.VoicePublicActive
		if vp.PublicHost != "" || vp.PublicState != "" {
			if err := h.d.Store.SetVoicePublicHost(srv.ID, "", ""); err != nil {
				log.Printf("voice: server %d: clear the voice name: %v", srv.ID, err)
				httpx.SendJSONError(w, "Failed to save the voice chat name", http.StatusInternalServerError)
				return
			}
		}
		// The row is the truth: the start sync writes our name even if this
		// write fails.
		if restart {
			if err := writeVoiceModHost(h.d.GRPCRegistry, srv, vp.Host, vp.Port); err != nil {
				log.Printf("voice: server %d: write our voice name back: %v", srv.ID, err)
			}
		}
		vp.PublicHost, vp.PublicState, vp.PublicStateAt = "", "", nil
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "voice": h.withVoiceNameInfo(ctx, newVoiceView(vp, h.voiceAllowed(srv)), srv, vp), "restartRequired": restart})
		return
	}

	if !services.GatewayRoutingEnabled(h.d.Store) {
		gate.GatewayDisabledResponse(w)
		return
	}
	if guard.RefuseIfSuspended(w, r, h.d.Store, h.d.SuspendGrace, srv) {
		return
	}
	own, reserved, edges, err := h.serverOwnDomains(ctx, srv)
	if err != nil {
		log.Printf("voice: server %d: read routes: %v", srv.ID, err)
		httpx.SendJSONError(w, "Could not read this server's routes. Try again.", http.StatusBadGateway)
		return
	}
	name, ok := validVoiceName(name, own, reserved)
	if !ok {
		voiceNameInvalid(w, name, own)
		return
	}
	state, code, status, msg := h.voiceNameVerdict(ctx, r, srv.OwnerID, name, vp.Region, "this server's", edges)
	if state == "" {
		httpx.SendJSONErrorCode(w, msg, code, status)
		return
	}
	if keepActive(state, vp.PublicState) {
		httpx.SendJSONErrorCode(w, msg, code, status)
		return
	}
	state = keepFallenBack(state, name, vp.PublicHost, vp.PublicState)
	// The config first: a failed write leaves the row as it was.
	if state == store.VoicePublicActive {
		if err := writeVoiceModHost(h.d.GRPCRegistry, srv, name, vp.Port); err != nil {
			log.Printf("voice: server %d: write the voice name: %v", srv.ID, err)
			httpx.SendJSONError(w, "The node could not write the voice chat config. Try again.", http.StatusBadGateway)
			return
		}
	}
	if err := h.d.Store.SetVoicePublicHost(srv.ID, name, state); err != nil {
		log.Printf("voice: server %d: save the voice name: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to save the voice chat name", http.StatusInternalServerError)
		return
	}
	restart := state == store.VoicePublicActive
	if code != "" {
		httpx.SendJSONErrorCode(w, msg, code, status)
		return
	}
	if vp.PublicState != state {
		now := time.Now()
		vp.PublicStateAt = &now // what the setter just stored
	}
	vp.PublicHost, vp.PublicState = name, state
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "voice": h.withVoiceNameInfo(ctx, newVoiceView(vp, h.voiceAllowed(srv)), srv, vp), "restartRequired": restart})
}

// setRemoteVoiceName is SetServerVoiceName on the managed gateway: this Core
// resolves nothing, the provider checks the name and its refusal sentences
// (400 voice_host_invalid, 409 voice_host_not_pointing, 503
// dns_lookup_failed, a rate limit) are passed through with nothing written.
// On a yes its mod_host goes into the mod config. Cancelling is refused only
// by what the provider answers.
func (h *GatewayHandler) setRemoteVoiceName(w http.ResponseWriter, r *http.Request, rg *services.RemoteGateway, srv *models.Server, name string) {
	if name != "" {
		if !services.GatewayRoutingEnabled(h.d.Store) {
			gate.GatewayDisabledResponse(w)
			return
		}
		if guard.RefuseIfSuspended(w, r, h.d.Store, h.d.SuspendGrace, srv) {
			return
		}
	}
	host, port, mod, state, changed, err := rg.SetServerVoiceName(r.Context(), srv.UUID, name, func(host string, port int) error {
		return writeVoiceModHost(h.d.GRPCRegistry, srv, host, port)
	})
	switch {
	case errors.Is(err, services.ErrRemoteVoiceOff):
		httpx.SendJSONErrorCode(w, "Turn voice chat on first.", "voice_off", http.StatusConflict)
		return
	case errors.Is(err, services.ErrVoiceNameUnsupported):
		httpx.SendJSONErrorCode(w, "The gateway provider does not offer a voice chat name on your own domain yet.", "voice_name_unavailable", http.StatusConflict)
		return
	case errors.Is(err, services.ErrVoiceConfigWrite):
		log.Printf("voice: server %d: voice chat name: %v", srv.ID, err)
		httpx.SendJSONError(w, "The node could not write the voice chat config. Try again.", http.StatusBadGateway)
		return
	case err != nil:
		if !gate.GatewayProviderError(w, err) {
			log.Printf("voice: server %d: voice chat name on the gateway provider: %v", srv.ID, err)
			httpx.SendJSONError(w, "Failed to save the voice chat name", http.StatusInternalServerError)
		}
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true,
		"voice": withRemoteVoiceName(voiceView{Enabled: true, Host: host, Port: port, Mod: mod, Allowed: h.voiceAllowed(srv)}, state), "restartRequired": changed})
}

// voiceNameInfo is what a voice view needs beyond its row to show the
// white-label states: our reserved domains, the edges (for the CNAME target)
// and the stored checks of the active names. Redis, settings and the database
// only, never DNS.
type voiceNameInfo struct {
	reserved []services.HosterDomain
	edges    []services.GatewayEdgeInfo
	checks   map[string]store.AddressDNSCheck
}

// loadVoiceNameInfo reads the hoster domains once and the stored checks of
// activeNames (no query for none). A failed check read leaves the failing
// stamps out: a view without them reads as healthy, as before.
func (h *GatewayHandler) loadVoiceNameInfo(edges []services.GatewayEdgeInfo, activeNames []string) voiceNameInfo {
	hosters, _, _ := h.loadGatewayDomainConfig()
	ni := voiceNameInfo{reserved: voiceNameReserved(hosters, edges), edges: edges}
	if len(activeNames) > 0 {
		checks, err := h.d.Store.GetAddressDNSChecks(activeNames)
		if err != nil {
			log.Printf("voice: stored checks of the voice names: %v", err)
		}
		ni.checks = checks
	}
	return ni
}

// target is the CNAME target the panel shows for a white-label name in
// region: our regional voice name there, else ourHost (no online edge seen).
func (ni voiceNameInfo) target(region, ourHost string) string {
	if t, ok := services.VoiceHostFor(region, ni.edges); ok {
		return t
	}
	return ourHost
}

// failingSince is the start of the current run of misses of an ACTIVE name.
// The stamp alone decides, not the status: the re-check stores a miss in
// another region as status ok with a stamp (services/voice.go).
func (ni voiceNameInfo) failingSince(host, state string) *time.Time {
	if state != store.VoicePublicActive || host == "" {
		return nil
	}
	if c, ok := ni.checks[strings.ToLower(host)]; ok {
		return c.FailingSince
	}
	return nil
}

// serverOwnDomains is the server's own-domain routes, the names a
// white-label name may live under (the list SetServerVoiceName admits
// against), with the reserved domains and the edges read for it.
func (h *GatewayHandler) serverOwnDomains(ctx context.Context, srv *models.Server) (own []string, reserved []services.HosterDomain, edges []services.GatewayEdgeInfo, err error) {
	routes, err := services.ReadRoutesFromRedis(ctx, h.d.Redis)
	if err != nil {
		return nil, nil, nil, err
	}
	edges = services.GetEdgesFromRedis(ctx, h.d.Redis)
	hosters, _, _ := h.loadGatewayDomainConfig()
	reserved = voiceNameReserved(hosters, edges)
	for _, rt := range routes {
		if d := validate.NormHost(rt.Domain); services.RouteBelongsToServer(rt, srv.UUID) && !reservedName(d, reserved) {
			own = append(own, d)
		}
	}
	slices.Sort(own)
	return own, reserved, edges, nil
}

// withVoiceNameInfo adds nameTarget, nameFailingSince and publicStateAt to a
// local-mode view of vp. nameTarget is "" for a server without an own domain
// (no name possible there). A failed route read leaves it out, which the panel
// reads as an older Core.
func (h *GatewayHandler) withVoiceNameInfo(ctx context.Context, v voiceView, srv *models.Server, vp *store.VoicePort) voiceView {
	if vp == nil {
		return v
	}
	v.PublicStateAt = vp.PublicStateAt
	own, _, edges, err := h.serverOwnDomains(ctx, srv)
	if err != nil {
		log.Printf("voice: server %d: read routes for the voice name: %v", srv.ID, err)
		return v
	}
	var active []string
	if vp.PublicState == store.VoicePublicActive && vp.PublicHost != "" {
		active = []string{vp.PublicHost}
	}
	ni := h.loadVoiceNameInfo(edges, active)
	v.NameFailingSince = ni.failingSince(vp.PublicHost, vp.PublicState)
	target := ""
	if len(own) > 0 {
		target = ni.target(vp.Region, vp.Host)
	}
	v.NameTarget = &target
	return v
}
