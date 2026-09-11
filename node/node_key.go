package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// nodeKeyFileName holds this node's identity: an Ed25519 key pair it generated
// itself. The file is the 32-byte seed (the RFC 8032 private key) as hex on one
// line, mode 0600, in nodeSecretDir beside .node_id and .node_secret.
//
// Only the public half ever leaves the machine (NodeAuth.node_public_key). The
// private half signs Core's login nonce and nothing else, and is never logged.
// It is the one credential nothing can re-issue: Core hands the service secret
// back to a node that proves this key, but a lost key can only be REPLACED, and
// only by an operator's decision.
const nodeKeyFileName = ".node_key"

var (
	nodeKeyMu sync.Mutex
	nodeKey   ed25519.PrivateKey
)

// currentNodeKey is the key this node presents, nil when it has none.
func currentNodeKey() ed25519.PrivateKey {
	nodeKeyMu.Lock()
	defer nodeKeyMu.Unlock()
	return nodeKey
}

func setNodeKey(k ed25519.PrivateKey) {
	nodeKeyMu.Lock()
	nodeKey = k
	nodeKeyMu.Unlock()
}

// publicKeyOf is the half that goes into NodeAuth; nil without a key.
func publicKeyOf(k ed25519.PrivateKey) ed25519.PublicKey {
	if k == nil {
		return nil
	}
	return k.Public().(ed25519.PublicKey)
}

// loadOrCreateNodeKey reads the node key, generating one only when the file
// does not exist. An unreadable or malformed file is an error and is left as it
// is: writing over it would silently replace this node's identity.
func loadOrCreateNodeKey(dir string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(filepath.Join(dir, nodeKeyFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return generateNodeKey(dir)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", nodeKeyFileName, err)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a hex Ed25519 seed; it was left untouched", nodeKeyFileName)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func generateNodeKey(dir string) (ed25519.PrivateKey, error) {
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate node key: %w", err)
	}
	if err := writeFileAtomic(dir, nodeKeyFileName, []byte(hex.EncodeToString(k.Seed()))); err != nil {
		return nil, fmt.Errorf("persist node key: %w", err)
	}
	return k, nil
}

// replaceRejectedNodeKey generates a new key after Core refused the one this
// node presented because an operator replaced it - but only while that is still
// the key it holds. A node dials every Core replica at once and each of them
// answers "rejected"; replacing on every answer would register one new key with
// one replica and then present a different one, which needs a second admission
// nobody armed.
//
// The new key is used only once it is on disk. One held only in memory would be
// registered and then lost at the next restart, with the rejected key still in
// the file.
func replaceRejectedNodeKey(dir string, presented ed25519.PublicKey) {
	nodeKeyMu.Lock()
	defer nodeKeyMu.Unlock()
	if nodeKey == nil || !bytes.Equal(publicKeyOf(nodeKey), presented) {
		return
	}
	k, err := generateNodeKey(dir)
	if err != nil {
		log.Printf("nodekey: Core refused this node's key, and a new one could not be made: %v", err)
		return
	}
	nodeKey = k
	log.Println("nodekey: Core refused this node's key because an operator replaced it; generated a new one and connecting again")
}
