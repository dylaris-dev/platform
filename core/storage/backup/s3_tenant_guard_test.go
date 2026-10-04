package backup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
)

// A tenant's own bucket is dialled through netguard. It used to go out on the
// SDK's default client, so the endpoint a tenant typed could aim Core at Redis,
// the database, the Hub or the metadata service, and "Test connection" handed
// the answer back. httptest listens on loopback: exactly such an address.
func TestATenantBucketCannotReachAnInternalAddress(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	cfg, _ := json.Marshal(S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "b",
		AccessKeyID: "k", SecretAccessKey: "s", ForcePathStyle: true})
	owner := "tenant-1"

	tenant, err := Open(context.Background(), &models.BackupStorage{Provider: "s3", Config: cfg, OwnerID: &owner}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	err = tenant.Put(context.Background(), "k", strings.NewReader("x"), 1)
	if err == nil || !strings.Contains(err.Error(), "blocked non-public address") {
		t.Fatalf("a tenant bucket on loopback was dialled: %v", err)
	}
	if hits != 0 {
		t.Fatalf("the internal address received %d requests", hits)
	}

	// The platform's own storage may sit on a private address on purpose.
	platform, err := Open(context.Background(), &models.BackupStorage{Provider: "s3", Config: cfg}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.Put(context.Background(), "k", strings.NewReader("x"), 1); err != nil {
		t.Fatalf("the operator's private storage was refused: %v", err)
	}
}

func TestTenantEndpoint(t *testing.T) {
	owner := "u"
	for _, tc := range []struct {
		bs   *models.BackupStorage
		want bool
	}{
		{&models.BackupStorage{Provider: "s3", OwnerID: &owner}, true},
		{&models.BackupStorage{Provider: "s3"}, false},
		{&models.BackupStorage{Provider: "connection", OwnerID: &owner}, false},
		{nil, false},
	} {
		if got := TenantEndpoint(tc.bs); got != tc.want {
			t.Errorf("TenantEndpoint(%+v) = %v, want %v", tc.bs, got, tc.want)
		}
	}
}
