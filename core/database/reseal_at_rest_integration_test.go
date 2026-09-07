package database

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/pkg/crypto"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
)

const (
	secretBefore = "cluster-secret-before-rotation"
	secretAfter  = "cluster-secret-after-rotation"
)

// Against a real Postgres because the whole operation is one transaction over
// five tables, and because every bucket is read back through the SAME store
// methods production uses - a hand-built decrypt would prove that this test can
// decrypt, not that Core can.
//
// On a scratch database rather than the shared one: this rewrites every row in
// five tables, so it must not run over rows another test is holding.
func TestResealAtRestMovesEveryBucket(t *testing.T) {
	db := freshSchemaDB(t)

	before := store.NewPostgresStore(db)
	before.SetBackupStorageEncryptionKey(secretBefore)
	before.SetStorageConnEncryptionKey(secretBefore)
	before.SetSettingsEncryptionKey(secretBefore)
	f := newFixture(t, before)

	// One value per bucket, written the way production writes it.
	nodeSecret, err := redisacl.LoadOrCreateNodeSecret(before, secretBefore, f.node.ID)
	if err != nil {
		t.Fatalf("mint node secret: %v", err)
	}

	bsID, err := before.CreateBackupStorage(&models.BackupStorage{
		Name: "reseal-r2", Provider: "s3",
		Config: json.RawMessage(`{"bucket":"b","secretAccessKey":"BACKUP-SECRET"}`),
	})
	if err != nil {
		t.Fatalf("CreateBackupStorage: %v", err)
	}

	scID, err := before.CreateStorageConnection(&models.StorageConnection{
		Name: "reseal-conn", Provider: "s3", AccessKey: "AKIA",
		SecretAccessKey: "CONN-SECRET", Config: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateStorageConnection: %v", err)
	}

	patCT, err := crypto.Encrypt(crypto.DeriveKey(secretBefore, "modrinth-pat"), []byte("mrp_TOKEN"))
	if err != nil {
		t.Fatalf("encrypt PAT: %v", err)
	}
	if err := before.SetModrinthPAT(f.user.ID, patCT, "someone"); err != nil {
		t.Fatalf("SetModrinthPAT: %v", err)
	}

	// An SMTP password rather than a fixed secret key, because its settings key
	// is built per purpose and is therefore the one that cannot be matched from
	// a list - if the reseal selected rows by key name it would miss exactly
	// this one.
	const mailKey = "smtp.reseal_probe.password"
	if err := before.SetSetting(mailKey, "MAIL-SECRET"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	rep, err := before.ResealAtRest(secretBefore, secretAfter)
	if err != nil {
		t.Fatalf("ResealAtRest: %v", err)
	}
	if rep.Unreadable() != 0 {
		t.Fatalf("report = %+v; every value was sealed under the source secret", rep.Buckets)
	}
	if len(rep.Buckets) != 5 {
		t.Fatalf("got %d buckets, want 5: %+v", len(rep.Buckets), rep.Buckets)
	}
	for _, b := range rep.Buckets {
		if b.Moved < 1 {
			t.Errorf("bucket %q moved nothing", b.Name)
		}
	}

	// A store keyed to the NEW secret, which is what the instance would be
	// running after a rotation.
	after := store.NewPostgresStore(db)
	after.SetBackupStorageEncryptionKey(secretAfter)
	after.SetStorageConnEncryptionKey(secretAfter)
	after.SetSettingsEncryptionKey(secretAfter)

	got, ok, err := redisacl.LoadNodeSecret(after, secretAfter, f.node.ID)
	if err != nil || !ok {
		t.Fatalf("node secret under the new key: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(got, nodeSecret) {
		t.Error("the node secret changed value; every paired node would have to re-enroll")
	}

	bs, err := after.GetBackupStorage(bsID)
	if err != nil {
		t.Fatalf("GetBackupStorage: %v", err)
	}
	if s := configField(t, bs.Config, "secretAccessKey"); s != "BACKUP-SECRET" {
		t.Errorf("backup storage secret = %q, want BACKUP-SECRET", s)
	}

	sc, err := after.GetStorageConnection(scID)
	if err != nil {
		t.Fatalf("GetStorageConnection: %v", err)
	}
	if sc.SecretAccessKey != "CONN-SECRET" {
		t.Errorf("storage connection secret = %q, want CONN-SECRET", sc.SecretAccessKey)
	}

	pat, err := after.GetModrinthPAT(f.user.ID)
	if err != nil || pat == nil {
		t.Fatalf("GetModrinthPAT: %v", err)
	}
	pt, err := crypto.Decrypt(crypto.DeriveKey(secretAfter, "modrinth-pat"), pat.Ciphertext)
	if err != nil || string(pt) != "mrp_TOKEN" {
		t.Errorf("PAT under the new key = %q (%v), want mrp_TOKEN", pt, err)
	}

	mail, err := after.GetSetting(mailKey)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if mail != "MAIL-SECRET" {
		t.Errorf("SMTP password = %q, want MAIL-SECRET", mail)
	}

	// And the old secret must no longer open any of it, or the rotation moved
	// nothing and every assertion above passed on an unchanged column.
	stale := store.NewPostgresStore(db)
	stale.SetSettingsEncryptionKey(secretBefore)
	if v, _ := stale.GetSetting(mailKey); v == "MAIL-SECRET" {
		t.Error("the old secret still reads the SMTP password; nothing was resealed")
	}
	if _, ok, _ := redisacl.LoadNodeSecret(stale, secretBefore, f.node.ID); ok {
		t.Error("the old secret still opens the node secret; nothing was resealed")
	}
}

// A row nobody can open is the state an instance is in after a rotation that
// happened before this path existed. It must be reported and left EXACTLY as it
// is - failing the whole run on it would make such an instance unable to ever
// rotate again, and rewriting it would destroy the only ciphertext the lost
// secret could still have opened.
func TestResealAtRestLeavesUnreadableValuesUntouched(t *testing.T) {
	db := freshSchemaDB(t)

	st := store.NewPostgresStore(db)
	st.SetSettingsEncryptionKey(secretBefore)
	f := newFixture(t, st)

	// Sealed under a third secret that neither side of the reseal holds.
	orphan, err := crypto.Encrypt(crypto.DeriveKey("a-secret-nobody-has", "modrinth-pat"), []byte("mrp_LOST"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := st.SetModrinthPAT(f.user.ID, orphan, "someone"); err != nil {
		t.Fatalf("SetModrinthPAT: %v", err)
	}

	rep, err := st.ResealAtRest(secretBefore, secretAfter)
	if err != nil {
		t.Fatalf("ResealAtRest: %v", err)
	}
	if rep.Unreadable() != 1 {
		t.Errorf("Unreadable = %d, want 1: %+v", rep.Unreadable(), rep.Buckets)
	}

	pat, err := st.GetModrinthPAT(f.user.ID)
	if err != nil || pat == nil {
		t.Fatalf("GetModrinthPAT: %v", err)
	}
	if pat.Ciphertext != orphan {
		t.Error("an unreadable value was rewritten; the ciphertext its own secret could still open is gone")
	}
}

// A settings value written before encryption existed carries no marker and is
// plaintext. It reads correctly under any key, so a reseal must leave it alone
// rather than helpfully encrypting it - that would change what a rollback to an
// older Core can read.
func TestResealAtRestIgnoresLegacyPlaintextSettings(t *testing.T) {
	db := freshSchemaDB(t)

	// No encryption key installed, which is exactly how a pre-encryption Core
	// wrote this row: SetSetting stores the value in the clear.
	plain := store.NewPostgresStore(db)
	const key = "smtp.legacy_probe.password"
	if err := plain.SetSetting(key, "LEGACY-PLAIN"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	st := store.NewPostgresStore(db)
	st.SetSettingsEncryptionKey(secretBefore)
	rep, err := st.ResealAtRest(secretBefore, secretAfter)
	if err != nil {
		t.Fatalf("ResealAtRest: %v", err)
	}
	for _, b := range rep.Buckets {
		if strings.Contains(b.Name, "settings") && (b.Moved != 0 || b.Unreadable != 0) {
			t.Errorf("settings bucket = %+v, want untouched", b)
		}
	}

	got, err := plain.GetSetting(key)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if got != "LEGACY-PLAIN" {
		t.Errorf("legacy plaintext = %q, want LEGACY-PLAIN", got)
	}
}

func TestResealAtRestRefusesADegenerateRotation(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)

	cases := []struct{ name, from, to string }{
		{"no source", "", secretAfter},
		{"no target", secretBefore, ""},
		// Not a rotation, and accepting it would rewrite every ciphertext in
		// the database for no change at all.
		{"identical", secretBefore, secretBefore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.ResealAtRest(tc.from, tc.to); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func configField(t *testing.T, raw json.RawMessage, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("config is not an object: %v", err)
	}
	s, _ := m[field].(string)
	return s
}
