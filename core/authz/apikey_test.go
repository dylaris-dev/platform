package authz

import "testing"

func TestValidKeyCap(t *testing.T) {
	tests := []struct {
		capID string
		want  bool
	}{
		{"rcon.exec", true},    // SERVER
		{"power.start", true},  // SERVER
		{"modpack.read", true}, // OWNER
		{"users.read", false},  // PANEL: rejected
		{"nope.nope", false},   // unknown: rejected
		{"", false},            // empty: rejected
		// Caps that hand out access never ride on a key (G-9).
		{"apikeys.write", false},
		{"apikeys.read", false},
		{"roles.write", false},
		{"members.write", false},
		{"members.delete", false},
		{"members.read", true},
		{"library.read", false}, // removed feature
	}
	for _, tt := range tests {
		if got := ValidKeyCap(tt.capID); got != tt.want {
			t.Errorf("ValidKeyCap(%q) = %v, want %v", tt.capID, got, tt.want)
		}
	}
}

func TestResolveAPIKey_ServerScope(t *testing.T) {
	res := ResolveAPIKey([]string{"rcon.exec"}, true)
	if !res.HasCap("rcon.exec") {
		t.Error("HasCap(rcon.exec) = false, want true when serverAllowed")
	}

	res = ResolveAPIKey([]string{"rcon.exec"}, false)
	if res.HasCap("rcon.exec") {
		t.Error("HasCap(rcon.exec) = true, want false when serverAllowed is false")
	}
}

func TestResolveAPIKey_OwnerScopeIgnoresServerAllowed(t *testing.T) {
	res := ResolveAPIKey([]string{"modpack.read"}, false)
	if !res.HasCap("modpack.read") {
		t.Error("HasCap(modpack.read) = false, want true (owner caps ignore serverAllowed=false)")
	}

	res = ResolveAPIKey([]string{"modpack.read"}, true)
	if !res.HasCap("modpack.read") {
		t.Error("HasCap(modpack.read) = false, want true (owner caps ignore serverAllowed=true)")
	}
}

func TestResolveAPIKey_PanelAndUnknownNeverGranted(t *testing.T) {
	res := ResolveAPIKey([]string{"users.read", "nope.nope"}, true)
	if res.HasCap("users.read") {
		t.Error("HasCap(users.read) = true, want false (PANEL cap must never be granted to a key)")
	}
	if res.HasCap("nope.nope") {
		t.Error("HasCap(nope.nope) = true, want false (unknown cap always denied)")
	}
	if res.HasCap("power.start") {
		t.Error("HasCap(power.start) = true, want false (cap not in the key's list, deny-by-default)")
	}
}

// A key minted before a capability was put out of reach must not keep it.
func TestResolveAPIKey_StripsCapsNoKeyMayHold(t *testing.T) {
	res := ResolveAPIKey([]string{"apikeys.write", "roles.write", "members.write", "console.send"}, true)
	for _, c := range []string{"apikeys.write", "roles.write", "members.write"} {
		if res.HasCap(c) {
			t.Errorf("resolved key holds %s", c)
		}
	}
	if !res.HasCap("console.send") {
		t.Error("console.send lost")
	}
	for _, c := range KeyCaps() {
		if !ValidKeyCap(c) {
			t.Errorf("KeyCaps lists %s, which ValidKeyCap refuses", c)
		}
	}
}
