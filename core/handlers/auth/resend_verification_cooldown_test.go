package auth

import (
	"os"
	"regexp"
	"testing"
	"time"
)

// ResendVerification sends a mail on every accepted request and replaces the
// verification token while doing it. Unthrottled, a loop against one address
// fills that inbox, bills the mail provider, and keeps invalidating the link
// the user is trying to click.
func TestVerificationResendAllowed(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }

	tests := []struct {
		name       string
		lastSentAt *time.Time
		want       bool
	}{
		{"never sent before", nil, true},
		{"sent a moment ago", ago(time.Second), false},
		{"sent just inside the window", ago(resendVerificationCooldown - time.Millisecond), false},
		{"sent exactly at the window", ago(resendVerificationCooldown), true},
		{"sent long ago", ago(time.Hour), true},
		// A row stamped by a host running ahead: refusing is the safe
		// direction, since the user can simply ask again.
		{"stamped in the future", func() *time.Time { t := now.Add(time.Minute); return &t }(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verificationResendAllowed(tt.lastSentAt, now, resendVerificationCooldown); got != tt.want {
				t.Errorf("verificationResendAllowed = %v, want %v", got, tt.want)
			}
		})
	}
}

// The per-address cooldown only covers the mail. The per-IP limiter is what
// bounds the request rate, and it was missing on every public auth POST that
// did not take a password - including the one that sends mail, while its
// sibling /auth/forgot-password had one.
//
// Reads routes.go rather than standing up the router: the handlers package has
// no store fake, and the point is the wiring, which is what drifts.
func TestPublicAuthPostRoutesAreRateLimited(t *testing.T) {
	src, err := os.ReadFile("../../routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	text := string(src)

	// Every unauthenticated POST under /auth/. Authenticated ones (profile,
	// 2fa/*) sit behind AuthMiddleware and are not part of this surface.
	public := []string{
		"/auth/login",
		"/auth/register",
		"/auth/demo-login",
		"/auth/verify-email",
		"/auth/resend-verification",
		"/auth/forgot-password",
		"/auth/validate-reset-token",
		"/auth/reset-password",
	}
	for _, route := range public {
		t.Run(route, func(t *testing.T) {
			line := regexp.MustCompile(`(?m)^.*HandleFunc\("` + regexp.QuoteMeta(route) + `".*$`)
			m := line.FindString(text)
			if m == "" {
				t.Fatalf("route %s not found in routes.go", route)
			}
			if !regexp.MustCompile(`authLimiter\.Limit\(`).MatchString(m) {
				t.Fatalf("public auth route %s has no rate limiter: %s", route, m)
			}
		})
	}
}
