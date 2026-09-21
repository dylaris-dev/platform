package handlers

import (
	"strings"
	"testing"
)

// RCON is handed out as its own capability, so a friend granted rcon.exec holds
// neither network.read nor any other right to the topology - and the node's
// error is a DIAL error, which carries the container's private overlay address.
// Measured on production: a delegate running one command read back
// "dial 10.20.13.16:25575: dial tcp 10.20.13.16:25575: connect: connection
// refused".
func TestRconFailureMessageNeverCarriesTheAddress(t *testing.T) {
	detail := "dial 10.20.13.16:25575: dial tcp 10.20.13.16:25575: connect: connection refused"
	got := rconFailureMessage("srv-uuid", 325, detail)

	for _, leak := range []string{"10.20.13.16", "25575", "dial"} {
		if strings.Contains(got, leak) {
			t.Errorf("the caller was told %q, which contains %q", got, leak)
		}
	}
	if got == "" {
		t.Error("a failure with no message leaves the panel with nothing to show")
	}
}

// The refusal is the one case worth naming, because it has an action attached
// and is exactly what an operator hits right after enabling RCON: Minecraft
// only opens the listener at JVM start.
func TestRconFailureMessageNamesTheActionableCases(t *testing.T) {
	cases := []struct {
		name   string
		detail string
		expect string
	}{
		{"refused points at the restart", "connect: connection refused", "restart"},
		{"a timeout says so", "context deadline exceeded", "in time"},
		{"a bad password points at regenerating it", "rcon: authentication failed", "password"},
		{"anything else is one plain sentence", "some upstream detail nobody should read", "could not be delivered"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rconFailureMessage("srv-uuid", 1, c.detail)
			if !strings.Contains(strings.ToLower(got), strings.ToLower(c.expect)) {
				t.Errorf("message = %q, want it to mention %q", got, c.expect)
			}
			if strings.Contains(got, c.detail) {
				t.Errorf("message = %q repeats the raw detail verbatim", got)
			}
		})
	}
}
