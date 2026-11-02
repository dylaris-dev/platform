package settings

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"dylaris-core/handlers/audit"
	"dylaris-core/handlers/httpx"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// TimezoneHandler serves the zone a person reads times in and the zone a
// server runs in. The server's zone reaches its container as TZ at the next
// start, and is the default for new scheduled tasks.
type TimezoneHandler struct{ d Deps }

func NewTimezoneHandler(d Deps) *TimezoneHandler { return &TimezoneHandler{d: d} }

var validTimeFormats = map[string]bool{"auto": true, "24h": true, "12h": true}

// GetMine GET /api/me/time-preferences - {timezone, timeFormat}; timezone ""
// means not chosen yet (the panel then offers the browser's).
func (h *TimezoneHandler) GetMine(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	tz, format, err := h.d.Store.GetUserTimePreferences(userID)
	if err != nil {
		httpx.SendJSONError(w, "Could not load your time settings", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"timezone": tz, "timeFormat": format})
}

// SetMine PUT /api/me/time-preferences - body {timezone, timeFormat}. An empty
// timezone clears the choice; servers that inherit it fall back to UTC.
func (h *TimezoneHandler) SetMine(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	var req struct {
		Timezone   string `json:"timezone"`
		TimeFormat string `json:"timeFormat"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Timezone != "" && !services.ValidTimezone(req.Timezone) {
		httpx.SendJSONError(w, "Unknown time zone", http.StatusBadRequest)
		return
	}
	if req.TimeFormat == "" {
		req.TimeFormat = "auto"
	}
	if !validTimeFormats[req.TimeFormat] {
		httpx.SendJSONError(w, "timeFormat must be auto, 24h or 12h", http.StatusBadRequest)
		return
	}
	if err := h.d.Store.SetUserTimePreferences(userID, req.Timezone, req.TimeFormat); err != nil {
		httpx.SendJSONError(w, "Could not save your time settings", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "timezone": req.Timezone, "timeFormat": req.TimeFormat})
}

// GetServer GET /api/servers/{id}/timezone - {timezone, effective}: timezone ""
// means the server inherits, effective is the zone it runs in.
func (h *TimezoneHandler) GetServer(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	own, eff, err := h.d.Store.GetServerTimezone(serverID)
	if err != nil {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"timezone": own, "effective": eff})
}

// SetServer PUT /api/servers/{id}/timezone - body {timezone}; "" inherits the
// owner's again. Takes effect at the next start; existing scheduled tasks keep
// the zone they were written in.
func (h *TimezoneHandler) SetServer(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	var req struct {
		Timezone string `json:"timezone"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Timezone != "" && !services.ValidTimezone(req.Timezone) {
		httpx.SendJSONError(w, "Unknown time zone", http.StatusBadRequest)
		return
	}
	if err := h.d.Store.SetServerTimezone(serverID, req.Timezone); err != nil {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return
	}
	_, eff, _ := h.d.Store.GetServerTimezone(serverID)
	actorID, _ := r.Context().Value("userID").(string)
	audit.LogServer(h.d.Store, r, serverID, audit.ServerAuditEventTimezoneChanged, actorID, "", map[string]interface{}{"to": req.Timezone})
	h.d.Events.Publish(r.Context(), "servers.changed", nil)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "timezone": req.Timezone, "effective": eff})
}
