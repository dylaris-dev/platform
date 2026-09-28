package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"dylaris-pkg/fileperms"
)

// Per-node beam ticket proof.
//
// The ticket JWT is signed with the fleet secret, because the beam-relay reads
// node_id out of it to route the connection and only holds that key. A BYON
// node never receives the fleet secret - the deploy snippet withholds it on
// purpose - so it could not verify that signature, and rejected every ticket:
// beam file access simply did not exist on a customer machine.
//
// So the ticket carries a SECOND authenticator alongside the signature. Core
// derives it from the PER-NODE secret, which it and that one node already
// share and which never crosses the wire. The node checks that instead of the
// signature, and needs no fleet secret at all.
//
// This is additive: the relay and the frozen beam stream header are untouched,
// and a fleet-signed ticket still validates the old way wherever the fleet
// secret exists. Nothing got weaker - the proof is a second lock on the same
// door, and only Core can turn it.

// nodeProofDomain separates this HMAC from every other use of the per-node
// secret (Redis credentials, the challenge response, the LAN certificate). The
// same key deriving two things from unseparated inputs is how one of them
// becomes an oracle for the other.
const nodeProofDomain = "dylaris-beam-node-proof:v1:"

// proofPayload is the exact byte string the proof covers.
//
// Every security-relevant claim is in here. That is the whole design: the node
// does NOT verify the JWT signature, so any claim left out of this string would
// be attacker-editable on an otherwise valid ticket - swap the username for an
// admin's, flip is_admin, point server_uuid at a neighbour, or push exp years
// out. Adding a claim to BeamClaims that decides access means adding it here.
func proofPayload(c BeamClaims) string {
	var exp, iat int64
	if c.ExpiresAt != nil {
		exp = c.ExpiresAt.Unix()
	}
	// iat is covered because the access epoch is compared against it: a
	// ticket that could be re-dated would walk past a revocation stamp the
	// moment one exists.
	if c.IssuedAt != nil {
		iat = c.IssuedAt.Unix()
	}
	return nodeProofDomain +
		c.NodeID + "|" +
		c.ServerUUID + "|" +
		c.Username + "|" +
		strconv.FormatBool(c.IsAdmin) + "|" +
		strconv.FormatInt(exp, 10) + "|" +
		strconv.FormatInt(iat, 10) + "|" +
		permsField(c.Perms)
}

// permsField encodes the permission claim for the proof.
//
// Perms was added to BeamClaims after this payload was written and was left out
// of it, which is the one thing the comment above says must never happen: the
// node authorizes every file operation from claims.Perms alone, and on this
// path nothing checks the signature. A read-only ticket could be edited to
// grant write and delete, the proof still matched, and the node accepted it.
//
// nil and "may do nothing" are DIFFERENT and must stay different in the bytes:
// nil means the ticket came from a Core too old to send permissions, which the
// node reports differently, so collapsing them here would let one be swapped
// for the other.
func permsField(p *fileperms.Perms) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatBool(p.Read) + "," +
		strconv.FormatBool(p.Write) + "," +
		strconv.FormatBool(p.Delete)
}

// NodeProof derives the per-node authenticator for a set of claims.
func NodeProof(nodeSecret []byte, c BeamClaims) string {
	m := hmac.New(sha256.New, nodeSecret)
	m.Write([]byte(proofPayload(c)))
	return hex.EncodeToString(m.Sum(nil))
}

// ValidateBeamTicketByNodeProof reads a ticket using ONLY the per-node secret.
//
// The signature is deliberately not checked - the holder of this key cannot
// check it. Authenticity comes from the proof, expiry and issuer are enforced
// here rather than by the JWT library (which will not validate claims on a
// token it did not verify), and the caller must still enforce
// NodeID == its own id, exactly as with the signature path.
func ValidateBeamTicketByNodeProof(nodeSecret []byte, token string) (*BeamClaims, error) {
	if len(nodeSecret) == 0 {
		return nil, errors.New("auth: empty node secret")
	}
	var c BeamClaims
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"HS256"}))
	if _, _, err := parser.ParseUnverified(token, &c); err != nil {
		return nil, err
	}
	if c.Issuer != BeamIssuer {
		return nil, errors.New("auth: not a beam ticket")
	}
	if c.ExpiresAt == nil {
		return nil, errors.New("auth: ticket has no expiry")
	}
	if time.Now().After(c.ExpiresAt.Time) {
		return nil, errors.New("auth: ticket expired")
	}
	if c.NodeProof == "" {
		return nil, errors.New("auth: ticket carries no node proof")
	}
	want := NodeProof(nodeSecret, c)
	if subtle.ConstantTimeCompare([]byte(c.NodeProof), []byte(want)) != 1 {
		return nil, errors.New("auth: node proof does not verify")
	}
	return &c, nil
}
