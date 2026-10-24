package servers

import (
	"reflect"
	"testing"

	pb "dylaris-proto/node"

	"dylaris-core/models"
)

func TestUnmanagedJars(t *testing.T) {
	listings := map[string][]dirEntry{
		"mods": {
			{Name: "sodium.jar", Size: 100},
			{Name: "handdropped.jar", Size: 200},
			{Name: "notes.txt", Size: 10},
			{Name: "somefolder", IsDir: true},
		},
		"plugins": {
			{Name: "worldedit.jar", Size: 300},
		},
	}

	tests := []struct {
		name          string
		known         []models.ServerMod
		installerType string
		want          []unmanagedFile
	}{
		{
			name:          "nothing installed means every jar is unmanaged",
			known:         nil,
			installerType: "fabric",
			want: []unmanagedFile{
				{Directory: "mods", Name: "sodium.jar", Size: 100},
				{Directory: "mods", Name: "handdropped.jar", Size: 200},
				{Directory: "plugins", Name: "worldedit.jar", Size: 300},
			},
		},
		{
			name: "an installed jar is claimed and drops out",
			known: []models.ServerMod{
				{FileName: "sodium.jar", TargetDir: "mods"},
			},
			installerType: "fabric",
			want: []unmanagedFile{
				{Directory: "mods", Name: "handdropped.jar", Size: 200},
				{Directory: "plugins", Name: "worldedit.jar", Size: 300},
			},
		},
		{
			// The trap this function exists to avoid: the row records "plugins",
			// the server was later re-declared as fabric, and deriving the
			// directory from the loader would look in "mods" and report the
			// plugin the panel itself installed as a stray file.
			name: "the recorded directory wins over the loader derivation",
			known: []models.ServerMod{
				{FileName: "worldedit.jar", TargetDir: "plugins"},
			},
			installerType: "fabric",
			want: []unmanagedFile{
				{Directory: "mods", Name: "sodium.jar", Size: 100},
				{Directory: "mods", Name: "handdropped.jar", Size: 200},
			},
		},
		{
			// Rows written before target_dir existed hold "", and those jars
			// were placed by the derived value, so the fallback is what they
			// need.
			name: "a row with no recorded directory falls back to the loader",
			known: []models.ServerMod{
				{FileName: "worldedit.jar", TargetDir: ""},
			},
			installerType: "paper",
			want: []unmanagedFile{
				{Directory: "mods", Name: "sodium.jar", Size: 100},
				{Directory: "mods", Name: "handdropped.jar", Size: 200},
			},
		},
		{
			// Same file name in both directories: claiming one must not silence
			// the other.
			name: "the claim is per directory, not per file name",
			known: []models.ServerMod{
				{FileName: "sodium.jar", TargetDir: "plugins"},
			},
			installerType: "fabric",
			want: []unmanagedFile{
				{Directory: "mods", Name: "sodium.jar", Size: 100},
				{Directory: "mods", Name: "handdropped.jar", Size: 200},
				{Directory: "plugins", Name: "worldedit.jar", Size: 300},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unmanagedJars(tc.known, tc.installerType, listings)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestUnmanagedJarsIgnoresNonJars(t *testing.T) {
	got := unmanagedJars(nil, "fabric", map[string][]dirEntry{
		"mods": {
			{Name: "config.yml"},
			{Name: "backup.jar.disabled"},
			{Name: "SODIUM.JAR"}, // case-insensitive suffix match
			{Name: "subdir", IsDir: true},
		},
	})
	want := []unmanagedFile{{Directory: "mods", Name: "SODIUM.JAR"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// The empty result must serialise as [] rather than null, or the panel renders
// "no data" where it means "nothing unmanaged".
func TestUnmanagedJarsReturnsEmptySliceNotNil(t *testing.T) {
	got := unmanagedJars(nil, "fabric", map[string][]dirEntry{})
	if got == nil {
		t.Fatal("got nil, want an empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want empty", got)
	}
}

// Copying a sub-server onto a name that is already taken has to be refused HERE,
// not only on the node.
//
// The node does refuse to copy onto an existing directory, so the files are
// safe. The mod inventory is not: Core writes the copy's mod rows whichever way
// the node decides, so without this check copying onto an existing sub-server's
// name merged the source's mods into that sub-server's list while the copy
// itself never happened - and the response still said the copy was queued.
func TestSubServerNameTaken(t *testing.T) {
	dir := []*pb.FileInfo{
		{Name: "server"},
		{Name: "server-121"},
		{Name: ".active_server"},
		nil, // a node answer with a hole in it must not panic
	}
	tests := []struct {
		name  string
		want  bool
		entry string
	}{
		{"an unused name is free", false, "server-1204"},
		{"an existing sub-server is taken", true, "server-121"},
		{"the active sub-server is taken", true, "server"},

		// Stricter than the filesystem on purpose: Linux would let both exist,
		// and two sub-servers differing only in case is a trap for every screen
		// that lists them.
		{"a name differing only in case is taken", true, "Server-121"},
		{"and in the other direction too", true, "SERVER"},

		// Whitespace is trimmed on both sides, so a padded name cannot slip
		// past a comparison the node would then collapse.
		{"a padded name is still taken", true, "  server-121  "},

		{"an empty name is not a collision", false, ""},
		{"whitespace only is not a collision", false, "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := subServerNameTaken(dir, tt.entry); got != tt.want {
				t.Errorf("subServerNameTaken(%q) = %v, want %v", tt.entry, got, tt.want)
			}
		})
	}
}

func TestSubServerNameTakenOnAnEmptyDirectory(t *testing.T) {
	if subServerNameTaken(nil, "server") {
		t.Error("reported a collision against an empty listing")
	}
}

// Identifying a stray copy of a mod that is already installed must be refused,
// not written.
//
// server_mods conflicts on (server_id, sub_server_name, modrinth_project_id), so
// the upsert did not add a row - it rewrote the existing one onto the stray's
// file name. The managed jar then read as unmanaged, and the next version move
// removed the stray while leaving the real one behind: two copies of one mod in
// the mods directory, and a server that will not start.
func TestDuplicateOfInstalled(t *testing.T) {
	claimed := map[string]string{
		"AANobbMI": "sodium-0.5.8.jar",
		"gvQqBUqZ": "lithium-0.11.2.jar",
	}
	tests := []struct {
		name      string
		projectID string
		fileName  string
		wantOther string
		wantDup   bool
	}{
		{"a second copy under another name is a duplicate",
			"AANobbMI", "sodium-old.jar", "sodium-0.5.8.jar", true},
		{"a project nobody has installed is fine",
			"P7dR8mSH", "fabric-api.jar", "", false},

		// Re-identifying the SAME file refreshes the row, which is exactly what
		// someone repairing a broken link is asking for.
		{"the same file is not a duplicate of itself",
			"AANobbMI", "sodium-0.5.8.jar", "", false},

		{"an empty claim map never collides",
			"AANobbMI", "sodium-0.5.8.jar", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := claimed
			if tt.name == "an empty claim map never collides" {
				m = map[string]string{}
			}
			other, dup := duplicateOfInstalled(m, tt.projectID, tt.fileName)
			if dup != tt.wantDup {
				t.Fatalf("duplicate = %v, want %v", dup, tt.wantDup)
			}
			if other != tt.wantOther {
				t.Errorf("other = %q, want %q", other, tt.wantOther)
			}
		})
	}
}
