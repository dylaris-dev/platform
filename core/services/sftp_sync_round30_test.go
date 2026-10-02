package services

import (
	"errors"
	"testing"
	"time"

	"dylaris-core/services/redisacl"
	"dylaris-core/store"
)

// SFTP has nowhere to ask for a second factor, and the sync published the
// password hash of every account: the password alone opened every file a 2FA
// account could reach. Such an account signs in with a key instead.
func TestASecondFactorAccountSignsInToSFTPWithAKeyOnly(t *testing.T) {
	fs := pruneFixture()
	fs.users[0].ID, fs.users[0].Is2FAEnabled = "u-alice", true
	fs.access[1] = append(fs.access[1], store.SFTPAccess{ServerID: 11, UserID: "u-bob", Username: "bob", IsOwner: true, ServerUUID: "srv-a2", ServerName: "A2"})
	fs.keys = map[string][]string{"u-alice": {"ssh-ed25519 AAAAC3Nza"}}
	svc, mr := newSFTPPruneTest(t, fs)
	svc.sync()

	if mr.Exists(redisacl.SFTPAuthKey(pruneNodeA, "alice")) {
		t.Error("the password hash of an account with a second factor was published")
	}
	if !mr.Exists(redisacl.SFTPAuthKey(pruneNodeA, "bob")) {
		t.Error("an account without a second factor lost its password login")
	}
	if got, _ := mr.Get("sftp:node:" + pruneNodeA + ":keys:alice"); got != `["ssh-ed25519 AAAAC3Nza"]` {
		t.Errorf("alice's keys = %q, want her one key", got)
	}
	if !mr.Exists("sftp:node:" + pruneNodeA + ":user:alice") {
		t.Error("alice lost her server list; she signs in with a key instead")
	}
}

// A platform that requires a second factor requires it of SFTP too, including
// of accounts that have not enrolled yet (the panel makes them enrol first).
func TestARequiredSecondFactorTakesThePasswordOffSFTP(t *testing.T) {
	for _, tc := range []struct {
		setting string
		admin   bool
		want    bool // password hash still published
	}{
		{"auth.require_2fa_for_all_users", false, false},
		{"auth.require_2fa_for_admins", true, false},
		{"auth.require_2fa_for_admins", false, true},
	} {
		fs := pruneFixture()
		fs.users[0].IsAdmin = tc.admin
		fs.settings = map[string]string{tc.setting: "true"}
		svc, mr := newSFTPPruneTest(t, fs)
		svc.sync()
		if got := mr.Exists(redisacl.SFTPAuthKey(pruneNodeA, "alice")); got != tc.want {
			t.Errorf("%s, admin %v: hash published = %v, want %v", tc.setting, tc.admin, got, tc.want)
		}
	}
}

// The panel refuses an unverified account's sign-in; SFTP took it.
func TestAnUnverifiedAccountGetsNoSFTP(t *testing.T) {
	fs := pruneFixture()
	fs.settings = map[string]string{"auth.email_verify_required": "true"}
	verified := time.Now()
	fs.users[1].EmailVerifiedAt = &verified
	svc, mr := newSFTPPruneTest(t, fs)
	svc.sync()
	if mr.Exists(redisacl.SFTPAuthKey(pruneNodeA, "alice")) || mr.Exists("sftp:node:"+pruneNodeA+":user:alice") {
		t.Error("an unverified account was published")
	}
	if !mr.Exists(redisacl.SFTPAuthKey(pruneNodeB, "bob")) {
		t.Error("a verified account lost SFTP")
	}
}

// Beam refused a ticket to a tenant suspended for non-payment; SFTP kept
// handing them, and everyone they invited, full write access.
func TestASuspendedOwnersServersAreNotOfferedOverSFTP(t *testing.T) {
	fs := pruneFixture()
	fs.owners = map[int]string{10: "u-alice", 20: "u-bob"}
	fs.suspended = map[string]bool{"u-alice": true}
	svc, mr := newSFTPPruneTest(t, fs)
	svc.sync()
	if mr.Exists("sftp:node:" + pruneNodeA + ":user:alice") {
		t.Error("a suspended owner's server was offered over SFTP")
	}
	if !mr.Exists("sftp:node:" + pruneNodeB + ":user:bob") {
		t.Error("a paying owner lost SFTP")
	}
}

// A server list left to its 5-minute TTL kept a withdrawn grant on the node for
// five minutes, and a node ends sessions only as fast as the list goes.
func TestAWithdrawnServerListIsDeletedOnTheNextTick(t *testing.T) {
	fs := pruneFixture()
	svc, mr := newSFTPPruneTest(t, fs)
	stale := "sftp:node:" + pruneNodeA + ":user:mallory"
	mr.Set(stale, `[{"uuid":"srv-a","r":true}]`)
	mr.SetTTL(stale, 4*time.Minute)
	staleKeys := "sftp:node:" + pruneNodeA + ":keys:mallory"
	mr.Set(staleKeys, `["ssh-ed25519 AAAA"]`)
	svc.sync()
	if mr.Exists(stale) || mr.Exists(staleKeys) {
		t.Error("a list for an account with no access left was kept")
	}
	if !mr.Exists("sftp:node:" + pruneNodeA + ":user:alice") {
		t.Error("the prune removed a list it had just published")
	}
}

// One failed read of the keys pruned every published key list, and the nodes
// then ended every key-signed session on the fleet within a minute.
func TestAFailedKeyReadEndsNoKeySessions(t *testing.T) {
	fs := pruneFixture()
	fs.keysErr = errors.New("connection reset")
	svc, mr := newSFTPPruneTest(t, fs)
	k := "sftp:node:" + pruneNodeA + ":keys:alice"
	mr.Set(k, `["ssh-ed25519 AAAA"]`)
	svc.sync()
	if !mr.Exists(k) {
		t.Fatal("a key list was pruned on a tick that could not read the keys")
	}
}
