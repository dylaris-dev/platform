package services

import "testing"

// TestHashAPIKey pins HashAPIKey against independently-known SHA-256 hex
// vectors (computed via crypto/sha256 + encoding/hex directly, not by
// re-invoking HashAPIKey itself). This is the canonical key hash used by
// api-key create + external auth + warp mint - a silent change here (e.g.
// swapping algorithms or adding a pepper) would lock out every existing key.
func TestHashAPIKey(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"empty string", "", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"known vector abc", "abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"known vector hello", "hello", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HashAPIKey(c.input); got != c.want {
				t.Errorf("HashAPIKey(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}

	t.Run("deterministic - same input same output", func(t *testing.T) {
		a := HashAPIKey("dyl_sometoken")
		b := HashAPIKey("dyl_sometoken")
		if a != b {
			t.Errorf("HashAPIKey not deterministic: %q != %q", a, b)
		}
	})

	t.Run("different input different output", func(t *testing.T) {
		a := HashAPIKey("dyl_token_a")
		b := HashAPIKey("dyl_token_b")
		if a == b {
			t.Errorf("HashAPIKey collided for different inputs: both %q", a)
		}
	})
}
