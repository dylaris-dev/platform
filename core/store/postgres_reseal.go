package store

import (
	"database/sql"
	"fmt"
	"strings"

	"dylaris-core/pkg/crypto"
)

// Re-encrypting every at-rest value from one key to another.
//
// Five buckets in this database hold ciphertext derived from CLUSTER_SECRET,
// each under its own purpose tag, and until now nothing could move any of them.
// Two consequences, both real rather than theoretical:
//
//   - Rotating CLUSTER_SECRET makes every one of them unreadable. Not
//     destroyed - the old secret would still open them - but every provider
//     build, every node handshake and every mail send fails in the meantime.
//     Two store files already describe this in their own comments as the
//     "rotated CLUSTER_SECRET" case they return "" for.
//   - A platform backup bundle cannot be restored anywhere but onto an instance
//     holding the same secret, which is exactly what the bundle passphrase
//     exists to fix.
//
// Both need the same operation, so it lives here once: read every at-rest
// value under one key, write it back under another.
//
// NOT covered here, and neither is an oversight:
//
//   - The mrpack storage path (purpose "mrpack-path") is an HMAC over a path,
//     not a stored ciphertext. Changing the secret changes where a pack build's
//     .mrpack LIVES, so a rotation has to re-key the OBJECTS in storage, which
//     is a data move rather than a column update.
//   - The warp leader WireGuard identity is derived from CLUSTER_SECRET and
//     stored nowhere at all, so there is nothing to re-encrypt. It is why the
//     old secret has to travel inside a bundle.
//   - A settings value written before encryption existed is plaintext with no
//     marker. It is left exactly as it is: it reads correctly under any key, so
//     moving it is not required, and encrypting it here would quietly change
//     what a rollback can read.

const (
	nodeSecretPurpose  = "node-redis-secret"
	modrinthPATPurpose = "modrinth-pat"
)

// mrpackPathPurpose is listed so the coverage test below can account for it by
// name. It is deliberately not resealable; see the comment above.
const mrpackPathPurpose = "mrpack-path"

// ResealBucket is what happened to one group of values.
type ResealBucket struct {
	// Name is the purpose tag, which is also what the key is derived with.
	Name string `json:"name"`
	// Moved is how many values now open under the target key.
	Moved int `json:"moved"`
	// Unreadable is how many did not open under the SOURCE key and were
	// therefore left untouched.
	//
	// Reported rather than fatal, and that is a decision. An instance whose
	// secret was rotated once without this path has rows nothing can ever open
	// again; failing the whole operation on one of them would make it
	// impossible to ever rotate again, which is the opposite of the point.
	// Nothing is lost either way - the ciphertext stays exactly where it was.
	Unreadable int `json:"unreadable"`
}

// ResealReport is the per-bucket outcome, for an operator who has to decide
// whether the rotation actually landed.
type ResealReport struct {
	Buckets []ResealBucket `json:"buckets"`
}

// Unreadable is the total across every bucket, which is the one number that
// decides whether a rotation left something behind.
func (r *ResealReport) Unreadable() int {
	n := 0
	for _, b := range r.Buckets {
		n += b.Unreadable
	}
	return n
}

// Moved is the total across every bucket.
func (r *ResealReport) Moved() int {
	n := 0
	for _, b := range r.Buckets {
		n += b.Moved
	}
	return n
}

