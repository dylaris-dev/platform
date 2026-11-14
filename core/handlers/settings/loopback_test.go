package settings

import "testing"

// loopbackDefault is the production value: package variables are initialised
// before any init function, so this reads it before the switch below.
var loopbackDefault = allowLoopbackHTTP

// The fake provider listens on plain http on 127.0.0.1, so this package's
// tests admit loopback http. Production code never sets it.
func init() { allowLoopbackHTTP = true }

func TestProviderURLLoopbackHTTPRefusedByDefault(t *testing.T) {
	if loopbackDefault {
		t.Fatal("allowLoopbackHTTP must default to false in the production binary")
	}
	allowLoopbackHTTP = loopbackDefault
	t.Cleanup(func() { allowLoopbackHTTP = true })
	for _, u := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		if validProviderURL(u) {
			t.Errorf("%s accepted with the production default", u)
		}
	}
	if !validProviderURL("https://panel.example.com") {
		t.Error("https refused with the production default")
	}
}
