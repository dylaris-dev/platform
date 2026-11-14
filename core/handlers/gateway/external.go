package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"dylaris-core/handlers/gate"
	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// The route-only product as a machine API: /api/external/gateway/*, reached
// with a user API key holding gateway.read / gateway.write.
//
// Every method here is an adapter around the panel handler that already does
// the work, for the same reason as the rest of the external surface (see
// handlers/auth/external_api.go): the entitlement, suspension, cap and
// ownership guards live in those handlers, and a copy would lose them. The
// adapters add two things only.
//
// The caller is never an admin here. ExternalOwnerRoute gives the request the
// KEY OWNER's identity, admin flag included, and the session handlers then
// skip TXT proof, the custom-domain cap, the blocked prefixes and the owner
// check on delete for an admin. A key is a credential that leaves the panel;
// an admin who wants those powers uses the panel.
//
// Every answer is JSON in one shape. The session handlers answer some errors
// with http.Error (text/plain) and list routes as a bare array, which is fine
// for the panel and useless for a script.

// ExternalGatewayHandler holds the session handlers it adapts. requireGateway,
// routeOptions and usage come from other handler packages and arrive as
// functions so this package does not import them.
type ExternalGatewayHandler struct {
	warp           *WarpHandler
	gw             *GatewayHandler
	domains        *CustomDomainHandler
	requireGateway func(http.HandlerFunc) http.HandlerFunc
	routeOptions   http.HandlerFunc
	usage          http.HandlerFunc
}

func NewExternalGatewayHandler(warp *WarpHandler, gw *GatewayHandler, domains *CustomDomainHandler,
	requireGateway func(http.HandlerFunc) http.HandlerFunc, routeOptions, usage http.HandlerFunc) *ExternalGatewayHandler {
	return &ExternalGatewayHandler{warp: warp, gw: gw, domains: domains,
		requireGateway: requireGateway, routeOptions: routeOptions, usage: usage}
}

// ListLinkKits GET /api/external/gateway/link-kits - the caller's route-only link kits, without secrets, plus used (live kits), retiring and limit; a retiring kit carries retire_at.
func (h *ExternalGatewayHandler) ListLinkKits(w http.ResponseWriter, r *http.Request) {
	serveExternal(w, r, h.warp.ListLinkKits, renameList("kits"))
}

// MintLinkKit POST /api/external/gateway/link-kits - creates a link kit; link_key is shown once.
func (h *ExternalGatewayHandler) MintLinkKit(w http.ResponseWriter, r *http.Request) {
	externalGateway(h.warp.MintLinkKit)(w, r)
}

// RevokeLinkKit DELETE /api/external/gateway/link-kits/{linkID} - revokes one of the caller's link kits and removes its routes.
func (h *ExternalGatewayHandler) RevokeLinkKit(w http.ResponseWriter, r *http.Request) {
	externalGateway(h.warp.RevokeLinkKit)(w, r)
}

// RollLinkKit POST /api/external/gateway/link-kits/{linkID}/roll - replaces a link kit's key; the old one keeps working for the grace period.
func (h *ExternalGatewayHandler) RollLinkKit(w http.ResponseWriter, r *http.Request) {
	externalGateway(h.warp.RollLinkKit)(w, r)
}

