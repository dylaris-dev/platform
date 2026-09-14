package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// L5: a refused presigned request reaches the run message a tenant reads, so it
// may carry the HTTP status and the S3 error code and nothing else of the body,
// which names the bucket, the key and the access key id.
func TestStorageStatusErrorKeepsTheBodyOut(t *testing.T) {
	const s3Body = `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match</Message><AWSAccessKeyId>AKIASECRETKEYID</AWSAccessKeyId><BucketName>customer-backups-bucket</BucketName><Key>backups/srv/job-1/run.tar.gz</Key></Error>`
	secrets := []string{"AKIASECRETKEYID", "customer-backups-bucket", "backups/srv", "signature we calculated"}

	for name, tt := range map[string]struct {
		body string
		want string
	}{
		"an S3 error names its code": {body: s3Body, want: "status 403 (SignatureDoesNotMatch)"},
		"a body that is not S3's":    {body: "bucket customer-backups-bucket is gone, key AKIASECRETKEYID", want: "status 403"},
		"a code that is not a code":  {body: `<Error><Code>see customer-backups-bucket</Code></Error>`, want: "status 403"},
		"no body at all":             {body: "", want: "status 403"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			u := &partUploader{client: srv.Client()}
			putErr := u.put(context.Background(), srv.URL+"/archive?partNumber=1", bytes.Repeat([]byte("x"), 16))
			_, getErr := downloadPresigned(context.Background(), srv.Client(), srv.URL+"/archive")

			for op, err := range map[string]error{"put": putErr, "presigned get": getErr} {
				if err == nil {
					t.Fatalf("%s: a 403 was not an error", op)
				}
				if !strings.HasSuffix(err.Error(), tt.want) {
					t.Errorf("%s: error %q, want it to end in %q", op, err, tt.want)
				}
				for _, s := range secrets {
					if strings.Contains(err.Error(), s) {
						t.Errorf("%s: error %q carries %q from the response body", op, err, s)
					}
				}
			}
		})
	}
}
