package handlers

import (
	"net/http"
	"testing"
)

// RCON took any characters; the console has refused control characters for
// a while.
func TestAnRCONCommandWithControlCharactersIsRefused(t *testing.T) {
	h := &RconHandler{state: &AppState{}}
	resp := h.execAgainstServer(t.Context(), 1, "u", 1, rconRequest{Command: "say hi\x00\x1b[2J"})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("status %d (%s), want 400", resp.status, resp.Error)
	}
}
