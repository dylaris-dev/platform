package remotegw

// ContractKeys are the JSON keys the client depends on, per response object.
// The fake (remotegwtest) is checked against them in contract_test.go and the
// real provider handlers in handlers/gateway/external_contract_test.go, so
// neither can drift from the other without a red test. Add keys, never rename.
var ContractKeys = map[string][]string{
	"account":                  {"userId", "product", "managed", "entitlementSource", "suspended", "scopes", "limits"},
	"account.limits":           {"maxLinks", "maxRoutes", "maxCustomDomains"},
	"check-domain":             {"success", "domain", "available", "reason"},
	"custom-domains":           {"success", "items"},
	"custom-domains.item":      {"domain", "state", "attempts"},
	"custom-domains.add":       {"success", "domain"},
	"custom-domains.txt-token": {"success", "domain"},
	"link-kits":                {"success", "items"},
	"link-kits.item":           {"id", "name", "link_id", "created_at", "region", "caps"},
	"link-kits.mint":           {"success", "link_key", "link_id"},
	"link-kits.retire":         {"success", "link_id", "retire_at"},
	"routes":                   {"success", "items"},
	"routes.item":              {"domain", "target_ip", "target_port", "link_id", "proxy_protocol", "voice", "bedrock"},
	"routes.voice":             {"success", "voice"},
	"routes.voice.voice":       {"enabled", "port", "target_port"},
	"routes.bedrock":           {"success", "bedrock"},
	"routes.bedrock.bedrock":   {"java", "bedrock", "has_key"},
	"routes.floodgate-key":     {"success", "domain", "key"},
	"regions":                  {"success", "hosterDomains", "customDomainsEnabled", "cnameTarget", "items"},
	"regions.item":             {"region", "wildcard", "online"},
	"usage":                    {"success", "usage", "final"},
	"usage.usage":              {"period", "edgeBytes", "relayBytes"},
	"players":                  {"success", "asOf", "total", "items", "peak"},
	"players.item":             {"domain", "java", "bedrock"},
	"players.peak":             {"today", "last30Days"},
	"players.peak.window":      {"java", "bedrock", "total"},
}
