package database

import (
	"testing"
)

// Pack slugs used to be unique across ALL customers, because one Solder URL
// served every tenant and a bare slug had to have one answer. The second tenant
// to want "skyfactory" could not have it - and nothing said so beyond a
// constraint violation. Uniqueness is per owner now, which is only sound because
// the read path is addressed per account too.
func TestSolderSlugsAreUniquePerOwnerNotGlobally(t *testing.T) {
	db := freshSchemaDB(t) // skips unless DYLARIS_TEST_DB_HOST is set

	mkUser := func(t *testing.T, name string) string {
		t.Helper()
		var id string
		if err := db.QueryRow(
			`INSERT INTO users (username, password, is_admin) VALUES ($1, 'x', false) RETURNING id`,
			uniqueName(name)).Scan(&id); err != nil {
			t.Fatalf("create user: %v", err)
		}
		return id
	}
	mkPack := func(ownerID, slug string) error {
		_, err := db.Exec(
			`INSERT INTO packs (owner_id, internal_name, internal_slug, solder_slug)
			 VALUES ($1, $2, $2, $3)`, ownerID, uniqueName("pack"), slug)
		return err
	}

	a, b := mkUser(t, "solder_a_"), mkUser(t, "solder_b_")
	slug := uniqueName("skyfactory")

	if err := mkPack(a, slug); err != nil {
		t.Fatalf("first owner could not take the slug: %v", err)
	}
	// The whole point of the change.
	if err := mkPack(b, slug); err != nil {
		t.Errorf("a second owner was refused the same slug: %v", err)
	}
	// And it is still unique WITHIN one owner, or that owner's own Solder would
	// have two packs at one address.
	if err := mkPack(a, slug); err == nil {
		t.Error("the same owner took the same slug twice")
	}
}

// A handle addresses an account, so two accounts must not share one - and the
// empty handle is what every account starts with, so it cannot be the thing the
// index collides on.
func TestSolderHandleIsUniqueButEmptyIsNot(t *testing.T) {
	db := freshSchemaDB(t)

	mkUser := func(t *testing.T, handle string) error {
		t.Helper()
		_, err := db.Exec(
			`INSERT INTO users (username, password, is_admin, solder_handle) VALUES ($1, 'x', false, $2)`,
			uniqueName("solder_h_"), handle)
		return err
	}

	h := uniqueName("handle")
	if err := mkUser(t, h); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := mkUser(t, h); err == nil {
		t.Error("two accounts hold the same Solder handle")
	}
	// Two accounts with no handle at all is the normal state of a fresh install.
	if err := mkUser(t, ""); err != nil {
		t.Fatalf("unclaimed account: %v", err)
	}
	if err := mkUser(t, ""); err != nil {
		t.Errorf("a second unclaimed account was refused: %v", err)
	}
}
