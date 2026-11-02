package gateway

import (
	"crypto/rand"
	"encoding/hex"
)

// generateLinkIdentity mints a stable, unique link identity (the warp key's
// node_id). The Link derives its tunnel token deterministically from this id +
// the cluster secret, so the id must be unguessable. 16 random bytes suffice.
func generateLinkIdentity() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "link-" + hex.EncodeToString(b), nil
}

// generateNodeWarpIdentity mints the warp identity for a BYON node's key.
//
// The "node-" prefix is what separates the two kinds of tenant warp key, and the
// separation is load-bearing in both directions: LinkBoot refuses a key whose id
// is not "link-…" so a node key can never mint a link credential, and
// CountLinkKitsByOwner counts only "link-%" so a node key never eats the tenant's
// route-only allowance. Do not change either prefix without changing both.
func generateNodeWarpIdentity() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "node-" + hex.EncodeToString(b), nil
}
