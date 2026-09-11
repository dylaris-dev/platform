package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// These derivation functions MUST stay byte-identical to the Core side
// (core/services/redisacl/derive.go). TestACLGoldenVectors pins the wire format;
// a drift here silently breaks this node's Redis auth.
// sftpAuthKey is where Core publishes one user's SFTP password hash FOR THIS
// NODE: "sftp:auth:<nodeToken>:<username>".
//
// CROSS-MODULE: byte-identical to redisacl.SFTPAuthKey in core (a separate Go
// module, so it cannot be imported). The hashes used to sit at
// "sftp:auth:<username>" with every node granted "%R~sftp:auth:*", which handed
// each node - a tenant's own BYON machine included - the bcrypt hash of every
// account on the platform. A drift between the two copies makes every SFTP login
// fail with "user not found".
func sftpAuthKey(nodeToken, username string) string {
	return "sftp:auth:" + nodeToken + ":" + username
}

func aclNodeUsername(token string) string { return "node-" + token }

// aclShipperUsername / aclShipperPassword are per SERVER, not per node.
//
// CROSS-MODULE: byte-identical to redisacl.ShipperUsername / ShipperPassword in
// core. One user per node used to be granted every server's keys on the machine,
// and dylaris:server:<u>:input is a stdin bridge into the JVM, so the credential
// baked into one tenant's container could read and write a neighbour's console.
// The server uuid is in the PASSWORD derivation too, not just the name, or two
// containers on this node could still compute each other's credential.
func aclShipperUsername(token, serverUUID string) string {
	return "node-" + token + "-shipper-" + serverUUID
}

func aclNodePassword(secret []byte, token string) string {
	return aclDerive(secret, "dylaris-redis-acl:v1:node:"+token)
}
func aclShipperPassword(secret []byte, token, serverUUID string) string {
	return aclDerive(secret, "dylaris-redis-acl:v1:shipper:"+token+":"+serverUUID)
}
func aclLinkUsername(token string) string { return "node-" + token + "-link" }
func aclLinkPassword(secret []byte, token string) string {
	return aclDerive(secret, "dylaris-redis-acl:v1:link:"+token)
}
func aclProof(secret []byte, token string) string {
	return aclDerive(secret, "dylaris-redis-acl:v1:proof:"+token)
}

// aclChallengeResponse mirrors core redisacl.ChallengeResponse: HMAC(secret,
// "dylaris-redis-acl:v1:challenge:"+nonce). Byte-identical to the Core side.
func aclChallengeResponse(secret []byte, nonce string) string {
	return aclDerive(secret, "dylaris-redis-acl:v1:challenge:"+nonce)
}

// aclHeartbeatSig mirrors core redisacl.HeartbeatSig: HMAC(perNodeSecret,
// "dylaris-redis-acl:v1:heartbeat:"+token+":"+ts). Byte-identical to Core.
func aclHeartbeatSig(secret []byte, token string, ts int64) string {
	return aclDerive(secret, "dylaris-redis-acl:v1:heartbeat:"+token+":"+strconv.FormatInt(ts, 10))
}

// aclClusterProof mirrors core redisacl.ClusterProof: HMAC(CLUSTER_SECRET, token).
func aclClusterProof(clusterSecret, token string) string {
	m := hmac.New(sha256.New, []byte(clusterSecret))
	m.Write([]byte("dylaris-node-cluster-proof:v1:" + token))
	return hex.EncodeToString(m.Sum(nil))
}

func aclDerive(secret []byte, domain string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(domain))
	return hex.EncodeToString(m.Sum(nil))
}

// loadNodeSecret reads the cached 32-byte secret (hex) from <workdir>/.node_secret.
// Returns ok=false when the file is missing or malformed.
func loadNodeSecret(workdir string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(workdir, ".node_secret"))
	if err != nil {
		return nil, false
	}
	raw, derr := hex.DecodeString(strings.TrimSpace(string(b)))
	if derr != nil || len(raw) != 32 {
		return nil, false
	}
	return raw, true
}

