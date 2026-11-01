package backup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A body the SDK cannot seek, like a capture stream or the probe's reader.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

// Over plain HTTP SigV4 hashes the payload, which needs a seekable body; the
// SDK refused such a Put with "request stream is not seekable". Measured on a
// LAN MinIO/SeaweedFS behind STORAGE_PRIVATE_ALLOW, where "Test" failed.
func TestPutNonSeekableBodyOverPlainHTTP(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			b, _ := io.ReadAll(r.Body)
			got = append(got, string(b))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	raw, _ := json.Marshal(S3Config{Endpoint: srv.URL, Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s", ForcePathStyle: true})
	st, err := NewS3(context.Background(), raw)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	if !st.plainHTTP {
		t.Fatal("plainHTTP = false for an http:// endpoint")
	}
	for _, size := range []int64{0, 5} {
		if err := st.Put(context.Background(), "k", onlyReader{strings.NewReader("hello")}, size); err != nil {
			t.Fatalf("Put(size=%d) over plain HTTP: %v", size, err)
		}
	}
	if len(got) != 2 || got[0] != "hello" || got[1] != "hello" {
		t.Fatalf("server received %q, want two bodies \"hello\"", got)
	}

	// Without the spool the SDK still refuses: proves the test reaches the bug.
	st.plainHTTP = false
	err = st.Put(context.Background(), "k", onlyReader{strings.NewReader("hello")}, 5)
	if err == nil || !strings.Contains(err.Error(), "not seekable") {
		t.Fatalf("unspooled Put over plain HTTP: err = %v, want the SDK's not-seekable refusal", err)
	}
}

func TestPlainHTTPOnlyForHTTPEndpoints(t *testing.T) {
	for ep, want := range map[string]bool{"http://10.0.0.5:9000": true, "HTTP://x": true, "https://r2.example": false, "": false} {
		raw, _ := json.Marshal(S3Config{Endpoint: ep, Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"})
		st, err := NewS3(context.Background(), raw)
		if err != nil {
			t.Fatalf("NewS3(%q): %v", ep, err)
		}
		if st.plainHTTP != want {
			t.Errorf("plainHTTP(%q) = %v, want %v", ep, st.plainHTTP, want)
		}
	}
}
