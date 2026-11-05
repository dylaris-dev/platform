package migration

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// A transport error from net/http is a *url.Error whose text carries the whole
// request URL. For a pre-signed URL that is the credential: it used to travel
// into the node log and the move status the server's owner and sub-users read.
func TestPresignedURLQueryNeverReachesTheError(t *testing.T) {
	srv := httptest.NewServer(nil)
	dead := srv.URL
	srv.Close() // nothing listens any more: Do fails at dial, before any status

	const secret = "X-Amz-Signature=deadbeefcafe"
	rawURL := dead + "/bucket/migration-transfer/x.zip?X-Amz-Credential=AKIA%2Fscope&" + secret
	cases := []struct {
		name string
		err  error
	}{
		{"PullURL", PullURL(context.Background(), rawURL, "00", filepath.Join(t.TempDir(), "out.zip"), 0, 0, nil)},
		{"withoutURL", withoutURL(&url.Error{Op: "Put", URL: rawURL, Err: errors.New("dial tcp: refused")})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.err == nil {
				t.Fatal("no error")
			}
			msg := c.err.Error()
			if strings.Contains(msg, "X-Amz") || strings.Contains(msg, "deadbeef") {
				t.Fatalf("error carries the signed query: %s", msg)
			}
		})
	}
	if withoutURL(nil) != nil {
		t.Error("nil error became non-nil")
	}
}
