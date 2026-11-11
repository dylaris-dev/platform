package auth

import (
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// SignTenantTicket signs a beam ticket EdDSA with a managed-gateway customer's
// own key; kid names that key in the JWT header so the relay can find the
// public half. Mirror of gateway/pkg/beam/auth/tenant.go (validator).
//
// iss is always stamped. iat/exp are kept if the caller set them and defaulted
// otherwise: a caller that also wants a NodeProof must set both BEFORE calling
// NodeProof(), because the proof covers them.
func SignTenantTicket(priv ed25519.PrivateKey, kid string, c BeamClaims) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("auth: bad ed25519 private key")
	}
	if kid == "" {
		return "", errors.New("auth: empty kid")
	}
	now := time.Now()
	c.Issuer = BeamIssuer
	if c.IssuedAt == nil {
		c.IssuedAt = jwt.NewNumericDate(now)
	}
	if c.ExpiresAt == nil {
		c.ExpiresAt = jwt.NewNumericDate(now.Add(BeamTicketTTL))
	}
	t := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	t.Header["kid"] = kid
	return t.SignedString(priv)
}
