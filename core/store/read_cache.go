package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"
	"time"

	"dylaris-core/models"
)

// readCacheTTL bounds how stale an in-process cached read may be. The owner's
// rule (2026-11-02): an answer at most a second old counts as live. Each Core
// replica keeps its own copy, so this is also the cross-replica lag.
const readCacheTTL = time.Second

// cacheNow is the clock the read caches use; tests move it.
var cacheNow = time.Now

// HasUsers backs RequireSetupComplete, which runs on every API request.
//
// Only TRUE is cached, for readCacheTTL. A fresh install must unlock on the
// very next request after the wizard finishes on any replica, so a false is
// always asked again. The only way back from true is deleting every account,
// and a second of the old answer there opens nothing: the auth middleware
// still needs a user row.
func (s *PostgresStore) HasUsers() (bool, error) {
	if cacheNow().UnixNano() < s.hasUsersUntil.Load() {
		return true, nil
	}
	var ok bool
	err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM users)`).Scan(&ok)
	if err == nil && ok {
		s.hasUsersUntil.Store(cacheNow().Add(readCacheTTL).UnixNano())
	}
	return ok, err
}

// settingsSnapshot is the whole settings table, held in memory for at most
// readCacheTTL. Feature flags, audiences, routing and file-access modes and
// half a dozen limits are read on nearly every request, one SELECT each; one
// snapshot answers all of them.
//
// Values are kept in their STORED form: a sealed secret stays sealed in
// memory and is opened per read by decodeSettingValue, exactly as before.
//
// Every write through this store invalidates it, so a replica sees its own
// writes at once. A write by another replica (or by hand) is seen within
// readCacheTTL.
type settingsSnapshot struct {
	mu    sync.Mutex
	vals  map[string]sql.NullString
	until time.Time
	gen   uint64 // bumped by every invalidation
	// loadMu lets one caller load while the others wait for its result
	// instead of all querying at once.
	loadMu sync.Mutex
}

// ErrSettingNull stands in for the Scan error a NULL value used to produce on
// a single-row read. The column is nullable; no writer here stores NULL.
// Exported so a caller that must tell "unreadable" from "unset" can.
var ErrSettingNull = errors.New("settings: value is NULL")

func (s *PostgresStore) invalidateSettings() {
	c := &s.settings
	c.mu.Lock()
	c.gen++
	c.until = time.Time{}
	c.mu.Unlock()
}

// settingsMap returns the current snapshot, loading it when it is older than
// readCacheTTL or was invalidated. The returned map is never mutated.
func (s *PostgresStore) settingsMap() (map[string]sql.NullString, error) {
	c := &s.settings
	fresh := func() (map[string]sql.NullString, bool) {
		if c.vals != nil && cacheNow().Before(c.until) {
			return c.vals, true
		}
		return nil, false
	}
	c.mu.Lock()
	if m, ok := fresh(); ok {
		c.mu.Unlock()
		return m, nil
	}
	c.mu.Unlock()

	c.loadMu.Lock()
	defer c.loadMu.Unlock()
	c.mu.Lock()
	if m, ok := fresh(); ok {
		c.mu.Unlock()
		return m, nil
	}
	gen := c.gen
	c.mu.Unlock()

	m, err := s.loadSettings()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	// A write that landed while this load ran may or may not be in m. Hand m
	// to this caller (its read overlapped the write) but do not keep it.
	if c.gen == gen {
		c.vals = m
		c.until = cacheNow().Add(readCacheTTL)
	}
	c.mu.Unlock()
	return m, nil
}

// settingsLoadTimeout bounds the snapshot load: every GetSetting in the
// process waits behind loadMu while it runs, so one hung connection must not
// hold them all. A timed-out load is not cached; the next caller retries.
const settingsLoadTimeout = 2 * time.Second

func (s *PostgresStore) loadSettings() (map[string]sql.NullString, error) {
	ctx, cancel := context.WithTimeout(context.Background(), settingsLoadTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]sql.NullString{}
	for rows.Next() {
		var k string
		var v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return m, nil
}

// GetSetting answers from the settings snapshot. A key without a row returns
// sql.ErrNoRows, as the single-row SELECT it replaces did, so every caller
// that tells "never saved" apart from "unreadable" keeps doing so.
func (s *PostgresStore) GetSetting(key string) (string, error) {
	m, err := s.settingsMap()
	if err != nil {
		return "", err
	}
	v, ok := m[key]
	if !ok {
		return "", sql.ErrNoRows
	}
	if !v.Valid {
		return "", ErrSettingNull
	}
	return s.decodeSettingValue(key, v.String), nil
}

// modulesSnapshot is the navbar module list, read by GET /api/modules on
// every panel page load. Same contract as the settings snapshot: at most
// readCacheTTL old, dropped by every module write through this store.
type modulesSnapshot struct {
	mu    sync.Mutex
	vals  []models.Module
	until time.Time
	gen   uint64
}

func (s *PostgresStore) invalidateModules() {
	c := &s.modules
	c.mu.Lock()
	c.gen++
	c.until = time.Time{}
	c.mu.Unlock()
}

// ListModules returns a COPY of the cached list: callers filter it in place.
func (s *PostgresStore) ListModules() ([]models.Module, error) {
	c := &s.modules
	c.mu.Lock()
	if c.vals != nil && cacheNow().Before(c.until) {
		out := slices.Clone(c.vals)
		c.mu.Unlock()
		return out, nil
	}
	gen := c.gen
	c.mu.Unlock()

	m, err := s.loadModules()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.gen == gen && m != nil {
		c.vals = m
		c.until = cacheNow().Add(readCacheTTL)
	}
	c.mu.Unlock()
	return slices.Clone(m), nil
}

// FreshModules reads the module list past the one-second cache. The derive
// helpers that compare a row with a flag and write only on a difference (or
// create a missing row) use it: a second-old copy there could skip a needed
// write or duplicate a row another replica has just created.
func FreshModules(st Store) ([]models.Module, error) {
	if pg, ok := st.(*PostgresStore); ok {
		return pg.loadModules()
	}
	return st.ListModules()
}
