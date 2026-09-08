package database

import (
	"database/sql"
	"fmt"
)

// Default ticket categories.
//
// Without at least one, the support inbox is CLOSED rather than empty:
// CreateTicket looks the category up and answers "Unknown or disabled category"
// to every attempt, so a user with a problem has no way to reach anyone. That
// is not a first-boot inconvenience, it is the state this platform was in on
// 2026-09-08 with the table empty and registration open.
//
// Seeded rather than documented for the same reason the panel roles are: an
// install that needs a manual INSERT before its support form works has a step
// nobody performs until somebody needs support, which is the worst moment to
// discover it.
//
// The set is deliberately short. Five choices a person can scan is a routing
// decision; fifteen is a quiz, and a mis-routed ticket costs more than a broad
// one. "Something else" is last and exists so nobody is ever stuck without a
// fit.

type ticketCategorySeed struct {
	Name           string
	Description    string
	RequiresServer bool
	Priority       string
	Color          string
	Position       int
}

func defaultTicketCategorySeeds() []ticketCategorySeed {
	return []ticketCategorySeed{
		{
			Name:           "Server issue",
			Description:    "A server will not start, crashes, or behaves incorrectly.",
			RequiresServer: true,
			Priority:       "high",
			Color:          "#E5484D",
			Position:       1,
		},
		{
			// Kept apart from "Server issue" because the answer comes from a
			// different place: this is the gateway path, not the container.
			Name:           "Connection and network",
			Description:    "Players cannot join, the address does not resolve, or the connection drops.",
			RequiresServer: false,
			Priority:       "high",
			Color:          "#F5A524",
			Position:       2,
		},
		{
			Name:           "Billing and payments",
			Description:    "Invoices, plan changes, refunds and anything about what you are charged.",
			RequiresServer: false,
			Priority:       "normal",
			Color:          "#30A46C",
			Position:       3,
		},
		{
			Name:           "Account and access",
			Description:    "Signing in, email address, two-factor, and account deletion.",
			RequiresServer: false,
			Priority:       "normal",
			Color:          "#7048C8",
			Position:       4,
		},
		{
			Name:           "Something else",
			Description:    "Anything that does not fit the categories above.",
			RequiresServer: false,
			Priority:       "normal",
			Color:          "#8B8D98",
			Position:       5,
		},
	}
}

// seedDefaultTicketCategories fills the table only while it is EMPTY.
//
// Not ON CONFLICT (name) on every boot, which is how the panel roles do it and
// is wrong here: those are is_system and undeletable, these are an operator's
// to rename, disable or delete. Seeding by name would resurrect a category
// somebody deleted on purpose at the very next restart, and the operator would
// have no way to make it stay gone.
//
// Empty is the one state worth acting on, because empty is not "no categories",
// it is a support inbox that REFUSES every ticket. An operator who wants a
// different set makes theirs and deletes ours; as long as one row survives,
// nothing comes back.
//
// ON CONFLICT (name) DO NOTHING is still there, as a race guard rather than a
// policy: two Core replicas boot together, both see an empty table, and both
// insert.
func seedDefaultTicketCategories(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ticket_categories`).Scan(&n); err != nil {
		return fmt.Errorf("count ticket categories: %w", err)
	}
	if n > 0 {
		return nil
	}
	for _, s := range defaultTicketCategorySeeds() {
		if _, err := db.Exec(
			`INSERT INTO ticket_categories
			   (name, description, requires_server, default_priority, color, enabled, position)
			 VALUES ($1, $2, $3, $4, $5, TRUE, $6)
			 ON CONFLICT (name) DO NOTHING`,
			s.Name, s.Description, s.RequiresServer, s.Priority, s.Color, s.Position); err != nil {
			return fmt.Errorf("seed ticket category %q: %w", s.Name, err)
		}
	}
	return nil
}
