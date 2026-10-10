package handlers

import "testing"

// A command Minecraft refused used to answer {success:true}: the panel said
// "banned" for a name that does not exist. The strings are vanilla en_us lang
// entries (1.12 and 1.13-1.21).
func TestClassifyPlayerCommandOutput(t *testing.T) {
	cases := []struct {
		name       string
		output     string
		wantFailed bool
		wantClean  string
	}{
		{"unknown player 1.13+", "That player does not exist", true, "That player does not exist"},
		{"colour coded", "§cThat player does not exist§r", true, "That player does not exist"},
		{"essentialsx refusal", "§4Error: §cPlayer not found.", true, "Error: Player not found."},
		{"no player found", "No player was found", true, "No player was found"},
		{"unknown command 1.13+", "Unknown or incomplete command, see below for error", true, "Unknown or incomplete command, see below for error"},
		{"unknown command 1.13.0", "Unknown command", true, "Unknown command"},
		{"incorrect argument", "Incorrect argument for command", true, "Incorrect argument for command"},
		{"1.12 player not found", "§cPlayer 'Ghost' cannot be found", true, "Player 'Ghost' cannot be found"},
		{"1.12 could not ban", "Could not ban player Ghost", true, "Could not ban player Ghost"},
		{"1.12 usage", "Usage: /kick <player> [reason ...]", true, "Usage: /kick <player> [reason ...]"},
		{"case-insensitive", "THAT PLAYER DOES NOT EXIST", true, "THAT PLAYER DOES NOT EXIST"},
		{"surrounding space", "  No player was found\n", true, "No player was found"},
		{"kick succeeded", "Kicked Notch: afk", false, "Kicked Notch: afk"},
		{"kick reason quoting a refusal", "Kicked Notch: No player was found", false, "Kicked Notch: No player was found"},
		{"whisper", "You whisper to Notch: unknown command", false, "You whisper to Notch: unknown command"},
		{"op no-op", "Nothing changed. The player already is an operator", false, "Nothing changed. The player already is an operator"},
		{"ban no-op", "Nothing changed. The player is already banned", false, "Nothing changed. The player is already banned"},
		{"pardon no-op", "Nothing changed. The player isn't banned", false, "Nothing changed. The player isn't banned"},
		{"whitelist add no-op", "Player is already whitelisted", false, "Player is already whitelisted"},
		{"whitelist remove no-op", "Player is not whitelisted", false, "Player is not whitelisted"},
		{"whitelist on no-op", "Whitelist is already turned on", false, "Whitelist is already turned on"},
		{"empty", "", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			failed, clean := classifyPlayerCommandOutput(c.output)
			if failed != c.wantFailed || clean != c.wantClean {
				t.Fatalf("classify(%q) = %v, %q; want %v, %q", c.output, failed, clean, c.wantFailed, c.wantClean)
			}
		})
	}
}
