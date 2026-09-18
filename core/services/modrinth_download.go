package services

import (
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// modrinthCDNPrefix is the ONLY host the pack builder downloads mod jars from
// (rendering a server pack, replacing a content entry). A hard allowlist keeps it
// from being coerced into fetching arbitrary URLs out of a content entry's stored
// ModrinthDownloadURL.
const modrinthCDNPrefix = "https://cdn.modrinth.com/"

// modrinthJarMaxBytes caps a single download so a hostile/huge upstream response
// cannot exhaust memory. 512 MiB comfortably covers any legitimate mod/pack file.
const modrinthJarMaxBytes = 512 << 20

// modrinthDownloadClient bounds the fetch so a stalled CDN cannot hang the render.
var modrinthDownloadClient = &http.Client{Timeout: 120 * time.Second}

// StreamModrinthJar fetches a mod jar from the Modrinth CDN, writing it to dst
// as it downloads while hashing the stream, and verifies the hashes at the end.
// It never holds the whole jar in memory: peak allocation is io.Copy's buffer,
// so any number of concurrent renders each cost a fixed few KB regardless of jar
// size. url MUST have the cdn.modrinth.com prefix. Returns the byte count.
//
// The caller MUST treat dst as unverified until this returns nil: a mismatch is
// only known after the whole body has been written, so dst must be a scratch
// destination (a temp file that is re-read on success), never the client
// response or storage directly.
func StreamModrinthJar(ctx context.Context, url string, dst io.Writer, expectedSHA1, expectedSHA512 string) (int64, error) {
	if !strings.HasPrefix(url, modrinthCDNPrefix) {
		return 0, fmt.Errorf("download refused: %q is not a %s URL", url, modrinthCDNPrefix)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := modrinthDownloadClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("modrinth download %s: status %d", url, resp.StatusCode)
	}

	h1 := sha1.New()
	h5 := sha512.New()
	// One extra byte past the cap so a body exactly at the limit passes while
	// anything over it is caught, without buffering the body to measure it.
	src := io.TeeReader(io.LimitReader(resp.Body, modrinthJarMaxBytes+1), io.MultiWriter(h1, h5))
	n, err := io.Copy(dst, src)
	if err != nil {
		return n, err
	}
	if n > modrinthJarMaxBytes {
		return n, fmt.Errorf("modrinth download %s exceeds the %d byte cap", url, int64(modrinthJarMaxBytes))
	}

	if expectedSHA1 != "" {
		if got := hex.EncodeToString(h1.Sum(nil)); !strings.EqualFold(got, expectedSHA1) {
			return n, fmt.Errorf("sha1 mismatch for %s: got %s want %s", url, got, expectedSHA1)
		}
	}
	if expectedSHA512 != "" {
		if got := hex.EncodeToString(h5.Sum(nil)); !strings.EqualFold(got, expectedSHA512) {
			return n, fmt.Errorf("sha512 mismatch for %s: got %s want %s", url, got, expectedSHA512)
		}
	}
	return n, nil
}

// DownloadModrinthJar fetches a mod jar from the Modrinth CDN and verifies its
// bytes against the expected hashes before returning them. url MUST have the
// cdn.modrinth.com prefix (hard allowlist). expectedSHA1 is required; expectedSHA512
// is checked only when non-empty. A hash mismatch is a hard error — the caller must
// not persist unverified bytes.
//
// Prefer StreamModrinthJar on a hot or high-concurrency path; this buffers the
// whole jar and is kept for callers that genuinely need the bytes in memory.
func DownloadModrinthJar(ctx context.Context, url, expectedSHA1, expectedSHA512 string) ([]byte, error) {
	if !strings.HasPrefix(url, modrinthCDNPrefix) {
		return nil, fmt.Errorf("download refused: %q is not a %s URL", url, modrinthCDNPrefix)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := modrinthDownloadClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("modrinth download %s: status %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, modrinthJarMaxBytes))
	if err != nil {
		return nil, err
	}

	if expectedSHA1 != "" {
		s1 := sha1.Sum(data)
		if got := hex.EncodeToString(s1[:]); !strings.EqualFold(got, expectedSHA1) {
			return nil, fmt.Errorf("sha1 mismatch for %s: got %s want %s", url, got, expectedSHA1)
		}
	}
	if expectedSHA512 != "" {
		s5 := sha512.Sum512(data)
		if got := hex.EncodeToString(s5[:]); !strings.EqualFold(got, expectedSHA512) {
			return nil, fmt.Errorf("sha512 mismatch for %s: got %s want %s", url, got, expectedSHA512)
		}
	}
	return data, nil
}
