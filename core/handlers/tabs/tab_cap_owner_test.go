package tabs

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// created_by is ON DELETE SET NULL, so a tab outlives the account that made it.
// With nobody to charge, the caps were skipped outright - which made "delete the
// creator" a way to uncap a tab, and the flip that turns a free direct tab into
// a proxied one is exactly where that mattered.
func TestUpdateTab_OwnerlessTabIsChargedToTheCaller(t *testing.T) {
	h, mock := newTabCapHandler(t, 3, 10, 20)

	mock.ExpectQuery(`SELECT mode FROM server_tabs`).
		WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("direct"))
	// No owner on the row.
	mock.ExpectQuery(`SELECT COALESCE\(created_by`).
		WillReturnRows(sqlmock.NewRows([]string{"created_by"}).AddRow(""))
	// The caller is charged instead, and they are already at their total.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM server_tabs\s+WHERE mode='proxied'`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(10))

	rec := httptest.NewRecorder()
	h.Update(rec, tabPatchRequest(`{"mode":"proxied","targetPort":8100,"targetPath":"/","surface":"tab","visibility":"private"}`))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 - an ownerless tab must still be charged to somebody (body=%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}
