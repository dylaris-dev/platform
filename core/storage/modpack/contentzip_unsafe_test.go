package modpack

import "testing"

// Every content zip the platform authors goes through these two, and a pack
// render copies what they produce verbatim into the instance root. The
// guard belongs here rather than at each call site: the pack text editor
// re-wraps a stored zip around a target path read back out of the database,
// which skips the validation the original upload passed through.
func TestContentZipAuthorsRefuseAnEscapingEntry(t *testing.T) {
	content := []byte("x")

	for _, bad := range []string{
		"../../../../etc/cron.d/evil",
		"mods/../../evil.jar",
		`mods\..\..\evil.jar`,
		"/etc/passwd",
		"C:/windows/system32/evil.dll",
	} {
		if _, err := BuildContentZip(bad, content); err == nil {
			t.Errorf("BuildContentZip wrote an entry at %q", bad)
		}
	}
	for _, bad := range []string{"../evil.jar", `..\evil.jar`, "../../x/y.jar"} {
		if _, err := WrapJarAsContentZip(bad, content); err == nil {
			t.Errorf("WrapJarAsContentZip wrote an entry at mods/%s", bad)
		}
	}

	// The ordinary cases must keep working, byte-stably.
	if _, err := BuildContentZip("mods/sodium.jar", content); err != nil {
		t.Fatalf("an ordinary inner path must still build: %v", err)
	}
	if _, err := WrapJarAsContentZip("sodium.jar", content); err != nil {
		t.Fatalf("an ordinary file name must still wrap: %v", err)
	}
}
