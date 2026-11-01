package handlers

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
	if backupstorage.CoreStorageSubPrefix != CoreStoragePrefixServerBackups {
		t.Errorf("backup.CoreStorageSubPrefix = %q, want %q (handlers.CoreStoragePrefixServerBackups)",
			backupstorage.CoreStorageSubPrefix, CoreStoragePrefixServerBackups)
	}
	if modpack.CoreStorageSubPrefix != CoreStoragePrefixModpacks {
		t.Errorf("modpack.CoreStorageSubPrefix = %q, want %q (handlers.CoreStoragePrefixModpacks)",
			modpack.CoreStorageSubPrefix, CoreStoragePrefixModpacks)
	}
}

func TestCoreStorageSubPrefixesAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for name, v := range map[string]string{
		"library":            CoreStoragePrefixLibrary,
		"ticket-attachments": CoreStoragePrefixAttachments,
		"ticket-backups":     CoreStoragePrefixBackups,
		"server-backups":     CoreStoragePrefixServerBackups,
		"modpacks":           CoreStoragePrefixModpacks,
		"templates":          CoreStoragePrefixTemplates,
	} {
		if prev, dup := seen[v]; dup {
			t.Errorf("sub-prefix %q used by both %s and %s; namespaces must not overlap", v, prev, name)
		}
		seen[v] = name
	}
}
