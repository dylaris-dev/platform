package services

import (
	"log"

	"dylaris-core/store"
)

// RevokeAllAPIKeys revokes every live key of an account and returns how many.
// It runs on each way an account is RECOVERED - a reset by mail, an operator
// setting the password or resetting the second factor - because each of them
// means the account was out of its owner's hands, and a key minted in that
// time is a way back in that no session check sees. A voluntary password
// change in the profile does not call it: the owner is signed in and holds
// their keys.
func RevokeAllAPIKeys(st store.Store, userID, why string) int {
	if st == nil {
		return 0
	}
	keys, err := st.ListAPIKeysByUser(userID)
	if err != nil {
		log.Printf("%s: list API keys of %s: %v", why, userID, err)
		return 0
	}
	revoked := 0
	for _, k := range keys {
		if k.RevokedAt != nil {
			continue
		}
		if rerr := st.RevokeAPIKey(k.ID, userID); rerr != nil {
			log.Printf("%s: revoke API key %d of %s: %v", why, k.ID, userID, rerr)
			continue
		}
		revoked++
	}
	return revoked
}

// RevokeAllSSHKeys removes every SFTP key of an account, on the same recovery
// paths as RevokeAllAPIKeys and for the same reason: a key added while the
// account was in someone else's hands is a way back in no password reset sees.
func RevokeAllSSHKeys(st store.Store, userID, why string) int {
	if st == nil {
		return 0
	}
	n, err := st.DeleteAllSSHKeys(userID)
	if err != nil {
		log.Printf("%s: remove SSH keys of %s: %v", why, userID, err)
	}
	return n
}
