package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"dylaris-core/authz"
	"dylaris-core/models"
)

// keyFailuresPerMin is how many failed key attempts (unknown, revoked or
// expired) one source IP may make per minute.
const keyFailuresPerMin = 120

// keyAuditWindow is how long one bundled audit entry for a key's reads runs.
const keyAuditWindow = 10 * time.Minute

// auditKeyRequest records what a request made with a real key came to. The
// owner's rule: a key counts like an invited member. Every refusal and every
// write is its own entry; reads are bundled, one entry per key, server and kind
// of access per 10 minutes with a count, because a monitoring script polling
// every few seconds would otherwise bury everything else.
//
// Entries go to the server's own trail when the request named a server the key
// is scoped to (which switches auditing on there, like an invite). An attempt
// on a server outside the key's scope goes to the identity log instead: it must
// neither write into nor switch on the log of a server that may belong to
// someone else.
func (h *APIKeysHandler) auditKeyRequest(r *http.Request, key *models.APIKey, shape keyRouteShape, serverUUID, requiredPerm string, status int) {
	if h.state == nil || h.state.Store == nil {
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
		if srv, err := h.state.Store.GetServerByUUID(serverUUID); err == nil && srv != nil {
			serverID = srv.ID
		}
	}
	meta := map[string]interface{}{"method": r.Method, "path": r.URL.Path, "status": status}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		meta["capability"] = requiredPerm
		if serverID > 0 {
			logServerAuditAs(h.state, r, key, serverID, ServerAuditEventAPIKeyDenied, key.UserID, "", meta)
		} else {
			logIdentityAuditAs(h.state, r, key, AuditEventAPIKeyDenied, key.UserID, key.UserID, meta)
		}
	case status == http.StatusTooManyRequests:
		h.auditBundled(r, key, 0, AuditEventAPIKeyRateLimited, "rate_limited", meta)
	case read:
		if status >= 200 && status < 300 {
			if serverID > 0 {
				h.auditBundled(r, key, serverID, kind, kind, meta)
			} else {
				meta["capability"] = requiredPerm
				h.auditBundled(r, key, 0, AuditEventAPIKeyRead, kind, meta)
			}
		}
	default:
		if authz.WasAudited(r.Context()) {
			return // the handler wrote its own entry, which carries the key
		}
		if serverID > 0 {
			logServerAuditAs(h.state, r, key, serverID, kind, key.UserID, "", meta)
		} else {
			meta["capability"] = requiredPerm
			logIdentityAuditAs(h.state, r, key, AuditEventAPIKeyUsed, key.UserID, key.UserID, meta)
		}
	}
}

// auditBundled writes the first request of a window as an entry and counts the
// rest into it. serverID 0 means the identity log. Without Redis every request
// is its own entry: losing the bundling must not lose the record.
func (h *APIKeysHandler) auditBundled(r *http.Request, key *models.APIKey, serverID int, eventType, kind string, meta map[string]interface{}) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rdb := h.state.Redis
	n := int64(1)
	counter := fmt.Sprintf("audit:apikey:%d:%d:%s", key.ID, serverID, kind)
	if rdb != nil {
		pipe := rdb.Pipeline()
		pipe.SetNX(ctx, counter, 0, keyAuditWindow)
		incr := pipe.Incr(ctx, counter)
		if _, err := pipe.Exec(ctx); err != nil {
			rdb = nil
		} else if n = incr.Val(); n > 1 {
			// The first request may still be writing the entry; a later one
			// then sets the count.
			if id, gerr := rdb.Get(ctx, counter+":row").Int64(); gerr == nil {
				var uerr error
				if serverID > 0 {
					uerr = h.state.Store.SetServerAuditCount(id, n)
				} else {
					uerr = h.state.Store.SetAuditIdentityCount(id, n)
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
		id = logServerAuditAs(h.state, r, key, serverID, eventType, key.UserID, "", meta)
	} else {
		id = logIdentityAuditAs(h.state, r, key, eventType, key.UserID, key.UserID, meta)
	}
	if rdb != nil && id > 0 {
		rdb.Set(ctx, counter+":row", id, keyAuditWindow)
	}
}
