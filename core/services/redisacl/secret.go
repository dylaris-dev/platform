package redisacl

import (
	"crypto/rand"
	"errors"

	"dylaris-core/pkg/crypto"
)

// secretReader is all the READING paths need. Kept separate from secretStore on
// purpose: verifying a proof must not be able to mint, and every caller that
// only checks a secret should not have to satisfy a write method it never uses.
type secretReader interface {
	GetNodeSecretEnc(id int) (string, error)
}

// secretStore is what MINTING needs. The compare-and-set is the whole reason
// this is a separate interface - see LoadOrCreateNodeSecret.
type secretStore interface {
	secretReader
	SetNodeSecretEncIfUnchanged(id int, prev, next string) (bool, error)
}

// ErrSecretContention means several Core replicas kept re-minting this node's
// secret past the retry budget. Refusing beats returning a secret the database
// does not hold - see LoadOrCreateNodeSecret.
var ErrSecretContention = errors.New("redisacl: node secret changed under every attempt to mint one")

// mintAttempts bounds the compare-and-set loop. Each miss means another replica
// won and stored a secret we can simply read, so one retry is normally enough;
// three covers a mint racing a rotation.
const mintAttempts = 3

// LoadOrCreateNodeSecret returns the node's per-node secret (32 raw bytes),
// minting + persisting one (AES-256-GCM at rest) on first use. clusterSecret is
// the platform CLUSTER_SECRET; the at-rest key is derived with purpose tag
// "node-redis-secret".
//
// The write is a COMPARE-AND-SET, and that is the whole point of this function
// rather than an implementation detail.
//
// A node dials every Core replica, in the same second - the mesh is built that
// way on purpose. So when the stored secret is absent or unreadable, every
// replica runs this function for the same node at the same moment. With an
// unconditional UPDATE they each mint a different secret, the node caches
// whichever AuthResult reaches it last, and the database keeps whichever write
// lands last. When those differ the node is locked out PERMANENTLY, not
// temporarily: the stored secret now exists, so every replica demands the
// challenge, and Core deliberately never re-issues a secret to a bare token
// holder. Recovery is an operator action.
//
// That is not theoretical. It happened in production on 2026-09-08: node
// eu-node-00 cached a secret at 12:56:01, went offline at 12:59:57 and could
// not authenticate again, while Core held a different secret it could decrypt
// perfectly well.
//
// So a loser must never overwrite the winner. It re-reads instead and returns
// the secret that is actually stored, which is the one the winner has already
// handed to the node.
func LoadOrCreateNodeSecret(st secretStore, clusterSecret string, nodeID int) ([]byte, error) {
	key := crypto.DeriveKey(clusterSecret, "node-redis-secret")
	for i := 0; i < mintAttempts; i++ {
		enc, err := st.GetNodeSecretEnc(nodeID)
		if err != nil {
			return nil, err
		}
		if enc != "" {
			if pt, derr := crypto.Decrypt(key, enc); derr == nil && len(pt) == 32 {
				return pt, nil
			}
			// Unreadable: fall through and re-mint. The compare is against what
			// was READ, not against empty, so a corrupt value is still
			// replaceable - guarding on empty would turn corruption into a
			// permanent lockout, which is the failure this function exists to
			// avoid.
		}
		secret := make([]byte, 32)
		if _, rerr := rand.Read(secret); rerr != nil {
			return nil, rerr
		}
		ct, eerr := crypto.Encrypt(key, secret)
		if eerr != nil {
			return nil, eerr
		}
		won, serr := st.SetNodeSecretEncIfUnchanged(nodeID, enc, ct)
		if serr != nil {
			return nil, serr
		}
		if won {
			return secret, nil
		}
		// Another replica stored one first. Loop: the next read returns THEIR
		// secret, which is what the node is being handed.
	}
	return nil, ErrSecretContention
}

// LoadNodeSecret loads + decrypts an existing secret WITHOUT minting. ok=false
// when no secret is stored yet. Used for proof verification on reconnect.
func LoadNodeSecret(st secretReader, clusterSecret string, nodeID int) (secret []byte, ok bool, err error) {
	enc, gerr := st.GetNodeSecretEnc(nodeID)
	if gerr != nil {
		return nil, false, gerr
	}
	if enc == "" {
		return nil, false, nil
	}
	key := crypto.DeriveKey(clusterSecret, "node-redis-secret")
	pt, derr := crypto.Decrypt(key, enc)
	if derr != nil || len(pt) != 32 {
		return nil, false, derr
	}
	return pt, true, nil
}
