package servers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/mux"

	"dylaris-core/authz"
	"dylaris-core/handlers/guard"
	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/nodeops"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/voicecfg"
	"dylaris-core/store"
)

// Extra ports per server in direct routing: see
// docs/superpowers/specs/2026-10-25-extra-server-ports-design.md.

const (
	noAddressMsg = "The node has no public address yet. Set its public IP under Nodes, or an address for extra ports under Settings, Extra ports."
	portNameMax  = 40
	portNoteMax  = 200
	// voicePresetPort is the port both voice mods listen on in the container.
	voicePresetPort = 24454
)

type portsView struct {
	Ports       []store.ServerPort `json:"ports"`
	Allowed     bool               `json:"allowed"`
	RoutingMode string             `json:"routingMode"`
	Limit       *int64             `json:"limit"`
	Used        int                `json:"used"`
	Range       string             `json:"range"`
	Address     string             `json:"address"`
	IsAdmin     bool               `json:"isAdmin"`
	// CanWrite is whether THIS caller holds network.write on the server, so
	// the panel offers add/edit/remove to exactly who the routes accept.
	CanWrite bool `json:"canWrite"`
}

func (h *ServerHandler) routingMode() string {
	m, _ := h.d.Store.GetSetting("routing_mode")
	if m == "" {
		return "ip_port"
	}
	return m
}

// nodePublicAddress is what a player dials for this node in direct routing:
// the operator-set public IP, else the address the heartbeat reported.
func nodePublicAddress(node *models.Node) string {
	if node == nil {
		return ""
	}
	if node.PublicIP != "" {
		return node.PublicIP
	}
	if node.Address == "0.0.0.0" {
		return ""
	}
	return node.Address
}

// effectivePortRange is the node's own range, else the global one, else the
// self-hosted default ("" = none).
func (h *ServerHandler) effectivePortRange(nodeID int) (string, error) {
	ranges, err := h.d.Store.GetNodeExtraPortRanges()
	if err != nil {
		return "", err
	}
	g, saved, err := services.GlobalPortRange(h.d.Store)
	if err != nil {
		return "", err
	}
	rng, _ := services.EffectivePortRange(ranges[nodeID], g, saved, h.d.Hosted)
	return rng, nil
}

// extraPortsAddress is what players dial for the node's extra ports: the
// address an admin set for them, else the node's public address.
func (h *ServerHandler) extraPortsAddress(node *models.Node) string {
	if node == nil {
		return ""
	}
	hosts, err := h.d.Store.GetNodeExtraPortsHosts()
	if err != nil {
		log.Printf("ports: node %d: extra ports address: %v", node.ID, err)
	}
	if v := hosts[node.ID]; v != "" {
		return v
	}
	return nodePublicAddress(node)
}

// GetServerPorts GET /api/servers/{id}/ports
func (h *ServerHandler) GetServerPorts(w http.ResponseWriter, r *http.Request) {
	srv, err := h.d.Store.GetServerByID(httpx.MustAtoi(mux.Vars(r)["id"]))
	if err != nil || srv == nil {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	ports, err := h.d.Store.ListServerPorts(srv.ID)
	if err != nil {
		log.Printf("ports: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to read the server's ports", http.StatusInternalServerError)
		return
	}
	v := portsView{Ports: ports, RoutingMode: h.routingMode(), IsAdmin: reqctx.IsAdmin(r)}
	v.CanWrite = v.IsAdmin
	if h.d.Authz != nil {
		if res, err := h.d.Authz.Resolve(authz.IdentityFromContext(r.Context()), srv.ID); err == nil {
			v.CanWrite = res.HasCap("network.write")
		}
	}
	v.Allowed = v.IsAdmin
	if !v.Allowed {
		v.Allowed, _, _ = services.ResolvePortsAllowed(h.d.Store, srv)
	}
	v.Limit, _, _ = services.ResolvePortLimit(h.d.Store, srv.OwnerID)
	v.Used, _ = h.d.Store.CountServerPortsByOwner(srv.OwnerID)
	v.Range, _ = h.effectivePortRange(srv.NodeID)
	if node, err := h.d.Store.GetNodeByID(srv.NodeID); err == nil {
		v.Address = h.extraPortsAddress(node)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "ports": v})
}

type createPortRequest struct {
	Protocol      string `json:"protocol"`
	ContainerPort int    `json:"containerPort"`
	Name          string `json:"name"`
	Note          string `json:"note"`
	Preset        string `json:"preset"`
	HostPort      int    `json:"hostPort"`
}

