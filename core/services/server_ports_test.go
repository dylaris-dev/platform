package services

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/store"
)

func TestParsePortRange(t *testing.T) {
	for _, c := range []struct {
		in       string
		min, max int
		ok       bool
	}{
		{"26000-26099", 26000, 26099, true},
		{" 30000 - 30000 ", 30000, 30000, true},
		{"26000-26999", 26000, 26999, true},
		{"26000-27000", 0, 0, false}, // 1001 ports
		{"1023-1100", 0, 0, false},
		{"65000-65536", 0, 0, false},
		{"26099-26000", 0, 0, false},
		{"25500-25530", 0, 0, false}, // the node's own ports
		{"25523-25600", 0, 0, false},
		{"25524-25600", 25524, 25600, true},
		{"26000", 0, 0, false},
		{"a-b", 0, 0, false},
		{"", 0, 0, false},
	} {
		min, max, err := ParsePortRange(c.in)
		if (err == nil) != c.ok || (c.ok && (min != c.min || max != c.max)) {
			t.Errorf("ParsePortRange(%q) = %d, %d, %v; want %d, %d, ok=%v", c.in, min, max, err, c.min, c.max, c.ok)
		}
	}
}

func TestPortRangesOverlap(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"26000-26099", "25600-25699", false},
		{"25650-25750", "25600-25699", true},
		{"25699-25700", "25600-25699", true},
		{"25700-25800", "25600-25699", false},
		{"26000-26099", "", false},
		{"bad", "25600-25699", false},
	} {
		if got := PortRangesOverlap(c.a, c.b); got != c.want {
			t.Errorf("PortRangesOverlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

type portPolicyStore struct {
	store.Store
	perms    map[string]bool
	limits   map[string]*int
	settings map[string]string
	role     *int
}

func (f *portPolicyStore) GetPortPermission(scope string) (*bool, error) {
	if v, ok := f.perms[scope]; ok {
		return &v, nil
	}
	return nil, nil
}
func (f *portPolicyStore) GetPortLimit(scope string) (*store.PortLimit, error) {
	if v, ok := f.limits[scope]; ok {
		return &store.PortLimit{Scope: scope, MaxPorts: v}, nil
	}
	return nil, nil
}
func (f *portPolicyStore) GetSetting(key string) (string, error) { return f.settings[key], nil }
func (f *portPolicyStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return f.role, store.CapOverrides{}, nil
}

func TestResolvePortsAllowedTakesTheMostSpecificScope(t *testing.T) {
	role := 4
	srv := &models.Server{ID: 7, OwnerID: "u1"}
	for _, c := range []struct {
		name     string
		perms    map[string]bool
		global   string
		want     bool
		decision string
	}{
		{"nothing set: off", nil, "", false, "global"},
		{"global on", nil, "true", true, "global"},
		{"role off beats global on", map[string]bool{"role:4": false}, "true", false, "role:4"},
		{"user on beats role off", map[string]bool{"role:4": false, "user:u1": true}, "", true, "user:u1"},
		{"server off beats user on", map[string]bool{"user:u1": true, "server:7": false}, "true", false, "server:7"},
	} {
		st := &portPolicyStore{perms: c.perms, role: &role, settings: map[string]string{PortsEnabledSettingKey: c.global}}
		got, by, err := ResolvePortsAllowed(st, srv)
		if err != nil || got != c.want || by != c.decision {
			t.Errorf("%s: = %v by %s (%v), want %v by %s", c.name, got, by, err, c.want, c.decision)
		}
	}
}

// The limit convention: nil = no limit, 0 = none. A row that exists has
// answered, including with NULL; a missing row asks the next scope.
func TestResolvePortLimitFollowsTheLimitConvention(t *testing.T) {
	role := 4
	zero, three := 0, 3
	for _, c := range []struct {
		name   string
		limits map[string]*int
		global string
		want   *int64
	}{
		{"nothing set: the product default", nil, "", &DefaultPortsPerUser},
		{"global unlimited", nil, "unlimited", nil},
		{"global none", nil, "0", ptr64(0)},
		{"role 3 beats global", map[string]*int{"role:4": &three}, "unlimited", ptr64(3)},
		{"user NULL row: no limit, beats role 3", map[string]*int{"role:4": &three, "user:u1": nil}, "", nil},
		{"user 0 is none, not unlimited", map[string]*int{"user:u1": &zero}, "unlimited", ptr64(0)},
	} {
		st := &portPolicyStore{limits: c.limits, role: &role, settings: map[string]string{PortsMaxPerUserSettingKey: c.global}}
		got, _, err := ResolvePortLimit(st, "u1")
		if err != nil || (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: = %v (%v), want %v", c.name, deref(got), err, deref(c.want))
		}
	}
}

func ptr64(v int64) *int64 { return &v }
func deref(p *int64) any {
	if p == nil {
		return "nil"
	}
	return *p
}

func TestValidPortScope(t *testing.T) {
	for _, c := range []struct {
		scope string
		limit bool
		want  bool
	}{
		{"server:5", false, true},
		{"server:5", true, false}, // limits count per owner
		{"user:abc", true, true},
		{"role:2", true, true},
		{"role:x", false, false},
		{"server:0", false, false},
		{"global", false, false},
		{"user:", false, false},
	} {
		if got := ValidPortScope(c.scope, c.limit); got != c.want {
			t.Errorf("ValidPortScope(%q, %v) = %v, want %v", c.scope, c.limit, got, c.want)
		}
	}
}

func TestPublishServerPortsWritesTheFrozenShape(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	if err := PublishServerPorts(ctx, rdb, "u-1", []store.ServerPort{{Protocol: "udp", HostPort: 26001, ContainerPort: 24454}}); err != nil {
		t.Fatal(err)
	}
	got, _ := mr.Get("dylaris:server:u-1:ports")
	if want := `{"v":1,"ports":[{"p":"udp","h":26001,"c":24454}]}`; got != want {
		t.Fatalf("key = %s, want %s", got, want)
	}
	if err := PublishServerPorts(ctx, rdb, "u-1", nil); err != nil {
		t.Fatal(err)
	}
	if mr.Exists("dylaris:server:u-1:ports") {
		t.Fatal("no ports must remove the key")
	}
}
