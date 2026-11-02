package servers

import (
	"strings"
	"testing"
)

const hexCharset = "0123456789abcdef"

func isHexString(s string) bool {
	if len(s) == 0 {
		return true
	}
	for _, c := range s {
		if !strings.ContainsRune(hexCharset, c) {
			return false
		}
	}
	return true
}

// TestGenerateRconPassword pins the output shape (hex length == nBytes*2,
// hex-only charset, two calls differ) without asserting exact bytes (random).
func TestGenerateRconPassword(t *testing.T) {
	t.Run("length is nBytes*2 hex chars", func(t *testing.T) {
		for _, n := range []int{1, 4, 24} {
			got, err := generateRconPassword(n)
			if err != nil {
				t.Fatalf("generateRconPassword(%d) error: %v", n, err)
			}
			if len(got) != n*2 {
				t.Errorf("generateRconPassword(%d) len = %d, want %d", n, len(got), n*2)
			}
			if !isHexString(got) {
				t.Errorf("generateRconPassword(%d) = %q, contains non-hex characters", n, got)
			}
		}
	})

	t.Run("two calls produce different values", func(t *testing.T) {
		a, err := generateRconPassword(16)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		b, err := generateRconPassword(16)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a == b {
			t.Errorf("two independent generateRconPassword(16) calls collided: %q", a)
		}
	})

	t.Run("nBytes==0 returns empty string, no error", func(t *testing.T) {
		got, err := generateRconPassword(0)
		if err != nil {
			t.Fatalf("generateRconPassword(0) error: %v", err)
		}
		if got != "" {
			t.Errorf("generateRconPassword(0) = %q, want empty string", got)
		}
	})

	// SMELL (not fixed - test-only task, this is documentation of real
	// behavior): generateRconPassword does not guard against a negative
	// nBytes. make([]byte, nBytes) with nBytes<0 panics at runtime
	// ("makeslice: len out of range") instead of returning an error. Every
	// current call site passes a hardcoded positive constant (rcon.go:245
	// passes 24), so this is unreachable today, but the function has no
	// defensive check if it's ever reused with a caller-influenced value.
	t.Run("nBytes<0 panics (documents current unguarded behavior)", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("expected generateRconPassword(-1) to panic (make with negative len), it did not")
			}
		}()
		_, _ = generateRconPassword(-1)
	})
}
