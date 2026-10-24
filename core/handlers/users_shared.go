package handlers

import "log"

// revokeAllSSHKeys removes every SFTP key of an account, on the same recovery
// paths as revokeAllAPIKeys and for the same reason: a key added while the
// account was in someone else's hands is a way back in no password reset sees.
func revokeAllSSHKeys(state *AppState, userID, why string) int {
	if state == nil || state.Store == nil {
		return 0
	}
	n, err := state.Store.DeleteAllSSHKeys(userID)
	if err != nil {
		log.Printf("%s: remove SSH keys of %s: %v", why, userID, err)
	}
	return n
}
