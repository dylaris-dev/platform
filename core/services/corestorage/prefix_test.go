package corestorage

import (
	"testing"

	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/storage/modpack"
)

// TestCoreStorageSubPrefixesMatch locks the duplicated sub-prefix literals
// together. storage/backup and storage/modpack must NOT import handlers
// (handlers already imports both), so each carries its own copy of the
// namespace string. This test is the only thing preventing them from drifting
// apart and silently splitting one data set across two prefixes.
func TestCoreStorageSubPrefixesMatch(t *testing.T) {
	if backupstorage.CoreStorageSubPrefix != PrefixServerBackups {
		t.Errorf("backup.CoreStorageSubPrefix = %q, want %q (handlers.PrefixServerBackups)",
			backupstorage.CoreStorageSubPrefix, PrefixServerBackups)
	}
	if modpack.CoreStorageSubPrefix != PrefixModpacks {
		t.Errorf("modpack.CoreStorageSubPrefix = %q, want %q (handlers.PrefixModpacks)",
			modpack.CoreStorageSubPrefix, PrefixModpacks)
	}
}

func TestCoreStorageSubPrefixesAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for name, v := range map[string]string{
		"library":            PrefixLibrary,
		"ticket-attachments": PrefixAttachments,
		"ticket-backups":     PrefixBackups,
		"server-backups":     PrefixServerBackups,
		"modpacks":           PrefixModpacks,
		"templates":          PrefixTemplates,
	} {
		if prev, dup := seen[v]; dup {
			t.Errorf("sub-prefix %q used by both %s and %s; namespaces must not overlap", v, prev, name)
		}
		seen[v] = name
	}
}
