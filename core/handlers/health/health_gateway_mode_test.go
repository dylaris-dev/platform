package health

import (
	"context"
	"strings"
	"testing"
)

// The gateway-backed components never read Redis unless the mode is local, and
// they say which of the two reasons applies: the managed gateway runs it, or
// the provider setting could not be read.
func TestGatewayComponentsInRemoteAndUnknownMode(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		mode   func() (bool, bool)
		status string
		detail string
	}{
		{"remote", func() (bool, bool) { return true, true }, "disabled", "managed gateway"},
		{"unknown", func() (bool, bool) { return false, false }, "degraded", "could not be read"},
	}
	for _, tt := range tests {
		h := NewHealthHandler(Deps{GatewayRemote: tt.mode})
		for _, c := range []healthComponent{h.gatewayComponent(ctx), h.voiceComponent(ctx), h.bedrockComponent(ctx)} {
			if c.Status != tt.status || !strings.Contains(c.Detail, tt.detail) {
				t.Errorf("%s: %s = %s %q, want %s and a detail containing %q", tt.name, c.Key, c.Status, c.Detail, tt.status, tt.detail)
			}
		}
	}
}
