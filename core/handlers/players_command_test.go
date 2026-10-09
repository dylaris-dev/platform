package handlers

import "testing"

// buildPlayerCommand is the security boundary of POST /players/action:
// everything a players.manage holder can make the server do is decided here.
// The point of the endpoint is that the caller names an action and never a
// command, so the two things worth pinning are that the verb set is closed and
// that nothing in the arguments can widen the command line.
func TestBuildPlayerCommand(t *testing.T) {
	cases := []struct {
		name    string
		action  string
		player  string
		reason  string
		message string
		want    string
		wantErr bool
	}{
		{name: "kick", action: "kick", player: "Notch", want: "kick Notch"},
		{name: "kick with a reason", action: "kick", player: "Notch", reason: "afk too long", want: "kick Notch afk too long"},
		{name: "ban", action: "ban", player: "Herobrine", want: "ban Herobrine"},
		{name: "ban with a reason", action: "ban", player: "Herobrine", reason: "griefing", want: "ban Herobrine griefing"},
		{name: "unban is pardon", action: "unban", player: "Herobrine", want: "pardon Herobrine"},
		{name: "op", action: "op", player: "Notch", want: "op Notch"},
		{name: "deop", action: "deop", player: "Notch", want: "deop Notch"},
		{name: "whitelist add", action: "whitelist_add", player: "Notch", want: "whitelist add Notch"},
		{name: "whitelist remove", action: "whitelist_remove", player: "Notch", want: "whitelist remove Notch"},
		{name: "tell", action: "tell", player: "Notch", message: "hello there", want: "tell Notch hello there"},
		{name: "action names are case-insensitive", action: "KICK", player: "Notch", want: "kick Notch"},

		// The whitelist switch takes no player, and nothing passed as one may
		// reach the command line.
		{name: "whitelist on", action: "whitelist_on", want: "whitelist on"},
		{name: "whitelist off", action: "whitelist_off", want: "whitelist off"},
		{name: "a player on whitelist_on is dropped", action: "whitelist_on", player: "Notch; stop", reason: "x", want: "whitelist on"},
		{name: "enforcement is not an action", action: "whitelist_enforce", wantErr: true},

		// A reason belongs to kick and ban; anywhere else it must not reach the
		// command line, or "op Notch" quietly becomes "op Notch <anything>".
		{name: "a reason on op is dropped", action: "op", player: "Notch", reason: "because", want: "op Notch"},
		{name: "a message on ban is dropped", action: "ban", player: "Notch", message: "because", want: "ban Notch"},

		{name: "tell without a message", action: "tell", player: "Notch", wantErr: true},
		{name: "an unknown action", action: "stop", player: "Notch", wantErr: true},
		{name: "an empty action", action: "", player: "Notch", wantErr: true},
		// The whole reason the verb set is a map and not a passthrough.
		{name: "a command is not an action", action: "kick Notch; stop", player: "Notch", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildPlayerCommand(c.action, c.player, c.reason, c.message)
			if c.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want an error (got command %q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if got != c.want {
				t.Errorf("command = %q, want %q", got, c.want)
			}
		})
	}
}

// RCON has no quoting: whatever goes in becomes the command line verbatim, so
// a name with a space in it silently changes what the command MEANS ("ban Foo
// Bar" bans Foo for reason "Bar") and a control character can break the line.
// Checked rather than escaped, because there is nothing to escape with.
func TestIsPlayerName(t *testing.T) {
	ok := []string{"Notch", "abc", "a_b_c", "Player123", "____", ".BedrockGuy", "*BedrockGuy", "ABCDEFGHIJKLMNOP"}
	for _, s := range ok {
		if !isPlayerName(s) {
			t.Errorf("isPlayerName(%q) = false, want true", s)
		}
	}
	bad := []string{
		"", " ", "Notch Bar", "Notch\nstop", "Notch;stop", "Notch\rx",
		"a\tb", "Notch!", "Notch-Bar", "much-too-long-a-name-for-anyone", ".", "*",
	}
	for _, s := range bad {
		if isPlayerName(s) {
			t.Errorf("isPlayerName(%q) = true, want false", s)
		}
	}
}

// A ban reason is prose, so spaces stay; control characters never do.
func TestSanitizePlayerFreeText(t *testing.T) {
	if got := sanitizePlayerFreeText("  griefing the spawn  "); got != "griefing the spawn" {
		t.Errorf("got %q", got)
	}
	if got := sanitizePlayerFreeText("line\nbreak"); got != "linebreak" {
		t.Errorf("a newline survived: %q", got)
	}
	if got := sanitizePlayerFreeText(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	long := ""
	for i := 0; i < 400; i++ {
		long += "x"
	}
	if got := sanitizePlayerFreeText(long); len(got) > maxPlayerFreeText {
		t.Errorf("length %d exceeds the cap %d", len(got), maxPlayerFreeText)
	}
}

// The panel shows the whitelist as on only when server.properties says so -
// a populated whitelist.json with white-list=false is the state an owner
// misread live as "the whitelist does not work".
func TestWhitelistFlags(t *testing.T) {
	cases := []struct {
		name              string
		props             string
		enabled, enforced bool
	}{
		{name: "missing file", props: ""},
		{name: "both off", props: "white-list=false\nenforce-whitelist=false\n"},
		{name: "enabled only", props: "motd=hi\nwhite-list=true\nenforce-whitelist=false\n", enabled: true},
		{name: "both on, CRLF, padded, upper case", props: "white-list = TRUE\r\nenforce-whitelist=true\r\n", enabled: true, enforced: true},
		{name: "commented out is not set", props: "#white-list=true\n!enforce-whitelist=true\n"},
		{name: "anything but true is false", props: "white-list=yes\nenforce-whitelist=1\n"},
		{name: "a similar key is not the key", props: "white-list-extra=true\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, f := whitelistFlags(c.props)
			if e != c.enabled || f != c.enforced {
				t.Errorf("whitelistFlags = (%v, %v), want (%v, %v)", e, f, c.enabled, c.enforced)
			}
		})
	}
}
