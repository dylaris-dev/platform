package files

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
