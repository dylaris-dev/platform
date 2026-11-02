// Package demo is the hosted build's showcase: one designated read-only demo
// account, and a list of servers every signed-in account may view. All of it
// is off unless the store integration is enabled, which callers pass in as
// enabled (AppState.StoreEnabled).
package demo

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"

	"dylaris-core/store"
)

// Demo servers are normal servers an admin flags as public read-only showcases.
// The list lives in a single setting (demo_server_uuids, JSON array of UUIDs)
// rather than a servers column, so no server scan/insert path changes. Non-owner
// viewers get READ-ONLY access to these servers (file list + read, console + stats
// view); every write stays denied by the default-deny access model (a demo viewer
// has no invite, so checkServerAccess returns false for all write endpoints).

const ServerUUIDsSetting = "demo_server_uuids"

// AccountUUIDSetting holds the UUID of the single designated demo account.
// That account is forced read-only (GET-only) in AuthMiddleware and is the only
// account that sees the demo servers in its sidebar. Empty = feature off.
const AccountUUIDSetting = "demo_account_uuid"

// IsAccount reports whether the given user id is the designated demo account.
// Always false when the store integration is off — the whole demo feature only
// exists on the hosted build (STORE_URL + STORE_SHARED_KEY set).
func IsAccount(st store.Store, enabled bool, userID string) bool {
	demo, _ := IsAccountChecked(st, enabled, userID)
	return demo
}

// IsAccountChecked is IsAccount with the lookup failure kept.
//
// The error used to be discarded, and the zero value is the PERMISSIVE answer:
// a failed settings read made the demo account look like an ordinary user, so
// AuthMiddleware skipped its read-only gate and a public demo session could
// mutate. Callers that gate a WRITE must use this form and refuse when the
// answer is unknown; callers that only decide how to render may keep the
// boolean, where being wrong costs a misdrawn button.
//
// Deliberately NOT "assume demo on error": IsAccount is consulted for every
// non-admin, so returning true on a failed read would turn one unreadable
// setting into a fleet-wide write outage. Unknown is its own answer, and only
// the write paths have to care.
func IsAccountChecked(st store.Store, enabled bool, userID string) (bool, error) {
	if !enabled || userID == "" {
		return false, nil
	}
	v, err := st.GetSetting(AccountUUIDSetting)
	if err != nil {
		// A missing row is the answer "no demo account is designated", not a
		// failed read. The setting is never seeded, so treating ErrNoRows as an
		// error made AuthMiddleware refuse EVERY non-admin write on any hosted
		// install where nobody had ever nominated a demo account - the whole
		// tenant side of the panel, 503, until an unrelated feature was
		// configured. Same convention as the node admission gate.
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return v != "" && v == userID, nil
}

// LoadServerUUIDs returns the admin-flagged demo server UUIDs.
func LoadServerUUIDs(st store.Store) []string {
	raw, _ := st.GetSetting(ServerUUIDsSetting)
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// IsServer reports whether the given server UUID is on the demo list.
// Always false when the store integration is off (open-core/self-host build).
func IsServer(st store.Store, enabled bool, uuid string) bool {
	if !enabled || uuid == "" {
		return false
	}
	for _, u := range LoadServerUUIDs(st) {
		if u == uuid {
			return true
		}
	}
	return false
}

// DropServers takes deleted servers off the demo list. The list holds
// UUIDs, and a UUID can come back: CreateServer accepts one from the client,
// and adopting a leftover folder reuses it. A stale entry then made a new,
// unrelated server readable by every signed-in account.
func DropServers(st store.Store, enabled bool, uuids []string) {
	if !enabled || st == nil || len(uuids) == 0 {
		return
	}
	gone := make(map[string]bool, len(uuids))
	for _, u := range uuids {
		gone[u] = true
	}
	err := EditList(st, func(cur []string) ([]string, error) {
		next := make([]string, 0, len(cur))
		for _, u := range cur {
			if !gone[u] {
				next = append(next, u)
			}
		}
		return next, nil
	})
	if err != nil {
		log.Printf("demo: could not drop deleted servers from the demo list: %v", err)
	}
}

// EditList changes the demo list under the settings row lock. A
// read-then-write let a delete on one replica and a demo toggle on the other
// each write back the list it had read, putting a deleted server back on it.
var ErrServerGone = errors.New("demo server no longer exists")

func EditList(st store.Store, edit func([]string) ([]string, error)) error {
	return st.UpdateSetting(ServerUUIDsSetting, func(raw string) (string, error) {
		var cur []string
		if raw != "" {
			// An unreadable list grants nothing (LoadServerUUIDs), so it is
			// replaced rather than kept.
			_ = json.Unmarshal([]byte(raw), &cur)
		}
		next, err := edit(cur)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(next)
		return string(data), err
	})
}
