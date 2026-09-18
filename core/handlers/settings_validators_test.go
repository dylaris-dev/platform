package handlers

import (
	"testing"
)

// TestNormalizeWarpPorts pins the leader-trusted port-list normalizer
// (settings.go): dedupe + numeric sort + range 1..65535, reject any
// non-numeric or out-of-range entry.
func TestNormalizeWarpPorts(t *testing.T) {
	cases := []struct {
		name    string
		csv     string
		want    string
		wantErr bool
	}{
		{"normal csv sorted as-is", "6379,25501,25551", "6379,25501,25551", false},
		{"out-of-order input gets sorted", "25551,6379,25501", "6379,25501,25551", false},
		{"duplicate entries collapse", "6379,6379,25501", "6379,25501", false},
		{"whitespace around entries trimmed", " 6379 , 25501 ", "6379,25501", false},
		{"empty segments skipped", "6379,,25501", "6379,25501", false},
		{"empty input returns empty string, no error", "", "", false},
		{"only empty segments returns empty string, no error", " , ", "", false},
		{"port 0 rejected (below range)", "0,6379", "", true},
		{"port 65536 rejected (above range)", "6379,65536", "", true},
		{"port exactly 1 accepted (boundary)", "1", "1", false},
		{"port exactly 65535 accepted (boundary)", "65535", "65535", false},
		{"non-numeric entry rejected", "6379,abc", "", true},
		{"negative number rejected", "-1", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeWarpPorts(c.csv)
			if (err != nil) != c.wantErr {
				t.Fatalf("normalizeWarpPorts(%q) err = %v, wantErr %v", c.csv, err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("normalizeWarpPorts(%q) = %q, want %q", c.csv, got, c.want)
			}
		})
	}
}

func TestValidHosterValidation(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"letters", true},
		{"alphanumeric", true},
		{"dns", true},
		{"regex", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.v, func(t *testing.T) {
			if got := validHosterValidation(c.v); got != c.want {
				t.Errorf("validHosterValidation(%q) = %v, want %v", c.v, got, c.want)
			}
		})
	}
}

func TestValidBackupMode(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"s3", true},
		{"node-local", true},
		{"shared", true},
		{"core-storage", true},
		{"local", false},
		{"", false},
		{"core storage", false},
	}
	for _, c := range cases {
		t.Run(c.v, func(t *testing.T) {
			if got := validBackupMode(c.v); got != c.want {
				t.Errorf("validBackupMode(%q) = %v, want %v", c.v, got, c.want)
			}
		})
	}
}

func TestValidRoutingMode(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"ip_port", true},
		{"both", true},
		{"gateway", true},
		{"dns", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.v, func(t *testing.T) {
			if got := validRoutingMode(c.v); got != c.want {
				t.Errorf("validRoutingMode(%q) = %v, want %v", c.v, got, c.want)
			}
		})
	}
}

func TestValidFileMode(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"sftp", true},
		{"both", true},
		{"beam", true},
		{"ftp", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.v, func(t *testing.T) {
			if got := validFileMode(c.v); got != c.want {
				t.Errorf("validFileMode(%q) = %v, want %v", c.v, got, c.want)
			}
		})
	}
}

func TestValidMaintenanceLevel(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"off", true},
		{"banner_only", true},
		{"block_writes", true},
		{"block_all", true},
		{"lockdown", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.v, func(t *testing.T) {
			if got := validMaintenanceLevel(c.v); got != c.want {
				t.Errorf("validMaintenanceLevel(%q) = %v, want %v", c.v, got, c.want)
			}
		})
	}
}

// normalizeTunnelSubnets feeds the deploy snippet the panel hands operators, so
// a value that looks right but means something else is worse than a rejection.
func TestNormalizeTunnelSubnets(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", ""},
		{"10.20.0.0/16", "10.20.0.0/16"},
		{" 10.20.0.0/16 , 10.0.0.0/24 ", "10.20.0.0/16,10.0.0.0/24"},
		{"10.20.0.0/16,10.20.0.0/16", "10.20.0.0/16"}, // deduped
	}
	for _, c := range ok {
		got, err := normalizeTunnelSubnets(c.in)
		if err != nil {
			t.Errorf("normalizeTunnelSubnets(%q) errored: %v", c.in, err)
		} else if got != c.want {
			t.Errorf("normalizeTunnelSubnets(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	bad := []string{
		"10.20.0.0", // no mask
		"not-a-cidr",
		"10.20.0.5/16", // host address: the client routes the whole /16 anyway
		"10.20.0.0/16,junk",
	}
	for _, in := range bad {
		if _, err := normalizeTunnelSubnets(in); err == nil {
			t.Errorf("normalizeTunnelSubnets(%q) should be rejected", in)
		}
	}
}
