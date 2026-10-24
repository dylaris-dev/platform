package beam

import "testing"

func TestBuildBeamDirectHints(t *testing.T) {
	const (
		fp   = "abc123"
		port = "25523"
	)
	tests := []struct {
		name        string
		relayAddr   string
		lanIPs      []string
		publicIP    string
		fingerprint string
		wantNil     bool
		wantIPs     []string
		wantPublic  string
	}{
		{
			name:        "relay present emits lan omits public",
			relayAddr:   "beam.example.com:25550",
			lanIPs:      []string{"10.0.0.5", "192.168.1.9"},
			publicIP:    "203.0.113.7",
			fingerprint: fp,
			wantIPs:     []string{"10.0.0.5", "192.168.1.9"},
			wantPublic:  "",
		},
		{
			name:        "relay present with only public is nil",
			relayAddr:   "beam.example.com:25550",
			publicIP:    "203.0.113.7",
			fingerprint: fp,
			wantNil:     true,
		},
		{
			name:        "relay present without fingerprint is nil",
			relayAddr:   "beam.example.com:25550",
			lanIPs:      []string{"10.0.0.5"},
			fingerprint: "",
			wantNil:     true,
		},
		{
			name:        "no relay exposes lan and public",
			lanIPs:      []string{"10.0.0.5", "192.168.1.9"},
			publicIP:    "203.0.113.7",
			fingerprint: fp,
			wantIPs:     []string{"10.0.0.5", "192.168.1.9"},
			wantPublic:  "203.0.113.7:25523",
		},
		{
			name:        "no relay public only",
			publicIP:    "203.0.113.7",
			fingerprint: fp,
			wantPublic:  "203.0.113.7:25523",
		},
		{
			name:        "no relay lan only",
			lanIPs:      []string{"10.0.0.5"},
			fingerprint: fp,
			wantIPs:     []string{"10.0.0.5"},
		},
		{
			name:        "missing fingerprint refuses direct path",
			lanIPs:      []string{"10.0.0.5"},
			publicIP:    "203.0.113.7",
			fingerprint: "",
			wantNil:     true,
		},
		{
			name:        "no relay and nothing dialable",
			fingerprint: fp,
			wantNil:     true,
		},
		{
			name:        "ipv6 public is bracketed",
			publicIP:    "2001:db8::1",
			fingerprint: fp,
			wantPublic:  "[2001:db8::1]:25523",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildBeamDirectHints(tt.relayAddr, tt.lanIPs, tt.publicIP, port, tt.fingerprint)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil hints, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected hints, got nil")
			}
			if got.Fingerprint != fp {
				t.Errorf("fingerprint = %q, want %q", got.Fingerprint, fp)
			}
			if got.Port != port {
				t.Errorf("port = %q, want %q", got.Port, port)
			}
			if got.PublicAddr != tt.wantPublic {
				t.Errorf("publicAddr = %q, want %q", got.PublicAddr, tt.wantPublic)
			}
			if len(got.IPs) != len(tt.wantIPs) {
				t.Fatalf("IPs = %v, want %v", got.IPs, tt.wantIPs)
			}
			for i := range got.IPs {
				if got.IPs[i] != tt.wantIPs[i] {
					t.Errorf("IPs[%d] = %q, want %q", i, got.IPs[i], tt.wantIPs[i])
				}
			}
		})
	}
}
