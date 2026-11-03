package main

import "testing"

func TestDownloadCallOpts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		isDir, lan bool
		gzip       bool
	}{
		{"world/server.properties", false, false, true},
		{"world/server.properties", false, true, false},
		{"world/region/r.0.0.mca", false, false, false},
		{"world/level.dat", false, false, false},
		{"world", true, false, false},
	} {
		got := len(downloadCallOpts(tc.name, tc.isDir, tc.lan)) > 0
		if got != tc.gzip {
			t.Errorf("downloadCallOpts(%q, dir=%v, lan=%v) gzip = %v, want %v", tc.name, tc.isDir, tc.lan, got, tc.gzip)
		}
	}
}
