package packs

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"

	"dylaris-core/models"
)

func TestPackRenderRejectsOverlappingStoredContent(t *testing.T) {
	for _, otherPath := range []string{"config/shared.toml", "CONFIG/SHARED.toml"} {
		t.Run(otherPath, func(t *testing.T) {
			dir := t.TempDir()
			h := localModpackHandler(t, dir)
			storeZip(t, dir, "uploads/raw.zip", "config/shared.toml", []byte("first=true"))
			storeZip(t, dir, "uploads/bundle.zip", otherPath, []byte("first=false"))
			content := []models.BuildContentEntry{uploadEntry("uploads/raw.zip"), uploadEntry("uploads/bundle.zip")}
			var server bytes.Buffer
			if err := h.renderServerPack(context.Background(), content, &server); err == nil || !strings.Contains(err.Error(), "duplicate pack output path") {
				t.Fatalf("server render error=%v, want duplicate rejection", err)
			}
			if _, err := h.renderMrpack(context.Background(), &models.Pack{InternalName: "test"}, &models.PackBuild{VersionString: "1.0", Minecraft: "1.20.1", Loader: "forge", LoaderVersion: "47.4.28"}, content); err == nil || !strings.Contains(err.Error(), "duplicate pack output path") {
				t.Fatalf("mrpack render error=%v, want duplicate rejection", err)
			}
		})
	}
}

func TestMrpackRejectsAnOverrideOverwritingALinkedMod(t *testing.T) {
	dir := t.TempDir()
	h := localModpackHandler(t, dir)
	storeZip(t, dir, "uploads/bundle.zip", "mods/shared.jar", []byte("unexpected override"))
	content := []models.BuildContentEntry{modrinthEntry("mods/shared.jar"), uploadEntry("uploads/bundle.zip")}
	if _, err := h.renderMrpack(context.Background(), &models.Pack{}, &models.PackBuild{}, content); err == nil || !strings.Contains(err.Error(), "duplicate pack output path") {
		t.Fatalf("render error=%v, want reference/override collision rejection", err)
	}
}

func TestUploadedBundleRejectsDuplicateEntries(t *testing.T) {
	var raw bytes.Buffer
	zw := zip.NewWriter(&raw)
	for _, name := range []string{"config/same.toml", "config/same.toml"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("value=true"))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := validateStoredModZip(bytes.NewReader(raw.Bytes()), int64(raw.Len())); err == nil || !strings.Contains(err.Error(), "duplicate pack output path") {
		t.Fatalf("validation=%v, want duplicate rejection", err)
	}
}

func TestMrpackPreservesSideSpecificOverrides(t *testing.T) {
	dir := t.TempDir()
	h := localModpackHandler(t, dir)
	storeZip(t, dir, "uploads/client.zip", "config/side.toml", []byte("client=true"))
	storeZip(t, dir, "uploads/server.zip", "config/side.toml", []byte("server=true"))
	storeZip(t, dir, "uploads/both.zip", "config/common.toml", []byte("common=true"))
	client, server, both := uploadEntry("uploads/client.zip"), uploadEntry("uploads/server.zip"), uploadEntry("uploads/both.zip")
	client.Side = models.SideClient
	server.Side = models.SideServer
	both.Side = models.SideBoth
	content := []models.BuildContentEntry{client, server, both}
	raw, err := h.renderMrpack(context.Background(), &models.Pack{}, &models.PackBuild{}, content)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, name := range []string{"client-overrides/config/side.toml", "server-overrides/config/side.toml", "overrides/config/common.toml"} {
		if !names[name] {
			t.Errorf("missing %q", name)
		}
	}
	if names["overrides/config/side.toml"] {
		t.Fatal("side-specific config leaked into the shared overrides")
	}
	var serverZIP bytes.Buffer
	if err := h.renderServerPack(context.Background(), content, &serverZIP); err != nil {
		t.Fatal(err)
	}
	sz, err := zip.NewReader(bytes.NewReader(serverZIP.Bytes()), int64(serverZIP.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(sz.File) != 2 {
		t.Fatalf("server pack entries=%d want server+common only", len(sz.File))
	}
}

func TestMrpackAllowsDisjointReferenceAndOverride(t *testing.T) {
	dir := t.TempDir()
	h := localModpackHandler(t, dir)
	storeZip(t, dir, "uploads/server.zip", "mods/shared.jar", []byte("server bytes"))
	ref := modrinthEntry("mods/shared.jar")
	ref.Side = models.SideClient
	if _, err := h.renderMrpack(context.Background(), &models.Pack{}, &models.PackBuild{}, []models.BuildContentEntry{ref, uploadEntry("uploads/server.zip")}); err != nil {
		t.Fatalf("disjoint client/server variants rejected: %v", err)
	}
}
