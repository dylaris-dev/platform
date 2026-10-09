package handlers

import (
	"bytes"
	"compress/gzip"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Rotated server logs are .log.gz; opening one showed the compressed bytes.
func TestOpeningAGzipShowsItDecompressedAndReadonly(t *testing.T) {
	logText := "[12:00:00] [Server thread/INFO]: Done\n"
	gz := gzipped(t, logText)
	bomb := gzipped(t, strings.Repeat("a", 101))

	for _, tc := range []struct {
		name         string
		path         string
		data         []byte
		wantContent  string
		wantReadonly bool
		wantStatus   int
	}{
		{"gzip is decompressed", "logs/2026-10-09-1.log.gz", gz, logText, true, 0},
		{"suffix is case-insensitive", "logs/OLD.LOG.GZ", gz, logText, true, 0},
		{"plain file untouched", "logs/latest.log", []byte(logText), logText, false, 0},
		{"inflates past the cap", "logs/big.log.gz", bomb, "", true, http.StatusRequestEntityTooLarge},
		{"not gzip at all", "logs/fake.log.gz", []byte(logText), "", true, http.StatusUnprocessableEntity},
		{"truncated gzip", "logs/cut.log.gz", gz[:len(gz)-6], "", true, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, ro, err := decodeOpenedFile(tc.path, tc.data, 100)
			if tc.wantStatus != 0 {
				if err == nil {
					t.Fatalf("no error, want status %d", tc.wantStatus)
				}
				if got := readErrStatus(err); got != tc.wantStatus {
					t.Fatalf("status %d, want %d (err %v)", got, tc.wantStatus, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err %v", err)
			}
			if string(out) != tc.wantContent || ro != tc.wantReadonly {
				t.Fatalf("content %q readonly %v, want %q %v", out, ro, tc.wantContent, tc.wantReadonly)
			}
		})
	}

	// Exactly at the cap still opens: the limit is "over max", like collectNodeFile.
	if _, _, err := decodeOpenedFile("a.gz", gzipped(t, strings.Repeat("a", 100)), 100); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	if _, _, err := decodeOpenedFile("a.gz", bomb, 100); !errors.Is(err, errGzipTooLargeToOpen) {
		t.Fatalf("over the cap: %v, want errGzipTooLargeToOpen", err)
	}
	// The demo filter keys on the base name, so a decompressed log stays hidden.
	if got := demoFileContent("logs/latest.log.gz", logText); got != demoHiddenNotice {
		t.Fatalf("demo .gz content %q, want the hidden notice", got)
	}
}
