package handlers

import (
	"testing"
)

func TestFormatBytesHuman(t *testing.T) {
	cases := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}
	for _, c := range cases {
		if got := formatBytesHuman(c.input); got != c.want {
			t.Errorf("formatBytesHuman(%d) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestSseEscape(t *testing.T) {
	cases := []struct{ input, want string }{
		{"line1\nline2", "line1 line2"},
		{"line1\r\nline2", "line1 line2"},
		{"line1\rline2", "line1 line2"},
		{"no newlines", "no newlines"},
	}
	for _, c := range cases {
		if got := sseEscape(c.input); got != c.want {
			t.Errorf("sseEscape(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}
