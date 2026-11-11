package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The node verifies by NodeProof and never looks at the signature, so an EdDSA
// tenant ticket must pass it unchanged. This pins "node_proof.go needs no change".
func TestNodeProofAcceptsEdDSATicket(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("per-node-secret")
	c := sampleClaims()
	c.Kit = "kit-a"
	c.IssuedAt = jwt.NewNumericDate(time.Now())
	c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(BeamTicketTTL))
	c.NodeProof = NodeProof(secret, c)
	tok, err := SignTenantTicket(priv, "k1", c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ValidateBeamTicketByNodeProof(secret, tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeID != c.NodeID || got.Kit != "kit-a" {
		t.Fatalf("unexpected claims %+v", got)
	}
}

func TestSignTenantTicketRejectsBadInput(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := SignTenantTicket(priv, "", sampleClaims()); err == nil {
		t.Error("empty kid accepted")
	}
	if _, err := SignTenantTicket(nil, "k1", sampleClaims()); err == nil {
		t.Error("nil key accepted")
	}
}
