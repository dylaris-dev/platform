package files

import (
	"strings"
	"testing"
)

// A demo server is readable by ANY authenticated account, so whatever this
// returns is public. It used to return everything except two names, which meant
// the set of protected files was the set somebody had thought of.
//
// These are the files a real Minecraft server actually keeps credentials in.
// None of them was on the old list.
func TestDemoFileContentHidesEverythingItDoesNotKnow(t *testing.T) {
	secret := "database.password: hunter2\ntoken: abcdef\n"
	for _, path := range []string{
		"plugins/LuckPerms/config.yml",
		"plugins/DiscordSRV/config.yml",
		"forwarding.secret",
		"velocity.toml",
		"config.yml",
		"ops.json",
		"whitelist.json",
		"banned-players.json",
		"usercache.json",
		".env",
		"logs/latest.log",
		`plugins\Windows\config.yml`,
	} {
		got := demoFileContent(path, secret)
		if strings.Contains(got, "hunter2") || strings.Contains(got, "abcdef") {
			t.Errorf("demoFileContent(%q) handed the file back verbatim", path)
		}
		if got != demoHiddenNotice {
			t.Errorf("demoFileContent(%q) = %q, want the notice", path, got)
		}
	}
}

// The two the demo exists to show. server.properties is the file a visitor
// opens first, so it comes back - without the one line in it that is a
// credential.
func TestDemoFileContentShowsTheShowcaseFiles(t *testing.T) {
	props := "motd=A Minecraft Server\nrcon.password=hunter2\nrcon.port=25575\n"
	got := demoFileContent("server.properties", props)
	if strings.Contains(got, "hunter2") {
		t.Error("server.properties came back with rcon.password intact")
	}
	if !strings.Contains(got, "rcon.password=REDACTED") {
		t.Errorf("server.properties = %q, want the password line redacted in place", got)
	}
	if !strings.Contains(got, "motd=A Minecraft Server") || !strings.Contains(got, "rcon.port=25575") {
		t.Errorf("server.properties = %q, want every other line unchanged", got)
	}

	// The path is what the file browser passes, so a nested one has to resolve
	// to the same answer as a bare name.
	if got := demoFileContent("some/dir/eula.txt", "eula=true\n"); got != "eula=true\n" {
		t.Errorf("eula.txt = %q, want it unchanged", got)
	}
	if got := demoFileContent("logs/../server.properties", props); strings.Contains(got, "hunter2") {
		t.Error("a path with a .. segment reached the default arm and leaked the password")
	}
}
