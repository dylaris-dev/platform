package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsPublicUnicast(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", false},
		{"10.0.0.10", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // cloud metadata
		{"100.64.0.1", false},      // CGNAT
		{"0.0.0.0", false},
		{"224.0.0.1", false},
		{"::1", false},
		{"fc00::1", false},
		{"fe80::1", false},
		{"1.1.1.1", true},
		{"94.130.98.3", true},
		{"2606:4700::1111", true},
	}
	for _, c := range cases {
		if got := isPublicUnicast(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("isPublicUnicast(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
	if isPublicUnicast(nil) {
		t.Error("nil IP must not count as public")
	}
}

// allowDials swaps the dial policy for one test.
func allowDials(t *testing.T, f func(host, port string) bool) {
	t.Helper()
	prev := dialAllowed
	dialAllowed = f
	t.Cleanup(func() { dialAllowed = prev })
}

func portOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

func TestGuardedDownloadRefusesPrivateAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "f")
	err := downloadFileGuarded(srv.URL, dst, 0)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("download from loopback: err = %v, want a refusal", err)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("a refused download left a file behind")
	}
}

// A redirect is dialled like any other address, so a permitted page pointing
// at a forbidden one gets no further than the forbidden one would directly.
func TestGuardedDownloadRefusesRedirectToForbidden(t *testing.T) {
	hit := false
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte("secret"))
	}))
	defer internal.Close()
	front := httptest.NewServer(http.RedirectHandler(internal.URL, http.StatusFound))
	defer front.Close()

	frontPort := portOf(t, front.URL)
	allowDials(t, func(_, port string) bool { return port == frontPort })

	err := downloadFileGuarded(front.URL, filepath.Join(t.TempDir(), "f"), 0)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v, want the redirect hop refused", err)
	}
	if hit {
		t.Error("the forbidden server was reached")
	}
}

func TestGuardedDownloadCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("01234567890")) // 11 bytes
	}))
	defer srv.Close()
	allowDials(t, func(string, string) bool { return true })

	dir := t.TempDir()
	if err := downloadFileGuarded(srv.URL, filepath.Join(dir, "ok"), 11); err != nil {
		t.Fatalf("exactly at the cap: %v", err)
	}
	err := downloadFileGuarded(srv.URL, filepath.Join(dir, "big"), 10)
	if !errors.Is(err, errDownloadTooLarge) {
		t.Fatalf("over the cap: err = %v, want errDownloadTooLarge", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "big")); statErr == nil {
		t.Error("an oversized download was kept")
	}
}

func TestInstallerDownloadsSendUserAgent(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.UserAgent())
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	allowDials(t, func(string, string) bool { return true })

	dir := t.TempDir()
	if err := downloadFile(srv.URL, filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	if err := downloadFileGuarded(srv.URL, filepath.Join(dir, "b"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadFileBounded(srv.URL, filepath.Join(dir, "c"), 100); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := fetchJSON(srv.URL, &v); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("requests = %d, want 4", len(got))
	}
	for i, ua := range got {
		if !strings.HasPrefix(ua, "Dylaris/") || !strings.Contains(ua, "https://dylaris.com") {
			t.Errorf("request %d User-Agent = %q", i, ua)
		}
	}
}
