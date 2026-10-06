//go:build linux
// +build linux

package linux

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestFromAppPageNeedsTheToken(t *testing.T) {
	if _, ok := fromAppPage(`C{"name":"main.App.DeleteFile"}`); ok {
		t.Fatal("a message without the token was accepted")
	}
	if _, ok := fromAppPage("dylaris-bridge-0000|DomReady"); ok {
		t.Fatal("a message with a guessed token was accepted")
	}
	got, ok := fromAppPage(bridgeToken + "DomReady")
	if !ok || got != "DomReady" {
		t.Fatalf("the app's own message was refused or mangled: %q %v", got, ok)
	}
}

// The script carries the start URL's origin, and BRIDGE_SCRIPT_OUT lets a
// WebKitGTK probe run the exact text (see VENDOR.md).
func TestBridgeGuardScriptNamesTheAppOrigin(t *testing.T) {
	u, _ := url.Parse("wails://wails/")
	s := bridgeGuardScript(u)
	for _, want := range []string{`"wails:"`, `"wails"`, bridgeToken} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %s:\n%s", want, s)
		}
	}
	if out := os.Getenv("BRIDGE_SCRIPT_OUT"); out != "" {
		p, _ := url.Parse("app://main/")
		os.WriteFile(out, []byte(bridgeToken+"\n"+bridgeGuardScript(p)), 0o644)
	}
}