// ensureSecretDir creates workdir if it is not there yet. On a first boot the
// node writes its identity and secret BEFORE anything creates that directory:
// parseConfig resolves nodeSecretDir, main calls ensureNodeSecret ~60 lines
// later, and the StorageManager that actually MkdirAll's the path is
// constructed after THAT. So every save below failed with ENOENT on a fresh
// volume, was only WARNed, and the node started its NEXT boot with no cached
// identity - re-enrolling as a brand new node and orphaning the old row plus its
// three Redis ACL users. Creating it here rather than fixing the call order
// keeps it correct whatever the order becomes.
//
// 0755, matching what the StorageManager creates the same path with: the MC
// server subdirectories live under it and MkdirAll does not touch the mode of a
// directory that already exists, so creating it 0700 here would silently change
// the permissions of the whole server tree. The files themselves stay 0600.
func ensureSecretDir(workdir string) error {
	return os.MkdirAll(workdir, 0755)
}

// saveNodeSecret persists the secret as hex with 0600 perms.
func saveNodeSecret(workdir string, secret []byte) error {
	if err := ensureSecretDir(workdir); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workdir, ".node_secret"), []byte(hex.EncodeToString(secret)), 0600)
}

// loadNodeID reads the cached server-assigned node id from <workdir>/.node_id.
// Returns ok=false when the file is missing, unreadable or empty.
func loadNodeID(workdir string) (string, bool) {
	id, err := cachedNodeID(workdir)
	return id, err == nil && id != ""
}

// cachedNodeID is loadNodeID for the caller that must not guess. A file that
// exists but cannot be read, or reads empty, is an error rather than "no
// identity": the fallback is dialling Core as NODE_ID or the hostname, which is
// a DIFFERENT identity, and a node holding CLUSTER_SECRET would be enrolled
// under it as a new node. "" with a nil error means there is no file.
func cachedNodeID(workdir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(workdir, ".node_id"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read .node_id: %w", err)
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", errors.New(".node_id is empty")
	}
	return id, nil
}

// saveNodeID persists the server-assigned node id with 0600 perms. Atomically:
// a plain WriteFile truncates first, and a crash in between left an empty file,
// which cachedNodeID now refuses to boot on.
func saveNodeID(workdir, id string) error {
	return writeFileAtomic(workdir, ".node_id", []byte(id))
}

// writeFileAtomic writes name in dir through a temp file in the same directory,
// fsynced, then renamed over the old one, so a crash leaves the old content or
// the new and never half of either. CreateTemp opens the file 0600, the mode
// every file beside .node_secret needs.
func writeFileAtomic(dir, name string, data []byte) error {
	if err := ensureSecretDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmpName, filepath.Join(dir, name))
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// loadLinkCreds reads the cached Core-delivered Link tunnel token + discovery proof
// from <workdir>/.link_secret and .link_discovery_proof. ok=false if either missing.
func loadLinkCreds(workdir string) (secret, proof string, ok bool) {
	s, err := os.ReadFile(filepath.Join(workdir, ".link_secret"))
	if err != nil {
		return "", "", false
	}
	p, err := os.ReadFile(filepath.Join(workdir, ".link_discovery_proof"))
	if err != nil {
		return "", "", false
	}
	secret = strings.TrimSpace(string(s))
	proof = strings.TrimSpace(string(p))
	if secret == "" || proof == "" {
		return "", "", false
	}
	return secret, proof, true
}

// saveLinkCreds persists the Link tunnel token + discovery proof (0600).
func saveLinkCreds(workdir, secret, proof string) error {
	if err := ensureSecretDir(workdir); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(workdir, ".link_secret"), []byte(secret), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workdir, ".link_discovery_proof"), []byte(proof), 0600)
}
