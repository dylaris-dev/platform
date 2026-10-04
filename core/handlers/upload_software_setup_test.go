package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The chosen software reaches the node and becomes the server's installer
// type; an upload that names none is sent as before.
func TestUploadSetupForwardsSoftware(t *testing.T) {
	h, fs, rdb := newInstallTest(t)
	rec := httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", setupBody(map[string]string{
		"type": "upload-zip", "structure": "direct", "software": "paper", "version": "1.21.11", "mcVersion": "1.21",
	}), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status %d: %s", rec.Code, rec.Body.String())
	}
	inst := queuedInstaller(t, rdb)
	if len(inst) != 1 || inst[0]["software"] != "paper" || inst[0]["type"] != "upload-zip" || inst[0]["version"] != "1.21.11" {
		t.Fatalf("queued %v", inst)
	}
	if fs.setupType != "paper" || fs.installRow.InstallerType != "paper" || fs.installRow.McVersion != "1.21" {
		t.Fatalf("recorded %q / sub-server row %+v, want paper on both", fs.setupType, fs.installRow)
	}
}

func TestUploadSetupKeepingTheJarRecordsTheUpload(t *testing.T) {
	h, fs, rdb := newInstallTest(t)
	rec := httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", setupBody(map[string]string{"type": "upload-zip", "structure": "direct"}), false))
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status %d: %s", rec.Code, rec.Body.String())
	}
	if inst := queuedInstaller(t, rdb); len(inst) != 1 || inst[0]["software"] != "" {
		t.Fatalf("queued %v", inst)
	}
	if fs.setupType != "upload-zip" || fs.installRow.InstallerType != "upload-zip" {
		t.Fatalf("recorded %q / %q, want upload-zip", fs.setupType, fs.installRow.InstallerType)
	}
}

func TestUploadSetupRefusedOnAnOldNode(t *testing.T) {
	h, fs, rdb := newInstallTest(t)
	rdb.Set(context.Background(), "dylaris:discovery:node-lib", `{"releaseVersion":"2026.10.04.8"}`, 0)
	rec := httptest.NewRecorder()
	h.SetupServer(rec, installRequest("setup", setupBody(map[string]string{
		"type": "upload-zip", "structure": "direct", "software": "paper", "version": "1.21.11",
	}), false))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if n := len(queuedInstaller(t, rdb)); n != 0 || fs.setupWrites != 0 {
		t.Fatalf("queued %d, wrote %d after a refusal", n, fs.setupWrites)
	}
}
