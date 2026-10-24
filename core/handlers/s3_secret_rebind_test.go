package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"

	"github.com/gorilla/mux"
)

// The platform has four places that store an s3 credential: core storage,
// backup storages, modpack storage and the shared storage connections. All
// four hide the secret on every read and treat a blank secret on write as
// "keep the stored one" - which is what makes the identity fields
// (endpoint, bucket, access key) the thing that decides WHERE that hidden
// secret gets used.
//
// Two of the four checked that those fields had not moved
// (mergeCoreStorageCandidate, and mergeBackupStorageSecret since the backup
// pass). Modpack storage and storage connections did not: a settings.write
// holder who cannot READ the secret could submit a new endpoint with the
// secret field left blank, and the operator's credential would sign requests
// to a host of the caller's choosing. These pin the two that were missing.

func connConfig(endpoint, bucket string) json.RawMessage {
	b, _ := json.Marshal(storageConnectionConfig{Endpoint: endpoint, Bucket: bucket})
	return b
}

func TestStorageConnectionSecretRebound(t *testing.T) {
	existing := &models.StorageConnection{
		ID:        5,
		Config:    connConfig("https://s3.example", "packs"),
		AccessKey: "AKIAREAL",
		SecretSet: true,
	}
	cases := []struct {
		name     string
		req      storageConnectionRequest
		existing *models.StorageConnection
		want     bool
	}{
		{"unchanged identity, blank secret",
			storageConnectionRequest{Config: connConfig("https://s3.example", "packs"), AccessKey: "AKIAREAL"}, existing, false},
		{"endpoint moved",
			storageConnectionRequest{Config: connConfig("https://attacker.example", "packs"), AccessKey: "AKIAREAL"}, existing, true},
		{"bucket moved",
			storageConnectionRequest{Config: connConfig("https://s3.example", "theirs"), AccessKey: "AKIAREAL"}, existing, true},
		{"access key moved",
			storageConnectionRequest{Config: connConfig("https://s3.example", "packs"), AccessKey: "AKIAOTHER"}, existing, true},
		{"an omitted config would blank the endpoint",
			storageConnectionRequest{AccessKey: "AKIAREAL"}, existing, true},
		{"a submitted secret is a rotation, always allowed",
			storageConnectionRequest{Config: connConfig("https://attacker.example", "x"), AccessKey: "z", SecretAccessKey: "new"}, existing, false},
		{"no stored secret means nothing to rebind",
			storageConnectionRequest{Config: connConfig("https://attacker.example", "x")},
			&models.StorageConnection{ID: 5, SecretSet: false}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := storageConnectionSecretRebound(c.req, c.existing); got != c.want {
				t.Fatalf("storageConnectionSecretRebound = %v, want %v", got, c.want)
			}
		})
	}
}

// End-to-end on the wire: the refusal has to happen before the metadata write,
// or the endpoint is already rebound by the time the caller sees the 400.
func TestUpdateConnection_RefusesARebindOnTheWire(t *testing.T) {
	fs := &connFakeStore{existing: &models.StorageConnection{
		ID:        5,
		Config:    connConfig("https://s3.example", "packs"),
		AccessKey: "AKIAREAL",
		SecretSet: true,
	}}
	h := NewStorageConnectionsHandler(&AppState{Store: fs})

	body, _ := json.Marshal(storageConnectionRequest{
		Name: "nas", Provider: "s3",
		Config:    connConfig("https://attacker.example", "packs"),
		AccessKey: "AKIAREAL",
	})
	req := httptest.NewRequest(http.MethodPatch, "/api/storage-connections/5", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": "5"})
	rw := httptest.NewRecorder()
	h.UpdateConnection(rw, req)

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rw.Code, rw.Body.String())
	}
	if fs.updated != 0 {
		t.Fatalf("the endpoint was persisted anyway (%d updates)", fs.updated)
	}
}