// CreateServerPort POST /api/servers/{id}/ports
func (h *ServerHandler) CreateServerPort(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	srv, err := h.d.Store.GetServerByID(httpx.MustAtoi(mux.Vars(r)["id"]))
	if err != nil || srv == nil {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req createPortRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	isAdmin := reqctx.IsAdmin(r)
	mode := h.routingMode()
	if mode == "gateway" {
		httpx.SendJSONErrorCode(w, "Extra ports need direct routing. Behind the gateway, use the voice chat switch under Routes, and share links or custom tabs for web views.", "ports_gateway_mode", http.StatusConflict)
		return
	}
	if guard.RefuseIfSuspended(w, r, h.d.Store, h.d.SuspendGrace, srv) {
		return
	}
	if !isAdmin {
		ok, _, err := services.ResolvePortsAllowed(h.d.Store, srv)
		if err != nil {
			log.Printf("ports: server %d: %v", srv.ID, err)
		}
		if !ok {
			httpx.SendJSONErrorCode(w, "Extra ports are not enabled for this server. Ask an administrator.", "ports_not_allowed", http.StatusForbidden)
			return
		}
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Note = strings.TrimSpace(req.Note)
	switch req.Preset {
	case "":
	case "svc", "plasmo":
		if mode != "ip_port" {
			httpx.SendJSONErrorCode(w, "With the gateway on, turn voice chat on under Routes instead.", "voice_preset_mode", http.StatusConflict)
			return
		}
		req.Protocol, req.ContainerPort = "udp", voicePresetPort
		if req.Name == "" {
			req.Name = "Voice chat"
		}
	default:
		httpx.SendJSONErrorCode(w, "Unknown preset.", "port_invalid", http.StatusBadRequest)
		return
	}
	req.Protocol = strings.ToLower(strings.TrimSpace(req.Protocol))
	switch {
	case req.Protocol != "tcp" && req.Protocol != "udp":
		httpx.SendJSONErrorCode(w, "Choose TCP or UDP.", "port_invalid", http.StatusBadRequest)
		return
	case req.ContainerPort < 1 || req.ContainerPort > 65535:
		httpx.SendJSONErrorCode(w, "The port inside the server must be between 1 and 65535.", "port_invalid", http.StatusBadRequest)
		return
	case req.Name == "":
		httpx.SendJSONErrorCode(w, "Give the port a name, for example the plugin it is for.", "port_invalid", http.StatusBadRequest)
		return
	case utf8.RuneCountInString(req.Name) > portNameMax || utf8.RuneCountInString(req.Note) > portNoteMax:
		httpx.SendJSONErrorCode(w, "The name may have 40 characters and the note 200.", "port_invalid", http.StatusBadRequest)
		return
	case req.HostPort != 0 && !isAdmin:
		httpx.SendJSONErrorCode(w, "Only an administrator picks a host port; yours is assigned from the range.", "port_invalid", http.StatusForbidden)
		return
	}

	existing, err := h.d.Store.ListServerPorts(srv.ID)
	if err != nil {
		log.Printf("ports: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to add the port", http.StatusInternalServerError)
		return
	}
	for _, p := range existing {
		if req.Preset != "" && p.Preset != "" {
			httpx.SendJSONErrorCode(w, "This server already has a voice chat port.", "voice_preset_exists", http.StatusConflict)
			return
		}
		if p.Protocol == req.Protocol && p.ContainerPort == req.ContainerPort {
			httpx.SendJSONErrorCode(w, "This server already publishes that port.", "port_taken", http.StatusConflict)
			return
		}
	}

	node, err := h.d.Store.GetNodeByID(srv.NodeID)
	if err != nil || node == nil {
		httpx.SendJSONError(w, "The server's node was not found", http.StatusNotFound)
		return
	}
	if !nodeops.NodeReleaseAtLeast(ctx, h.d.Redis, node.Token, services.PortsNodeSince) {
		httpx.SendJSONErrorCode(w, "Update this server's node first; it is too old to publish extra ports.", "node_too_old", http.StatusConflict)
		return
	}

	if !isAdmin {
		limit, _, err := services.ResolvePortLimit(h.d.Store, srv.OwnerID)
		used, cerr := h.d.Store.CountServerPortsByOwner(srv.OwnerID)
		if err != nil || cerr != nil {
			log.Printf("ports: server %d: limit: %v / %v", srv.ID, err, cerr)
			httpx.SendJSONError(w, "Failed to add the port", http.StatusInternalServerError)
			return
		}
		if services.AtOrOver(limit, int64(used)) {
			httpx.SendJSONErrorCode(w, "The owner of this server holds as many extra ports as they may ("+strconv.FormatInt(*limit, 10)+"). Remove one, or ask an administrator for more.", "ports_limit_reached", http.StatusConflict)
			return
		}
	}

	// The voice preset needs the mod's config before a port is taken, so a
	// missing mod costs nothing.
	var voiceMod, voiceRel, voiceContent string
	address := h.extraPortsAddress(node)
	if req.Preset != "" {
		if address == "" {
			httpx.SendJSONErrorCode(w, noAddressMsg, "no_node_address", http.StatusConflict)
			return
		}
		if srv.ActiveSubServer == "" {
			httpx.SendJSONErrorCode(w, "Install the server first.", "no_sub_server", http.StatusConflict)
			return
		}
		voiceMod, voiceRel, voiceContent, err = FindVoiceConfig(h.d.GRPCRegistry, srv.NodeID, srv.UUID, srv.ActiveSubServer)
		switch {
		case errors.Is(err, ErrVoiceModMissing):
			httpx.SendJSONErrorCode(w, "Install Simple Voice Chat or Plasmo Voice and start the server once.", "voice_mod_missing", http.StatusConflict)
			return
		case errors.Is(err, ErrVoiceModAmbiguous):
			httpx.SendJSONErrorCode(w, "Both Simple Voice Chat and Plasmo Voice are installed. Remove one, then try again.", "voice_mod_ambiguous", http.StatusConflict)
			return
		case err != nil:
			log.Printf("ports: server %d: %v", srv.ID, err)
			httpx.SendJSONError(w, "Could not read the server's files on its node. Try again.", http.StatusBadGateway)
			return
		}
		req.Preset = voiceMod
	}

	gameRange := ""
	if hb := services.LoadHeartbeat(ctx, h.d.Redis, node.Token); hb != nil {
		gameRange = hb.PortRange
	}
	p := store.ServerPort{ServerID: srv.ID, NodeID: srv.NodeID, Protocol: req.Protocol, ContainerPort: req.ContainerPort,
		Name: req.Name, Note: req.Note, Preset: req.Preset}
	p.CreatedBy, _ = r.Context().Value("userID").(string)
	got, code, msg := h.takePort(p, req.HostPort, gameRange)
	if code != "" {
		status := http.StatusConflict
		if code == "port_out_of_range" || code == "port_invalid" {
			status = http.StatusBadRequest
		}
		httpx.SendJSONErrorCode(w, msg, code, status)
		return
	}

	if req.Preset != "" {
		merged := voicecfg.SvcEnable(voiceContent, address, got.HostPort)
		if voiceMod == "plasmo" {
			merged = voicecfg.PlasmoEnable(voiceContent, address, got.HostPort)
		}
		if merged != voiceContent {
			if err := nodeops.WriteNodeFileString(h.d.GRPCRegistry, srv.NodeID, srv.UUID, srv.ActiveSubServer+"/"+voiceRel, merged); err != nil {
				log.Printf("ports: server %d: write %s: %v", srv.ID, voiceRel, err)
				if derr := h.d.Store.DeleteServerPort(got.ID); derr != nil {
					log.Printf("ports: server %d: roll back port %d: %v", srv.ID, got.ID, derr)
				}
				httpx.SendJSONError(w, "The node could not write the voice chat config. Try again.", http.StatusBadGateway)
				return
			}
		}
	}
	h.publishPorts(ctx, srv)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "port": got, "restartRequired": true})
}

