package servers

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestValidEdgeMotdMode pins the accepted edge-MOTD modes. A mode silently
// slipping through would be published to Redis and confuse the edge (which
// treats unknown non-"off"/"custom" values as "auto").
func TestValidEdgeMotdMode(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{"auto", true},
		{"custom", true},
		{"off", true},
		{"private", true},
		{"", false},
		{"AUTO", false},
		{"disabled", false},
		{"on", false},
		{" auto", false},
	}
	for _, tt := range tests {
		if got := validEdgeMotdMode(tt.mode); got != tt.want {
			t.Errorf("validEdgeMotdMode(%q) = %v, want %v", tt.mode, got, tt.want)
		}
	}
}

// TestTruncateEdgeMotdText pins that the cap counts characters, never bytes: a
// byte cut splits a multi-byte rune and the Postgres write fails (C28/C29).
func TestTruncateEdgeMotdText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"short ascii", "hello", "hello"},
		{"empty", "", ""},
		{"umlauts under the char cap but over 256 bytes", strings.Repeat("ä", 200), strings.Repeat("ä", 200)},
		{"exactly the cap in section signs", strings.Repeat("§a", 128), strings.Repeat("§a", 128)},
		{"ascii over the cap", strings.Repeat("a", 300), strings.Repeat("a", 256)},
		{"odd byte boundary would split a rune", "a" + strings.Repeat("ä", 300), "a" + strings.Repeat("ä", 255)},
		{"emoji over the cap", strings.Repeat("😀", 260), strings.Repeat("😀", 256)},
	}
	for _, tt := range tests {
		got := truncateEdgeMotdText(tt.in)
		if got != tt.want {
			t.Errorf("%s: got %d runes / %d bytes, want %d runes / %d bytes", tt.name,
				utf8.RuneCountInString(got), len(got), utf8.RuneCountInString(tt.want), len(tt.want))
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: result is not valid UTF-8", tt.name)
		}
	}
}
