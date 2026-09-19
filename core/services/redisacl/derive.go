// Package redisacl provides deterministic per-node Redis (Valkey) credential
// derivation and ACL-rule construction for BYON node isolation.
//
// The derive functions MUST be reproduced byte-identically by the node agent
// (node/redisacl.go, a separate Go module). No cross-module test can enforce
// that directly, so TestGoldenVectors below pins the exact wire format: the
// node side must produce the same hex for the same (secret, token). A drift of
// a single character in either copy silently breaks every node's Redis auth.
package redisacl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// SFTPAuthKeyPrefix is the Redis prefix under which Core publishes the SFTP
// password hashes a given node is allowed to see: "sftp:auth:<token>:".
//
// The hashes used to live at "sftp:auth:<username>" with the node ACL granting
// "%R~sftp:auth:*", so every node held the bcrypt hash of every account on the
// platform - including a tenant's own BYON machine, for users with nothing on it.
// Keying by node lets the grant be "%R~sftp:auth:<token>:*" instead, and lets
// Core publish a hash only to the nodes where that user actually has a server.
//
// CROSS-MODULE: node/redisacl.go carries a byte-identical copy, because the node
// agent is a separate Go module and cannot import this one. A drift in either
// direction makes SFTP authentication fail with "user not found" on every node.
func SFTPAuthKeyPrefix(nodeToken string) string { return "sftp:auth:" + nodeToken + ":" }

// SFTPAuthKey is the full key holding one user's SFTP password hash for one node.
func SFTPAuthKey(nodeToken, username string) string {
	return SFTPAuthKeyPrefix(nodeToken) + username
}

// NodeUsername / ShipperUsername are the ACL usernames for a node.
func NodeUsername(token string) string { return "node-" + token }

// ShipperUsername is the ACL username for ONE Minecraft container's log-shipper,
// on one node.
//
// Per SERVER, not per node. The previous "node-<token>-shipper" was a single
// user granted "~dylaris:server:<u>:*" for every server on the machine - and
// "dylaris:server:<u>:input" is a stdin bridge into the JVM (log-shipper BLPops
// it straight into the process). So the credential handed to one tenant's
// container could read AND write a neighbouring tenant's console on the same
// node. Splitting the user is what makes the per-server key grant mean
// something.
//
// CROSS-MODULE: node/redisacl.go carries a byte-identical copy.
func ShipperUsername(token, serverUUID string) string {
	return "node-" + token + "-shipper-" + serverUUID
}

// NodePassword derives the node agent's Redis password from its per-node secret.
func NodePassword(secret []byte, token string) string {
	return derive(secret, "dylaris-redis-acl:v1:node:"+token)
}

// ShipperPassword derives one container's log-shipper Redis password.
//
// serverUUID is part of the derivation, not only of the username: two containers
// on the same node must not be able to compute each other's credential, or
// splitting the ACL user would buy nothing.
//
// CROSS-MODULE: node/redisacl.go carries a byte-identical copy.
func ShipperPassword(secret []byte, token, serverUUID string) string {
	return derive(secret, "dylaris-redis-acl:v1:shipper:"+token+":"+serverUUID)
}

// LinkUsername is the ACL username for a node's Link sidecar.
func LinkUsername(token string) string { return "node-" + token + "-link" }

// LinkPassword derives the Link sidecar's Redis password from the per-node secret.
func LinkPassword(secret []byte, token string) string {
	return derive(secret, "dylaris-redis-acl:v1:link:"+token)
}

// LinkTunnelToken derives the Link tunnel token (AgentSecret) = SHA256(nodeToken +
// clusterSecret). Used ONLY to scope the Link ACL user's own registration keys.
// MUST stay byte-identical to services.DeriveLinkToken and the gateway Link/Hub
// deriveLinkToken - a drift silently breaks Link's edge auth. Duplicated here (not
// imported) because services already imports redisacl (a back-import would cycle).
func LinkTunnelToken(nodeToken, clusterSecret string) string {
	h := sha256.New()
	h.Write([]byte(nodeToken + clusterSecret))
	return hex.EncodeToString(h.Sum(nil))
}

// Proof is the HMAC the node presents to prove it holds the per-node secret.
func Proof(secret []byte, token string) string {
	return derive(secret, "dylaris-redis-acl:v1:proof:"+token)
}

// VerifyProof constant-time compares a presented proof against the expected one.
func VerifyProof(secret []byte, token, got string) bool {
	return hmac.Equal([]byte(Proof(secret, token)), []byte(got))
}

// ChallengeResponse is the HMAC the node returns for a Core-issued nonce, proving
// possession of the per-node secret without a replayable static value.
func ChallengeResponse(secret []byte, nonce string) string {
	return derive(secret, "dylaris-redis-acl:v1:challenge:"+nonce)
}

// VerifyChallenge constant-time compares a presented challenge response against
// the expected one for the given nonce.
func VerifyChallenge(secret []byte, nonce, got string) bool {
	return hmac.Equal([]byte(ChallengeResponse(secret, nonce)), []byte(got))
}

// HeartbeatSig is the HMAC a node stamps on its Redis discovery heartbeat to
// prove it authored it, replacing the raw-CLUSTER_SECRET compare on the
// hardened path. Keyed by the per-node secret; scoped by the node token + unix
// timestamp so a captured heartbeat cannot be replayed past the freshness window.
func HeartbeatSig(secret []byte, token string, ts int64) string {
	return derive(secret, "dylaris-redis-acl:v1:heartbeat:"+token+":"+strconv.FormatInt(ts, 10))
}

// VerifyHeartbeatSig constant-time compares a presented heartbeat signature.
func VerifyHeartbeatSig(secret []byte, token string, ts int64, got string) bool {
	return hmac.Equal([]byte(HeartbeatSig(secret, token, ts)), []byte(got))
}

// ClusterProof is the HMAC a node presents to prove it holds CLUSTER_SECRET,
// used to gate first-issuance of a known node's per-node secret. Keyed by
// CLUSTER_SECRET (NOT the per-node secret), so a bare-token attacker without
// CLUSTER_SECRET cannot forge it.
func ClusterProof(clusterSecret, token string) string {
	m := hmac.New(sha256.New, []byte(clusterSecret))
	m.Write([]byte("dylaris-node-cluster-proof:v1:" + token))
	return hex.EncodeToString(m.Sum(nil))
}

func derive(secret []byte, domain string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(domain))
	return hex.EncodeToString(m.Sum(nil))
}
