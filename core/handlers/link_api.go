package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"dylaris-core/services"
	"dylaris-core/store"

	"dylaris-pkg/protocol"

	"github.com/redis/go-redis/v9"
)

// The route-only link's API. A route-only kit is one container that talks to
// Core and to nothing else of ours: no warp, no Redis login. Everything it used
// to read and write in Redis it now asks of Core here, authenticated by its kit
// key (WarpAPIKeyMiddleware), and Core writes the Redis keys the edges, the Hub
// and the panel read - with the TTLs the link used to write them with, so none
// of those readers changed.
//
// What that buys is authority. The link used to re-assert its own token as
// valid on every keepalive, so revoking the kit depended on its Redis login
// being dropped in the same moment. Now only Core writes the token key, and it
// writes it only while the key is live and the account is not cut off.

const (
	// linkTokenTTL and linkOnlineTTL are the TTLs the link itself used to write
	// (gateway/link providers.go SelfRegister / KeepAlive). The edge rechecks
	// link:<token> every 30s; the panel reads online_link:<token>.
	linkTokenTTL  = 24 * time.Hour
	linkOnlineTTL = 15 * time.Second

	// linkStatsMaxLen matches the link's own publisher (~18 min at 3s).
	linkStatsMaxLen = 360
)

// routeOnlyKey is the gate every route-only call passes: a live link- kit key
// with an owner, gateway routing on, and an owner who is not cut off. It
// answers the refusal itself.
//
// cutOff reports that the owner is past a grace, so the heartbeat can take the
// link's keys down at once instead of leaving the token valid until the next
// hourly enforcement pass.
func (h *WarpHandler) routeOnlyKey(w http.ResponseWriter, r *http.Request) (key store.WarpAPIKey, token string, ok, cutOff bool) {
	key, ok = r.Context().Value(warpKeyCtx).(store.WarpAPIKey)
	if !ok {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return key, "", false, false
	}
	// Same shape LinkBoot accepts for a kit: an owner-less link- key is not one.
	if !strings.HasPrefix(key.NodeID, "link-") || key.OwnerID == "" {
		sendJSONError(w, "Not a route-only link key", http.StatusForbidden)
		return key, "", false, false
	}
	if !h.state.gatewayEnabled() || h.state.Gateway == nil || h.state.Redis == nil {
		sendJSONError(w, "Gateway routing is disabled", http.StatusConflict)
		return key, "", false, false
	}
	token = h.state.Gateway.LinkToken(key.NodeID)
	// Fail open on a DB fault, loudly, exactly as LinkBoot does: a blip must not
	// take a paying tenant's link down.
	if b, berr := h.state.Store.GetUserBilling(key.OwnerID); berr != nil {
		log.Printf("route-only link %s: billing lookup failed, suspension gate skipped: %v", key.NodeID, berr)
	} else if store.OwnerCutOff(b, h.state.SuspendGrace, services.OverLimitGrace, time.Now()) {
		sendJSONError(w, "Account suspended", http.StatusForbidden)
		return key, token, false, true
	}
	return key, token, true, false
}

// linkRevoked re-reads the key. The middleware read it before the handler ran,
// and a revoke can land in between.
func (h *WarpHandler) linkRevoked(nodeID string) bool {
	fresh, err := h.state.Store.GetWarpAPIKeyByNodeID(nodeID)
	if err != nil {
		log.Printf("route-only link %s: fresh revoke check failed, proceeding: %v", nodeID, err)
		return false
	}
	return fresh.RevokedAt != nil
}

