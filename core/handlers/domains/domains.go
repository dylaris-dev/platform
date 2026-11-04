// Package domains serves direct-mode server domains: a server's own domains
// (domains.read / domains.write) and the administrator's DNS accounts, zones,
// access and limits (settings.*). See
// docs/superpowers/specs/2026-11-03-direct-mode-domains-design.md.
package domains

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"dylaris-core/authz"
	"dylaris-core/handlers/audit"
	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/dnsprov"
	"dylaris-core/store"
)

// Deps is what the domain handlers read; routes.go fills it once.
type Deps struct {
	Store         store.Store
	FeatureFlags  *services.FeatureFlags
	ClusterSecret string
}

type DomainsHandler struct {
	d Deps
	// newProvider builds a provider client; tests swap it.
	newProvider func(kind string, creds map[string]string) (dnsprov.Provider, error)
}

func NewDomainsHandler(d Deps) *DomainsHandler {
	return &DomainsHandler{d: d, newProvider: dnsprov.New}
}

const disabledMsg = "DNS updater is not enabled"

// AdminGate answers 404 while the feature flag is off.
func (h *DomainsHandler) AdminGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.d.FeatureFlags.IsDNSUpdaterEnabled(r.Context()) {
			httpx.SendJSONError(w, disabledMsg, http.StatusNotFound)
			return
		}
		next(w, r)
	}
}

// ServerGate is AdminGate plus direct routing: only routing_mode "gateway"
// hides the feature, ip_port and both are direct.
func (h *DomainsHandler) ServerGate(next http.HandlerFunc) http.HandlerFunc {
	return h.AdminGate(func(w http.ResponseWriter, r *http.Request) {
		mode, err := h.d.Store.GetSetting("routing_mode")
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			// Failing open here would let a gateway platform write DNS records.
			log.Printf("domains: read routing_mode: %v", err)
			httpx.SendJSONError(w, "Routing mode is unavailable, try again", http.StatusServiceUnavailable)
			return
		}
		if mode == "gateway" {
			httpx.SendJSONError(w, "Server domains are not available while the platform routes through the gateway", http.StatusNotFound)
			return
		}
		next(w, r)
	})
}

