package main

import (
	"testing"
)

func TestIsProtectedFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{".active_server", true},
		{"servers/uuid/.active_server", true},
		{"/var/data/.active_server", true},
		{"server.jar", false},
		{".active_server_backup", false},
		{"eula.txt", false},
	}
	for _, c := range cases {
		if got := isProtectedFile(c.path); got != c.want {
			t.Errorf("isProtectedFile(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		input uint64
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
		if got := formatBytes(c.input); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.input, got, c.want)
		}
	}
}
