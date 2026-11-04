package dnsprov

import (
	"fmt"
	"strings"

	"github.com/libdns/cloudflare"
	"github.com/libdns/desec"
	"github.com/libdns/godaddy"
	hetzner "github.com/libdns/hetzner/v2"
	"github.com/libdns/inwx"
	"github.com/libdns/ionos"
	"github.com/libdns/namecheap"
	"github.com/libdns/netcup"
	"github.com/libdns/ovh"
	"github.com/libdns/porkbun"
	"github.com/libdns/route53"
)

// CredentialField describes one input of a provider's credential form.
// Optional fields may stay empty.
type CredentialField struct {
	Key, Label string
	Secret     bool
	Optional   bool
}

// Kind is one supported provider. ZoneListing is what the pinned package really
// implements (checked against its source), so the panel only offers "pick a
// zone" where it can work.
type Kind struct {
	ID, Label   string
	Fields      []CredentialField
	ZoneListing bool
}

type kindDef struct {
	Kind
	build func(v map[string]string) any
}

func secret(key, label string) CredentialField {
	return CredentialField{Key: key, Label: label, Secret: true}
}

var kindDefs = []kindDef{
	{Kind{"cloudflare", "Cloudflare", []CredentialField{secret("api_token", "API token")}, true},
		func(v map[string]string) any { return &cloudflare.Provider{APIToken: v["api_token"]} }},
	{Kind{"hetzner", "Hetzner DNS", []CredentialField{secret("api_token", "API token")}, true},
		func(v map[string]string) any { return &hetzner.Provider{APIToken: v["api_token"]} }},
	{Kind{"desec", "deSEC", []CredentialField{secret("token", "API token")}, true},
		func(v map[string]string) any { return &desec.Provider{Token: v["token"]} }},
	{Kind{"porkbun", "Porkbun", []CredentialField{secret("api_key", "API key"), secret("api_secret_key", "Secret API key")}, true},
		func(v map[string]string) any {
			return &porkbun.Provider{APIKey: v["api_key"], APISecretKey: v["api_secret_key"]}
		}},
	// inwx v0.3.1 has no ListZones, despite the earlier research notes.
	{Kind{"inwx", "INWX", []CredentialField{
		{Key: "username", Label: "Username"},
		secret("password", "Password"),
		{Key: "shared_secret", Label: "2FA shared secret", Secret: true, Optional: true},
	}, false},
		func(v map[string]string) any {
			return &inwx.Provider{Username: v["username"], Password: v["password"], SharedSecret: v["shared_secret"]}
		}},
	{Kind{"ionos", "IONOS", []CredentialField{secret("auth_api_token", "API token")}, true},
		func(v map[string]string) any { return &ionos.Provider{AuthAPIToken: v["auth_api_token"]} }},
	{Kind{"ovh", "OVH", []CredentialField{
		{Key: "endpoint", Label: "Endpoint (e.g. ovh-eu)"},
		secret("application_key", "Application key"),
		secret("application_secret", "Application secret"),
		secret("consumer_key", "Consumer key"),
	}, false},
		func(v map[string]string) any {
			return &ovh.Provider{Endpoint: v["endpoint"], ApplicationKey: v["application_key"],
				ApplicationSecret: v["application_secret"], ConsumerKey: v["consumer_key"]}
		}},
	{Kind{"netcup", "netcup", []CredentialField{
		{Key: "customer_number", Label: "Customer number"},
		secret("api_key", "API key"),
		secret("api_password", "API password"),
	}, false},
		func(v map[string]string) any {
			return &netcup.Provider{CustomerNumber: v["customer_number"], APIKey: v["api_key"], APIPassword: v["api_password"]}
		}},
	{Kind{"namecheap", "Namecheap", []CredentialField{
		{Key: "user", Label: "Username"},
		secret("api_key", "API key"),
		{Key: "client_ip", Label: "Whitelisted client IP"},
	}, false},
		func(v map[string]string) any {
			return &namecheap.Provider{User: v["user"], APIKey: v["api_key"], ClientIP: v["client_ip"]}
		}},
	{Kind{"godaddy", "GoDaddy", []CredentialField{secret("api_token", "API key:secret")}, false},
		func(v map[string]string) any { return &godaddy.Provider{APIToken: v["api_token"]} }},
	{Kind{"route53", "AWS Route 53", []CredentialField{
		{Key: "access_key_id", Label: "Access key ID"},
		secret("secret_access_key", "Secret access key"),
		{Key: "region", Label: "Region (default us-east-1)", Optional: true},
	}, false},
		func(v map[string]string) any {
			region := v["region"]
			if region == "" {
				region = "us-east-1"
			}
			return &route53.Provider{AccessKeyId: v["access_key_id"], SecretAccessKey: v["secret_access_key"], Region: region}
		}},
}

// Kinds lists the supported providers in display order.
func Kinds() []Kind {
	out := make([]Kind, len(kindDefs))
	for i, d := range kindDefs {
		out[i] = d.Kind
	}
	return out
}

// New builds a provider client. Missing required fields are reported by label
// here, because the vendor's own error on the first API call names none.
func New(kind string, creds map[string]string) (Provider, error) {
	for _, d := range kindDefs {
		if d.ID != kind {
			continue
		}
		v := make(map[string]string, len(creds))
		for k, s := range creds {
			v[k] = strings.TrimSpace(s)
		}
		var missing []string
		for _, f := range d.Fields {
			if !f.Optional && v[f.Key] == "" {
				missing = append(missing, f.Label)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("%s is missing: %s", d.Label, strings.Join(missing, ", "))
		}
		return newAdapter(d.build(v))
	}
	return nil, fmt.Errorf("unknown DNS provider %q", kind)
}
