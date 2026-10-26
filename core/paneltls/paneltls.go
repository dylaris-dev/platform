// Package paneltls serves the panel over HTTPS from Core itself, for a
// self-hosted install that has no reverse proxy in front of it.
//
// It is an EXTRA listener beside the plain HTTP one, never a replacement: the
// container healthcheck, a proxy that already terminates TLS and every existing
// deployment keep talking to API_PORT exactly as before. Off unless PANEL_TLS
// says otherwise.
package paneltls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Mode is how the certificate is obtained.
type Mode string

const (
	Off        Mode = "off"
	SelfSigned Mode = "self-signed"
	Files      Mode = "files"
)

// Settings are read from the environment by FromEnv.
type Settings struct {
	Mode     Mode
	Port     string
	CertFile string   // Files mode
	KeyFile  string   // Files mode
	Hosts    []string // names and IPs the self-signed certificate is valid for
	Dir      string   // where a self-signed certificate is kept
}

// FromEnv reads PANEL_TLS (off|self-signed|files), PANEL_TLS_PORT (25443),
// PANEL_TLS_CERT_FILE, PANEL_TLS_KEY_FILE and PANEL_TLS_HOSTS (comma list).
// An unknown mode is an error rather than a silent "off": an operator who
// typed it expects HTTPS.
func FromEnv(getenv func(string) string, dataDir string) (Settings, error) {
	s := Settings{
		Mode:     Mode(strings.ToLower(strings.TrimSpace(getenv("PANEL_TLS")))),
		Port:     strings.TrimSpace(getenv("PANEL_TLS_PORT")),
		CertFile: strings.TrimSpace(getenv("PANEL_TLS_CERT_FILE")),
		KeyFile:  strings.TrimSpace(getenv("PANEL_TLS_KEY_FILE")),
		Dir:      filepath.Join(dataDir, "panel-tls"),
	}
	if s.Mode == "" {
		s.Mode = Off
	}
	if s.Port == "" {
		s.Port = "25443"
	}
	for _, h := range strings.Split(getenv("PANEL_TLS_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			s.Hosts = append(s.Hosts, h)
		}
	}
	switch s.Mode {
	case Off, SelfSigned:
	case Files:
		if s.CertFile == "" || s.KeyFile == "" {
			return s, fmt.Errorf("PANEL_TLS=files needs PANEL_TLS_CERT_FILE and PANEL_TLS_KEY_FILE")
		}
	default:
		return s, fmt.Errorf("PANEL_TLS=%q: use off, self-signed or files", s.Mode)
	}
	return s, nil
}

// Certificate returns the certificate to serve and its SHA-256 fingerprint
// (hex, so an operator can compare it with what the browser shows).
func (s Settings) Certificate(now time.Time) (tls.Certificate, string, error) {
	certFile, keyFile := s.CertFile, s.KeyFile
	if s.Mode == SelfSigned {
		certFile, keyFile = filepath.Join(s.Dir, "panel.crt"), filepath.Join(s.Dir, "panel.key")
		if err := ensureSelfSigned(certFile, keyFile, s.Hosts, now); err != nil {
			return tls.Certificate{}, "", err
		}
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("load panel certificate: %w", err)
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return cert, hex.EncodeToString(sum[:]), nil
}

// ensureSelfSigned keeps an existing certificate while it still covers every
// configured host and has more than 30 days left; otherwise it writes a new one.
// Keeping it is the point: a browser exception the operator accepted once
// stays valid across restarts.
func ensureSelfSigned(certFile, keyFile string, hosts []string, now time.Time) error {
	if data, err := os.ReadFile(certFile); err == nil {
		if block, _ := pem.Decode(data); block != nil {
			if c, err := x509.ParseCertificate(block.Bytes); err == nil && c.NotAfter.Sub(now) > 30*24*time.Hour && covers(c, hosts) {
				if _, err := os.Stat(keyFile); err == nil {
					return nil
				}
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "DYLARIS panel", Organization: []string{"DYLARIS self-hosted"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(2, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range append([]string{"localhost"}, hosts...) {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP("127.0.0.1"))
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func covers(c *x509.Certificate, hosts []string) bool {
	for _, h := range hosts {
		if c.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}