// RetireLinkKit POST /api/external/gateway/link-kits/{linkID}/retire - retires one of the caller's link kits to replace a node, irreversibly: it stops counting toward maxLinks at once, keeps working but takes no new routes, voice or Bedrock, and is revoked with its routes at retire_at, 8 hours later; 409 kit_already_retiring (with retire_at), retire_not_needed (no host limit), retire_limit (as many retiring as hosts) or retire_budget (4 per host per UTC month).
func (h *ExternalGatewayHandler) RetireLinkKit(w http.ResponseWriter, r *http.Request) {
	owner := reqctx.ByonCallerID(r)
	if owner == "" {
		writeExternalError(w, http.StatusUnauthorized, "Unauthorized", "unauthorized")
		return
	}
	linkID := mux.Vars(r)["linkID"]
	if !strings.HasPrefix(linkID, "link-") {
		writeExternalError(w, http.StatusNotFound, "Link not found", "link_not_found")
		return
	}
	retireAt, err := services.RetireLinkKit(h.warp.d.Store, owner, linkID, time.Now())
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "link_id": linkID, "retire_at": retireAt.UTC().Format(time.RFC3339)})
	case errors.Is(err, sql.ErrNoRows):
		writeExternalError(w, http.StatusNotFound, "Link not found", "link_not_found")
	case errors.Is(err, store.ErrLinkKitRetiring):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "This link is already retiring.",
			"code": "kit_already_retiring", "retire_at": retireAt.UTC().Format(time.RFC3339)})
	case errors.Is(err, services.ErrRetireNotNeeded):
		writeExternalError(w, http.StatusConflict, "Your account has no host limit to make room under: mint the new link, then revoke the old one when it is empty.", "retire_not_needed")
	case errors.Is(err, services.ErrRetireLimit):
		writeExternalError(w, http.StatusConflict, "As many links are retiring as your plan has hosts. Wait for one to finish.", "retire_limit")
	case errors.Is(err, services.ErrRetireBudget):
		writeExternalError(w, http.StatusConflict, "This month's link retirements are used up.", "retire_budget")
	default:
		log.Printf("external gateway retire %s for %s: %v", linkID, owner, err)
		writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
	}
}

// ListRoutes GET /api/external/gateway/routes - the caller's route-only addresses; voice_allowed, bedrock_allowed and bedrock_active answer for this caller as its enables would, an active managed gateway plan included.
func (h *ExternalGatewayHandler) ListRoutes(w http.ResponseWriter, r *http.Request) {
	externalGateway(h.gw.ListLinkRoutes)(w, withManagedAPI(r, h.gw.d.Store, reqctx.ByonCallerID(r)))
}

// CreateRoute POST /api/external/gateway/routes - creates or retargets an address on one of the caller's links; a retiring link takes no route changes except removing an address or moving it to another link (409 kit_retiring).
func (h *ExternalGatewayHandler) CreateRoute(w http.ResponseWriter, r *http.Request) {
	if !h.checkExternalTarget(w, r) {
		return
	}
	externalGateway(h.requireGateway(h.gw.CreateLinkRoute))(w, r)
}

// DeleteRoute DELETE /api/external/gateway/routes/{domain} - removes one of the caller's addresses.
func (h *ExternalGatewayHandler) DeleteRoute(w http.ResponseWriter, r *http.Request) {
	externalGateway(h.gw.DeleteLinkRoute)(w, r)
}

// SetRouteProxyProtocol PATCH /api/external/gateway/routes/{domain}/proxy-protocol - body {"enabled": bool}, real player IP forwarding.
func (h *ExternalGatewayHandler) SetRouteProxyProtocol(w http.ResponseWriter, r *http.Request) {
	externalGateway(h.requireGateway(h.gw.SetLinkRouteProxyProtocol))(w, r)
}

// SetRouteVoice PUT /api/external/gateway/routes/{domain}/voice - body {"enabled": bool, "target_port": int}, voice chat relay; an active managed gateway plan lifts the platform's global voice permission for its account (an explicit per-user deny and the other checks, such as a suspension or a retiring link, still refuse), any other account only where the platform's voice permissions allow (409 voice_not_allowed).
func (h *ExternalGatewayHandler) SetRouteVoice(w http.ResponseWriter, r *http.Request) {
	externalGateway(h.gw.SetLinkRouteVoice)(w, withManagedAPI(r, h.gw.d.Store, reqctx.ByonCallerID(r)))
}

