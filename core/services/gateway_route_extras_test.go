package services

import (
	"testing"

	"dylaris-core/services/remotegw"
)

func TestVoiceDomainFor(t *testing.T) {
	regions := []remotegw.Region{
		{Region: "eu", Wildcard: "*.eu.example.com", Online: true},
		{Region: "us", Wildcard: "*.us.example.com", Online: false},
	}
	for _, c := range []struct {
		name    string
		domains []string
		domain  string
		region  string
		ok      bool
	}{
		{"one label under an online wildcard", []string{"a.eu.example.com"}, "a.eu.example.com", "eu", true},
		{"two labels", []string{"x.a.eu.example.com"}, "", "", false},
		{"the wildcard's own base", []string{"eu.example.com"}, "", "", false},
		{"offline region", []string{"a.us.example.com"}, "", "", false},
		{"own domain", []string{"play.acme.tld"}, "", "", false},
		{"lexically first of several", []string{"z.eu.example.com", "play.acme.tld", "b.eu.example.com", "x.a.eu.example.com"}, "b.eu.example.com", "eu", true},
		{"none", nil, "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, r, ok := VoiceDomainFor(c.domains, regions)
			if d != c.domain || r != c.region || ok != c.ok {
				t.Fatalf("VoiceDomainFor(%v) = %q %q %v, want %q %q %v", c.domains, d, r, ok, c.domain, c.region, c.ok)
			}
		})
	}
	// The caller's slice is not reordered.
	in := []string{"z.eu.example.com", "b.eu.example.com"}
	VoiceDomainFor(in, regions)
	if in[0] != "z.eu.example.com" {
		t.Fatalf("input reordered: %v", in)
	}
}
