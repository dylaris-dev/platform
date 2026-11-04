package domains

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/mux"

	"dylaris-core/handlers/audit"
	"dylaris-core/handlers/httpx"
	"dylaris-core/services"
	"dylaris-core/services/dnsprov"
	"dylaris-core/store"
)

const (
	dnsFlagKey      = "feature_dns_updater_enabled"
	accountNameMax  = 100
	adminBodyMax    = 16 << 10
	providerTimeout = 30 * time.Second
)

type fieldView struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Secret   bool   `json:"secret"`
	Optional bool   `json:"optional"`
}

type kindView struct {
	ID          string      `json:"id"`
	Label       string      `json:"label"`
	Fields      []fieldView `json:"fields"`
	ZoneListing bool        `json:"zoneListing"`
}

func kindsView() []kindView {
	out := []kindView{}
	for _, k := range dnsprov.Kinds() {
		v := kindView{ID: k.ID, Label: k.Label, ZoneListing: k.ZoneListing, Fields: []fieldView{}}
		for _, f := range k.Fields {
			v.Fields = append(v.Fields, fieldView{Key: f.Key, Label: f.Label, Secret: f.Secret, Optional: f.Optional})
		}
		out = append(out, v)
	}
	return out
}

type zoneDomainView struct {
	// ID is the server_domains row, so the admin view can remove it.
	ID            int    `json:"id"`
	FQDN          string `json:"fqdn"`
	ServerID      int    `json:"serverId"`
	ServerName    string `json:"serverName"`
	CreatedBy     string `json:"createdBy"`
	CreatedByName string `json:"createdByName"`
	// AccessLost: the creator may no longer use the zone. The domain keeps
	// working; losing access never deletes anything.
	AccessLost bool `json:"accessLost"`
}

type zoneView struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Manual bool   `json:"manual"`
	// Listed false: the provider's listing no longer names the zone, but it is
	// kept because domains or owned records still use it.
	Listed  bool             `json:"listed"`
	Access  []string         `json:"access"`
	Domains []zoneDomainView `json:"domains"`
}

// accountView carries hasCredentials, never the credentials.
type accountView struct {
	ID             int        `json:"id"`
	Name           string     `json:"name"`
	Provider       string     `json:"provider"`
	HasCredentials bool       `json:"hasCredentials"`
	LastError      string     `json:"lastError"`
	LastErrorAt    *time.Time `json:"lastErrorAt"`
	Zones          []zoneView `json:"zones"`
}

type adminLimits struct {
	PerUser   string `json:"perUser"`
	PerServer string `json:"perServer"`
}

func (h *DomainsHandler) limitsSetting(key string) string {
	raw, _ := h.d.Store.GetSetting(key)
	return services.FormatLimitSetting(services.ParseLimitSetting(strings.TrimSpace(raw), nil))
}

