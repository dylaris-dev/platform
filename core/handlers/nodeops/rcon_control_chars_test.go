package nodeops

import (
	"net/http"
	"testing"
)

// RCON took any characters; the console has refused control characters for
// a while.
func TestAnRCONCommandWithControlCharactersIsRefused(t *testing.T) {
	resp := ExecRcon(t.Context(), nil, nil, 1, "u", 1, RconRequest{Command: "say hi\x00\x1b[2J"})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status %d (%s), want 400", resp.Status, resp.Error)
	}
}
