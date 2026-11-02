package auth

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"dylaris-core/authz"
	"dylaris-core/handlers/audit"
	"dylaris-core/models"
)

// keyFailuresPerMin is how many failed key attempts one source IP may make per
// minute: unknown, revoked or expired keys, and refusals of a real key.
const keyFailuresPerMin = 120

// keyAuditWindow is how long one bundled audit entry for a key runs.
const keyAuditWindow = 10 * time.Minute

// keyCountEvery bounds how often a bundled entry's count is written back:
// at most once per this interval, plus whenever the count reaches a power of
// two, so a key polling every few seconds costs a handful of UPDATEs per window
// rather than one per request.
const keyCountEvery = 30 * time.Second

// keyRefusal reports whether a key request ended refused: the statuses the
// key checks and the handlers behind them answer with when the key may not do
// it (or its target is gone).
func keyRefusal(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable:
		return true
	}
	return false
}

// auditKeyRequest records what a request made with a real key came to. The
// owner's rule: a key counts like an invited member. Every write is its own
// entry; reads and refusals are bundled, one entry per key, server and kind
// per 10 minutes with a count, because a script polling every few seconds -
// or hammering a server it may not touch - would otherwise bury everything.
//
// Entries go to the server's own trail when the request named a server the key
// is scoped to and its owner can still reach (which switches auditing on there,
// like an invite). Otherwise the identity log: a refusal on a server the owner
// lost, or never had, must neither write into nor switch on that server's log.
func (h *APIKeysHandler) auditKeyRequest(r *http.Request, key *models.APIKey, shape keyRouteShape, serverUUID, requiredPerm string, status int) {
	if h.d.Store == nil {
		return
	}
	if status == 0 {
		status = http.StatusOK
	}
	kind := requiredPerm
	if kind == "" {
		kind = "api_request"
	}
	serverID := 0
	if shape == keyRouteServer && serverUUID != "" && key.Scope.AllowsServer(serverUUID) {
		if srv, err := h.d.Store.GetServerByUUID(serverUUID); err == nil && srv != nil {
			serverID = srv.ID
		}
	}
	meta := map[string]interface{}{"method": r.Method, "path": r.URL.Path, "status": status}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case keyRefusal(status):
		meta["capability"] = requiredPerm
		if serverID > 0 && !h.ownerReaches(key, serverID) {
			serverID = 0
		}
		if serverID > 0 {
			h.auditBundled(r, key, serverID, audit.ServerAuditEventAPIKeyDenied, "denied", meta)
		} else {
			h.auditBundled(r, key, 0, audit.AuditEventAPIKeyDenied, "denied", meta)
		}
	case status == http.StatusTooManyRequests:
		h.auditBundled(r, key, 0, audit.AuditEventAPIKeyRateLimited, "rate_limited", meta)
	case read:
		if status >= 200 && status < 300 {
			if serverID > 0 {
				h.auditBundled(r, key, serverID, kind, kind, meta)
			} else {
				meta["capability"] = requiredPerm
				h.auditBundled(r, key, 0, audit.AuditEventAPIKeyRead, kind, meta)
			}
		}
	default:
		if authz.WasAudited(r.Context()) {
			return // the handler wrote its own entry, which carries the key
		}
		if serverID > 0 {
			audit.LogServerAs(h.d.Store, r, key, serverID, kind, key.UserID, "", meta)
		} else {
			meta["capability"] = requiredPerm
			audit.LogIdentityAs(h.d.Store, r, key, audit.AuditEventAPIKeyUsed, key.UserID, key.UserID, meta)
		}
	}
}

// ownerReaches reports whether the key's owner still holds anything on the
// server. Fails closed: an error reads as no.
func (h *APIKeysHandler) ownerReaches(key *models.APIKey, serverID int) bool {
	if h.d.Authz == nil {
		return false
	}
	owner, err := h.d.Store.GetUserByID(key.UserID)
	if err != nil || owner == nil {
		return false
	}
	res, err := h.d.Authz.Resolve(authz.Identity{UserID: owner.ID, Username: owner.Username, IsAdmin: owner.IsAdmin}, serverID)
	return err == nil && res.HasAnyServerCap()
}

// auditBundled writes the first request of a window as an entry and counts the
// rest into it. serverID 0 means the identity log. Without Redis every request
// is its own entry: losing the bundling must not lose the record.
func (h *APIKeysHandler) auditBundled(r *http.Request, key *models.APIKey, serverID int, eventType, kind string, meta map[string]interface{}) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rdb := h.d.Redis
	n := int64(1)
	counter := fmt.Sprintf("audit:apikey:%d:%d:%s", key.ID, serverID, kind)
	if rdb != nil {
		// One transaction, and EXPIRE NX in it: an INCR on its own recreates
		// a counter that expired in between WITHOUT a TTL, and that window
		// would then never close.
		pipe := rdb.TxPipeline()
		incr := pipe.Incr(ctx, counter)
		pipe.ExpireNX(ctx, counter, keyAuditWindow)
		if _, err := pipe.Exec(ctx); err != nil {
			rdb = nil
		} else if n = incr.Val(); n > 1 {
			if n&(n-1) != 0 {
				if ok, err := rdb.SetNX(ctx, counter+":tick", 1, keyCountEvery).Result(); err != nil || !ok {
					return
				}
			}
			// The first request may still be writing the entry; a later one
			// then sets the count.
			if id, gerr := rdb.Get(ctx, counter+":row").Int64(); gerr == nil {
				var uerr error
				if serverID > 0 {
					uerr = h.d.Store.SetServerAuditCount(id, n)
				} else {
					uerr = h.d.Store.SetAuditIdentityCount(id, n)
				}
				if uerr != nil {
					log.Printf("api-key audit: count entry %d: %v", id, uerr)
				}
			}
			return
		}
	}
	meta["count"] = n
	meta["windowMinutes"] = int(keyAuditWindow / time.Minute)
	var id int64
	if serverID > 0 {
		id = audit.LogServerAs(h.d.Store, r, key, serverID, eventType, key.UserID, "", meta)
	} else {
		id = audit.LogIdentityAs(h.d.Store, r, key, eventType, key.UserID, key.UserID, meta)
	}
	if rdb != nil && id > 0 {
		rdb.Set(ctx, counter+":row", id, keyAuditWindow)
		rdb.Set(ctx, counter+":tick", 1, keyCountEvery)
	}
}
