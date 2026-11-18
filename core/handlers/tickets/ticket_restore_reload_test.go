package tickets

import (
	"regexp"
	"strings"
	"testing"

	"dylaris-core/store"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	reloadKept = "11111111-1111-4111-8111-111111111111"
	reloadGone = "22222222-2222-4222-8222-222222222222"
)

func expectWipe(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	tables := store.TicketTablesInOrder()
	for i := len(tables) - 1; i >= 0; i-- {
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM " + tables[i])).WillReturnResult(sqlmock.NewResult(0, 0))
	}
}

func expectSetval(mock sqlmock.Sqlmock, table string) {
	mock.ExpectExec(regexp.QuoteMeta("SELECT setval(pg_get_serial_sequence('" + table + "', 'id')")).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// wipeAndReload must (a) refuse a backup key that is not a plain identifier,
// like bulkInsertRows does, (b) apply the live FK rules to users deleted
// since the backup instead of failing the whole restore, and (c) move the
// serial sequences past the restored ids so the next ticket does not collide.
func TestWipeAndReload(t *testing.T) {
	t.Run("unsafe column name is refused", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectWipe(mock)
		mock.ExpectRollback()
		err = wipeAndReload(&ticketBackupFakeStore{db: db}, map[string][]map[string]interface{}{
			"ticket_categories": {{"id": float64(1), "name) VALUES (1); DROP TABLE users; --": "x"}},
		})
		if err == nil || !strings.Contains(err.Error(), "unsafe column") {
			t.Fatalf("err = %v, want an unsafe column refusal", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("deleted users follow the FK rules and sequences move", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectWipe(mock)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT id::text FROM users")).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(reloadKept))
		// Ticket 1 is kept; its assignee was deleted, so assigned_user_id clears.
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO tickets (assigned_user_id, id, user_id)")).
			WithArgs(nil, float64(1), reloadKept).WillReturnResult(sqlmock.NewResult(0, 1))
		expectSetval(mock, "tickets")
		// Ticket 2's author was deleted: CASCADE drops it and its message.
		// The message on ticket 1 by the deleted user keeps the body, loses the author.
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ticket_messages (id, ticket_id, user_id)")).
			WithArgs(float64(10), float64(1), nil).WillReturnResult(sqlmock.NewResult(0, 1))
		expectSetval(mock, "ticket_messages")
		mock.ExpectCommit()

		err = wipeAndReload(&ticketBackupFakeStore{db: db}, map[string][]map[string]interface{}{
			"tickets": {
				{"id": float64(1), "user_id": reloadKept, "assigned_user_id": reloadGone},
				{"id": float64(2), "user_id": reloadGone},
			},
			"ticket_messages": {
				{"id": float64(10), "ticket_id": float64(1), "user_id": reloadGone},
				{"id": float64(11), "ticket_id": float64(2), "user_id": reloadKept},
			},
		})
		if err != nil {
			t.Fatalf("wipeAndReload: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
}
