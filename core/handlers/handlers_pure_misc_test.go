package handlers

import (
	"errors"
	"testing"
)

func TestNormalizeTagList(t *testing.T) {
	got := normalizeTagList([]string{"Foo", " Bar ", "", "  ", "BAZ"})
	want := []string{"foo", "bar", "baz"}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}

	t.Run("nil input returns empty (non-nil) slice", func(t *testing.T) {
		got := normalizeTagList(nil)
		if len(got) != 0 {
			t.Errorf("got %#v, want empty slice", got)
		}
	})
}

func TestDedupe(t *testing.T) {
	got := dedupe([]string{"a", "b", "a", "c", "b"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}

	t.Run("case-sensitive - different case is NOT deduped", func(t *testing.T) {
		got := dedupe([]string{"X", "x"})
		if len(got) != 2 {
			t.Errorf("got %#v, want both entries kept (case-sensitive)", got)
		}
	})

	t.Run("empty input returns empty slice", func(t *testing.T) {
		got := dedupe(nil)
		if len(got) != 0 {
			t.Errorf("got %#v, want empty slice", got)
		}
	})
}

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