// SetRouteBedrock PUT /api/external/gateway/routes/{domain}/bedrock - body {"java": bool, "bedrock": bool}, the address's editions; an active managed gateway plan lifts the platform's Bedrock audience setting for its account (the other checks, such as a suspension, a retiring link or no Bedrock entry in the region, still refuse), any other account only while the platform offers Bedrock to it (409 bedrock_disabled).
func (h *ExternalGatewayHandler) SetRouteBedrock(w http.ResponseWriter, r *http.Request) {
	externalGateway(h.gw.SetLinkRouteBedrock)(w, withManagedAPI(r, h.gw.d.Store, reqctx.ByonCallerID(r)))
}

// managedAPIKey marks a request as an external call of an account that holds
// a managed gateway plan; the value is that owner's id.
type managedAPIKey struct{}

// noManagedPlanKey marks an external call whose owner's managed plan lookup
// answered "none"; the value is that owner's id.
type noManagedPlanKey struct{}

// managedLookupKey marks a request whose managed plan lookup failed; the value
// is a managedLookupFailure.
type managedLookupKey struct{}

type managedLookupFailure struct {
	ownerID string
	err     error
}

// withManagedAPI marks r for ownerID when the account holds a managed gateway
// plan (owner decision 2026-11-14: such an account may always enable voice and
// Bedrock on the external API). Only SetRouteVoice, SetRouteBedrock and
// ListRoutes (whose flags must say what those allow) call it, so the panel
// routes never carry the mark. A failed lookup leaves r unmarked,
// so it grants nothing; where the global rules would then refuse, the handler
// answers a server error instead (managedLookupErr): that refusal would be a
// guess against the customer.
func withManagedAPI(r *http.Request, st store.Store, ownerID string) *http.Request {
	if ownerID == "" {
		return r
	}
	managed, err := services.HasManagedProduct(st, ownerID)
	if err != nil {
		log.Printf("external gateway %s: managed plan lookup for %s: %v", r.URL.Path, ownerID, err)
		return r.WithContext(context.WithValue(r.Context(), managedLookupKey{}, managedLookupFailure{ownerID, err}))
	}
	if !managed {
		return r.WithContext(context.WithValue(r.Context(), noManagedPlanKey{}, ownerID))
	}
	return r.WithContext(context.WithValue(r.Context(), managedAPIKey{}, ownerID))
}

// managedAPICall reports whether r was marked by withManagedAPI for ownerID,
// the owner of what the handler is about to change.
func managedAPICall(r *http.Request, ownerID string) bool {
	id, _ := r.Context().Value(managedAPIKey{}).(string)
	return id != "" && id == ownerID
}

// planNeededRefusal is msg, a refusal by the global rules, plus the sentence
// that a managed gateway plan would lift it, for an external call whose owner
// holds none (after a plan ended the refusal says why). Otherwise msg as is.
func planNeededRefusal(r *http.Request, ownerID, msg string) string {
	if id, _ := r.Context().Value(noManagedPlanKey{}).(string); id != "" && id == ownerID {
		return msg + " " + gate.ManagedPlanNeededText
	}
	return msg
}

// managedLookupErr is the error of withManagedAPI's failed lookup for ownerID;
// nil when the lookup answered or was never made (the panel path).
func managedLookupErr(r *http.Request, ownerID string) error {
	f, ok := r.Context().Value(managedLookupKey{}).(managedLookupFailure)
	if !ok || ownerID == "" || f.ownerID != ownerID {
		return nil
	}
	return fmt.Errorf("managed plan lookup: %w", f.err)
}

// ListCustomDomains GET /api/external/gateway/custom-domains - the caller's own domain claims and their TXT records.
func (h *ExternalGatewayHandler) ListCustomDomains(w http.ResponseWriter, r *http.Request) {
	serveExternal(w, r, h.domains.List, renameList("domains"))
}

// AddCustomDomain POST /api/external/gateway/custom-domains - body {"domain": "play.example.com"}, returns the TXT record to publish.
func (h *ExternalGatewayHandler) AddCustomDomain(w http.ResponseWriter, r *http.Request) {
	externalGateway(h.domains.Add)(w, r)
}

