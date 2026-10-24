package handlers

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

const demoServerUUIDsSetting = "demo_server_uuids"

// demoAccountUUIDSetting holds the UUID of the single designated demo account.
// That account is forced read-only (GET-only) in AuthMiddleware and is the only
// account that sees the demo servers in its sidebar. Empty = feature off.
const demoAccountUUIDSetting = "demo_account_uuid"

// isDemoAccount reports whether the given user id is the designated demo account.
// Always false when the store integration is off — the whole demo feature only
// exists on the hosted build (STORE_URL + STORE_SHARED_KEY set).
func isDemoAccount(s *AppState, userID string) bool {
	demo, _ := isDemoAccountChecked(s, userID)
	return demo
}

// isDemoAccountChecked is isDemoAccount with the lookup failure kept.
//
// The error used to be discarded, and the zero value is the PERMISSIVE answer:
// a failed settings read made the demo account look like an ordinary user, so
// AuthMiddleware skipped its read-only gate and a public demo session could
// mutate. Callers that gate a WRITE must use this form and refuse when the
// answer is unknown; callers that only decide how to render may keep the
// boolean, where being wrong costs a misdrawn button.
//
// Deliberately NOT "assume demo on error": isDemoAccount is consulted for every
// non-admin, so returning true on a failed read would turn one unreadable
// setting into a fleet-wide write outage. Unknown is its own answer, and only
// the write paths have to care.
func isDemoAccountChecked(s *AppState, userID string) (bool, error) {
	if s == nil || !s.StoreEnabled || userID == "" {
		return false, nil
	}
	v, err := s.Store.GetSetting(demoAccountUUIDSetting)
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

// loadDemoServerUUIDs returns the admin-flagged demo server UUIDs.
func loadDemoServerUUIDs(st store.Store) []string {
	raw, _ := st.GetSetting(demoServerUUIDsSetting)
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// isDemoServer reports whether the given server UUID is on the demo list.
// Always false when the store integration is off (open-core/self-host build).
func isDemoServer(s *AppState, uuid string) bool {
	if s == nil || !s.StoreEnabled || uuid == "" {
		return false
	}
	for _, u := range loadDemoServerUUIDs(s.Store) {
		if u == uuid {
			return true
		}
	}
	return false
}

// IsDemoServerID reports whether the numeric server id maps to an admin-flagged
// demo showcase server. Exported so main can hand the authz resolver a demo-read
// predicate without the resolver importing handlers. Always false when the
// store integration is off (open-core/self-host build).
func (s *AppState) IsDemoServerID(serverID int) bool {
	if s == nil || !s.StoreEnabled || s.Store == nil {
		return false
	}
	srv, err := s.Store.GetServerByID(serverID)
	if err != nil || srv == nil {
		return false
	}
	return isDemoServer(s, srv.UUID)
}

// dropDemoServers takes deleted servers off the demo list. The list holds
// UUIDs, and a UUID can come back: CreateServer accepts one from the client,
// and adopting a leftover folder reuses it. A stale entry then made a new,
// unrelated server readable by every signed-in account.
func dropDemoServers(s *AppState, uuids []string) {
	if s == nil || !s.StoreEnabled || s.Store == nil || len(uuids) == 0 {
		return
	}
	gone := make(map[string]bool, len(uuids))
	for _, u := range uuids {
		gone[u] = true
	}
	err := editDemoList(s.Store, func(cur []string) ([]string, error) {
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

// editDemoList changes the demo list under the settings row lock. A
// read-then-write let a delete on one replica and a demo toggle on the other
// each write back the list it had read, putting a deleted server back on it.
var errDemoServerGone = errors.New("demo server no longer exists")

func editDemoList(st store.Store, edit func([]string) ([]string, error)) error {
	return st.UpdateSetting(demoServerUUIDsSetting, func(raw string) (string, error) {
		var cur []string
		if raw != "" {
			// An unreadable list grants nothing (loadDemoServerUUIDs), so it is
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
