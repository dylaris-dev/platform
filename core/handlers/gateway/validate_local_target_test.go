package gateway

import "testing"

// TestValidateLocalTarget pins validateLocalTarget (gateway_link_routes.go).
// Route-only targets are dialed by the CUSTOMER's own Link on their local
// network, so LAN/loopback/private IPs are intentionally accepted here -
// only empty, malformed, or wildcard hostnames are rejected.
func TestValidateLocalTarget(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{"valid hostname", "my-pc.local", false},
		{"valid IPv4", "203.0.113.5", false},
		{"loopback IPv4 accepted (Link, not Core, decides what it dials)", "127.0.0.1", false},
		{"loopback IPv6 refused: the Link only treats 127.0.0.1 as this machine", "::1", true},
		{"IPv4-mapped loopback refused for the same reason", "::ffff:127.0.0.1", true},
		{"LAN IPv4 accepted", "192.168.1.50", false},
		{"mixed-case hostname is lowercased and accepted", "My-PC.LOCAL", false},
		{"wildcard rejected", "*.example.com", true},
		{"empty rejected", "", true},
		{"whitespace-only rejected", "   ", true},
		{"malformed hostname (space) rejected", "not a host", true},
		{"malformed hostname (invalid chars) rejected", "host_$$_name", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateLocalTarget(c.host)
			if (err != nil) != c.wantErr {
				t.Errorf("validateLocalTarget(%q) err = %v, wantErr %v", c.host, err, c.wantErr)
			}
		})
	}
}