// DeleteCustomDomain DELETE /api/external/gateway/custom-domains/{domain} - removes the caller's claim; refused while a route uses it.
func (h *ExternalGatewayHandler) DeleteCustomDomain(w http.ResponseWriter, r *http.Request) {
	externalGateway(h.domains.Delete)(w, r)
}

// IssueCustomDomainTXTToken POST /api/external/gateway/custom-domains/{domain}/txt-token - returns the claim's TXT record, minting it once.
func (h *ExternalGatewayHandler) IssueCustomDomainTXTToken(w http.ResponseWriter, r *http.Request) {
	externalGateway(h.domains.IssueTXTToken)(w, r)
}

// VerifyCustomDomainTXT POST /api/external/gateway/custom-domains/{domain}/verify-txt - checks the published TXT record now.
func (h *ExternalGatewayHandler) VerifyCustomDomainTXT(w http.ResponseWriter, r *http.Request) {
	externalGateway(h.domains.VerifyTXT)(w, r)
}

// externalRegion is one gateway region as a customer needs it: where an
// address on our domains can live and whether it is up. Edge addresses stay
// out on purpose; a region is reached through its wildcard.
type externalRegion struct {
	Region   string `json:"region"`
	Wildcard string `json:"wildcard"`
	Online   bool   `json:"online"`
}

// ListRegions GET /api/external/gateway/regions - the gateway regions with their wildcard and liveness, plus the hoster domains and CNAME target.
func (h *ExternalGatewayHandler) ListRegions(w http.ResponseWriter, r *http.Request) {
	serveExternal(w, r, h.routeOptions, func(_ *http.Request, obj map[string]interface{}) error {
		obj["items"] = h.regions(r.Context())
		return nil
	})
}

func (h *ExternalGatewayHandler) regions(ctx context.Context) []externalRegion {
	out := []externalRegion{}
	if h.gw.d.Redis == nil {
		return out
	}
	byRegion := map[string]int{}
	for _, e := range services.GetEdgesFromRedis(ctx, h.gw.d.Redis) {
		if e.Region == "" {
			continue
		}
		i, ok := byRegion[e.Region]
		if !ok {
			i = len(out)
			byRegion[e.Region] = i
			out = append(out, externalRegion{Region: e.Region})
		}
		if out[i].Wildcard == "" && strings.HasPrefix(e.Wildcard, "*.") {
			out[i].Wildcard = e.Wildcard
		}
		out[i].Online = out[i].Online || e.Status == "online"
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Region < out[b].Region })
	return out
}

// externalRegionUsage is one (region, kind) of the period. Products are
// folded together: the allowance is pooled across them.
type externalRegionUsage struct {
	Region string `json:"region"`
	Kind   string `json:"kind"`
	Bytes  int64  `json:"bytes"`
}

// GetUsage GET /api/external/gateway/usage - the period's totals (as /api/external/usage) plus regions [{region, kind, bytes}], final (false while the month runs: an estimate as of asOf) and, for the running month of a managed plan, pool {includedGb, maxPurchaseGb, usedBytes, percent}; ?period=YYYY-MM.
func (h *ExternalGatewayHandler) GetUsage(w http.ResponseWriter, r *http.Request) {
	// The panel helper falls back to this month on a bad period; a script
	// asking for March and silently getting November is worse than a 400.
	if p := r.URL.Query().Get("period"); p != "" {
		if _, err := time.Parse("2006-01", p); err != nil {
			writeExternalError(w, http.StatusBadRequest, "period must be YYYY-MM", "bad_request")
			return
		}
	}
	serveExternal(w, r, h.usage, func(r *http.Request, obj map[string]interface{}) error {
		rows, err := h.gw.d.Store.GetTrafficUsageRegions(reqctx.ByonCallerID(r), httpx.UsagePeriod(r))
		if err != nil {
			return err
		}
		out := []externalRegionUsage{}
		at := map[[2]string]int{}
		for _, row := range rows {
			k := [2]string{row.Region, row.Kind}
			i, ok := at[k]
			if !ok {
				i = len(out)
				at[k] = i
				out = append(out, externalRegionUsage{Region: row.Region, Kind: row.Kind})
			}
			out[i].Bytes += row.Bytes
		}
		sort.SliceStable(out, func(a, b int) bool { return out[a].Bytes > out[b].Bytes })
		obj["regions"] = out
		return usageStanding(h.gw.d.Store, reqctx.ByonCallerID(r), httpx.UsagePeriod(r), time.Now().UTC(), obj)
	})
}

