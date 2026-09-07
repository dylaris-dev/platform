package store

import (
	"database/sql"
	"errors"

	"dylaris-core/models"
)

// ErrSolderHandleSet is returned by SetSolderHandle when the account already has
// one. Changing it is refused rather than allowed, because the Technic Platform
// stores the Solder URL per modpack and a launcher keeps it inside the installed
// pack: a change silently breaks every linked pack and every install of it. An
// account gets to choose once.
var ErrSolderHandleSet = errors.New("solder handle already set")

// GetUserIDBySolderHandle resolves the handle in /solder/u/{handle}/api/ to its
// account. Returns "" for an unknown handle, which every caller must answer as a
// 404 rather than as an error: on the launcher read path an unknown address is
// simply an address with nothing behind it.
//
// The empty handle is never resolvable. Every account starts with ” and a
// lookup for it would otherwise match all of them.
func (s *PostgresStore) GetUserIDBySolderHandle(handle string) (string, error) {
	if handle == "" {
		return "", nil
	}
	var id string
	err := s.db.QueryRow(`SELECT id FROM users WHERE solder_handle = $1`, handle).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetSolderHandle returns an account's handle, or "" when it has not chosen one.
func (s *PostgresStore) GetSolderHandle(userID string) (string, error) {
	var h string
	err := s.db.QueryRow(`SELECT COALESCE(solder_handle, '') FROM users WHERE id = $1`, userID).Scan(&h)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return h, err
}

// SetSolderHandle claims a handle for an account, once.
//
// The WHERE clause carries both rules, so neither can be lost by a caller:
// solder_handle = ” makes this a claim rather than a rename, and the unique
// index makes it a claim nobody else already holds. Zero rows means the account
// already has one; a unique violation means somebody else does.
func (s *PostgresStore) SetSolderHandle(userID, handle string) error {
	res, err := s.db.Exec(
		`UPDATE users SET solder_handle = $2 WHERE id = $1 AND COALESCE(solder_handle, '') = ''`,
		userID, handle)
	if isUniqueViolation(err) {
		return ErrNameTaken
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrSolderHandleSet
	}
	return nil
}

// ListPublicSolderPacksByOwner is ListPublicSolderPacks narrowed to one account.
//
// The unnarrowed one listed every tenant's public packs together, so opening the
// shared Solder URL enumerated the whole platform's modpacks. Per-account URLs
// list only their own.
func (s *PostgresStore) ListPublicSolderPacksByOwner(ownerID string) ([]models.Pack, error) {
	rows, err := s.db.Query(`SELECT `+packCols+` FROM packs
		WHERE owner_id = $1 AND solder_slug <> '' AND private = false AND hidden = false
		ORDER BY internal_name ASC`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Pack
	for rows.Next() {
		p, err := scanPack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetPackBySolderSlugForOwner resolves a slug WITHIN one account.
//
// Slugs are unique per owner now (packs_owner_solder_slug_uniq), not globally,
// so a bare slug is only an address when the account is known - which is exactly
// what the per-account URL supplies. Returns (nil, nil) when that account has no
// pack with that slug.
func (s *PostgresStore) GetPackBySolderSlugForOwner(ownerID, slug string) (*models.Pack, error) {
	row := s.db.QueryRow(`SELECT `+packCols+` FROM packs WHERE owner_id = $1 AND solder_slug = $2`, ownerID, slug)
	p, err := scanPack(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}