// LinkHeartbeat POST /api/warp/link/heartbeat - a route-only link says it is
// running. Core marks its tunnel token valid for the edges (24 h) and the link
// online for the panel (15 s). A link beats every 5 s.
//
// Refused while the owner is cut off, and then the two keys are deleted at once:
// the edges drop the tunnel within their 30 s recheck, the same as a revoke.
func (h *WarpHandler) LinkHeartbeat(w http.ResponseWriter, r *http.Request) {
	key, token, ok, cutOff := h.routeOnlyKey(w, r)
	if cutOff {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := h.state.Redis.Del(ctx, "link:"+token, "online_link:"+token).Err(); err != nil {
			log.Printf("route-only link %s: cut off, deleting its tunnel key failed: %v", key.NodeID, err)
		}
	}
	if !ok {
		return
	}
	if h.linkRevoked(key.NodeID) {
		sendJSONError(w, "Key revoked", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := h.state.Redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, "link:"+token, "valid", linkTokenTTL)
		p.Set(ctx, "online_link:"+token, "1", linkOnlineTTL)
		return nil
	})
	if err != nil {
		log.Printf("route-only link %s: heartbeat write failed: %v", key.NodeID, err)
		sendJSONError(w, "Heartbeat not recorded", http.StatusServiceUnavailable)
		return
	}
	// Asked again AFTER the write. RevokeLinkKitTeardown marks the key revoked
	// and then deletes the token key. If this read still sees it live, the
	// revoke's mark comes later, so its delete comes after this write and wins.
	// If it sees it revoked, the delete may already have run before the write,
	// so this undoes the write itself. Either way no revoked token survives.
	if h.linkRevoked(key.NodeID) {
		h.state.Redis.Del(ctx, "link:"+token, "online_link:"+token)
		sendJSONError(w, "Key revoked", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// linkEdge is one edge as a route-only link needs it: where to dial and which
// certificate to expect there.
type linkEdge struct {
	ID          string `json:"id"`
	Addr        string `json:"addr"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// LinkEdges GET /api/warp/link/edges - every edge in sys:edges with a public
// tunnel address, and the certificate fingerprint it published. The link holds
// a tunnel to EVERY edge in this list, so it is not filtered by region or
// anything else: an edge missing here is an edge that cannot reach this link's
// servers.
//
// Only the PUBLIC address: a route-only link is outside our network, and the
// private one is not a path it could take.
func (h *WarpHandler) LinkEdges(w http.ResponseWriter, r *http.Request) {
	key, _, ok, _ := h.routeOnlyKey(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	edges, err := readLinkEdges(ctx, h.state.Redis)
	if err != nil {
		log.Printf("route-only link %s: reading the edge list failed: %v", key.NodeID, err)
		sendJSONError(w, "Edge list unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"edges": edges})
}

// readLinkEdges reads what gateway/link RedisEdgeProvider.readEdges and
// verifyEdgeFingerprint read, in one round trip after the member list. An edge
// whose registry entry has expired is gone and left out; the link keeps its last
// good list when this comes back empty.
func readLinkEdges(ctx context.Context, rdb *redis.Client) ([]linkEdge, error) {
	ids, err := rdb.SMembers(ctx, "sys:edges").Result()
	if err != nil {
		return nil, err
	}
	out := make([]linkEdge, 0, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	pipe := rdb.Pipeline()
	regs := make([]*redis.StringCmd, len(ids))
	fps := make([]*redis.StringCmd, len(ids))
	for i, id := range ids {
		regs[i] = pipe.Get(ctx, "edge:registry:"+id)
		fps[i] = pipe.Get(ctx, "edge:cert:fingerprint:"+id)
	}
	// redis.Nil on a missing key is expected per command; only a failure of the
	// pipeline as a whole is an error here.
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	for i, id := range ids {
		raw, err := regs[i].Result()
		if err != nil {
			continue
		}
		var reg struct {
			EdgeID      string `json:"edge_id"`
			IP          string `json:"ip"`
			ServicePort string `json:"service_port"`
		}
		if json.Unmarshal([]byte(raw), &reg) != nil || reg.IP == "" || reg.ServicePort == "" {
			continue
		}
		e := linkEdge{ID: reg.EdgeID, Addr: reg.IP + ":" + reg.ServicePort}
		if e.ID == "" {
			e.ID = id
		}
		e.Fingerprint, _ = fps[i].Result()
		out = append(out, e)
	}
	return out, nil
}

// LinkStats POST /api/warp/link/stats - one of the link's telemetry records,
// written to the stream every consumer already reads,
// dylaris:link:<link id>:stats. The link does not get to name the stream or
// the id in it: both are the kit's own id.
func (h *WarpHandler) LinkStats(w http.ResponseWriter, r *http.Request) {
	key, _, ok, _ := h.routeOnlyKey(w, r)
	if !ok {
		return
	}
	var s protocol.GatewayStats
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&s); err != nil {
		sendJSONError(w, "Invalid stats record", http.StatusBadRequest)
		return
	}
	s.Component, s.ID = "link", key.NodeID
	data, err := protocol.MarshalGatewayStats(s)
	if err != nil {
		sendJSONError(w, "Invalid stats record", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.state.Redis.XAdd(ctx, &redis.XAddArgs{
		Stream: "dylaris:link:" + key.NodeID + ":stats",
		MaxLen: linkStatsMaxLen,
		Approx: true,
		Values: map[string]interface{}{"data": string(data)},
	}).Err(); err != nil {
		log.Printf("route-only link %s: stats write failed: %v", key.NodeID, err)
		sendJSONError(w, "Stats not recorded", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