// externalPool is a managed-gateway plan's one allowance for the period.
type externalPool struct {
	IncludedGB    int64  `json:"includedGb"`
	MaxPurchaseGB *int64 `json:"maxPurchaseGb"`
	UsedBytes     int64  `json:"usedBytes"`
	Percent       int64  `json:"percent"`
}

// usageStanding says how far the numbers can be trusted, and where a managed
// plan stands against its allowance.
//
// final is true once the month has ended: counters are added to the month the
// aggregator runs in, so nothing is ever added to a month that is over. Until
// then every figure is an estimate that grows, as of asOf. A package func so
// the admin view of one customer (AdminUserGateway) shows the same standing.
func usageStanding(st store.Store, userID string, period, now time.Time, obj map[string]interface{}) error {
	obj["final"] = !period.AddDate(0, 1, 0).After(now)
	u, err := st.GetTrafficUsage(userID, period)
	if err != nil {
		return err
	}
	if !u.UpdatedAt.IsZero() {
		obj["asOf"] = u.UpdatedAt.UTC().Format(time.RFC3339)
	}
	l, err := st.GetTrafficLimit("user:"+userID, services.TrafficRegionAny, services.TrafficKindEdge)
	if err != nil {
		return err
	}
	// Only the running month: the row is today's plan, and an earlier month
	// measured against it would show a percentage that was never true.
	running := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if l == nil || l.IncludedGB == nil || !period.Equal(running) {
		return nil // not a managed plan: no single pool to stand against
	}
	p := externalPool{IncludedGB: *l.IncludedGB, MaxPurchaseGB: l.MaxPurchaseGB, UsedBytes: services.ManagedPoolBytes(u)}
	if limit := p.IncludedGB * 1_000_000_000; limit > 0 {
		p.Percent = p.UsedBytes * 100 / limit
	} else if p.UsedBytes > 0 {
		p.Percent = 100 // an allowance of none is used up by the first byte
	}
	obj["pool"] = p
	return nil
}

// externalGateway runs a session handler for a key caller: non-admin, and
// with its answer normalised to the external JSON shape.
func externalGateway(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveExternal(w, r, next, nil)
	}
}

// reshapeFunc edits a successful JSON object answer before it is sent.
type reshapeFunc func(r *http.Request, obj map[string]interface{}) error

// renameList moves a handler's list field to "items", the one name every
// external list uses.
func renameList(key string) reshapeFunc {
	return func(_ *http.Request, obj map[string]interface{}) error {
		if v, ok := obj[key]; ok {
			obj["items"] = v
			delete(obj, key)
		}
		return nil
	}
}

// capturedResponse buffers the inner handler's answer, headers included, so
// http.Error's text/plain and nosniff never reach the client.
type capturedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *capturedResponse) Header() http.Header { return c.header }

func (c *capturedResponse) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *capturedResponse) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(b)
}

