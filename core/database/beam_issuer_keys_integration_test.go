package database

import (
	"sync"
	"testing"
	"time"

	"dylaris-core/models"
)

// Two live keys per owner hold even when registrations race; a revoke frees a
// slot, is owner-scoped, and an account deletion cascades the rows away.
// Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationBeamIssuerKeys(t *testing.T) {
	db, st := integrationDB(t)
	newUser := func(name string) string {
		var id string
		if err := db.QueryRow(`INSERT INTO users (username, password, role) VALUES ($1, 'x', 'user') RETURNING id`,
			name+"-"+time.Now().Format("150405.000000000")).Scan(&id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, id) })
		return id
	}
	a, b := newUser("beam-a"), newUser("beam-b")
	var wg sync.WaitGroup
	created := make(chan bool, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := models.BeamIssuerKey{ID: hex32(a, i), OwnerID: a, PublicKey: make([]byte, 32)}
			ok, err := st.CreateBeamIssuerKeyUnderCap(k, 2)
			if err != nil {
				t.Error(err)
			}
			created <- ok
		}(i)
	}
	wg.Wait()
	close(created)
	n := 0
	for ok := range created {
		if ok {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d keys created under a cap of 2", n)
	}
	keys, err := st.ListBeamIssuerKeysByOwner(a)
	if err != nil || len(keys) != 2 {
		t.Fatalf("list: %v %v", keys, err)
	}
	if found, err := st.RevokeBeamIssuerKey(b, keys[0].ID); err != nil || found {
		t.Fatalf("another owner revoked a's key: %v %v", found, err)
	}
	if found, err := st.RevokeBeamIssuerKey(a, keys[0].ID); err != nil || !found {
		t.Fatalf("revoke: %v %v", found, err)
	}
	if ok, err := st.CreateBeamIssuerKeyUnderCap(models.BeamIssuerKey{ID: hex32(a, 9), OwnerID: a, PublicKey: make([]byte, 32)}, 2); err != nil || !ok {
		t.Fatalf("a revoke did not free a slot: %v %v", ok, err)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if keys, _ := st.ListBeamIssuerKeysByOwner(a); len(keys) != 0 {
		t.Errorf("%d keys survived the account deletion", len(keys))
	}
}

// hex32 is a kid unique per owner and index.
func hex32(owner string, i int) string {
	s := ""
	for _, c := range owner {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			s += string(c)
		}
	}
	s = s[:30] + string("0123456789abcdef"[i]) + "f"
	return s
}
