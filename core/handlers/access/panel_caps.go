package access

import (
	"errors"

	"dylaris-core/authz"
)

// NormalizeCaps turns a nil capability list into an empty one, so JSON says [].
func NormalizeCaps(caps []string) []string {
	if caps == nil {
		return []string{}
	}
	return caps
}

// CatalogCaps is NormalizeCaps minus ids the catalog no longer has (e.g. the
// retired regions.*). Stored roles and overrides keep such ids, and the role
// editor sends back what it was shown, so showing one would make every later
// save fail ValidatePanelCaps with "unknown capability". Read views only; the
// audit metadata keeps the stored list as it was.
func CatalogCaps(caps []string) []string {
	out := make([]string, 0, len(caps))
	for _, id := range caps {
		if _, ok := authz.Get(id); ok {
			out = append(out, id)
		}
	}
	return out
}

// ValidatePanelCaps returns an error naming the first capability that is not a
// real PANEL-scope capability. The catalog is the single source of truth, so an
// unknown id or a server/owner-scope id is rejected (deny-by-default). An empty
// slice is valid. Reused by the per-user assignment endpoint (user_role_perms.go).
func ValidatePanelCaps(caps []string) error {
	for _, id := range caps {
		c, ok := authz.Get(id)
		if !ok {
			return errors.New("unknown capability: " + id)
		}
		if c.Scope != authz.ScopePanel {
			return errors.New("not a panel capability: " + id)
		}
	}
	return nil
}
