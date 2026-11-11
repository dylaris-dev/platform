// Package auth mirrors gateway/pkg/beam/auth so platform/core (signer) and
// platform/node (validator) can speak the same Beam-ticket JWT contract
// without a cross-repo Go-module dependency.
//
// Keep this in sync with: gateway/pkg/beam/auth/auth.go (byte-for-byte
// equivalent claim structure + signing parameters). An integration smoke
// test verifies a ticket signed here validates in the gateway relay.
//
// Both validators require the issuer, and that is a contract, not a detail.
// The key is shared with core's panel session signing key (BEAM_JWT_SECRET is
// wired to JWT_SECRET in every compose and stack file, in both repos), so
// "signed with this key" does not mean "is a beam ticket" - the issuer is what
// separates the two token types, and core's AuthMiddleware enforces the mirror
// rule that a token WITH an issuer is not a session.
//
// The two copies used to disagree here: gateway validated laxly, for
// third-party hoster panels that might not set an issuer. Nothing was relying
// on that (both signers have always set it), and it left the relay separating
// the token types on the presence of node_id alone, which is a routing claim.
// Gateway enforces it too as of 2026-09-02.
package auth

import (
	"dylaris-pkg/fileperms"

	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const BeamIssuer = "dylaris-beam"
const BeamTicketTTL = 30 * time.Minute

type BeamClaims struct {
	ServerUUID string `json:"server_uuid"`
	NodeID     string `json:"node_id"`
	Username   string `json:"username"`
	IsAdmin    bool   `json:"is_admin"`
	// NodeProof authenticates this ticket to ONE node using the per-node secret,
	// so a node that holds no fleet secret can still trust it. Empty on a ticket
	// minted for a node whose secret Core could not load. See node_proof.go.
	NodeProof string `json:"node_proof,omitempty"`
	// Kit names the managed-gateway kit a tenant-signed (EdDSA) ticket claims to
	// belong to. Deliberately NOT part of proofPayload: the relay checks it, the
	// node does not use it.
	Kit string `json:"kit,omitempty"`
	// Perms is what this account may do to the server's files, resolved by Core
	// from the same capability catalog the HTTP file API enforces.
	//
	// It rides in the TICKET rather than being looked up on the node, because
	// the two surfaces learn who the caller is differently: SFTP has a username
	// and reads the per-(node, user) key the SFTP sync publishes, while beam has
	// only this ticket - and an administrator, who may hold no SFTP access row
	// at all, appears in no published list. Both values come out of one
	// resolution in Core, so the delivery differing does not make the answer
	// differ.
	//
	// A POINTER, and the nil case is load-bearing: a ticket minted by a Core
	// that predates this field carries no permissions, and the node must be able
	// to tell that apart from a ticket that grants none. It refuses the
	// operation either way, but only one of the two is worth telling an operator
	// to update Core about.
	Perms *fileperms.Perms `json:"perms,omitempty"`
	jwt.RegisteredClaims
}

// SignBeamTicket signs a Beam ticket compatible with gateway/beam-relay.
func SignBeamTicket(secret string, c BeamClaims) (string, error) {
	return SignBeamTicketWithTTL(secret, c, BeamTicketTTL)
}

func SignBeamTicketWithTTL(secret string, c BeamClaims, ttl time.Duration) (string, error) {
	return SignBeamTicketWithNodeProof(secret, c, ttl, nil)
}

// SignBeamTicketWithNodeProof signs the ticket AND, when nodeSecret is
// non-empty, stamps the per-node authenticator into it.
//
// The proof has to be computed here rather than by the caller: it covers the
// expiry, and the expiry is only decided at this point. A caller building it
// beforehand would be signing a promise about a timestamp that did not exist
// yet.
//
// nodeSecret nil/empty produces the old ticket exactly, so a deployment where
// Core cannot load a node's secret keeps working over the fleet-signature path
// instead of minting something nobody can read.
func SignBeamTicketWithNodeProof(secret string, c BeamClaims, ttl time.Duration, nodeSecret []byte) (string, error) {
	if secret == "" {
		return "", errors.New("auth: empty secret")
	}
	// A caller-set iat is kept: it is when the caller decided the ticket's
	// rights, and the node's access epoch is compared against it. Any stamp
	// written after that decision then refuses the ticket, however late it
	// is signed. Never later than now.
	iat := time.Now()
	if c.IssuedAt != nil && c.IssuedAt.Time.Before(iat) {
		iat = c.IssuedAt.Time
	}
	c.RegisteredClaims = jwt.RegisteredClaims{
		Issuer:    BeamIssuer,
		IssuedAt:  jwt.NewNumericDate(iat),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
	}
	// After the expiry is set, before the token is built: the proof covers the
	// claims as they will actually be serialised.
	if len(nodeSecret) > 0 {
		c.NodeProof = NodeProof(nodeSecret, c)
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	return t.SignedString([]byte(secret))
}

// ValidateBeamTicket is the read side — used by beam-relay (gateway) and
// node-side BeamNodeService.Authenticate. Callers must additionally enforce
// NodeID == localNodeID.
//
// The issuer is REQUIRED, not decorative. This secret is the same value as
// core's session signing key (BEAM_JWT_SECRET is wired to JWT_SECRET in both
// compose files), so without checking who issued it, "signed with this key" is
// not the same statement as "is a beam ticket". Core's AuthMiddleware enforces
// the mirror rule - a token WITH an issuer is not a session - and the pair is
// what keeps the two token types apart on one key.
func ValidateBeamTicket(secret, token string) (*BeamClaims, error) {
	if secret == "" {
		return nil, errors.New("auth: empty secret")
	}
	var c BeamClaims
	tk, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired(),
		jwt.WithIssuer(BeamIssuer))
	if err != nil {
		return nil, err
	}
	if !tk.Valid {
		return nil, errors.New("auth: invalid token")
	}
	return &c, nil
}
