package storage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A tenant typed the endpoint, so it must go through netguard: one pointed at
// loopback is refused before a single byte reaches it.
func TestTenantS3ProviderDialsThroughNetguard(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	var hits atomic.Int32
	// TLS: over plain http the SDK may refuse an unseekable body before it
	// dials, and the guard would never be asked.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	raw, _ := json.Marshal(map[string]any{
		"endpoint": srv.URL, "bucket": "b", "accessKeyId": "AK", "secretAccessKey": "SK", "forcePathStyle": true,
	})
	prov, err := NewTenantS3Provider(context.Background(), raw, "templates")
	if err != nil {
		t.Fatalf("NewTenantS3Provider: %v", err)
	}
	ctx := context.Background()
	ops := []struct {
		name string
		run  func() error
	}{
		{"write", func() error { return prov.WriteFile(ctx, "u/1/a.yml", strings.NewReader("x")) }},
		{"list", func() error { _, err := prov.ListFiles(ctx, "u/1"); return err }},
		{"delete", func() error { return prov.DeletePath(ctx, "u/1") }},
	}
	for _, op := range ops {
		err := op.run()
		if err == nil || !strings.Contains(err.Error(), "blocked non-public address") {
			t.Errorf("%s against loopback: err = %v, want a netguard refusal", op.name, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the loopback endpoint received %d requests; the unguarded client was used", n)
	}
}

func TestTenantS3ProviderRejectsBadConfig(t *testing.T) {
	if _, err := NewTenantS3Provider(context.Background(), json.RawMessage(`{"endpoint":"https://s3.example.test"}`), "templates"); err == nil {
		t.Fatal("config without bucket or keys was accepted")
	}
}
