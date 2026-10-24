package tabs

import "testing"

func TestValidateProxiedTab(t *testing.T) {
	tests := []struct {
		name       string
		port       int
		path       string
		surface    string
		visibility string
		wantErr    bool
	}{
		{"valid tab private", 8100, "/", "tab", "private", false},
		{"valid page public", 25565, "/map", "page", "public", false},
		{"valid both", 1, "/x/y", "both", "private", false},
		{"port zero", 0, "/", "tab", "private", true},
		{"port too high", 65536, "/", "tab", "private", true},
		{"port negative", -1, "/", "tab", "private", true},
		{"path missing leading slash", 8100, "map", "tab", "private", true},
		{"path empty", 8100, "", "tab", "private", true},
		{"bad surface", 8100, "/", "sidebar", "private", true},
		{"bad visibility", 8100, "/", "tab", "world", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProxiedTab(tt.port, tt.path, tt.surface, tt.visibility)
			if tt.wantErr && err == nil {
				t.Fatalf("validateProxiedTab(%d,%q,%q,%q) = nil, want error", tt.port, tt.path, tt.surface, tt.visibility)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateProxiedTab(%d,%q,%q,%q) = %v, want nil", tt.port, tt.path, tt.surface, tt.visibility, err)
			}
		})
	}
}

func TestValidateProxiedTabPatch(t *testing.T) {
	tests := []struct {
		name       string
		port       int
		path       string
		mode       string
		surface    string
		visibility string
		wantErr    bool
	}{
		{"valid partial patch (only name/icon-style fields set, no proxied fields)", 0, "", "", "", "", false},
		{"valid partial patch, port only", 8100, "", "", "", "", false},
		{"out of range port", 70000, "", "", "", "", true},
		{"negative port", -1, "", "", "", "", true},
		{"path set without leading slash", 0, "map", "", "", "", true},
		{"invalid mode", 0, "", "sideways", "", "", true},
		{"invalid surface", 0, "", "", "sidebar", "", true},
		{"invalid visibility", 0, "", "", "", "world", true},
		{"valid full proxied patch", 25565, "/map", "proxied", "page", "public", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProxiedTabPatch(tt.port, tt.path, tt.mode, tt.surface, tt.visibility)
			if tt.wantErr && err == nil {
				t.Fatalf("validateProxiedTabPatch(%d,%q,%q,%q,%q) = nil, want error", tt.port, tt.path, tt.mode, tt.surface, tt.visibility)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateProxiedTabPatch(%d,%q,%q,%q,%q) = %v, want nil", tt.port, tt.path, tt.mode, tt.surface, tt.visibility, err)
			}
		})
	}
}
