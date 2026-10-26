package paneltls

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		mode    Mode
		port    string
		wantErr bool
	}{
		{"unset is off", nil, Off, "25443", false},
		{"self-signed with hosts", map[string]string{"PANEL_TLS": "Self-Signed", "PANEL_TLS_HOSTS": "panel.example.com, 10.0.0.5"}, SelfSigned, "25443", false},
		{"files needs both paths", map[string]string{"PANEL_TLS": "files", "PANEL_TLS_CERT_FILE": "/c"}, Files, "25443", true},
		{"typo is an error, not off", map[string]string{"PANEL_TLS": "on"}, "on", "25443", true},
		{"own port", map[string]string{"PANEL_TLS": "off", "PANEL_TLS_PORT": "8443"}, Off, "8443", false},
	}
	for _, c := range cases {
		s, err := FromEnv(env(c.env), "/data")
		if (err != nil) != c.wantErr || s.Mode != c.mode || s.Port != c.port {
			t.Errorf("%s: mode %q port %q err %v", c.name, s.Mode, s.Port, err)
		}
	}
}

// The self-signed certificate is made once and kept, so a browser exception
// survives a restart; a new host name makes a new one.
func TestSelfSignedIsKeptAndCoversItsHosts(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	s, _ := FromEnv(env(map[string]string{"PANEL_TLS": "self-signed", "PANEL_TLS_HOSTS": "panel.example.com,192.168.1.20"}), dir)

	c1, fp1, err := s.Certificate(now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(c1.Certificate[0])
	for _, h := range []string{"panel.example.com", "192.168.1.20", "localhost", "127.0.0.1"} {
		if err := leaf.VerifyHostname(h); err != nil {
			t.Errorf("certificate does not cover %s: %v", h, err)
		}
	}
	if info, _ := os.Stat(filepath.Join(dir, "panel-tls", "panel.key")); info == nil || info.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("key file missing or not 0600: %v", info)
	}

	_, fp2, _ := s.Certificate(now.Add(time.Hour))
	if fp1 != fp2 {
		t.Error("a restart replaced a still-valid certificate")
	}
	s.Hosts = append(s.Hosts, "other.example.com")
	_, fp3, _ := s.Certificate(now)
	if fp3 == fp1 {
		t.Error("a new host name did not produce a new certificate")
	}
	_, fp4, _ := s.Certificate(now.AddDate(2, 0, 0))
	if fp4 == fp3 {
		t.Error("an expiring certificate was not renewed")
	}
}
