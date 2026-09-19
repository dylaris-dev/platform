package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
)

// installerUserAgent names DYLARIS and a contact URL on every installer
// download. Modrinth's API docs threaten to block generic agents, and Go's
// default "Go-http-client/1.1" is exactly that.
func installerUserAgent() string {
	v := "dev"
	if rv := nodeReleaseVersion(); !rv.IsZero() {
		v = rv.String()
	}
	return "Dylaris/" + v + " (+https://dylaris.com)"
}

// uaTransport sets the User-Agent on every request that leaves through it, so
// no call site can forget it.
type uaTransport struct{ base http.RoundTripper }

func (t uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A RoundTripper must not modify the caller's request.
	r := req.Clone(req.Context())
	r.Header.Set("User-Agent", installerUserAgent())
	return t.base.RoundTrip(r)
}

// The Technic installer downloads from a host the PACK AUTHOR chooses. Our own
// nodes sit in the private network next to Redis and Postgres, so this client
// refuses every address that is not public unicast. The check runs in
// Dialer.Control, on the address actually dialled: a DNS answer or a redirect
// that points inside is refused the same way.

var cgnatRange = func() *net.IPNet {
	_, n, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		panic(err)
	}
	return n
}()

func isPublicUnicast(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && cgnatRange.Contains(v4) {
		return false
	}
	return true
}

// dialAllowed decides each dial. A variable only so tests can serve from
// httptest on loopback.
var dialAllowed = func(host, _ string) bool { return isPublicUnicast(net.ParseIP(host)) }

var errDownloadTooLarge = errors.New("download exceeds the size limit")

var guardedDownloadClient = &http.Client{
	Transport: uaTransport{base: &http.Transport{
		// No proxy: a proxy would dial for us and the check would only ever see
		// the proxy's address. A node behind a mandatory proxy cannot install
		// Technic packs; that is the price of the check.
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout: 15 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				host, port, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				if !dialAllowed(host, port) {
					return fmt.Errorf("refused: %s is not a public address", host)
				}
				return nil
			},
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}},
}

// downloadFileGuarded downloads through the guarded client, with the same stall
// watchdog as every installer download, and fails once more than maxBytes
// arrive instead of keeping a truncated file.
func downloadFileGuarded(url, destPath string, maxBytes int64) error {
	err := downloadWith(guardedDownloadClient, url, destPath, installerStallTimeout, maxBytes)
	if err != nil {
		os.Remove(destPath)
	}
	return err
}

// cappedWriter fails the copy on the first byte past the cap.
type cappedWriter struct {
	w    io.Writer
	left int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > c.left {
		return 0, errDownloadTooLarge
	}
	c.left -= int64(len(p))
	return c.w.Write(p)
}
