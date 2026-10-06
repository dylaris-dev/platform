package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"dylaris-core/models"
)

// auditedRuntimeStore is runtimeFakeStore with the server audit switched on.
type auditedRuntimeStore struct {
	runtimeFakeStore
	audits []*models.ServerAuditEvent
}

func (f *auditedRuntimeStore) GetServerAuditState(int) (bool, bool, int, error) {
	return true, false, 0, nil
}
func (f *auditedRuntimeStore) InsertServerAudit(ev *models.ServerAuditEvent) error {
	f.audits = append(f.audits, ev)
	return nil
}

// The runtime audit row says whether the JVM flags changed, never what they
// are: a flag can carry a secret and the trail keeps old values indefinitely.
func TestRuntimeAuditRowCarriesNoJvmFlags(t *testing.T) {
	for _, c := range []struct {
		before, sent string
		changed      bool
	}{
		{"", "-Dapi.token=s3cret", true},
		{"-Dapi.token=s3cret", " -Dapi.token=s3cret ", false},
	} {
		srv := onlineServer()
		srv.ExtraJvmFlags = c.before
		fs := &auditedRuntimeStore{runtimeFakeStore: runtimeFakeStore{srv: srv}}
		body, _ := json.Marshal(map[string]string{"javaImage": "ghcr.io/dylaris-dev/platform-mc-java25:latest", "extraJvmFlags": c.sent})
		rw := runtimeRequest(t, fs, string(body))
		if rw.Code != 200 || len(fs.audits) != 1 {
			t.Fatalf("status %d, %d audit rows", rw.Code, len(fs.audits))
		}
		meta, _ := json.Marshal(fs.audits[0].Metadata)
		if strings.Contains(string(meta), "s3cret") {
			t.Errorf("audit row carries the flags: %s", meta)
		}
		if got, _ := fs.audits[0].Metadata["jvm_flags_changed"].(bool); got != c.changed {
			t.Errorf("before %q sent %q: jvm_flags_changed = %v, want %v", c.before, c.sent, got, c.changed)
		}
	}
}
