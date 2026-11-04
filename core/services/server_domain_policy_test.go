package services

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services/dnsprov"
	"dylaris-core/store"
)

func TestValidateDomainInput(t *testing.T) {
	for _, tc := range []struct {
		label string
		ok    bool
	}{
		{"survival", true},
		{"mc-1", true},
		{"UPPER", true}, // lower-cased by the caller, as the handler does
		{"a..b", false},
		{"-x", false},
		{"x-", false},
		{strings.Repeat("a", 64), false},
		{strings.Repeat("a", 63), true},
		{"*", false},
		{"_minecraft", false},
		{"", false},
		{"a.b", false},
		{"é", false},
	} {
		err := ValidateSubdomainLabel(strings.ToLower(tc.label))
		if (err == nil) != tc.ok {
			t.Errorf("label %q: err = %v, want ok=%v", tc.label, err, tc.ok)
		}
	}

	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"play.example.com", "play.example.com", true},
		{"Play.Example.COM.", "play.example.com", true},
		{"  mc.example.co.uk ", "mc.example.co.uk", true},
		{"https://x.com", "", false},
		{"x.com:25565", "", false},
		{"localhost", "", false},
		{"x", "", false},
		{"", "", false},
		{"203.0.113.10", "", false},
		{"a..b.com", "", false},
		{"-a.com", "", false},
		{"*.example.com", "", false},
		{"_minecraft._tcp.example.com", "", false},
		{"x.com/path", "", false},
		{strings.Repeat("a.", 127) + "com", "", false},
	} {
		got, err := ValidateFreeFQDN(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ValidateFreeFQDN(%q) = %q, %v; want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestCanUseZone(t *testing.T) {
	access := []store.DNSZoneAccess{
		{ZoneID: 1, Principal: "user:alice"},
		{ZoneID: 1, Principal: "role:7"},
		{ZoneID: 3, Principal: "user:bob"},
	}
	for _, tc := range []struct {
		name       string
		principals []string
		zone       int
		admin      bool
		want       bool
	}{
		{"admin always", nil, 2, true, true},
		{"empty access is admins only", []string{"user:alice", "role:7"}, 2, false, false},
		{"user principal", []string{"user:alice"}, 1, false, true},
		{"role principal", []string{"user:carol", "role:7"}, 1, false, true},
		{"other zone's grant does not count", []string{"user:bob"}, 1, false, false},
		{"other role", []string{"user:carol", "role:8"}, 1, false, false},
	} {
		if got := CanUseZone(tc.principals, access, tc.zone, tc.admin); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

type limitFakeStore struct {
	store.Store
	settings map[string]string
	rows     map[string]*int64 // present = row exists; nil value = NULL
}

func (f *limitFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *limitFakeStore) GetDNSSubdomainLimit(scope string) (*store.DNSSubdomainLimit, error) {
	v, ok := f.rows[scope]
	if !ok {
		return nil, nil
	}
	return &store.DNSSubdomainLimit{Scope: scope, Max: v}, nil
}

func TestSubdomainLimitScopes(t *testing.T) {
	five, three := int64(5), int64(3)
	for _, tc := range []struct {
		name     string
		rows     map[string]*int64
		setting  string
		want     *int64
		wantNone bool
	}{
		{"user row wins", map[string]*int64{"user:u1": &five, "user_default": &three}, "1", &five, false},
		{"user_default over setting", map[string]*int64{"user_default": &three}, "1", &three, false},
		{"setting when no rows", nil, "1", LimitPtr(1), false},
		{"setting zero is none", nil, "0", LimitPtr(0), false},
		{"NULL user row is no limit", map[string]*int64{"user:u1": nil, "user_default": &three}, "1", nil, true},
		{"missing user row falls through to NULL default", map[string]*int64{"user_default": nil}, "1", nil, true},
		{"nothing saved is no limit", nil, "", nil, true},
		{"unlimited setting", nil, "unlimited", nil, true},
	} {
		st := &limitFakeStore{settings: map[string]string{DNSMaxPerUserSettingKey: tc.setting}, rows: tc.rows}
		got := SubdomainLimitFor(st, "u1")
		if tc.wantNone {
			if got != nil {
				t.Errorf("%s: got %d, want no limit", tc.name, *got)
			}
			continue
		}
		if got == nil || *got != *tc.want {
			t.Errorf("%s: got %v, want %d", tc.name, got, *tc.want)
		}
	}

	st := &limitFakeStore{settings: map[string]string{DNSMaxPerServerSettingKey: "2"}}
	if got := ServerSubdomainLimit(st); got == nil || *got != 2 {
		t.Errorf("ServerSubdomainLimit = %v, want 2", got)
	}
	st.settings[DNSMaxPerServerSettingKey] = ""
	if got := ServerSubdomainLimit(st); got != nil {
		t.Errorf("ServerSubdomainLimit unset = %d, want no limit", *got)
	}
}

func TestDirectAddress(t *testing.T) {
	for _, tc := range []struct {
		node *models.Node
		port int
		want string
	}{
		{&models.Node{PublicIP: "203.0.113.10", Address: "198.51.100.1"}, 25650, "203.0.113.10:25650"},
		{&models.Node{PublicIP: "192.168.1.5", Address: "198.51.100.1"}, 25565, "198.51.100.1:25565"},
		{&models.Node{PublicIP: "10.0.0.1", Address: "0.0.0.0"}, 25565, ""},
		{&models.Node{PublicIP: "203.0.113.10"}, 0, ""},
		{nil, 25565, ""},
	} {
		if got := DirectAddress(tc.node, tc.port); got != tc.want {
			t.Errorf("DirectAddress(%+v, %d) = %q, want %q", tc.node, tc.port, got, tc.want)
		}
	}
}

type deleteAccountStore struct {
	store.Store
	owned []store.DNSOwnedRecord
}

func (f *deleteAccountStore) ListDNSOwnedRecords() ([]store.DNSOwnedRecord, error) {
	return f.owned, nil
}

type failingZoneProvider struct {
	fakeDNSProvider
	failGet string
}

func (p *failingZoneProvider) GetRecords(ctx context.Context, zone string) ([]dnsprov.Record, error) {
	if zone == p.failGet {
		return nil, errors.New("403 token tok-SECRET-123 not valid for zone")
	}
	return p.fakeDNSProvider.GetRecords(ctx, zone)
}

// Deleting an account removes exactly the ledger's records - by value, so a
// record repointed or added by hand at a ledger name stays - leaves foreign
// ones, and names what it could not reach.
func TestDeleteDNSAccountRecords(t *testing.T) {
	enc, err := EncryptDNSCredentials(testClusterSecret, map[string]string{"api_token": "tok-SECRET-123"})
	if err != nil {
		t.Fatal(err)
	}
	acc := store.DNSAccount{ID: 1, Provider: "cloudflare", EncCredentials: enc}
	st := &deleteAccountStore{owned: []store.DNSOwnedRecord{
		{ID: 1, AccountID: 1, Zone: "example.com", Name: "play", Type: "A", Value: "203.0.113.10"},
		{ID: 2, AccountID: 1, Zone: "example.com", Name: "_minecraft._tcp.play", Type: "SRV", Value: "play.example.com", Port: 25650},
		{ID: 3, AccountID: 1, Zone: "down.net", Name: "mc", Type: "A", Value: "203.0.113.10"},
		{ID: 4, AccountID: 2, Zone: "example.com", Name: "other", Type: "A", Value: "198.51.100.8"},
		{ID: 5, AccountID: 1, Zone: "example.com", Name: "lobby", Type: "A", Value: "203.0.113.10"},
	}}
	prov := &failingZoneProvider{failGet: "down.net", fakeDNSProvider: fakeDNSProvider{recs: map[string][]dnsprov.Record{
		"example.com": {
			{Name: "play", Type: "A", Value: "203.0.113.10"},
			{Name: "play", Type: "A", Value: "198.51.100.5"}, // added by hand beside Core's
			{Name: "play", Type: "SRV", Value: "Play.Example.com.", Port: 25650},
			{Name: "www", Type: "A", Value: "198.51.100.9"},
			{Name: "other", Type: "A", Value: "198.51.100.8"},
			{Name: "lobby", Type: "A", Value: "198.51.100.77"}, // Core's, repointed by hand
		},
	}}}
	newProv := func(string, map[string]string) (dnsprov.Provider, error) { return prov, nil }

	orphaned, err := DeleteDNSAccountRecords(context.Background(), st, testClusterSecret, acc, newProv)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orphaned, []string{"mc.down.net"}) {
		t.Errorf("orphaned = %v, want [mc.down.net]", orphaned)
	}
	left := prov.recs["example.com"]
	wantLeft := []dnsprov.Record{
		{Name: "play", Type: "A", Value: "198.51.100.5"},
		{Name: "www", Type: "A", Value: "198.51.100.9"},
		{Name: "other", Type: "A", Value: "198.51.100.8"},
		{Name: "lobby", Type: "A", Value: "198.51.100.77"},
	}
	if !reflect.DeepEqual(left, wantLeft) {
		t.Errorf("left at provider = %+v, want %+v (hand-made, foreign, other account)", left, wantLeft)
	}

	// Credentials that cannot be decrypted: nothing is touched, everything is named.
	acc.EncCredentials = "garbage"
	orphaned, err = DeleteDNSAccountRecords(context.Background(), st, testClusterSecret, acc, newProv)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"_minecraft._tcp.play.example.com", "lobby.example.com", "mc.down.net", "play.example.com"}
	if !reflect.DeepEqual(orphaned, want) {
		t.Errorf("orphaned = %v, want %v", orphaned, want)
	}
}
