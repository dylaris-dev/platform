package backup

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// The SDK default is RequestChecksumCalculationWhenSupported, which attaches a
// CRC32 to every PutObject. When the body is neither seekable nor
// length-known - a backup archive streaming out of an io.Pipe, for instance -
// that checksum travels in an aws-chunked TRAILER, and an S3-compatible backend
// that does not implement the trailer reads the payload as empty and answers:
//
//	BadDigest: ... You provided a CRC32 checksum with value: da5532b3
//	Actual CRC32 was: 00000000
//
// Measured against Cloudflare R2, where it made every backup write fail while
// the core-storage probe beside it passed - that one used a seekable
// strings.NewReader, so its checksum went in a header instead.
//
// This client is the object store behind BOTH server backups and Core file
// storage (storage/s3provider.go), so the option has to survive here.
func TestNewS3DisablesDefaultRequestChecksums(t *testing.T) {
	raw, err := json.Marshal(S3Config{
		Endpoint:        "https://example.r2.cloudflarestorage.com",
		Region:          "auto",
		Bucket:          "dylaris",
		AccessKeyID:     "key",
		SecretAccessKey: "secret",
		ForcePathStyle:  true,
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	st, err := NewS3(context.Background(), raw)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}

	got := st.client.Options().RequestChecksumCalculation
	if got != aws.RequestChecksumCalculationWhenRequired {
		t.Errorf("RequestChecksumCalculation = %v, want %v (WhenRequired); "+
			"the default WhenSupported sends a trailing CRC32 that R2 rejects on an unseekable body",
			got, aws.RequestChecksumCalculationWhenRequired)
	}
}

// The endpoint and path-style options must survive alongside it - a config
// function that returns early or overwrites o would silently drop them, and a
// wrong endpoint fails in a way that looks like a credential problem.
func TestNewS3KeepsEndpointAndPathStyle(t *testing.T) {
	raw, _ := json.Marshal(S3Config{
		Endpoint:        "https://example.r2.cloudflarestorage.com",
		Bucket:          "dylaris",
		AccessKeyID:     "key",
		SecretAccessKey: "secret",
		ForcePathStyle:  true,
	})
	st, err := NewS3(context.Background(), raw)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	opts := st.client.Options()
	if opts.BaseEndpoint == nil || *opts.BaseEndpoint != "https://example.r2.cloudflarestorage.com" {
		t.Errorf("BaseEndpoint = %v, want the configured endpoint", opts.BaseEndpoint)
	}
	if !opts.UsePathStyle {
		t.Error("UsePathStyle = false, want true")
	}
}
