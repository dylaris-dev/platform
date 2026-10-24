package handlers

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/redis/go-redis/v9"

	"dylaris-core/services"
	"dylaris-core/store"
)

// normHost is how a typed domain is stored: lowercase, and without the one
// trailing dot of a fully qualified name. The edge strips that dot from the
// player's handshake before it looks the route up, so a stored dot would be a
// route nobody can reach.
func normHost(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// isHostname reports whether s is a lowercase DNS name of at least minLabels
// labels: each label 1-63 letters, digits or inner hyphens, 253 in all. No
// wildcard - the gateway routes exact Minecraft hostnames only.
func isHostname(s string, minLabels int) bool {
	if len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < minLabels {
		return false
	}
	for _, l := range labels {
		if len(l) > 63 || !subRegexDNS.MatchString(l) {
			return false
		}
	}
	return true
}

// Per-hoster subdomain validation regexes — kept narrow on purpose so the
// admin's choice in settings actually constrains what users can register.
var (
	subRegexLetters      = regexp.MustCompile(`^[a-z]+$`)
	subRegexAlphanumeric = regexp.MustCompile(`^[a-z0-9]+$`)
	subRegexDNS          = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// validateSubdomain matches a user-entered subdomain against the validation
// mode the admin picked for that hoster domain.
func validateSubdomain(sub, mode string) bool {
	if sub == "" || len(sub) > 63 {
		return false
	}
	switch mode {
	case "letters":
		return subRegexLetters.MatchString(sub)
	case "alphanumeric":
		return subRegexAlphanumeric.MatchString(sub)
	case "dns":
		return subRegexDNS.MatchString(sub)
	default:
		return false
	}
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

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
