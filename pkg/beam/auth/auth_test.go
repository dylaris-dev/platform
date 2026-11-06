package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func sampleClaims() BeamClaims {
	return BeamClaims{
		ServerUUID: "server-abc",
		NodeID:     "node-123",
		Username:   "playerOne",
		IsAdmin:    true,
	}
}

func TestSignValidateBeamTicket_RoundTrip(t *testing.T) {
	secret := "s3cr3t-beam-key"
	c := sampleClaims()

	tok, err := SignBeamTicket(secret, c)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	got, err := ValidateBeamTicket(secret, tok)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got.ServerUUID != c.ServerUUID {
		t.Errorf("ServerUUID = %q, want %q", got.ServerUUID, c.ServerUUID)
	}
	if got.NodeID != c.NodeID {
		t.Errorf("NodeID = %q, want %q", got.NodeID, c.NodeID)
	}
	if got.Username != c.Username {
		t.Errorf("Username = %q, want %q", got.Username, c.Username)
	}
	if got.IsAdmin != c.IsAdmin {
		t.Errorf("IsAdmin = %v, want %v", got.IsAdmin, c.IsAdmin)
	}
	if got.Issuer != BeamIssuer {
		t.Errorf("Issuer = %q, want %q", got.Issuer, BeamIssuer)
	}
	if got.ExpiresAt == nil {
		t.Fatal("expected ExpiresAt to be set")
	}
	if got.IssuedAt == nil {
		t.Fatal("expected IssuedAt to be set")
	}
	wantExp := got.IssuedAt.Add(BeamTicketTTL)
	if diff := got.ExpiresAt.Sub(wantExp); diff < -time.Second || diff > time.Second {
		t.Errorf("ExpiresAt not ~BeamTicketTTL after IssuedAt: diff=%v", diff)
	}
}

func TestSignBeamTicketWithTTL_EmptySecretRejected(t *testing.T) {
	if _, err := SignBeamTicketWithTTL("", sampleClaims(), time.Minute); err == nil {
		t.Fatal("expected error signing with empty secret")
	}
}

func TestValidateBeamTicket_EmptySecretRejected(t *testing.T) {
	tok, err := SignBeamTicket("some-real-secret", sampleClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := ValidateBeamTicket("", tok); err == nil {
		t.Fatal("expected error validating with empty secret")
	}
}

func TestValidateBeamTicket_WrongSecretRejected(t *testing.T) {
	tok, err := SignBeamTicket("secret-A", sampleClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := ValidateBeamTicket("secret-B", tok); err == nil {
		t.Fatal("expected error validating with a different secret")
	}
}

func TestValidateBeamTicket_ExpiredRejected(t *testing.T) {
	secret := "expiry-secret"
	tok, err := SignBeamTicketWithTTL(secret, sampleClaims(), -1*time.Minute)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, err = ValidateBeamTicket(secret, tok)
	if err == nil {
		t.Fatal("expected error validating an already-expired ticket")
	}
	if !errors.Is(err, jwt.ErrTokenExpired) {
		t.Fatalf("expected jwt.ErrTokenExpired in error chain, got %v", err)
	}
}

// TestValidateBeamTicket_AlgConfusionRejected is the key security case: a
// forger who controls the token header (but not the HMAC secret) must not be
// able to swap the alg to something ValidateBeamTicket's keyfunc would treat
// as trivially valid (e.g. "none", or a different HMAC variant). auth.go pins
// jwt.WithValidMethods([]string{"HS256"}), so any other alg must be rejected
// before the keyfunc / secret is ever consulted.
func TestValidateBeamTicket_AlgConfusionRejected(t *testing.T) {
	secret := "alg-confusion-secret"

	forgedClaims := func() BeamClaims {
		c := sampleClaims()
		c.RegisteredClaims = jwt.RegisteredClaims{
			Issuer:    BeamIssuer,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(30 * time.Minute)),
		}
		return c
	}

	tests := []struct {
		name string
		mint func() (string, error)
	}{
		{
			name: "alg none",
			mint: func() (string, error) {
				tok := jwt.NewWithClaims(jwt.SigningMethodNone, forgedClaims())
				return tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
			},
		},
		{
			// Signed with the CORRECT secret, just under a different HMAC
			// variant, to prove the allowlist (not secret mismatch) is what
			// rejects this.
			name: "HS384 with correct secret",
			mint: func() (string, error) {
				tok := jwt.NewWithClaims(jwt.SigningMethodHS384, forgedClaims())
				return tok.SignedString([]byte(secret))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forged, err := tt.mint()
			if err != nil {
				t.Fatalf("mint forged token: %v", err)
			}
			if _, err := ValidateBeamTicket(secret, forged); err == nil {
				t.Fatal("expected forged-alg token to be rejected")
			} else if !errors.Is(err, jwt.ErrTokenSignatureInvalid) {
				t.Fatalf("expected jwt.ErrTokenSignatureInvalid (alg not in allowlist), got %v", err)
			}
		})
	}
}

// The caller may backdate iat to when it decided the ticket's rights; the
// access epoch is compared against iat, so a fresh one taken at signing time
// would pass a stamp written between that decision and the signature.
func TestSignBeamTicketKeepsACallerIssuedAt(t *testing.T) {
	past := time.Now().Add(-10 * time.Second).Truncate(time.Second)
	for _, c := range []struct {
		name string
		iat  *jwt.NumericDate
		want func(got time.Time) bool
	}{
		{"caller sets iat", jwt.NewNumericDate(past), func(got time.Time) bool { return got.Equal(past) }},
		{"caller leaves it empty", nil, func(got time.Time) bool { return got.After(past) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl := sampleClaims()
			cl.IssuedAt = c.iat
			secret := []byte("node-secret")
			tok, err := SignBeamTicketWithNodeProof("s3cret", cl, time.Minute, secret)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ValidateBeamTicketByNodeProof(secret, tok)
			if err != nil {
				t.Fatalf("proof does not cover the iat it was signed with: %v", err)
			}
			if got.IssuedAt == nil || !c.want(got.IssuedAt.Time) {
				t.Errorf("iat = %v, past = %v", got.IssuedAt, past)
			}
			if got.ExpiresAt == nil || got.ExpiresAt.Time.Before(time.Now()) {
				t.Errorf("exp = %v, want measured from now", got.ExpiresAt)
			}
		})
	}
}
