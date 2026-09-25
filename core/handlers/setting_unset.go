package handlers

import (
	"database/sql"
	"errors"

	"dylaris-core/store"
)

// settingOrUnset reads a setting, treating "there is no such row" as the empty
// string rather than as a fault.
//
// GetSetting answers a key that was never written with sql.ErrNoRows, which is
// the database describing its own state, not the platform's. Most callers cope
// by discarding the error entirely - which also discards a real outage - and
// the ones that check it turn "not configured yet" into "Database error".
//
// Measured on production: the platform-backup passphrase had never been set, so
// the endpoint that reports whether one IS set answered 500, the endpoint that
// SETS the first one answered 500 before it could store anything, and the
// backup itself failed with "reading the passphrase: sql: no rows in result
// set" instead of the prepared sentence telling the operator to set one. A
// feature that could not be started, explaining itself as a broken database.
//
// GetSetting's own contract is deliberately left alone: several call sites
// reason about ErrNoRows on purpose, and one of them - the BYON ownership fence
// - fails CLOSED on a read error. Turning a missing row into a clean empty
// string there would open it.
func settingOrUnset(st store.Store, key string) (string, error) {
	v, err := st.GetSetting(key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
