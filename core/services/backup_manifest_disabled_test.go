package services

import "testing"

// A switched-off mod comes back switched off from the archive that holds its
// jar in .disabled/, and a file name that is not one never becomes a row.
func TestManifestModRowsCarryDisabledAndDropBadNames(t *testing.T) {
	rows := ManifestModRows([]BackupManifestMod{
		{ModrinthProjectID: "a", FileName: "a.jar", Disabled: true},
		{ModrinthProjectID: "b", FileName: "b.jar"},
		{ModrinthProjectID: "c", FileName: "../../other/c.jar"},
		{ModrinthProjectID: "d", FileName: ""},
	})
	if len(rows) != 2 {
		t.Fatalf("rows %+v, want a and b only", rows)
	}
	if !rows[0].Disabled || rows[1].Disabled {
		t.Fatalf("disabled = %v %v, want true false", rows[0].Disabled, rows[1].Disabled)
	}
}
