package tabs

import (
	"strings"
	"testing"
)

func TestGenerateShareToken(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		tok, err := generateShareToken()
		if err != nil {
			t.Fatalf("generateShareToken error: %v", err)
		}
		if len(tok) != shareTokenLen {
			t.Fatalf("token %q len = %d, want %d", tok, len(tok), shareTokenLen)
		}
		for _, c := range tok {
			if !strings.ContainsRune(shareTokenAlphabet, c) {
				t.Fatalf("token %q has out-of-alphabet rune %q", tok, c)
			}
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q within 500 draws (entropy too low)", tok)
		}
		seen[tok] = true
	}
}

func TestCapReached(t *testing.T) {
	cases := []struct {
		current int
		max     int
		want    bool
	}{
		{0, 10, false},
		{9, 10, false},
		{10, 10, true},
		{11, 10, true},
	}
	for _, c := range cases {
		if got := capReached(c.current, c.max); got != c.want {
			t.Errorf("capReached(%d,%d) = %v, want %v", c.current, c.max, got, c.want)
		}
	}
}
