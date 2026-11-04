package dnsprov

import (
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/libdns/libdns"
)

func TestToLibdnsSRV(t *testing.T) {
	in := Record{Name: "survival", Type: "SRV", Value: "survival.example.com", Port: 25650}
	got, err := toLibdns(in, recordTTL)
	if err != nil {
		t.Fatal(err)
	}
	want := libdns.SRV{Service: "minecraft", Transport: "tcp", Name: "survival", Target: "survival.example.com.",
		Port: 25650, Priority: 0, Weight: 5, TTL: 60 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	back, _ := fromLibdns([]libdns.Record{got})
	if len(back) != 1 || back[0] != in {
		t.Fatalf("round trip = %#v, want %#v", back, in)
	}
	// Providers hand SRVs back as plain RR; that path must round-trip too.
	back, _ = fromLibdns([]libdns.Record{got.RR()})
	if len(back) != 1 || back[0] != in {
		t.Fatalf("RR round trip = %#v, want %#v", back, in)
	}
}

func TestToLibdnsA(t *testing.T) {
	got, err := toLibdns(Record{Name: "play", Type: "A", Value: "203.0.113.7"}, recordTTL)
	if err != nil {
		t.Fatal(err)
	}
	want := libdns.Address{Name: "play", TTL: 60 * time.Second, IP: netip.MustParseAddr("203.0.113.7")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for _, bad := range []string{"", "nope", "2001:db8::1"} {
		if _, err := toLibdns(Record{Name: "play", Type: "A", Value: bad}, recordTTL); err == nil {
			t.Errorf("value %q accepted", bad)
		}
	}
}

func TestFromLibdnsDropsForeignTypes(t *testing.T) {
	in := []libdns.Record{
		libdns.Address{Name: "v6", IP: netip.MustParseAddr("2001:db8::1")},
		libdns.TXT{Name: "x", Text: "hi"},
		libdns.SRV{Service: "sip", Transport: "udp", Name: "x", Target: "a.example.com.", Port: 1},
		libdns.Address{Name: "play", IP: netip.MustParseAddr("203.0.113.7")},
	}
	got, err := fromLibdns(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "play" {
		t.Fatalf("got %#v", got)
	}
}

func TestFromLibdnsReportsCNAMEByName(t *testing.T) {
	in := []libdns.Record{
		libdns.CNAME{Name: "play", Target: "elsewhere.example.net."},
		libdns.RR{Name: "_minecraft._tcp.mc", Type: "CNAME", Data: "x.example.net."},
	}
	got, err := fromLibdns(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{{Name: "play", Type: "CNAME"}, {Name: "_minecraft._tcp.mc", Type: "CNAME"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %#v, want %#v (name only, no target)", got, want)
	}
}

func TestNewRejectsUnknownKind(t *testing.T) {
	if _, err := New("nope", nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestNewRequiresFields(t *testing.T) {
	_, err := New("porkbun", map[string]string{"api_key": "k"})
	if err == nil || !strings.Contains(err.Error(), "Secret API key") {
		t.Fatalf("err = %v, want one naming the missing label", err)
	}
	if _, err := New("inwx", map[string]string{"username": "u", "password": "p"}); err != nil {
		t.Fatalf("optional field must not be required: %v", err)
	}
}

func TestKindsCoverSpecList(t *testing.T) {
	want := []string{"cloudflare", "hetzner", "desec", "porkbun", "inwx", "ionos", "ovh", "netcup", "namecheap", "godaddy", "route53"}
	// inwx is absent: v0.3.1 of its package has no ListZones (verified in source).
	listing := map[string]bool{"cloudflare": true, "hetzner": true, "desec": true, "porkbun": true, "ionos": true}
	ks := Kinds()
	if len(ks) != len(want) {
		t.Fatalf("got %d kinds, want %d", len(ks), len(want))
	}
	for i, k := range ks {
		if k.ID != want[i] {
			t.Errorf("kind %d = %q, want %q", i, k.ID, want[i])
		}
		if k.ZoneListing != listing[k.ID] {
			t.Errorf("%s ZoneListing = %v, want %v", k.ID, k.ZoneListing, listing[k.ID])
		}
		// Build with dummy values (no network): proves the package implements the
		// interfaces and that Kind.ZoneListing matches what it really has.
		creds := map[string]string{}
		for _, f := range k.Fields {
			creds[f.Key] = "x"
		}
		p, err := New(k.ID, creds)
		if err != nil {
			t.Errorf("%s: New: %v", k.ID, err)
			continue
		}
		if has := p.(*adapter).zones != nil; has != k.ZoneListing {
			t.Errorf("%s: package ZoneLister = %v, Kind.ZoneListing = %v", k.ID, has, k.ZoneListing)
		}
		if !listing[k.ID] {
			if _, err := p.ListZones(t.Context()); !errors.Is(err, ErrNoZoneListing) {
				t.Errorf("%s: ListZones err = %v, want ErrNoZoneListing", k.ID, err)
			}
		}
	}
}

func TestScrubProviderErrorRemovesSecrets(t *testing.T) {
	tok := "ab/c+d=e f"
	err := errors.New("GET https://api.example/zones?token=" + url.QueryEscape(tok) + " failed: bad token " + tok + " (path " + url.PathEscape(tok) + ")")
	got := Scrub(err, map[string]string{"api_token": tok, "empty": ""})
	if strings.Contains(got, tok) || strings.Contains(got, url.QueryEscape(tok)) || strings.Contains(got, url.PathEscape(tok)) {
		t.Fatalf("secret leaked: %s", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("no marker: %s", got)
	}
	if Scrub(nil, nil) != "" {
		t.Fatal("nil error must scrub to empty")
	}
}

// A provider that echoes its JSON request body puts the token in JSON-escaped
// form, which none of the plain or URL forms match.
func TestScrubRemovesJSONEscapedSecrets(t *testing.T) {
	tok := `k"y\z<&>/` + "é"
	for _, html := range []bool{true, false} {
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(html)
		if err := enc.Encode(map[string]string{"token": tok}); err != nil {
			t.Fatal(err)
		}
		body := b.String()
		got := Scrub(errors.New("400 bad request: "+body), map[string]string{"api_token": tok})
		inner := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(body), `{"token":"`), `"}`)
		if strings.Contains(got, inner) || !strings.Contains(got, "[redacted]") {
			t.Fatalf("html=%v: escaped secret survived: %s", html, got)
		}
	}
}

func TestClipKeepsUTF8Whole(t *testing.T) {
	short := "fine"
	if Clip(short) != short {
		t.Fatalf("Clip(%q) changed it", short)
	}
	long := strings.Repeat("a", MaxErrorLen-1) + "éé"
	got := Clip(long)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "...") || len(got) > MaxErrorLen+3 {
		t.Fatalf("Clip = %q (len %d)", got[len(got)-8:], len(got))
	}
}

func TestDeleteInputUsesWildcardTTL(t *testing.T) {
	in, err := toLibdnsAll([]Record{
		{Name: "play", Type: "A", Value: "203.0.113.7"},
		{Name: "play", Type: "SRV", Value: "play.example.com", Port: 25650},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range in {
		if ttl := r.RR().TTL; ttl != 0 {
			t.Errorf("%s delete input TTL = %v, want 0", r.RR().Type, ttl)
		}
	}
	if got := in[1].RR().Data; got != "0 5 25650 play.example.com." {
		t.Errorf("SRV value = %q", got)
	}
}

func TestApexName(t *testing.T) {
	for _, name := range []string{"", "@"} {
		got, err := toLibdns(Record{Name: name, Type: "A", Value: "203.0.113.7"}, recordTTL)
		if err != nil {
			t.Fatal(err)
		}
		if n := got.RR().Name; n != "@" {
			t.Errorf("name %q became %q, want @", name, n)
		}
	}
}

func TestFromLibdnsErrorsOnUnparseableOwnedTypes(t *testing.T) {
	bad := []libdns.Record{libdns.RR{Name: "play", Type: "A", Data: "not-an-ip"}}
	if _, err := fromLibdns(bad); err == nil {
		t.Fatal("unparseable A must error")
	}
	bad = []libdns.Record{libdns.RR{Name: "_minecraft._tcp.play", Type: "SRV", Data: "garbage"}}
	if _, err := fromLibdns(bad); err == nil {
		t.Fatal("unparseable minecraft SRV must error")
	}
	if got, err := fromLibdns([]libdns.Record{libdns.RR{Name: "x", Type: "WEIRD", Data: "?"}}); err != nil || len(got) != 0 {
		t.Fatalf("unknown type must be skipped: %v %v", got, err)
	}
}
