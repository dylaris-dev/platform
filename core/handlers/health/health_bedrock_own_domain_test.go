package health

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type bedrockHealthStore struct {
	store.Store
	refs []store.BedrockServerRef
	dns  map[string]store.AddressDNSCheck
}

func (f *bedrockHealthStore) GetSetting(k string) (string, error) {
	if k == "routing_mode" {
		return "gateway", nil
	}
	return "", sql.ErrNoRows
}
func (f *bedrockHealthStore) ListBedrockServerRefs() ([]store.BedrockServerRef, error) {
	return f.refs, nil
}
func (f *bedrockHealthStore) GetAddressDNSChecks(names []string) (map[string]store.AddressDNSCheck, error) {
	out := map[string]store.AddressDNSCheck{}
	for _, n := range names {
		if c, ok := f.dns[n]; ok {
			out[n] = c
		}
	}
	return out, nil
}

// A Bedrock server whose only address is its own domain counts under the
// region its stored check found; without a stored ok it is not counted. The
// component has no resolver to ask, so it cannot resolve.
func TestBedrockHealthCountsOwnDomainsByStoredRegion(t *testing.T) {
	cases := []struct {
		name   string
		dns    map[string]store.AddressDNSCheck
		entry  string // region of the one live entry
		status string
		detail string
	}{
		{"stored ok, entry there", map[string]store.AddressDNSCheck{"play.example.com": {Status: "ok", Region: "us"}}, "us", "up", ""},
		{"stored ok, entry elsewhere", map[string]store.AddressDNSCheck{"play.example.com": {Status: "ok", Region: "us"}}, "eu", "degraded", "us"},
		{"stored miss", map[string]store.AddressDNSCheck{"play.example.com": {Status: "elsewhere", Region: "us"}}, "eu", "disabled", ""},
		{"never checked", nil, "eu", "disabled", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			mr.Set("route:play.example.com", `{"target_ip":"mc_srv-7","target_port":25565,"server_uuid":"srv-7"}`)
			mr.SAdd("sys:index:routes", "play.example.com")
			mr.Set("edge:registry:e1", `{"edge_id":"e1","status":"online","region":"eu","wildcard":"*.eu.dylaris.com","ip":"203.0.113.10"}`)
			mr.SAdd("sys:edges", "e1")
			mr.Set("bedrock:registry:b1", `{"id":"b1","region":"`+c.entry+`","version":"1"}`)
			mr.SAdd("sys:bedrock", "b1")
			fs := &bedrockHealthStore{refs: []store.BedrockServerRef{{ServerID: 7, ServerUUID: "srv-7"}}, dns: c.dns}
			h := NewHealthHandler(Deps{Store: fs, Redis: rdb})
			comp := h.bedrockComponent(context.Background())
			if comp.Status != c.status {
				t.Fatalf("status %s (%s / %s), want %s", comp.Status, comp.Detail, comp.Reason, c.status)
			}
			if c.detail != "" && (len(comp.Items) != 1 || comp.Items[0].Name != c.detail || !strings.Contains(comp.Reason, c.detail)) {
				t.Fatalf("items %+v reason %q, want region %s", comp.Items, comp.Reason, c.detail)
			}
			if c.status == "up" && (len(comp.Items) != 1 || comp.Items[0].Name != "us") {
				t.Fatalf("items %+v, want us", comp.Items)
			}
		})
	}
}