// AdminGet GET /api/admin/dns. Answers while the flag is off too (enabled
// false), so the screen that turns it on can render.
func (h *DomainsHandler) AdminGet(w http.ResponseWriter, r *http.Request) {
	accounts, err := h.d.Store.ListDNSAccounts()
	if err != nil {
		log.Printf("dns admin: accounts: %v", err)
		httpx.SendJSONError(w, "Failed to read the DNS accounts", http.StatusInternalServerError)
		return
	}
	zones, err := h.d.Store.ListDNSZones()
	if err != nil {
		log.Printf("dns admin: zones: %v", err)
		httpx.SendJSONError(w, "Failed to read the DNS zones", http.StatusInternalServerError)
		return
	}
	access, err := h.d.Store.ListDNSZoneAccess()
	if err != nil {
		log.Printf("dns admin: access: %v", err)
		httpx.SendJSONError(w, "Failed to read the zone access", http.StatusInternalServerError)
		return
	}
	domains, err := h.d.Store.ListServerDomains()
	if err != nil {
		log.Printf("dns admin: domains: %v", err)
		httpx.SendJSONError(w, "Failed to read the domains", http.StatusInternalServerError)
		return
	}
	servers, err := h.d.Store.ListAllServers()
	if err != nil {
		log.Printf("dns admin: servers: %v", err)
		httpx.SendJSONError(w, "Failed to read the servers", http.StatusInternalServerError)
		return
	}
	serverName := make(map[int]string, len(servers))
	for _, s := range servers {
		serverName[s.ID] = s.Name
	}

	type creator struct {
		name       string
		admin      bool
		principals []string
	}
	creators := map[string]creator{}
	creatorOf := func(id string) creator {
		if c, ok := creators[id]; ok {
			return c
		}
		c := creator{principals: []string{"user:" + id}}
		if u, err := h.d.Store.GetUserByID(id); err == nil && u != nil {
			c.name, c.admin = u.Username, u.IsAdmin
		}
		if p, err := services.ZonePrincipals(h.d.Store, id); err == nil {
			c.principals = p
		} else {
			log.Printf("dns admin: %v", err)
		}
		creators[id] = c
		return c
	}

	byZone := map[int][]zoneDomainView{}
	for _, d := range domains {
		if d.Mode != store.DomainModeManaged || d.ZoneID == nil {
			continue
		}
		c := creatorOf(d.CreatedBy)
		byZone[*d.ZoneID] = append(byZone[*d.ZoneID], zoneDomainView{
			ID: d.ID, FQDN: d.FQDN, ServerID: d.ServerID, ServerName: serverName[d.ServerID],
			CreatedBy: d.CreatedBy, CreatedByName: c.name,
			AccessLost: !services.CanUseZone(c.principals, access, *d.ZoneID, c.admin),
		})
	}
	zonesOf := map[int][]zoneView{}
	for _, z := range zones {
		zv := zoneView{ID: z.ID, Name: z.Name, Manual: z.Manual, Listed: z.Listed, Access: []string{}, Domains: byZone[z.ID]}
		if zv.Domains == nil {
			zv.Domains = []zoneDomainView{}
		}
		for _, a := range access {
			if a.ZoneID == z.ID {
				zv.Access = append(zv.Access, a.Principal)
			}
		}
		zonesOf[z.AccountID] = append(zonesOf[z.AccountID], zv)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	views := make([]accountView, 0, len(accounts))
	for _, a := range accounts {
		v := accountOf(a)
		if z := zonesOf[a.ID]; z != nil {
			v.Zones = z
		}
		views = append(views, v)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"enabled":         h.d.FeatureFlags.IsDNSUpdaterEnabled(r.Context()),
		"intervalSeconds": int(services.DNSReconcileInterval(h.d.Store) / time.Second),
		"kinds":           kindsView(),
		"accounts":        views,
		"limits": adminLimits{PerUser: h.limitsSetting(services.DNSMaxPerUserSettingKey),
			PerServer: h.limitsSetting(services.DNSMaxPerServerSettingKey)},
	})
}

func accountOf(a store.DNSAccount) accountView {
	return accountView{ID: a.ID, Name: a.Name, Provider: a.Provider, HasCredentials: a.EncCredentials != "",
		LastError: a.LastError, LastErrorAt: a.LastErrorAt, Zones: []zoneView{}}
}

