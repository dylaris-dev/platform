package validate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
)

// ValidatePublicBaseURL accepts an empty value (the feature stays unconfigured
// and says so) or an absolute http/https URL with a host and no credentials.
//
// Stricter than corestorage.ValidateS3Endpoint, which only has to survive being handed to
// the AWS SDK. This value is published to third-party launchers as the base
// they download from, AND it is the host isSnapshotFetchHostAllowed compares
// against, so a relative or scheme-less string would both hand clients a URL
// they cannot resolve and widen an SSRF allowlist by accident.
func ValidatePublicBaseURL(subject, raw string) error {
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

func HashURL(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}