type domainView struct {
	ID         int        `json:"id"`
	FQDN       string     `json:"fqdn"`
	Mode       string     `json:"mode"`
	ZoneID     *int       `json:"zoneId"`
	Label      string     `json:"label"`
	State      string     `json:"state"`
	Problems   []string   `json:"problems"`
	ObservedAt *time.Time `json:"observedAt"`
	CreatedBy  string     `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
}

func viewOf(d store.ServerDomain) domainView {
	if d.Problems == nil {
		d.Problems = []string{}
	}
	return domainView{ID: d.ID, FQDN: d.FQDN, Mode: d.Mode, ZoneID: d.ZoneID, Label: d.Label, State: d.State,
		Problems: d.Problems, ObservedAt: d.ObservedAt, CreatedBy: d.CreatedBy, CreatedAt: d.CreatedAt}
}

type zoneOption struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// limitsView: Max nil is no limit. Used/Max count the owner's domains on all
// their servers, ServerUsed/ServerMax this server's.
type limitsView struct {
	Used       int    `json:"used"`
	Max        *int64 `json:"max"`
	ServerUsed int    `json:"serverUsed"`
	ServerMax  *int64 `json:"serverMax"`
}

func (h *DomainsHandler) server(w http.ResponseWriter, r *http.Request) *models.Server {
	srv, err := authz.ServerByID(r.Context(), h.d.Store, httpx.MustAtoi(mux.Vars(r)["id"]))
	if err != nil || srv == nil {
		httpx.SendJSONError(w, "Server not found", http.StatusNotFound)
		return nil
	}
	return srv
}

func callerID(r *http.Request) string {
	id, _ := r.Context().Value("userID").(string)
	return id
}

// usableZones is every zone the caller may create subdomains in. A zone the
// provider no longer lists is not: its domains keep working, but a new one
// would be written into a zone the credentials may not reach any more.
func (h *DomainsHandler) usableZones(r *http.Request) ([]store.DNSZone, []store.DNSZoneAccess, error) {
	zones, err := h.d.Store.ListDNSZones()
	if err != nil {
		return nil, nil, err
	}
	access, err := h.d.Store.ListDNSZoneAccess()
	if err != nil {
		return nil, nil, err
	}
	principals, err := services.ZonePrincipals(h.d.Store, callerID(r))
	if err != nil {
		return nil, nil, err
	}
	out := []store.DNSZone{}
	for _, z := range zones {
		if z.Listed && services.CanUseZone(principals, access, z.ID, reqctx.IsAdmin(r)) {
			out = append(out, z)
		}
	}
	return out, access, nil
}

// List GET /api/servers/{id}/domains
func (h *DomainsHandler) List(w http.ResponseWriter, r *http.Request) {
	srv := h.server(w, r)
	if srv == nil {
		return
	}
	domains, err := h.d.Store.ListServerDomainsByServer(srv.ID)
	if err != nil {
		log.Printf("domains: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to read the server's domains", http.StatusInternalServerError)
		return
	}
	zones, _, err := h.usableZones(r)
	if err != nil {
		log.Printf("domains: server %d: zones: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to read the DNS zones", http.StatusInternalServerError)
		return
	}
	used, err := h.d.Store.CountServerDomainsByOwner(srv.OwnerID)
	if err != nil {
		log.Printf("domains: server %d: count: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to read the server's domains", http.StatusInternalServerError)
		return
	}
	views := make([]domainView, 0, len(domains))
	for _, d := range domains {
		views = append(views, viewOf(d))
	}
	opts := make([]zoneOption, 0, len(zones))
	for _, z := range zones {
		opts = append(opts, zoneOption{ID: z.ID, Name: z.Name})
	}
	direct := ""
	if node, err := h.d.Store.GetNodeByID(srv.NodeID); err == nil {
		direct = services.DirectAddress(node, srv.HostPort)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "domains": views, "zones": opts, "directAddress": direct,
		// The panel judges how old a state may be against the reconcile interval.
		"intervalSeconds": int(services.DNSReconcileInterval(h.d.Store) / time.Second),
		"limits": limitsView{Used: used, Max: services.SubdomainLimitFor(h.d.Store, srv.OwnerID),
			ServerUsed: len(domains), ServerMax: services.ServerSubdomainLimit(h.d.Store)},
	})
}

type createRequest struct {
	Mode   string `json:"mode"`
	FQDN   string `json:"fqdn"`
	ZoneID int    `json:"zoneId"`
	Label  string `json:"label"`
}

// Create POST /api/servers/{id}/domains - {mode:"manual", fqdn} or
// {mode:"managed", zoneId, label}. The domain belongs to the server and counts
// against its OWNER, whoever adds it; a managed one also needs the caller's
// own access to the zone.
func (h *DomainsHandler) Create(w http.ResponseWriter, r *http.Request) {
	srv := h.server(w, r)
	if srv == nil {
		return
	}
	var req createRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	d := store.ServerDomain{ServerID: srv.ID, Mode: req.Mode, State: services.DomainGrey, CreatedBy: callerID(r)}
	switch req.Mode {
	case store.DomainModeManual:
		fqdn, err := services.ValidateFreeFQDN(req.FQDN)
		if err != nil {
			httpx.SendJSONErrorCode(w, capitalize(err.Error())+".", "domain_invalid", http.StatusBadRequest)
			return
		}
		d.FQDN = fqdn
	case store.DomainModeManaged:
		label := strings.ToLower(strings.TrimSpace(req.Label))
		if err := services.ValidateSubdomainLabel(label); err != nil {
			httpx.SendJSONErrorCode(w, capitalize(err.Error())+".", "domain_invalid", http.StatusBadRequest)
			return
		}
		zones, err := h.d.Store.ListDNSZones()
		if err != nil {
			log.Printf("domains: server %d: zones: %v", srv.ID, err)
			httpx.SendJSONError(w, "Failed to read the DNS zones", http.StatusInternalServerError)
			return
		}
		var zone *store.DNSZone
		for i := range zones {
			if zones[i].ID == req.ZoneID {
				zone = &zones[i]
			}
		}
		if zone == nil {
			httpx.SendJSONErrorCode(w, "That zone does not exist (any more). Pick another one.", "domain_invalid", http.StatusBadRequest)
			return
		}
		if !zone.Listed {
			httpx.SendJSONErrorCode(w, "The DNS provider no longer lists that zone. Pick another one, or ask an administrator.", "domain_invalid", http.StatusBadRequest)
			return
		}
		usable, _, err := h.usableZones(r)
		if err != nil {
			log.Printf("domains: server %d: zone access: %v", srv.ID, err)
			httpx.SendJSONError(w, "Failed to read the DNS zones", http.StatusInternalServerError)
			return
		}
		allowed := false
		for _, z := range usable {
			allowed = allowed || z.ID == zone.ID
		}
		if !allowed {
			httpx.SendJSONErrorCode(w, "You may not use this zone. Ask an administrator for access.", "zone_forbidden", http.StatusForbidden)
			return
		}
		d.FQDN, d.Label, d.ZoneID = label+"."+zone.Name, label, &zone.ID
		if len(d.FQDN) > 253 {
			httpx.SendJSONErrorCode(w, "That name is longer than 253 characters.", "domain_invalid", http.StatusBadRequest)
			return
		}
	default:
		httpx.SendJSONErrorCode(w, `Mode must be "manual" or "managed".`, "domain_invalid", http.StatusBadRequest)
		return
	}

	used, err := h.d.Store.CountServerDomainsByOwner(srv.OwnerID)
	if err != nil {
		log.Printf("domains: server %d: count: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to add the domain", http.StatusInternalServerError)
		return
	}
	// ponytail: count-then-insert races, two parallel adds can pass one over; a soft, admin-set quota, add a locked count if it ever matters.
	if limit := services.SubdomainLimitFor(h.d.Store, srv.OwnerID); services.AtOrOver(limit, int64(used)) {
		httpx.SendJSONErrorCode(w, "Subdomain limit reached ("+strconv.FormatInt(*limit, 10)+"). The owner of this server holds as many domains as they may; remove one, or ask an administrator for more.", "domain_limit_reached", http.StatusConflict)
		return
	}
	onServer, err := h.d.Store.ListServerDomainsByServer(srv.ID)
	if err != nil {
		log.Printf("domains: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to add the domain", http.StatusInternalServerError)
		return
	}
	if limit := services.ServerSubdomainLimit(h.d.Store); services.AtOrOver(limit, int64(len(onServer))) {
		httpx.SendJSONErrorCode(w, "Subdomain limit reached ("+strconv.FormatInt(*limit, 10)+"). This server holds as many domains as one server may; remove one first.", "domain_limit_reached", http.StatusConflict)
		return
	}

	id, err := h.d.Store.CreateServerDomain(d)
	if errors.Is(err, store.ErrNameTaken) {
		msg := d.FQDN + " is already on this server."
		if d.Mode == store.DomainModeManaged {
			msg = d.FQDN + " is already in use. Pick another name."
		}
		httpx.SendJSONErrorCode(w, msg, "domain_taken", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("domains: server %d: create: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to add the domain", http.StatusInternalServerError)
		return
	}
	d.ID = id
	audit.LogServer(h.d.Store, r, srv.ID, audit.ServerAuditEventDomainAdded, callerID(r), "",
		map[string]interface{}{"fqdn": d.FQDN, "mode": d.Mode})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "domain": viewOf(d)})
}

// Delete DELETE /api/servers/{id}/domains/{domainId}. Core's records for it
// are removed by the reconciler's next pass (the ledger row loses its domain).
func (h *DomainsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	srv := h.server(w, r)
	if srv == nil {
		return
	}
	domainID := httpx.MustAtoi(mux.Vars(r)["domainId"])
	domains, err := h.d.Store.ListServerDomainsByServer(srv.ID)
	if err != nil {
		log.Printf("domains: server %d: %v", srv.ID, err)
		httpx.SendJSONError(w, "Failed to remove the domain", http.StatusInternalServerError)
		return
	}
	var found *store.ServerDomain
	for i := range domains {
		if domains[i].ID == domainID {
			found = &domains[i]
		}
	}
	if found == nil {
		httpx.SendJSONError(w, "Domain not found", http.StatusNotFound)
		return
	}
	if err := h.d.Store.DeleteServerDomain(found.ID); err != nil {
		log.Printf("domains: server %d: delete %d: %v", srv.ID, found.ID, err)
		httpx.SendJSONError(w, "Failed to remove the domain", http.StatusInternalServerError)
		return
	}
	audit.LogServer(h.d.Store, r, srv.ID, audit.ServerAuditEventDomainRemoved, callerID(r), "",
		map[string]interface{}{"fqdn": found.FQDN, "mode": found.Mode})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
