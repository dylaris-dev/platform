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

type bedrockRouteView struct {
	Domain  string `json:"domain"`
	Java    bool   `json:"java"`
	Bedrock bool   `json:"bedrock"`
}

type bedrockView struct {
	// Enabled: at least one route of the server has Bedrock (stored).
	Enabled bool `json:"enabled"`
	// Active: the platform switch is not off, so those routes take Bedrock now.
	Active bool `json:"active"`
	// Allowed: THIS caller may give a route Bedrock (switch all, or admin and
	// the caller is a panel admin).
	Allowed   bool   `json:"allowed"`
	Available bool   `json:"available"`
	Refusal   string `json:"refusal,omitempty"`
	Address   string `json:"address"`
	Loader    string `json:"loader"`
	// Routes lists only the routes that differ from Java only.
	Routes []bedrockRouteView `json:"routes"`
}

const (
	bedrockNotRouted = "This server is not routed through the gateway."
	bedrockNoEntry   = "Bedrock needs the gateway: no Bedrock entry is running in this server's region yet."
	bedrockOff       = "Bedrock is turned off on this platform."
)

// bedrockAllowed is whether this caller may give a route Bedrock.
func (h *GatewayHandler) bedrockAllowed(r *http.Request) bool {
	if h.state.FeatureFlags == nil {
		return false
	}
	switch h.state.FeatureFlags.BedrockAudience(r.Context()) {
	case "all":
		return true
	case "admin":
		return handlers.IsAdmin(r)
	}
	return false
}

func (h *GatewayHandler) bedrockService() *services.BedrockService {
	svc := services.NewBedrockService(h.state.Store, h.state.Redis)
	svc.SetFlags(h.state.FeatureFlags)
	return svc
}

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
	rows, err := h.state.Store.ListRouteEditions(srv.ID)
	if err != nil {
		log.Printf("bedrock: server %d: %v", srv.ID, err)
		handlers.SendJSONError(w, "Failed to read the Bedrock setting", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	addr, refusal := h.bedrockAvailability(ctx, srv)
	v := bedrockView{
		Active: services.BedrockActive(r.Context(), h.state.FeatureFlags), Allowed: h.bedrockAllowed(r),
		Available: refusal == "", Refusal: refusal, Address: addr,
		Loader: servers.DefaultTargetDirForLoader(srv.InstallerType), Routes: []bedrockRouteView{},
	}
	for _, e := range rows {
		v.Enabled = v.Enabled || e.Bedrock
		v.Routes = append(v.Routes, bedrockRouteView{Domain: e.Domain, Java: e.Java, Bedrock: e.Bedrock})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "bedrock": v})
}

// SetRouteEdition PUT /api/servers/{id}/bedrock/routes - body
// {"domain": "...", "java": bool, "bedrock": bool}. Giving a route Bedrock is
// refused while the caller may not, or the region has no Bedrock entry;
// taking it away or changing Java never is, so an owner can always get out of
// a state. The table is written first and the gateway list after it.
func (h *GatewayHandler) SetRouteEdition(w http.ResponseWriter, r *http.Request) {
	srv, err := h.state.Store.GetServerByID(handlers.MustAtoi(mux.Vars(r)["id"]))
	if err != nil || srv == nil {
		handlers.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	var req struct {
		Domain  string `json:"domain"`
		Java    *bool  `json:"java"`
		Bedrock *bool  `json:"bedrock"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Java == nil || req.Bedrock == nil || req.Domain == "" {
		handlers.SendJSONError(w, `Invalid request: expected {"domain": "...", "java": true|false, "bedrock": true|false}`, http.StatusBadRequest)
		return
	}
	if !*req.Java && !*req.Bedrock {
		handlers.SendJSONErrorCode(w, "A route needs Java, Bedrock or both.", "edition_invalid", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	routes, err := services.ReadRoutesFromRedis(ctx, h.state.Redis)
	if err != nil {
		log.Printf("bedrock: server %d: read routes: %v", srv.ID, err)
		handlers.SendJSONError(w, "Could not read this server's routes. Try again.", http.StatusBadGateway)
		return
	}
	found := false
	for _, rt := range routes {
		if rt.Domain == req.Domain && services.RouteBelongsToServer(rt, srv.UUID) {
			found = true
			break
		}
	}
	if !found {
		handlers.SendJSONError(w, "Route not found for this server", http.StatusNotFound)
		return
	}
	rows, err := h.state.Store.ListRouteEditions(srv.ID)
	if err != nil {
		log.Printf("bedrock: server %d: %v", srv.ID, err)
		handlers.SendJSONError(w, "Failed to read the Bedrock setting", http.StatusInternalServerError)
		return
	}
	hadBedrock := false
	for _, e := range rows {
		if e.Domain == req.Domain {
			hadBedrock = e.Bedrock
		}
	}
	if *req.Bedrock && !hadBedrock {
		if !h.bedrockAllowed(r) {
			handlers.SendJSONErrorCode(w, bedrockOff, "bedrock_disabled", http.StatusConflict)
			return
		}
		if handlers.RefuseIfSuspended(w, r, h.state, srv) {
			return
		}
		if _, refusal := h.bedrockAvailability(ctx, srv); refusal != "" {
			handlers.SendJSONErrorCode(w, refusal, "bedrock_unavailable", http.StatusConflict)
			return
		}
	}
	if err := h.state.Store.SetRouteEdition(srv.ID, req.Domain, *req.Java, *req.Bedrock); err != nil {
		log.Printf("bedrock: server %d: route %s: %v", srv.ID, req.Domain, err)
		handlers.SendJSONError(w, "Failed to save the route", http.StatusInternalServerError)
		return
	}
	if err := h.bedrockService().PublishEditions(ctx, services.BedrockActive(r.Context(), h.state.FeatureFlags)); err != nil {
		log.Printf("bedrock: server %d: publish editions: %v", srv.ID, err)
		handlers.SendJSONError(w, "The route was saved, but the gateway could not be told yet. It applies within a minute.", http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true,
		"route": bedrockRouteView{Domain: req.Domain, Java: *req.Java, Bedrock: *req.Bedrock}})
}