// SetSettings PUT /api/admin/dns/settings - {enabled?, intervalSeconds?,
// perUser?, perServer?}. Not behind AdminGate: it is where the flag is turned
// on. Limits are "" (no limit), "unlimited" or a number of 0 or more.
func (h *DomainsHandler) SetSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled         *bool   `json:"enabled"`
		IntervalSeconds *int    `json:"intervalSeconds"`
		PerUser         *string `json:"perUser"`
		PerServer       *string `json:"perServer"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminBodyMax)).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	writes := [][2]string{}
	if req.Enabled != nil {
		writes = append(writes, [2]string{dnsFlagKey, strconv.FormatBool(*req.Enabled)})
	}
	if req.IntervalSeconds != nil {
		// Clamped like the reconciler reads it, so what is stored is what runs.
		n := min(max(*req.IntervalSeconds, 30), 86400)
		writes = append(writes, [2]string{services.DNSIntervalSettingKey, strconv.Itoa(n)})
	}
	for _, l := range []struct {
		v   *string
		key string
	}{{req.PerUser, services.DNSMaxPerUserSettingKey}, {req.PerServer, services.DNSMaxPerServerSettingKey}} {
		if l.v == nil {
			continue
		}
		v := strings.TrimSpace(*l.v)
		if v != "" && v != services.LimitUnlimited {
			if n, err := strconv.Atoi(v); err != nil || n < 0 {
				httpx.SendJSONError(w, `A limit must be "", "unlimited" or a number of 0 or more`, http.StatusBadRequest)
				return
			}
		}
		writes = append(writes, [2]string{l.key, v})
	}
	changed := map[string]interface{}{}
	for _, kv := range writes {
		if err := h.d.Store.SetSetting(kv[0], kv[1]); err != nil {
			log.Printf("dns admin: save %s: %v", kv[0], err)
			httpx.SendJSONError(w, "Failed to save the settings", http.StatusInternalServerError)
			return
		}
		changed[kv[0]] = kv[1] // none of these is secret
	}
	if len(changed) > 0 {
		audit.LogIdentity(h.d.Store, r, audit.AuditEventDNSSettingsUpdated, callerID(r), "", changed)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

type accountRequest struct {
	Name        string            `json:"name"`
	Provider    string            `json:"provider"`
	Credentials map[string]string `json:"credentials"`
}

// mergeCredentials checks in against the provider's fields: a blank field
// takes the stored value, a field the provider does not have is refused, and
// every required field must end up set. The error never carries a value.
func mergeCredentials(provider string, in, stored map[string]string) (map[string]string, error) {
	var kind *dnsprov.Kind
	for _, k := range dnsprov.Kinds() {
		if k.ID == provider {
			kind = &k
		}
	}
	if kind == nil {
		return nil, errors.New("unknown DNS provider")
	}
	known := map[string]bool{}
	for _, f := range kind.Fields {
		known[f.Key] = true
	}
	for k := range in {
		if !known[k] {
			return nil, fmt.Errorf("%s has no credential field of that name", kind.Label)
		}
	}
	out := map[string]string{}
	var missing []string
	for _, f := range kind.Fields {
		v := strings.TrimSpace(in[f.Key])
		if v == "" {
			v = stored[f.Key]
		}
		if v != "" {
			out[f.Key] = v
		} else if !f.Optional {
			missing = append(missing, f.Label)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s needs: %s", kind.Label, strings.Join(missing, ", "))
	}
	return out, nil
}

func (h *DomainsHandler) decodeAccount(w http.ResponseWriter, r *http.Request) (accountRequest, bool) {
	var req accountRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminBodyMax)).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return req, false
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > accountNameMax {
		httpx.SendJSONError(w, "Give the account a name of at most 100 characters", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func (h *DomainsHandler) account(w http.ResponseWriter, r *http.Request) *store.DNSAccount {
	acc, err := h.d.Store.GetDNSAccount(httpx.MustAtoi(mux.Vars(r)["id"]))
	if err != nil {
		log.Printf("dns admin: account: %v", err)
		httpx.SendJSONError(w, "Failed to read the DNS account", http.StatusInternalServerError)
		return nil
	}
	if acc == nil {
		httpx.SendJSONError(w, "DNS account not found", http.StatusNotFound)
	}
	return acc
}

func (h *DomainsHandler) auditAccount(r *http.Request, event string, a store.DNSAccount) {
	audit.LogIdentity(h.d.Store, r, event, callerID(r), "", map[string]interface{}{"name": a.Name, "provider": a.Provider})
}

// CreateAccount POST /api/admin/dns/accounts - {name, provider, credentials}.
func (h *DomainsHandler) CreateAccount(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decodeAccount(w, r)
	if !ok {
		return
	}
	creds, err := mergeCredentials(req.Provider, req.Credentials, nil)
	if err != nil {
		httpx.SendJSONError(w, capitalize(err.Error()), http.StatusBadRequest)
		return
	}
	enc, err := services.EncryptDNSCredentials(h.d.ClusterSecret, creds)
	if err != nil {
		log.Printf("dns admin: encrypt: %v", err)
		httpx.SendJSONError(w, "Failed to store the credentials", http.StatusInternalServerError)
		return
	}
	a := store.DNSAccount{Name: req.Name, Provider: req.Provider, EncCredentials: enc}
	if a.ID, err = h.d.Store.CreateDNSAccount(a); err != nil {
		log.Printf("dns admin: create account: %v", err)
		httpx.SendJSONError(w, "Failed to add the DNS account", http.StatusInternalServerError)
		return
	}
	h.auditAccount(r, audit.AuditEventDNSAccountCreated, a)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "account": accountOf(a)})
}

// UpdateAccount PUT /api/admin/dns/accounts/{id}. A blank credential field
// keeps the stored one. The provider is fixed: the account's zones and its
// ledger of owned records are keyed by (account, zone, name, type), so under a
// new provider Core would treat same-named records there as its own and
// overwrite or delete records it never created.
func (h *DomainsHandler) UpdateAccount(w http.ResponseWriter, r *http.Request) {
	acc := h.account(w, r)
	if acc == nil {
		return
	}
	req, ok := h.decodeAccount(w, r)
	if !ok {
		return
	}
	if req.Provider != acc.Provider {
		httpx.SendJSONErrorCode(w, "The provider of an account cannot be changed - delete the account and add a new one", "provider_change", http.StatusConflict)
		return
	}
	var stored map[string]string
	if acc.EncCredentials != "" {
		// Undecryptable (a rotated cluster secret) leaves nothing to keep: the
		// admin must enter every field again, which mergeCredentials says.
		stored, _ = services.DecryptDNSCredentials(h.d.ClusterSecret, acc.EncCredentials)
	}
	creds, err := mergeCredentials(req.Provider, req.Credentials, stored)
	if err != nil {
		httpx.SendJSONError(w, capitalize(err.Error()), http.StatusBadRequest)
		return
	}
	enc, err := services.EncryptDNSCredentials(h.d.ClusterSecret, creds)
	if err != nil {
		log.Printf("dns admin: encrypt: %v", err)
		httpx.SendJSONError(w, "Failed to store the credentials", http.StatusInternalServerError)
		return
	}
	acc.Name, acc.Provider, acc.EncCredentials = req.Name, req.Provider, enc
	acc.LastError, acc.LastErrorAt = "", nil
	if err := h.d.Store.UpdateDNSAccount(*acc); err != nil {
		log.Printf("dns admin: update account %d: %v", acc.ID, err)
		httpx.SendJSONError(w, "Failed to save the DNS account", http.StatusInternalServerError)
		return
	}
	h.auditAccount(r, audit.AuditEventDNSAccountUpdated, *acc)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "account": accountOf(*acc)})
}

// DeleteAccount DELETE /api/admin/dns/accounts/{id}. Core's records go first,
// through the provider; what it cannot reach is named in orphanedNames (and in
// the audit event, the only place it outlives the response) for the admin to
// remove by hand. Then the managed domains in the account's zones go, each
// with its own server audit event: left behind they would point at no zone,
// do nothing, and still count against their owner's limit. The account goes
// either way.
func (h *DomainsHandler) DeleteAccount(w http.ResponseWriter, r *http.Request) {
	acc := h.account(w, r)
	if acc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*providerTimeout)
	defer cancel()
	orphaned, err := services.DeleteDNSAccountRecords(ctx, h.d.Store, h.d.ClusterSecret, *acc, h.newProvider)
	if err != nil {
		log.Printf("dns admin: delete account %d: %v", acc.ID, err)
		httpx.SendJSONError(w, "Failed to read the account's records; nothing was deleted", http.StatusInternalServerError)
		return
	}
	if err := h.deleteAccountDomains(r, acc.ID); err != nil {
		log.Printf("dns admin: delete account %d: domains: %v", acc.ID, err)
		httpx.SendJSONError(w, "Failed to remove the account's domains; the account is kept", http.StatusInternalServerError)
		return
	}
	if err := h.d.Store.DeleteDNSAccount(acc.ID); err != nil {
		log.Printf("dns admin: delete account %d: %v", acc.ID, err)
		httpx.SendJSONError(w, "Failed to delete the DNS account", http.StatusInternalServerError)
		return
	}
	audit.LogIdentity(h.d.Store, r, audit.AuditEventDNSAccountDeleted, callerID(r), "", map[string]interface{}{
		"name": acc.Name, "provider": acc.Provider, "orphanedNames": orphaned})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "orphanedNames": orphaned})
}

// deleteAccountDomains removes the managed domains in the account's zones.
func (h *DomainsHandler) deleteAccountDomains(r *http.Request, accountID int) error {
	zones, err := h.accountZones(accountID)
	if err != nil {
		return fmt.Errorf("zones: %w", err)
	}
	mine := map[int]bool{}
	for _, z := range zones {
		mine[z.ID] = true
	}
	domains, err := h.d.Store.ListServerDomains()
	if err != nil {
		return fmt.Errorf("domains: %w", err)
	}
	for _, d := range domains {
		if d.Mode != store.DomainModeManaged || d.ZoneID == nil || !mine[*d.ZoneID] {
			continue
		}
		if err := h.d.Store.DeleteServerDomain(d.ID); err != nil {
			return fmt.Errorf("delete domain %d: %w", d.ID, err)
		}
		audit.LogServer(h.d.Store, r, d.ServerID, audit.ServerAuditEventDomainRemoved, callerID(r), "",
			map[string]interface{}{"fqdn": d.FQDN, "mode": d.Mode, "reason": "dns_account_deleted"})
	}
	return nil
}

// provider opens the account's provider, or answers the request itself.
func (h *DomainsHandler) provider(w http.ResponseWriter, acc *store.DNSAccount) (dnsprov.Provider, map[string]string, bool) {
	creds, err := services.DecryptDNSCredentials(h.d.ClusterSecret, acc.EncCredentials)
	if err != nil {
		httpx.SendJSONError(w, "The stored credentials cannot be decrypted. Enter them again.", http.StatusConflict)
		return nil, nil, false
	}
	prov, err := h.newProvider(acc.Provider, creds)
	if err != nil {
		httpx.SendJSONError(w, providerMessage(err, creds), http.StatusBadRequest)
		return nil, nil, false
	}
	return prov, creds, true
}

func providerMessage(err error, creds map[string]string) string {
	return dnsprov.Clip(dnsprov.Scrub(err, creds))
}

func (h *DomainsHandler) accountZones(accountID int) ([]store.DNSZone, error) {
	zones, err := h.d.Store.ListDNSZones()
	if err != nil {
		return nil, err
	}
	out := []store.DNSZone{}
	for _, z := range zones {
		if z.AccountID == accountID {
			out = append(out, z)
		}
	}
	return out, nil
}

// TestAccount POST /api/admin/dns/accounts/{id}/test: lists the zones, or
// where the provider cannot, reads the first zone's records.
func (h *DomainsHandler) TestAccount(w http.ResponseWriter, r *http.Request) {
	acc := h.account(w, r)
	if acc == nil {
		return
	}
	prov, creds, ok := h.provider(w, acc)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), providerTimeout)
	defer cancel()
	_, err := prov.ListZones(ctx)
	if errors.Is(err, dnsprov.ErrNoZoneListing) {
		zones, zerr := h.accountZones(acc.ID)
		if zerr != nil {
			log.Printf("dns admin: zones: %v", zerr)
			httpx.SendJSONError(w, "Failed to read the DNS zones", http.StatusInternalServerError)
			return
		}
		if len(zones) == 0 {
			httpx.SendJSONError(w, "This provider cannot list zones. Add a zone by hand, then test again.", http.StatusBadRequest)
			return
		}
		_, err = prov.GetRecords(ctx, zones[0].Name)
	}
	if err != nil {
		httpx.SendJSONError(w, providerMessage(err, creds), http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// RefreshZones POST /api/admin/dns/accounts/{id}/zones/refresh: the zones the
// provider lists replace the listed ones; zones added by hand stay.
func (h *DomainsHandler) RefreshZones(w http.ResponseWriter, r *http.Request) {
	acc := h.account(w, r)
	if acc == nil {
		return
	}
	prov, creds, ok := h.provider(w, acc)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), providerTimeout)
	defer cancel()
	listed, err := prov.ListZones(ctx)
	if errors.Is(err, dnsprov.ErrNoZoneListing) {
		httpx.SendJSONError(w, "This provider cannot list zones. Add them by hand.", http.StatusBadRequest)
		return
	}
	if err != nil {
		httpx.SendJSONError(w, providerMessage(err, creds), http.StatusBadGateway)
		return
	}
	names := []string{}
	seen := map[string]bool{}
	for _, z := range listed {
		if n, err := services.ValidateFreeFQDN(z); err == nil && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	// An empty listing is far more often a token scoped wrong than an account
	// with no zones, and replacing with it would drop every zone's access list.
	if len(names) == 0 {
		httpx.SendJSONError(w, "The provider listed no zones. Nothing was changed; check what the credentials may access.", http.StatusConflict)
		return
	}
	if err := h.d.Store.ReplaceDNSZones(acc.ID, names, false); err != nil {
		log.Printf("dns admin: replace zones of %d: %v", acc.ID, err)
		httpx.SendJSONError(w, "Failed to save the zones", http.StatusInternalServerError)
		return
	}
	h.respondZones(w, acc.ID)
}

// AddZone POST /api/admin/dns/accounts/{id}/zones - {name}.
func (h *DomainsHandler) AddZone(w http.ResponseWriter, r *http.Request) {
	acc := h.account(w, r)
	if acc == nil {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminBodyMax)).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	name, err := services.ValidateFreeFQDN(req.Name)
	if err != nil {
		httpx.SendJSONError(w, capitalize(err.Error())+".", http.StatusBadRequest)
		return
	}
	if err := h.d.Store.AddDNSZone(acc.ID, name); err != nil {
		log.Printf("dns admin: add zone to %d: %v", acc.ID, err)
		httpx.SendJSONError(w, "Failed to add the zone", http.StatusInternalServerError)
		return
	}
	h.respondZones(w, acc.ID)
}

func (h *DomainsHandler) respondZones(w http.ResponseWriter, accountID int) {
	zones, err := h.accountZones(accountID)
	if err != nil {
		log.Printf("dns admin: zones: %v", err)
		httpx.SendJSONError(w, "Failed to read the DNS zones", http.StatusInternalServerError)
		return
	}
	out := make([]zoneOption, 0, len(zones))
	for _, z := range zones {
		out = append(out, zoneOption{ID: z.ID, Name: z.Name})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "zones": out})
}

// SetZoneAccess PUT /api/admin/dns/zones/{zoneId}/access - {principals:[]},
// each "user:<id>" or "role:<id>". Taking access away deletes nothing.
func (h *DomainsHandler) SetZoneAccess(w http.ResponseWriter, r *http.Request) {
	zoneID := httpx.MustAtoi(mux.Vars(r)["zoneId"])
	var req struct {
		Principals []string `json:"principals"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminBodyMax)).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	zones, err := h.d.Store.ListDNSZones()
	if err != nil {
		log.Printf("dns admin: zones: %v", err)
		httpx.SendJSONError(w, "Failed to read the DNS zones", http.StatusInternalServerError)
		return
	}
	found := false
	for _, z := range zones {
		found = found || z.ID == zoneID
	}
	if !found {
		httpx.SendJSONError(w, "Zone not found", http.StatusNotFound)
		return
	}
	out := []string{}
	seen := map[string]bool{}
	for _, p := range req.Principals {
		if !services.ValidZonePrincipal(p) {
			httpx.SendJSONError(w, `Every entry must be "user:<id>" or "role:<id>"`, http.StatusBadRequest)
			return
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if err := h.d.Store.SetDNSZoneAccess(zoneID, out); err != nil {
		log.Printf("dns admin: zone %d access: %v", zoneID, err)
		httpx.SendJSONError(w, "Failed to save the zone access", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "access": out})
}