// takePort inserts p on the host port an admin named, or on the lowest free
// port of the node's range. It answers an error code and sentence instead of
// an error, so every refusal reaches the panel as something to act on.
func (h *ServerHandler) takePort(p store.ServerPort, hostPort int, gameRange string) (store.ServerPort, string, string) {
	if hostPort != 0 {
		if hostPort < 1024 || hostPort > 65535 || (hostPort >= 25520 && hostPort <= 25523) {
			return p, "port_out_of_range", "Pick a host port between 1024 and 65535, outside the node's own ports 25520-25523."
		}
		if gameRange != "" && services.InPortRange(hostPort, gameRange) {
			return p, "port_out_of_range", "That port is in the node's game port range (" + gameRange + "). Pick one outside it."
		}
		p.HostPort = hostPort
		got, err := h.d.Store.CreateServerPort(p)
		if errors.Is(err, store.ErrPortTaken) {
			return p, "port_taken", "Another server already uses " + strings.ToUpper(p.Protocol) + " port " + strconv.Itoa(hostPort) + " on this node."
		}
		if err != nil {
			log.Printf("ports: server %d: %v", p.ServerID, err)
			return p, "port_failed", "Failed to add the port."
		}
		return got, "", ""
	}

	rng, err := h.effectivePortRange(p.NodeID)
	if err != nil {
		log.Printf("ports: server %d: range: %v", p.ServerID, err)
		return p, "port_failed", "Failed to add the port."
	}
	if rng == "" {
		return p, "ports_no_range", "No port range is set for this node. An administrator sets one under Settings, Extra ports."
	}
	min, max, err := services.ParsePortRange(rng)
	if err != nil {
		return p, "ports_no_range", "The port range of this node is invalid (" + err.Error() + "). An administrator fixes it under Settings, Extra ports."
	}
	if gameRange != "" && services.PortRangesOverlap(rng, gameRange) {
		return p, "ports_range_conflict", "The extra port range " + rng + " overlaps the node's game port range " + gameRange + ". An administrator moves it under Settings, Extra ports."
	}
	for attempt := 0; attempt < 3; attempt++ {
		free, err := h.d.Store.FreeServerPort(p.NodeID, p.Protocol, min, max, nil)
		if errors.Is(err, store.ErrPortRangeFull) {
			return p, "ports_range_full", "Every " + strings.ToUpper(p.Protocol) + " port in this node's range (" + rng + ") is taken. Ask an administrator to widen the range."
		}
		if err != nil {
			log.Printf("ports: server %d: %v", p.ServerID, err)
			return p, "port_failed", "Failed to add the port."
		}
		p.HostPort = free
		got, err := h.d.Store.CreateServerPort(p)
		if errors.Is(err, store.ErrPortTaken) {
			continue
		}
		if err != nil {
			log.Printf("ports: server %d: %v", p.ServerID, err)
			return p, "port_failed", "Failed to add the port."
		}
		return got, "", ""
	}
	return p, "port_taken", "The port was taken by someone else at the same moment. Try again."
}