// ResealAtRest moves every at-rest value from a key derived off fromSecret to
// one derived off toSecret.
//
// One transaction over all five buckets. A half-moved database is strictly
// worse than an unmoved one: half the nodes would authenticate and half would
// not, and there would be no single secret that opens the rest.
//
// The store's own cached keys are NOT touched. Which key a running Core uses is
// decided by its CLUSTER_SECRET at boot, and this call may be preparing a
// database for a DIFFERENT instance entirely - a bundle restore. The caller
// installs the new keys if and only if it is the instance that will run under
// them.
func (s *PostgresStore) ResealAtRest(fromSecret, toSecret string) (*ResealReport, error) {
	if fromSecret == "" || toSecret == "" {
		return nil, fmt.Errorf("reseal: both the source and the target secret are required")
	}
	if fromSecret == toSecret {
		return nil, fmt.Errorf("reseal: the source and target secret are identical")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("reseal: begin: %w", err)
	}
	defer tx.Rollback()

	rep := &ResealReport{}

	columns := []struct {
		purpose string
		table   string
		idCol   string
		valCol  string
	}{
		{nodeSecretPurpose, "nodes", "id", "node_secret_enc"},
		{backupStorageSecretPurpose, "backup_storages", "id", "secret_enc"},
		{storageConnSecretPurpose, "storage_connections", "id", "secret_enc"},
		{modrinthPATPurpose, "modrinth_pats", "user_id", "ciphertext"},
	}
	for _, c := range columns {
		b, err := resealColumn(tx, c.table, c.idCol, c.valCol,
			crypto.DeriveKey(fromSecret, c.purpose), crypto.DeriveKey(toSecret, c.purpose))
		if err != nil {
			return nil, fmt.Errorf("reseal %s: %w", c.purpose, err)
		}
		b.Name = c.purpose
		rep.Buckets = append(rep.Buckets, b)
	}

	b, err := resealSettings(tx,
		crypto.DeriveKey(fromSecret, settingsSecretPurpose),
		crypto.DeriveKey(toSecret, settingsSecretPurpose))
	if err != nil {
		return nil, fmt.Errorf("reseal %s: %w", settingsSecretPurpose, err)
	}
	b.Name = settingsSecretPurpose
	rep.Buckets = append(rep.Buckets, b)

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("reseal: commit: %w", err)
	}
	return rep, nil
}

// resealColumn moves one hex ciphertext column.
//
// The identifiers are interpolated because Postgres has no placeholder for a
// table or column name. They come from the constant list above and from
// nowhere else - no caller supplies one - so there is no path from a request to
// this string.
func resealColumn(tx *sql.Tx, table, idCol, valCol string, fromKey, toKey []byte) (ResealBucket, error) {
	var b ResealBucket

	//nolint:gosec // identifiers are compile-time constants, see above
	rows, err := tx.Query(fmt.Sprintf(
		`SELECT %s, %s FROM %s WHERE %s IS NOT NULL AND %s <> ''`,
		idCol, valCol, table, valCol, valCol))
	if err != nil {
		return b, err
	}

	type pending struct {
		id  any
		val string
	}
	// Read the whole set before writing any of it. A cursor held open across
	// UPDATEs on the same table it is scanning is a shape Postgres does not
	// promise anything useful about, and these sets are tens of rows.
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.val); err != nil {
			rows.Close()
			return b, err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return b, err
	}
	rows.Close()

	for _, p := range todo {
		moved, err := crypto.Reseal(fromKey, toKey, p.val)
		if err != nil {
			b.Unreadable++
			continue
		}
		//nolint:gosec // identifiers are compile-time constants, see above
		if _, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET %s=$1 WHERE %s=$2`, table, valCol, idCol),
			moved, p.id); err != nil {
			return b, err
		}
		b.Moved++
	}
	return b, nil
}

// resealSettings moves the encrypted settings values.
//
// Two things make this bucket different from a plain column. The rows are
// selected by KEY SHAPE rather than by a column, because which settings are
// secret is decided by isSecretSettingKey - including the SMTP passwords, whose
// key is built per purpose and cannot be listed. And the stored value carries
// the "enc:v1:" marker that tells an encrypted value apart from a legacy
// plaintext one, so the marker has to come off before decrypting and go back on
// after.
func resealSettings(tx *sql.Tx, fromKey, toKey []byte) (ResealBucket, error) {
	var b ResealBucket

	rows, err := tx.Query(`SELECT key, value FROM settings WHERE value LIKE $1`, settingsEncMarker+"%")
	if err != nil {
		return b, err
	}
	type pending struct{ key, val string }
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.key, &p.val); err != nil {
			rows.Close()
			return b, err
		}
		// A value carrying the marker under a key that is not secret cannot
		// have been written by this code. Leaving it alone is the only safe
		// answer: we do not know what key it was sealed with.
		if !isSecretSettingKey(p.key) {
			continue
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return b, err
	}
	rows.Close()

	for _, p := range todo {
		moved, err := crypto.Reseal(fromKey, toKey, strings.TrimPrefix(p.val, settingsEncMarker))
		if err != nil {
			b.Unreadable++
			continue
		}
		if _, err := tx.Exec(`UPDATE settings SET value=$1 WHERE key=$2`,
			settingsEncMarker+moved, p.key); err != nil {
			return b, err
		}
		b.Moved++
	}
	return b, nil
}
