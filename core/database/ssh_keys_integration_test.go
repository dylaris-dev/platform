package database

import (
	"errors"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

func TestIntegrationSSHKeys(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	uid := f.user.ID
	key := func(fp string) *models.SSHKey {
		return &models.SSHKey{UserID: uid, Name: "k", PublicKey: "ssh-ed25519 " + fp, Fingerprint: fp}
	}
	if added, err := st.AddSSHKey(key("a"), 2); err != nil || !added {
		t.Fatalf("first key: %v %v", added, err)
	}
	if _, err := st.AddSSHKey(key("a"), 2); !errors.Is(err, store.ErrSSHKeyExists) {
		t.Fatalf("same key twice: %v, want ErrSSHKeyExists", err)
	}
	if added, err := st.AddSSHKey(key("b"), 2); err != nil || !added {
		t.Fatalf("second key: %v %v", added, err)
	}
	if added, err := st.AddSSHKey(key("c"), 2); err != nil || added {
		t.Fatalf("a key over the cap: added=%v err=%v", added, err)
	}
	all, err := st.ListAllSSHKeys()
	if err != nil || len(all[uid]) != 2 {
		t.Fatalf("ListAllSSHKeys = %v, %v", all, err)
	}
	keys, _ := st.ListSSHKeysByUser(uid)
	if found, err := st.DeleteSSHKey("00000000-0000-0000-0000-000000000000", keys[0].ID); err != nil || found {
		t.Fatalf("another account deleted the key: %v %v", found, err)
	}
	if n, err := st.DeleteAllSSHKeys(uid); err != nil || n != 2 {
		t.Fatalf("DeleteAllSSHKeys = %d, %v", n, err)
	}
}
