package handlers

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A console line written past the log-shipper went to the browser at whatever
// size it was stored.
func TestConsoleLinesAreCapped(t *testing.T) {
	if got := capConsoleLine("short"); got != "short" {
		t.Fatalf("a short line changed: %q", got)
	}
	long := strings.Repeat("ä", consoleLineMax) // 2 bytes each: the cut lands mid-rune
	got := capConsoleLine(long)
	if len(got) > consoleLineMax+len(" [line truncated]") || !strings.HasSuffix(got, "[line truncated]") {
		t.Fatalf("long line not capped: %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("the cut left an invalid rune")
	}
}