// UpdateServerPort PATCH /api/servers/{id}/ports/{portId} - name, note and,
// when sent, the preset: "" turns the voice port into a plain one, "svc" or
// "plasmo" turns a UDP port into the voice port (the installed mod decides
// which, as on create).
func (h *ServerHandler) UpdateServerPort(w http.ResponseWriter, r *http.Request) {
	srv, p, ok := h.loadServerPort(w, r)
	if !ok {
		return
	}
	var req struct {
		Name   string  `json:"name"`
		Note   string  `json:"note"`
		Preset *string `json:"preset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	req.Name, req.Note = strings.TrimSpace(req.Name), strings.TrimSpace(req.Note)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > portNameMax || utf8.RuneCountInString(req.Note) > portNoteMax {
		httpx.SendJSONErrorCode(w, "The name is required and may have 40 characters; the note 200.", "port_invalid", http.StatusBadRequest)
		return
	}
	restart := false
	if req.Preset != nil && (*req.Preset != "") != (p.Preset != "") {
		if !h.changePortPreset(w, r, srv, p, *req.Preset) {
			return
		}
		restart = true
	}
	if err := h.d.Store.UpdateServerPortMeta(p.ID, req.Name, req.Note); err != nil {
		log.Printf("ports: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to save the port", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "restartRequired": restart})
}

// changePortPreset turns p into the voice port or back into a plain one, and
// writes the mod's config to match. It answers its own refusals.
func (h *ServerHandler) changePortPreset(w http.ResponseWriter, r *http.Request, srv *models.Server, p *store.ServerPort, preset string) bool {
	if preset == "" {
		if err := h.d.Store.UpdateServerPortPreset(p.ID, "", p.ContainerPort); err != nil {
			log.Printf("ports: server %d: %v", srv.ID, err)
			httpx.SendJSONError(w, "Failed to save the port", http.StatusInternalServerError)
			return false
		}
		h.writeBackVoice(srv)
		return true
	}
	if preset != "svc" && preset != "plasmo" {
		httpx.SendJSONErrorCode(w, "Unknown preset.", "port_invalid", http.StatusBadRequest)
		return false
	}
	if h.routingMode() != "ip_port" {
		httpx.SendJSONErrorCode(w, "With the gateway on, turn voice chat on under Routes instead.", "voice_preset_mode", http.StatusConflict)
		return false
	}
	if guard.RefuseIfSuspended(w, r, h.d.Store, h.d.SuspendGrace, srv) {
		return false
	}
	if p.Protocol != "udp" {
		httpx.SendJSONErrorCode(w, "Voice chat needs a UDP port. Add a UDP port with the voice chat preset instead.", "port_invalid", http.StatusBadRequest)
		return false
	}
	others, err := h.d.Store.ListServerPorts(srv.ID)
	if err != nil {
		log.Printf("ports: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to save the port", http.StatusInternalServerError)
		return false
	}
	for _, o := range others {
		if o.ID == p.ID {
			continue
		}
		if o.Preset != "" {
			httpx.SendJSONErrorCode(w, "This server already has a voice chat port.", "voice_preset_exists", http.StatusConflict)
			return false
		}
		if o.Protocol == "udp" && o.ContainerPort == voicePresetPort {
			httpx.SendJSONErrorCode(w, "Another port of this server already publishes UDP "+strconv.Itoa(voicePresetPort)+", the voice chat port.", "port_taken", http.StatusConflict)
			return false
		}
	}
	node, err := h.d.Store.GetNodeByID(srv.NodeID)
	if err != nil || node == nil {
		httpx.SendJSONError(w, "The server's node was not found", http.StatusNotFound)
		return false
	}
	address := h.extraPortsAddress(node)
	if address == "" {
		httpx.SendJSONErrorCode(w, noAddressMsg, "no_node_address", http.StatusConflict)
		return false
	}
	if srv.ActiveSubServer == "" {
		httpx.SendJSONErrorCode(w, "Install the server first.", "no_sub_server", http.StatusConflict)
		return false
	}
	mod, rel, content, err := FindVoiceConfig(h.d.GRPCRegistry, srv.NodeID, srv.UUID, srv.ActiveSubServer)
	switch {
	case errors.Is(err, ErrVoiceModMissing):
		httpx.SendJSONErrorCode(w, "Install Simple Voice Chat or Plasmo Voice and start the server once.", "voice_mod_missing", http.StatusConflict)
		return false
	case errors.Is(err, ErrVoiceModAmbiguous):
		httpx.SendJSONErrorCode(w, "Both Simple Voice Chat and Plasmo Voice are installed. Remove one, then try again.", "voice_mod_ambiguous", http.StatusConflict)
		return false
	case err != nil:
		log.Printf("ports: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Could not read the server's files on its node. Try again.", http.StatusBadGateway)
		return false
	}
	merged := voicecfg.SvcEnable(content, address, p.HostPort)
	if mod == "plasmo" {
		merged = voicecfg.PlasmoEnable(content, address, p.HostPort)
	}
	if merged != content {
		if err := nodeops.WriteNodeFileString(h.d.GRPCRegistry, srv.NodeID, srv.UUID, srv.ActiveSubServer+"/"+rel, merged); err != nil {
			log.Printf("ports: server %d: write %s: %v", srv.ID, rel, err)
			httpx.SendJSONError(w, "The node could not write the voice chat config. Try again.", http.StatusBadGateway)
			return false
		}
	}
	if err := h.d.Store.UpdateServerPortPreset(p.ID, mod, voicePresetPort); err != nil {
		log.Printf("ports: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to save the port", http.StatusInternalServerError)
		return false
	}
	h.publishPorts(r.Context(), srv)
	return true
}

// DeleteServerPort DELETE /api/servers/{id}/ports/{portId}. Never refused for
// mode or permission: taking a port away must always work.
func (h *ServerHandler) DeleteServerPort(w http.ResponseWriter, r *http.Request) {
	srv, p, ok := h.loadServerPort(w, r)
	if !ok {
		return
	}
	if err := h.d.Store.DeleteServerPort(p.ID); err != nil {
		log.Printf("ports: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to remove the port", http.StatusInternalServerError)
		return
	}
	if p.Preset != "" {
		h.writeBackVoice(srv)
	}
	h.publishPorts(r.Context(), srv)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "restartRequired": true})
}

// loadServerPort reads the server and the port, and answers 404 unless the
// port belongs to THAT server: the route's access check is on {id}.
func (h *ServerHandler) loadServerPort(w http.ResponseWriter, r *http.Request) (*models.Server, *store.ServerPort, bool) {
	vars := mux.Vars(r)
	srv, err := h.d.Store.GetServerByID(httpx.MustAtoi(vars["id"]))
	if err != nil || srv == nil {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return nil, nil, false
	}
	p, err := h.d.Store.GetServerPort(httpx.MustAtoi(vars["portId"]))
	if err != nil || p == nil || p.ServerID != srv.ID {
		httpx.SendJSONError(w, "Port not found", http.StatusNotFound)
		return nil, nil, false
	}
	return srv, p, true
}

func (h *ServerHandler) publishPorts(ctx context.Context, srv *models.Server) {
	ports, err := h.d.Store.ListServerPorts(srv.ID)
	if err != nil {
		log.Printf("ports: server %d: %v", srv.ID, err)
		return
	}
	if err := services.PublishServerPorts(ctx, h.d.Redis, srv.UUID, ports); err != nil {
		log.Printf("ports: server %d: %v", srv.ID, err)
	}
}

// writeBackVoice strips the voice address from the mod config again.
func (h *ServerHandler) writeBackVoice(srv *models.Server) {
	if srv.ActiveSubServer == "" {
		return
	}
	mod, rel, content, err := FindVoiceConfig(h.d.GRPCRegistry, srv.NodeID, srv.UUID, srv.ActiveSubServer)
	if err != nil {
		log.Printf("ports: server %d: voice config write-back skipped: %v", srv.ID, err)
		return
	}
	back := voicecfg.SvcDisable(content)
	if mod == "plasmo" {
		back = voicecfg.PlasmoDisable(content)
	}
	if back != content {
		if err := nodeops.WriteNodeFileString(h.d.GRPCRegistry, srv.NodeID, srv.UUID, srv.ActiveSubServer+"/"+rel, back); err != nil {
			log.Printf("ports: server %d: voice config write-back of %s failed: %v", srv.ID, rel, err)
		}
	}
}

// syncPortsToStart runs before a start, restart or sub-server switch: a port
// taken on another node (the server moved) is re-taken on this one, the voice
// preset's config is written for subName, and the node's key is rewritten.
// It never blocks the start: a port it cannot keep is dropped and logged.
func (h *ServerHandler) syncPortsToStart(srv *models.Server, subName string) {
	ports, err := h.d.Store.ListServerPorts(srv.ID)
	if err != nil || len(ports) == 0 {
		if err != nil {
			log.Printf("ports: server %d: %v", srv.ID, err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node, err := h.d.Store.GetNodeByID(srv.NodeID)
	if err != nil || node == nil {
		return
	}
	for _, p := range ports {
		if p.NodeID == srv.NodeID {
			continue
		}
		if err := h.d.Store.MoveServerPort(p.ID, srv.NodeID, p.HostPort); err == nil {
			continue
		}
		gameRange := ""
		if hb := services.LoadHeartbeat(ctx, h.d.Redis, node.Token); hb != nil {
			gameRange = hb.PortRange
		}
		_ = h.d.Store.DeleteServerPort(p.ID)
		p.NodeID = srv.NodeID
		if _, code, msg := h.takePort(p, 0, gameRange); code != "" {
			log.Printf("ports: server %d: port %q dropped after the move: %s", srv.ID, p.Name, msg)
		}
	}
	ports, err = h.d.Store.ListServerPorts(srv.ID)
	if err != nil {
		return
	}
	address := h.extraPortsAddress(node)
	for _, p := range ports {
		if p.Preset == "" || subName == "" || address == "" {
			continue
		}
		mod, rel, content, err := FindVoiceConfig(h.d.GRPCRegistry, srv.NodeID, srv.UUID, subName)
		if err != nil {
			log.Printf("ports: server %d: voice config: %v", srv.ID, err)
			continue
		}
		merged := voicecfg.SvcEnable(content, address, p.HostPort)
		if mod == "plasmo" {
			merged = voicecfg.PlasmoEnable(content, address, p.HostPort)
		}
		if merged != content {
			if err := nodeops.WriteNodeFileString(h.d.GRPCRegistry, srv.NodeID, srv.UUID, subName+"/"+rel, merged); err != nil {
				log.Printf("ports: server %d: voice config write: %v", srv.ID, err)
			}
		}
	}
	if err := services.PublishServerPorts(ctx, h.d.Redis, srv.UUID, ports); err != nil {
		log.Printf("ports: server %d: %v", srv.ID, err)
	}
}
