package servers

import "testing"

// The setup path took the requested image raw while reinstall fell back to the
// stored one. A setup call without an image therefore recreated the container
// with no image at all - unstartable, and destructive, because the previous
// sub-server's container is removed before the new one is built.
func TestResolveJavaImage(t *testing.T) {
	const stored = "ghcr.io/dylaris-dev/platform-mc-java21:latest"
	const requested = "ghcr.io/dylaris-dev/platform-mc-java25:latest"

	tests := []struct {
		name      string
		requested string
		stored    string
		want      string
	}{
		{"requested wins", requested, stored, requested},
		{"empty request falls back to stored", "", stored, stored},
		{"blank request falls back to stored", "   ", stored, stored},
		{"request is trimmed", "  " + requested + " ", stored, requested},
		{"nothing to fall back to", "", "", ""},
		{"blank stored is not an image", "", "  ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveJavaImage(tt.requested, tt.stored); got != tt.want {
				t.Errorf("resolveJavaImage(%q, %q) = %q, want %q", tt.requested, tt.stored, got, tt.want)
			}
		})
	}
}
