package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
)

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// validatePublicBaseURL accepts an empty value (the feature stays unconfigured
// and says so) or an absolute http/https URL with a host and no credentials.
//
// Stricter than validateS3Endpoint, which only has to survive being handed to
// the AWS SDK. This value is published to third-party launchers as the base
// they download from, AND it is the host isSnapshotFetchHostAllowed compares
// against, so a relative or scheme-less string would both hand clients a URL
// they cannot resolve and widen an SSRF allowlist by accident.
func validatePublicBaseURL(subject, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", subject, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must start with http:// or https://", subject)
	}
	if u.Host == "" {
		return fmt.Errorf("%s must include a host, e.g. https://panel.example.com", subject)
	}
	if u.User != nil {
		return fmt.Errorf("%s must not contain credentials", subject)
	}
	return nil
}

func hashURL(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// maxCachedResponseBytes caps what one cache entry may weigh.
//
// Measured against the live API: a project's version list runs 290 KB for
// Sodium filtered to one loader, 494 KB unfiltered, and 1.19 MB for Fabric API.
// The proxy keys those per filter combination for an hour and the shipped Redis
// runs with no maxmemory, so without a cap the tail of that distribution is what
// decides how much memory the control plane has left. Over the cap the response
// is served straight through: one extra upstream call, no stored megabyte.
const maxCachedResponseBytes = 512 << 10
