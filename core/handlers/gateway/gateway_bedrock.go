package gateway

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"dylaris-core/handlers"
	"dylaris-core/handlers/servers"
	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

type bedrockView struct {
	Enabled   bool   `json:"enabled"`
	Available bool   `json:"available"`
	Address   string `json:"address"`
	Loader    string `json:"loader"`
}

const (
	bedrockNotRouted = "This server is not routed through the gateway."
	bedrockNoEntry   = "Bedrock needs the gateway: no Bedrock entry is running in this server's region yet."
)

// bedrockAvailability reports whether Bedrock can be turned on and, when it
// cannot, why. address is the voice address rule's pick: the lexically first
// route domain exactly one label under an online edge's wildcard, because that
// is the only name that resolves and the only one whose region is known.
func (h *GatewayHandler) bedrockAvailability(ctx context.Context, srv *models.Server) (address, refusal string) {
	if !h.state.GatewayEnabled() || h.state.Redis == nil {
		return "", bedrockNotRouted
	}
	routes, err := services.ReadRoutesFromRedis(ctx, h.state.Redis)
	if err != nil {
		log.Printf("bedrock: server %d: read routes: %v", srv.ID, err)
		return "", "Could not read this server's routes. Try again."
	}
	host, region, ok := services.VoiceRouteFor(routes, services.GetEdgesFromRedis(ctx, h.state.Redis), srv.UUID)
	if !ok {
		return "", bedrockNotRouted
	}
	entries, err := services.LiveBedrockEntries(ctx, h.state.Redis)
	if err != nil {
		log.Printf("bedrock: server %d: %v", srv.ID, err)
		return host, "Could not check the Bedrock entries. Try again."
	}
	for _, e := range entries {
		if e.Region == region {
			return host, ""
		}
	}
	return host, bedrockNoEntry
}

// GetServerBedrock GET /api/servers/{id}/bedrock.
func (h *GatewayHandler) GetServerBedrock(w http.ResponseWriter, r *http.Request) {
	srv, err := h.state.Store.GetServerByID(handlers.MustAtoi(mux.Vars(r)["id"]))
	if err != nil || srv == nil {
		handlers.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	_, enabled, err := h.state.Store.GetBedrockKey(srv.ID)
	if err != nil {
		log.Printf("bedrock: server %d: %v", srv.ID, err)
		handlers.SendJSONError(w, "Failed to read the Bedrock setting", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	addr, refusal := h.bedrockAvailability(ctx, srv)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "bedrock": bedrockView{
		Enabled: enabled, Available: refusal == "", Address: addr, Loader: servers.DefaultTargetDirForLoader(srv.InstallerType),
	}})
}

// SetServerBedrock PUT /api/servers/{id}/bedrock - body {"enabled": bool}.
// Off is never refused, like voice: it is how an owner gets out of a state.
func (h *GatewayHandler) SetServerBedrock(w http.ResponseWriter, r *http.Request) {
	srv, err := h.state.Store.GetServerByID(handlers.MustAtoi(mux.Vars(r)["id"]))
	if err != nil || srv == nil {
		handlers.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	enabled, ok := decodeProxyProtocolBody(r)
	if !ok {
		handlers.SendJSONError(w, `Invalid request: expected {"enabled": true|false}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	svc := services.NewBedrockService(h.state.Store, h.state.Redis)
	if !enabled {
		if err := svc.Disable(ctx, srv.ID, srv.UUID); err != nil {
			log.Printf("bedrock: server %d: disable: %v", srv.ID, err)
			handlers.SendJSONError(w, "Failed to turn Bedrock off", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
		return
	}
	if handlers.RefuseIfSuspended(w, r, h.state, srv) {
		return
	}
	if _, refusal := h.bedrockAvailability(ctx, srv); refusal != "" {
		handlers.SendJSONErrorCode(w, refusal, "bedrock_unavailable", http.StatusConflict)
		return
	}
	if err := svc.Enable(ctx, srv.ID, srv.UUID); err != nil {
		log.Printf("bedrock: server %d: enable: %v", srv.ID, err)
		handlers.SendJSONError(w, "Failed to turn Bedrock on", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
