package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"testing"

	"dylaris-core/storage"
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
