package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// writeImportArchive builds a .tar.gz in dir under the name the importer looks
// for. entries are written in order, so a test can put the manifest first the
// way a real archive does.
func writeImportArchive(t *testing.T, dir string, entries []tar.Header, bodies [][]byte) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for i, h := range entries {
		hdr := h
		hdr.Size = int64(len(bodies[i]))
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("header %q: %v", hdr.Name, err)
		}
		if _, err := tw.Write(bodies[i]); err != nil {
			t.Fatalf("body %q: %v", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupImportArchiveName), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallFromBackupArchive_ExtractsFilesAndReturnsTheManifest(t *testing.T) {
	dir := t.TempDir()
	writeImportArchive(t,
		dir,
		[]tar.Header{
			{Name: ".dylaris/backup.json", Typeflag: tar.TypeReg},
			{Name: "world/level.dat", Typeflag: tar.TypeReg},
			{Name: "mods/sodium.jar", Typeflag: tar.TypeReg},
		},
		[][]byte{[]byte(`{"schema":1,"scope":"survival"}`), []byte("world"), []byte("jar")},
	)

	manifest, err := installFromBackupArchive(dir)
	if err != nil {
		t.Fatalf("installFromBackupArchive: %v", err)
	}
	if string(manifest) != `{"schema":1,"scope":"survival"}` {
		t.Errorf("manifest = %q, want the archive's description", manifest)
	}
	for _, p := range []string{"world/level.dat", "mods/sodium.jar"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s was not extracted: %v", p, err)
		}
	}
	// The description belongs to the archive, not to the server. Extracting it
	// would leave a stale manifest that the NEXT backup would archive.
	if _, err := os.Stat(filepath.Join(dir, ".dylaris")); !os.IsNotExist(err) {
		t.Error("the manifest directory was extracted into the server tree")
	}
	// On success the archive is not part of the installed server.
	if _, err := os.Stat(filepath.Join(dir, backupImportArchiveName)); !os.IsNotExist(err) {
		t.Error("the uploaded archive was left in the server directory")
	}
}

// An archive with no description is the ordinary case for anything written
// before manifests existed, and for anything assembled by hand. The files must
// still install; only the automatic configuration is unavailable.
func TestInstallFromBackupArchive_NoManifestStillInstalls(t *testing.T) {
	dir := t.TempDir()
	writeImportArchive(t, dir,
		[]tar.Header{{Name: "server.properties", Typeflag: tar.TypeReg}},
		[][]byte{[]byte("motd=hi")})

	manifest, err := installFromBackupArchive(dir)
	if err != nil {
		t.Fatalf("installFromBackupArchive: %v", err)
	}
	if manifest != nil {
		t.Errorf("manifest = %q, want nil", manifest)
	}
	if _, err := os.Stat(filepath.Join(dir, "server.properties")); err != nil {
		t.Errorf("server.properties was not extracted: %v", err)
	}
}

// The archive is a file a user supplied. It writes into a directory that is
// bind-mounted into a tenant's container, so an entry that names its way out has
// to be dropped rather than followed.
func TestInstallFromBackupArchive_RefusesToWriteOutsideTheServer(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "sub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeImportArchive(t, dir,
		[]tar.Header{
			{Name: "../escaped.txt", Typeflag: tar.TypeReg},
			{Name: "ok.txt", Typeflag: tar.TypeReg},
		},
		[][]byte{[]byte("nope"), []byte("yes")})

	if _, err := installFromBackupArchive(dir); err != nil {
		t.Fatalf("installFromBackupArchive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "escaped.txt")); !os.IsNotExist(err) {
		t.Error("an entry escaped the server directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "ok.txt")); err != nil {
		t.Errorf("the safe entry was dropped along with the unsafe one: %v", err)
	}
}

// A link in a user-supplied archive is a write primitive aimed wherever its
// target says. Nothing a Minecraft server needs to run depends on one.
func TestInstallFromBackupArchive_DropsLinks(t *testing.T) {
	dir := t.TempDir()
	writeImportArchive(t, dir,
		[]tar.Header{
			{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
			{Name: "ok.txt", Typeflag: tar.TypeReg},
		},
		[][]byte{nil, []byte("yes")})

	if _, err := installFromBackupArchive(dir); err != nil {
		t.Fatalf("installFromBackupArchive: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "evil")); !os.IsNotExist(err) {
		t.Error("a link entry was recreated")
	}
}

// A retry must not need a re-upload, so a failed import leaves the archive where
// it was.
func TestInstallFromBackupArchive_KeepsTheArchiveOnFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, backupImportArchiveName), []byte("not gzip at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installFromBackupArchive(dir); err == nil {
		t.Fatal("err = nil for a file that is not an archive, want an error")
	}
	if _, err := os.Stat(filepath.Join(dir, backupImportArchiveName)); err != nil {
		t.Errorf("the archive was removed after a failed import: %v", err)
	}
}

// A manifest entry that is not plausible JSON is not a manifest. Taking it would
// hand Core a description it cannot parse, reported as if the archive had one.
func TestInstallFromBackupArchive_IgnoresAnUnparseableManifest(t *testing.T) {
	dir := t.TempDir()
	writeImportArchive(t, dir,
		[]tar.Header{
			{Name: ".dylaris/backup.json", Typeflag: tar.TypeReg},
			{Name: "ok.txt", Typeflag: tar.TypeReg},
		},
		[][]byte{[]byte("this is not json"), []byte("yes")})

	manifest, err := installFromBackupArchive(dir)
	if err != nil {
		t.Fatalf("installFromBackupArchive: %v", err)
	}
	if manifest != nil {
		t.Errorf("manifest = %q, want nil for unparseable content", manifest)
	}
}

// The archive is a file the USER supplies, so it must be writable through every
// upload path. It is not, if its name lands in the platform-reserved namespace:
// isPlatformReservedName refuses every write beginning with ".dylaris", which is
// what the first name for this file did - the Beam client could not upload it at
// all while the HTTP path, checking the narrower isProtectedFile set, allowed it.
//
// Both guards are asked here rather than one, because it was their disagreement
// that hid the problem.
func TestImportArchiveNameIsWritable(t *testing.T) {
	if isPlatformReservedName(backupImportArchiveName) {
		t.Fatalf("%q is platform-reserved, so no client may upload it", backupImportArchiveName)
	}
	if isProtectedFile(backupImportArchiveName) {
		t.Fatalf("%q is protected, so no client may upload it", backupImportArchiveName)
	}
	if reservedComponent("survival/"+backupImportArchiveName) != "" {
		t.Fatalf("%q is reserved as a path component", backupImportArchiveName)
	}
	// It must still be a dotfile: it sits in the sub-server directory and a
	// visible one would show up in the file browser of every imported server.
	if backupImportArchiveName[0] != '.' {
		t.Fatalf("%q is not hidden", backupImportArchiveName)
	}
}
