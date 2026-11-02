package handlers

import (
	"errors"
	"testing"
)

// TestDbUnavailable pins the transient-vs-genuine-failure classifier
// (auth.go) that drives the 503-vs-500 choice on login.
func TestDbUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"connection refused", errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"), true},
		{"no such host", errors.New("dial tcp: lookup db.internal: no such host"), true},
		{"broken pipe", errors.New("write: broken pipe"), true},
		{"i/o timeout", errors.New("read tcp: i/o timeout"), true},
		{"database starting up", errors.New("pq: the database system is starting up"), true},
		{"schema not yet rebuilt", errors.New("pq: relation \"users\" does not exist"), true},
		{"case-insensitive match", errors.New("CONNECTION REFUSED"), true},
		{"genuine query error is not transient", errors.New("pq: syntax error at or near \"SELCT\""), false},
		{"generic not-found error is not transient", errors.New("record not found"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dbUnavailable(c.err); got != c.want {
				t.Errorf("dbUnavailable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
