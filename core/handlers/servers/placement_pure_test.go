package servers

import (
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
