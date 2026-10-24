package handlers

import (
	"errors"
	"testing"
)

func TestNormalizeLibraryPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already normalized", "foo/bar", "foo/bar"},
		{"leading slash stripped", "/foo/bar", "foo/bar"},
		{"trailing slash stripped", "foo/bar/", "foo/bar"},
		{"both leading and trailing stripped", "/foo/bar/", "foo/bar"},
		{"multiple leading and trailing slashes fully stripped", "///foo///", "foo"},
		{"whitespace trimmed first", "  foo/bar  ", "foo/bar"},
		{"empty stays empty", "", ""},
		{"single slash normalizes to empty (root)", "/", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeLibraryPath(c.in); got != c.want {
				t.Errorf("normalizeLibraryPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestJoinLibraryPath(t *testing.T) {
	cases := []struct {
		name   string
		parent string
		child  string
		want   string
	}{
		{"empty parent returns child unchanged", "", "file.txt", "file.txt"},
		{"root-normalized empty parent returns child unchanged", "/", "file.txt", "file.txt"},
		{"non-empty parent joined with slash", "foo", "bar.txt", "foo/bar.txt"},
		{"parent with slashes normalized before joining", "/foo/", "bar.txt", "foo/bar.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := joinLibraryPath(c.parent, c.child); got != c.want {
				t.Errorf("joinLibraryPath(%q, %q) = %q, want %q", c.parent, c.child, got, c.want)
			}
		})
	}
}

func TestIsPathBlocked(t *testing.T) {
	disabled := map[string]struct{}{
		"foo":        {},
		"baz/nested": {},
	}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"root path is never blockable", "", false},
		{"exact match is blocked", "foo", true},
		{"descendant of a blocked ancestor is blocked", "foo/bar", true},
		{"deep descendant of a blocked ancestor is blocked", "foo/bar/baz", true},
		{"unrelated path is not blocked", "other/bar", false},
		{"blocked nested path with unrelated top-level sibling", "baz/nested/child", true},
		{"sibling of a blocked path is not blocked", "baz/other", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPathBlocked(c.path, disabled); got != c.want {
				t.Errorf("isPathBlocked(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}

	t.Run("empty disabled set blocks nothing", func(t *testing.T) {
		if isPathBlocked("foo/bar", map[string]struct{}{}) {
			t.Errorf("expected false with an empty disabled set")
		}
	})
}

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
