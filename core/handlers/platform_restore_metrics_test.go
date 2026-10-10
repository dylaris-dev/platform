package handlers

import (
	"context"
	"strings"
	"testing"

	"dylaris-core/services"
)

func TestSameDatabase(t *testing.T) {
	base := services.DBConnParams{Host: "db", Port: "5432", DBName: "dylaris"}
	cases := []struct {
		name string
		b    services.DBConnParams
		want bool
	}{
		{"identical", base, true},
		{"host case and default port", services.DBConnParams{Host: "DB", DBName: "dylaris"}, true},
		{"other database", services.DBConnParams{Host: "db", Port: "5432", DBName: "metrics"}, false},
		{"other port", services.DBConnParams{Host: "db", Port: "6432", DBName: "dylaris"}, false},
		{"other host", services.DBConnParams{Host: "db2", Port: "5432", DBName: "dylaris"}, false},
	}
	for _, tc := range cases {
		if got := sameDatabase(base, tc.b); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Refused before either database is opened: nothing listens on these hosts, so
// reaching a connection attempt would answer with a different error.
func TestRestoreRefusesOneDatabaseForBothDumps(t *testing.T) {
	h := &PlatformBackupHandler{state: &AppState{}}
	target := restoreTargetRequest{Host: "nowhere.invalid", User: "u", DBName: "restored"}
	_, _, err := h.restorer(context.Background(), restoreRequest{
		Components:    services.PlatformRestoreSelection{Database: true, MetricsDB: true},
		Target:        target,
		MetricsTarget: target,
	})
	if err == nil || !strings.Contains(err.Error(), "two different databases") {
		t.Fatalf("err = %v", err)
	}
}
