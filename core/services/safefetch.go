package services

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// isDisallowedIP reports whether an IP must never be dialed by the import
// fetcher. It blocks loopback, private (RFC1918 / IPv6 ULA), link-local
// (including the cloud metadata endpoint 169.254.169.254 and fe80::/10),
// unspecified, and multicast ranges. IPv4-mapped IPv6 is normalized first so
// that e.g. ::ffff:10.0.0.1 is caught as private.
func isDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	// 100.64.0.0/10 (CGNAT, RFC 6598) is a standard SSRF blocklist range that
	// IsPrivate does not cover.
	if len(ip) == net.IPv4len && ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127 {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// safeFetchDialer validates the concrete post-resolution IP right before
// connect. Because Control runs on the already-resolved address (not the
// hostname), it also defeats DNS-rebinding: a name that resolves to a public
// IP at lookup time but a private IP at dial time is still rejected here.
var safeFetchDialer = &net.Dialer{
	Timeout:   10 * time.Second,
	KeepAlive: -1,
	Control: func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || isDisallowedIP(ip) {
			return fmt.Errorf("blocked non-public address: %s", address)
		}
		return nil
	},
}

// safeFetchClient is the hardened outbound client. Redirects are re-validated
// (scheme + hop cap) and every dial - initial or redirected - passes through
// safeFetchDialer.Control, so an internal redirect target is blocked too. It
// has no client-level Timeout: SafeFetch bounds each call via the context.
var safeFetchClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           safeFetchDialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		DisableKeepAlives:     true,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("disallowed redirect scheme: %s", req.URL.Scheme)
		}
		return nil
	},
}

// SafeDialContext is the SSRF-guarded dial safeFetchClient uses, exported so a
// caller that must STREAM its response can get the same protection. SafeFetch
// buffers into a []byte and caps the size, which is right for a manifest and
// wrong for a multi-hundred-megabyte binary; such a caller builds its own
// http.Client and sets this as its Transport.DialContext.
//
// The check runs on the already-resolved address, so it covers every hop of a
// redirect chain and defeats DNS rebinding the same way it does for SafeFetch.
// What it does NOT bring along is SafeFetch's scheme check and size cap -
// those belong to the caller, because a streaming caller has its own answer
// for both.
func SafeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return safeFetchDialer.DialContext(ctx, network, addr)
}

// SafeFetch GETs a user-supplied URL through the hardened client and returns
// the response body, capped at maxBytes. The URL must be http/https; the
// dialer blocks any request that resolves to a non-public address. A body
// larger than maxBytes is an error (not silently truncated). timeout bounds
// the whole call (connect + headers + body).
func SafeFetch(ctx context.Context, rawURL string, maxBytes int64, timeout time.Duration) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme not allowed: %q (use http/https)", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("missing host")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", DylarisUserAgent())
	resp, err := safeFetchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("response exceeds %d byte cap", maxBytes)
	}
	return body, nil
}

// SafeHead issues an SSRF-guarded HEAD and returns the HTTP status code. Unlike
// SafeFetch it does NOT require 200 — the caller classifies the status (e.g.
// 401/403 => not publicly readable). It reuses safeFetchClient, whose dialer
// rejects private/loopback/link-local targets at dial time, so this cannot be
// an SSRF lever. A guarded/blocked target surfaces as a transport error.
func SafeHead(ctx context.Context, rawURL string, timeout time.Duration) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, fmt.Errorf("safehead: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return 0, fmt.Errorf("safehead: unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return 0, fmt.Errorf("safehead: missing host")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := safeFetchClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}
