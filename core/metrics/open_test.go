package metrics

import (
	"testing"

	"github.com/lib/pq"
)

// Asked of lib/pq's own parser, so the test fails if the option is spelled in
// a way the driver ignores, not just if the string lacks it.
func TestWithBinaryParameters(t *testing.T) {
	for _, dsn := range []string{
		// The form MetricsDBTarget.DSN builds.
		"host='db' port='6432' user='m' password='p w' dbname='stats' sslmode='disable'",
		"postgres://m:p%40w@db:6432/stats?sslmode=disable",
		"postgresql://m@db:6432/stats",
	} {
		t.Run(dsn, func(t *testing.T) {
			before, err := pq.NewConfig(dsn)
			if err != nil {
				t.Fatalf("fixture does not parse: %v", err)
			}
			if before.BinaryParameters {
				t.Fatal("fixture already has binary_parameters")
			}
			after, err := pq.NewConfig(withBinaryParameters(dsn))
			if err != nil {
				t.Fatalf("result does not parse: %v", err)
			}
			if !after.BinaryParameters {
				t.Error("binary_parameters is not on")
			}
			after.BinaryParameters = false
			if after.Host != before.Host || after.Port != before.Port || after.User != before.User ||
				after.Password != before.Password || after.Database != before.Database || after.SSLMode != before.SSLMode {
				t.Errorf("connection settings changed: %+v -> %+v", before, after)
			}
		})
	}
}
