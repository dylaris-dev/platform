package packs

// PATLoader loads and decrypts a user's Modrinth PAT for outbound API calls.
// Implemented by *ModrinthPATHandler (which owns the AES-GCM key + store access),
// so PacksHandler does not duplicate the crypto/key wiring.
type PATLoader interface {
	LoadPAT(userID string) (pat string, username string, err error)
}