func serveExternal(w http.ResponseWriter, r *http.Request, next http.HandlerFunc, reshape reshapeFunc) {
	r = r.WithContext(context.WithValue(r.Context(), "isAdmin", false))
	c := &capturedResponse{header: http.Header{}}
	next(c, r)
	if c.status == 0 {
		c.status = http.StatusOK
	}
	for k, v := range c.header {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Type", "Content-Length", "X-Content-Type-Options":
		default:
			w.Header()[k] = v
		}
	}

	raw := bytes.TrimSpace(c.body.Bytes())
	var parsed interface{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // byte counts survive the round trip exactly
	isJSON := len(raw) > 0 && dec.Decode(&parsed) == nil

	// Only the gate's own 503 is written for the caller; any other 503 (a
	// handler relaying a Redis/DB fault) is masked like the rest of 5xx.
	if c.status >= 500 && !isGatewayDisabled(c.status, parsed, isJSON) {
		// Session handlers put raw Redis/Postgres errors in 5xx bodies; the
		// panel tolerates that, a public API must not.
		log.Printf("external gateway %s %s: %d %s", r.Method, r.URL.Path, c.status, strings.TrimSpace(string(raw)))
		writeExternalError(w, c.status, "Internal error. Try again later.", "internal")
		return
	}
	if c.status >= 400 {
		msg, code := externalErrorFields(c.status, raw, parsed, isJSON)
		writeExternalError(w, c.status, msg, code)
		return
	}
	var out interface{}
	switch v := parsed.(type) {
	case []interface{}:
		out = map[string]interface{}{"success": true, "items": v}
	case map[string]interface{}:
		if reshape == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			w.Write(raw)
			return
		}
		if err := reshape(r, v); err != nil {
			writeExternalError(w, http.StatusInternalServerError, "Lookup failed", "internal")
			return
		}
		out = v
	default:
		out = map[string]interface{}{"success": true}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(c.status)
	json.NewEncoder(w).Encode(out)
}

func isGatewayDisabled(status int, parsed interface{}, isJSON bool) bool {
	obj, ok := parsed.(map[string]interface{})
	return status == http.StatusServiceUnavailable && isJSON && ok && obj["error"] == "gateway_disabled"
}

func writeExternalError(w http.ResponseWriter, status int, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": msg, "code": code})
}

// externalErrorFields reads the message and machine code out of an error
// answer in any of the three shapes the session handlers produce:
// http.Error text, {message} / {message, code}, and the gate's
// {error: <code>, message}.
func externalErrorFields(status int, raw []byte, parsed interface{}, isJSON bool) (msg, code string) {
	if obj, ok := parsed.(map[string]interface{}); isJSON && ok {
		msg, _ = obj["message"].(string)
		errField, _ := obj["error"].(string)
		code, _ = obj["code"].(string)
		if msg == "" {
			msg = errField
		} else if code == "" {
			code = errField
		}
	} else {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	if code == "" {
		code = externalErrorCode(status, msg)
	}
	return msg, code
}

// externalErrorCodes maps today's prose refusals to stable codes. Matched on
// fragments of the handlers' own strings, so a reworded message falls back to
// the status code rather than to a wrong code; the external tests pin the
// ones a client acts on.
var externalErrorCodes = []struct{ fragment, code string }{
	{"invalid target port", "invalid_port"},
	{"target host is required", "invalid_target"},
	{"invalid target host", "invalid_target"},
	{"use 127.0.0.1 for this machine", "invalid_target"},
	{"unknown link", "link_not_found"},
	{"link not found", "link_not_found"},
	{"invalid link id", "link_not_found"},
	{"link limit reached", "link_limit"},
	{"addresses on our domains", "route_limit"},
	{"route creation is disabled", "route_limit"},
	{"routes on your own domains", "custom_domain_limit"},
	{"prove you own this domain", "domain_unverified"},
	{"is blocked for your account", "domain_blocked"},
	{"is already in use", "domain_taken"},
	{"suspended by an administrator", "route_suspended"},
	{"unknown region", "unknown_region"},
}

func externalErrorCode(status int, msg string) string {
	m := strings.ToLower(msg)
	for _, e := range externalErrorCodes {
		if strings.Contains(m, e.fragment) {
			return e.code
		}
	}
	switch {
	case status == http.StatusUnauthorized:
		return "unauthorized"
	case status == http.StatusForbidden:
		return "forbidden"
	case status == http.StatusNotFound:
		return "not_found"
	case status == http.StatusConflict:
		return "conflict"
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status >= 500:
		return "internal"
	}
	return "bad_request"
}
