package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/storage"
	"dylaris-core/store"
)

func TestTemplateStorageRoundTrip(t *testing.T) {
	ctx := context.Background()
	prov := &storage.LocalProvider{BasePath: t.TempDir()}
	const root = "owner-1/7"
	files := map[string]string{"a.yml": "alpha", "plugins/b/c.yml": "gamma-gamma"}

	for rel, body := range files {
		if err := templateWrite(ctx, prov, root, rel, strings.NewReader(body)); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	if err := templateWrite(ctx, prov, root, "../x", strings.NewReader("evil")); err == nil {
		t.Fatal("write of ../x was accepted")
	}

	list, err := templateList(ctx, prov, root)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var names []string
	var sum int64
	for _, f := range list {
		names = append(names, f.Name)
		sum += f.Size
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "a.yml,plugins/b/c.yml" {
		t.Fatalf("list = %v", names)
	}
	size, err := templateSize(ctx, prov, root)
	if err != nil || size != sum || size != int64(len("alpha")+len("gamma-gamma")) {
		t.Fatalf("size = %d, %v (sum %d)", size, err, sum)
	}

	rc, err := templateRead(ctx, prov, root, "plugins/b/c.yml")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "gamma-gamma" {
		t.Fatalf("read = %q", got)
	}

	var buf bytes.Buffer
	if err := templateZip(ctx, prov, root, &buf); err != nil {
		t.Fatalf("zip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("reopen zip: %v", err)
	}
	if len(zr.File) != len(files) {
		t.Fatalf("zip has %d entries, want %d", len(zr.File), len(files))
	}
	for _, zf := range zr.File {
		want, ok := files[zf.Name]
		if !ok {
			t.Fatalf("unexpected zip entry %q", zf.Name)
		}
		if zf.Method != zip.Deflate {
			t.Errorf("%s method = %d, want Deflate", zf.Name, zf.Method)
		}
		r, _ := zf.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		if string(b) != want {
			t.Errorf("%s = %q, want %q", zf.Name, b, want)
		}
	}

	if err := templateDelete(ctx, prov, root, "a.yml"); err != nil {
		t.Fatalf("delete file: %v", err)
	}
	if list, _ = templateList(ctx, prov, root); len(list) != 1 {
		t.Fatalf("after file delete: %v", list)
	}
	if err := templateDelete(ctx, prov, root, ""); err != nil {
		t.Fatalf("delete root: %v", err)
	}
	if list, _ = templateList(ctx, prov, root); len(list) != 0 {
		t.Fatalf("after root delete: %v", list)
	}
	if err := templateDelete(ctx, prov, root, "../x"); err == nil {
		t.Fatal("delete of ../x was accepted")
	}
}

func TestTemplateRootKeyRefusesUnsafeIDs(t *testing.T) {
	for _, owner := range []string{"0a1b2c3d-0000-4000-8000-000000000000", "xavier-x0"} {
		if k, err := templateRootKey(owner, 3); err != nil || k != owner+"/3" {
			t.Errorf("owner %q refused: %q, %v", owner, k, err)
		}
	}
	for _, owner := range []string{"", "a/b", ".", "..", `a\b`, "a\x00b"} {
		if _, err := templateRootKey(owner, 1); err == nil {
			t.Errorf("owner %q accepted", owner)
		}
	}
	if _, err := templateRootKey("o", 0); err == nil {
		t.Error("id 0 accepted")
	}
	if k, err := templateRootKey("o", 5); err != nil || k != "o/5" {
		t.Errorf("got %q, %v", k, err)
	}
}

type tenantTemplateStore struct {
	store.Store
	byID map[int]*models.BackupStorage
}

func (f *tenantTemplateStore) GetBackupStorage(id int) (*models.BackupStorage, error) {
	return f.byID[id], nil
}

// A template on a tenant storage is served from that storage or not at all:
// missing, foreign or non-s3 is an error, never a fall back to Core storage.
func TestTemplateStoreTenantStorage(t *testing.T) {
	owner, other := "owner-1", "owner-2"
	cfg := json.RawMessage(`{"endpoint":"https://s3.example.test","bucket":"b","accessKeyId":"AK","secretAccessKey":"SK"}`)
	st := &tenantTemplateStore{byID: map[int]*models.BackupStorage{
		1: {ID: 1, Provider: "s3", OwnerID: &owner, Config: cfg},
		2: {ID: 2, Provider: "s3", OwnerID: &other, Config: cfg},
		3: {ID: 3, Provider: "s3", Config: cfg},
		4: {ID: 4, Provider: "local", OwnerID: &owner, Config: json.RawMessage(`{"path":"/tmp"}`)},
	}}
	s := &AppState{Store: st}
	tests := []struct {
		name    string
		id      int
		wantErr bool
	}{
		{"own s3", 1, false},
		{"foreign", 2, true},
		{"platform", 3, true},
		{"not s3", 4, true},
		{"missing", 99, true},
	}
	for _, tc := range tests {
		id := tc.id
		tpl := &models.Template{ID: 7, OwnerID: owner, BackupStorageID: &id}
		prov, root, err := s.TemplateStore(tpl)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: got a provider, want an error", tc.name)
			}
			continue
		}
		if err != nil || prov == nil || root != "owner-1/7" {
			t.Errorf("%s: prov %v root %q err %v", tc.name, prov, root, err)
		}
		if _, ok := prov.(*storage.S3Provider); !ok {
			t.Errorf("%s: provider is %T, want *storage.S3Provider", tc.name, prov)
		}
		if s.TemplateStorageNeverConfigured(tpl) {
			t.Errorf("%s: a tenant-storage template reported as never configured", tc.name)
		}
	}
}
