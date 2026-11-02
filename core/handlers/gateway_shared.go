package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/redis/go-redis/v9"

	"dylaris-core/services"
	"dylaris-core/store"
)

// SettingMaxCustomDomains caps the routes one account may hold on its own
// domains. Those cost us no address, but each one is a claim the verifier
// queries a tenant-chosen nameserver for and a key every edge loads, and there
// was no bound on them at all.
const SettingMaxCustomDomains = "gateway_max_custom_domains"

var defaultMaxCustomDomains = services.LimitPtr(50)

// linkCapClientAddr is the capability of a link that writes a PROXY v2 header
// for stream type 0x05.
const linkCapClientAddr = "client-addr"

// linkAnnouncesCap reports whether online_link:<token> carries cap. A missing
// key (link offline) or an old link's "1" is false; only a Redis failure is an error.
func linkAnnouncesCap(ctx context.Context, rdb *redis.Client, token, cap string) (bool, error) {
	return services.LinkAnnouncesCap(ctx, rdb, token, cap)
}

// ownNodeKey resolves one of the caller's live BYON node keys by its identity, or
// the status and message to refuse with. "Not yours" reads exactly like "does
// not exist", so the answer never confirms that someone else's identity is real.
func ownNodeKey(st store.Store, nodeID, userID string) (*store.WarpAPIKey, int, string) {
	if !strings.HasPrefix(nodeID, "node-") {
		return nil, http.StatusBadRequest, "Not a node key"
	}
	k, err := st.GetWarpAPIKeyByNodeID(nodeID)
	if err != nil || k.OwnerID == "" || k.OwnerID != userID {
		return nil, http.StatusNotFound, "Node key not found"
	}
	if k.RevokedAt != nil {
		return nil, http.StatusConflict, "This node key has been revoked"
	}
	return k, 0, ""
}
