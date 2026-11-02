package corestorage

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidateS3Endpoint refuses an endpoint that carries a credential in its
// userinfo component (https://AKIA...:secret@minio.internal). subject names the
// settings group in the error so the operator knows which form to fix.
//
// Why this is a VALIDATION rule and not only a rendering concern: the endpoint
// is persisted verbatim (core_storage_s3_endpoint, modpack_storage_s3_endpoint)
// and it is the raw material for every backend label the migration engine
// writes - into storage_manifests.backend_label (durable, in Postgres), the
// Redis job record (7-day TTL, readable by every settings.read holder), the
// panel job log and the manifest CSV export. The modpack endpoint is echoed
// back by the modpack settings GET on top of that. Sanitizing at render time
// alone leaves the credential sitting in the settings table; refusing it here
// means it never enters the system. storagemigrate.SanitizeBackendLabel is the
// second half of the same guarantee, for configs this check never saw.
//
// It FAILS CLOSED on "@", exactly as sanitizeEndpoint does: a valid S3 endpoint
// host cannot contain one, and a password with a space or a control character
// defeats url.Parse, so "parses cleanly with no userinfo" is not a safe
// acceptance test. An EMPTY endpoint is allowed - that is the AWS default,
// where the region alone selects the host.
func ValidateS3Endpoint(subject, endpoint string) error {
	if endpoint == "" {
		return nil
	}
	if strings.Contains(endpoint, "@") {
		return fmt.Errorf("%s: s3 endpoint must not contain credentials; supply the access key and secret in their own fields, and give the endpoint as scheme://host[:port]", subject)
	}
	if _, err := url.Parse(endpoint); err != nil {
		return fmt.Errorf("%s: s3 endpoint is not a valid URL: %w", subject, err)
	}
	return nil
}
