package store

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSetGatewayCustomDomainLimit_NegativeIsWrittenAsNoCap(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectExec("gateway_custom_domain_limits").
		WithArgs("global", nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	n := int64(-1)
	if err := NewPostgresStore(db).SetGatewayCustomDomainLimit("global", &n); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
