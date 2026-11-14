package remotegw

import (
	"context"
	"errors"
	"testing"
)

func TestClientRefusesNonHTTPSUnlessLoopback(t *testing.T) {
	ctx := context.Background()
	for _, base := range []string{"http://example.com", "ftp://127.0.0.1", "example.com", "",
		"http://127.0.0.1.evil.com", "http://localhost.evil.com", "http://127.0.0.1@evil.com"} {
		if _, err := New(base, func() string { return "k" }).Account(ctx); !errors.Is(err, ErrBaseURL) {
			t.Errorf("%q: err = %v, want ErrBaseURL", base, err)
		}
	}
	for _, base := range []string{"http://127.0.0.1:1", "http://localhost:1", "http://[::1]:1"} {
		// Allowed means the failure is a dial error, not the refusal.
		if _, err := New(base, func() string { return "k" }).Account(ctx); errors.Is(err, ErrBaseURL) {
			t.Errorf("%q must be allowed", base)
		}
	}
	// https is accepted by construction; no dial needed to prove it.
	if c := New("https://panel.example.com/", func() string { return "k" }); c.baseErr != nil {
		t.Errorf("https refused: %v", c.baseErr)
	}
}
