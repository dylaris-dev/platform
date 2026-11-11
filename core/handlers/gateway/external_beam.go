package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"dylaris-core/handlers/guard"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/beamaccess"

	"github.com/gorilla/mux"
)

// The managed-gateway beam surface of the provider API. A customer's own Core
// signs beam tickets with an Ed25519 key it registers here; our Core publishes
// the key as beam:issuer:<kid> (services/beam_issuers.go) and the relay admits
// those tickets against it. These handlers are native rather than adapters:
// there is no panel handler for them to wrap.

type externalBeamKey struct {
	Kid       string    `json:"kid"`
	PublicKey string    `json:"public_key"`
	CreatedAt time.Time `json:"created_at"`
}

// publishBeamIssuers republishes the owner's records after a change. A failure
// is logged, not answered: the DB change stands and the reconcile loop heals
// Redis within five minutes.
func (h *ExternalGatewayHandler) publishBeamIssuers(ctx context.Context, ownerID string) {
	publishBeamIssuers(ctx, h.gw.d, ownerID)
}

func publishBeamIssuers(ctx context.Context, d Deps, ownerID string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := services.PublishBeamIssuers(ctx, d.Store, d.Redis, d.SuspendGrace, ownerID); err != nil {
		log.Printf("beam issuers %s: publish failed, the reconcile loop retries: %v", ownerID, err)
	}
}

// RegisterBeamKey POST /api/external/gateway/beam/keys - body {"public_key": base64 Ed25519 public key}; returns the kid to put in ticket headers. At most two live keys.
func (h *ExternalGatewayHandler) RegisterBeamKey(w http.ResponseWriter, r *http.Request) {
	owner := reqctx.ByonCallerID(r)
	if owner == "" {
		writeExternalError(w, http.StatusUnauthorized, "Unauthorized", "unauthorized")
		return
	}
	st := h.gw.d.Store
	if msg := guard.OwnerRefusal(st, h.gw.d.SuspendGrace, r, owner); msg != "" {
		writeExternalError(w, http.StatusForbidden, msg, "account_suspended")
		return
	}
	managed, err := services.HasManagedProduct(st, owner)
	if err != nil {
		log.Printf("beam key register %s: managed lookup: %v", owner, err)
		writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
		return
	}
	if !managed {
		writeExternalError(w, http.StatusForbidden, "Beam keys need a managed gateway plan", "managed_required")
		return
	}
	var req struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeExternalError(w, http.StatusBadRequest, "Invalid JSON body", "bad_request")
		return
	}
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.PublicKey))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		writeExternalError(w, http.StatusBadRequest, "public_key must be a base64 Ed25519 public key (32 bytes)", "invalid_public_key")
		return
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
		return
	}
	kid := hex.EncodeToString(raw)
	created, err := st.CreateBeamIssuerKeyUnderCap(models.BeamIssuerKey{ID: kid, OwnerID: owner, PublicKey: pub}, services.MaxBeamIssuerKeysPerOwner)
	if err != nil {
		log.Printf("beam key register %s: %v", owner, err)
		writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
		return
	}
	if !created {
		writeExternalError(w, http.StatusConflict, "Two beam keys are already registered; revoke one first", "beam_key_limit")
		return
	}
	h.publishBeamIssuers(r.Context(), owner)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "kid": kid})
}

// ListBeamKeys GET /api/external/gateway/beam/keys - the caller's live beam keys.
func (h *ExternalGatewayHandler) ListBeamKeys(w http.ResponseWriter, r *http.Request) {
	owner := reqctx.ByonCallerID(r)
	if owner == "" {
		writeExternalError(w, http.StatusUnauthorized, "Unauthorized", "unauthorized")
		return
	}
	keys, err := h.gw.d.Store.ListBeamIssuerKeysByOwner(owner)
	if err != nil {
		log.Printf("beam key list %s: %v", owner, err)
		writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
		return
	}
	out := []externalBeamKey{}
	for _, k := range keys {
		if k.RevokedAt == nil {
			out = append(out, externalBeamKey{Kid: k.ID, PublicKey: base64.StdEncoding.EncodeToString(k.PublicKey), CreatedAt: k.CreatedAt})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "items": out})
}

// RevokeBeamKey DELETE /api/external/gateway/beam/keys/{kid} - revokes one of the caller's beam keys; the relay refuses its tickets at once.
func (h *ExternalGatewayHandler) RevokeBeamKey(w http.ResponseWriter, r *http.Request) {
	owner := reqctx.ByonCallerID(r)
	if owner == "" {
		writeExternalError(w, http.StatusUnauthorized, "Unauthorized", "unauthorized")
		return
	}
	kid := mux.Vars(r)["kid"]
	if !services.ValidBeamKid(kid) {
		writeExternalError(w, http.StatusBadRequest, "kid must be 32 lowercase hex characters", "invalid_kid")
		return
	}
	found, err := h.gw.d.Store.RevokeBeamIssuerKey(owner, kid)
	if err != nil {
		log.Printf("beam key revoke %s: %v", owner, err)
		writeExternalError(w, http.StatusInternalServerError, "Internal error. Try again later.", "internal")
		return
	}
	if !found {
		writeExternalError(w, http.StatusNotFound, "Beam key not found", "beam_key_not_found")
		return
	}
	// The record goes now, independent of the rest of the republish: a revoke
	// must not wait on a billing or settings read.
	if h.gw.d.Redis != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		if err := h.gw.d.Redis.Del(ctx, services.BeamIssuerKeyPrefix+kid).Err(); err != nil {
			log.Printf("beam key revoke %s: delete record %s: %v", owner, kid, err)
		}
		cancel()
	}
	h.publishBeamIssuers(r.Context(), owner)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

type externalBeamRelay struct {
	Region     string `json:"region"`
	PublicHost string `json:"public_host"`
	ClientPort string `json:"client_port"`
}

// ListBeamRelays GET /api/external/gateway/beam/relays - the beam relays a customer Core puts in its beam config [{region, public_host, client_port}]; ?region= narrows to one region.
func (h *ExternalGatewayHandler) ListBeamRelays(w http.ResponseWriter, r *http.Request) {
	region := strings.TrimSpace(r.URL.Query().Get("region"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	out := []externalBeamRelay{}
	seen := map[externalBeamRelay]bool{}
	for _, info := range beamaccess.DiscoverBeamRelays(ctx, h.gw.d.Redis) {
		if region != "" && !strings.EqualFold(strings.TrimSpace(info.Region), region) {
			continue
		}
		host := publicRelayHost(info)
		port := info.ClientPort
		if port == "" {
			port = info.ServicePort // a relay predating client_port serves clients there
		}
		e := externalBeamRelay{Region: info.Region, PublicHost: host, ClientPort: port}
		if host == "" || port == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Region != out[b].Region {
			return out[a].Region < out[b].Region
		}
		return out[a].PublicHost < out[b].PublicHost
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "items": out})
}

// publicRelayHost is the relay's public name, else its advertised public IP.
// Never private_ip: these answers leave our network.
func publicRelayHost(info beamaccess.BeamRelayInfo) string {
	if h := strings.TrimSpace(info.PublicHost); h != "" {
		return h
	}
	return strings.TrimSpace(info.IP)
}
