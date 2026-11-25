package services

import (
	"encoding/json"
	"sort"
	"testing"

	"dylaris-core/models"
)

type removedSubsFakeStore struct {
	manifestFakeStore
	server *models.Server
	// Each sub-server whose install record, mod rows and modpack snapshot were
	// all removed, i.e. the RemoveCopyTargetRows set.
	installDeleted  []string
	modpackRemoved  []string
	modsClearedSubs []string
}

func (f *removedSubsFakeStore) GetServerByID(int) (*models.Server, error) { return f.server, nil }

func (f *removedSubsFakeStore) DeleteSubServerInstall(_ int, sub string) error {
	f.installDeleted = append(f.installDeleted, sub)
	return nil
}

func (f *removedSubsFakeStore) ReplaceServerModpackContents(_ int, sub string, rows []models.ServerModpackContent) error {
	if rows == nil {
		f.modpackRemoved = append(f.modpackRemoved, sub)
	}
	return nil
}

func (f *removedSubsFakeStore) ReplaceServerMods(serverID int, sub string, mods []models.ServerMod) error {
	if mods == nil {
		f.modsClearedSubs = append(f.modsClearedSubs, sub)
	}
	return f.manifestFakeStore.ReplaceServerMods(serverID, sub, mods)
}

// R44-03: a whole-server restore that is not an overlay replaces the server
// root, so a sub-server created after the backup is gone from disk. Its rows
// must go with it, as on a sub-server delete; a scoped or overlay restore, a run
// without a manifest, and the active sub-server leave the rows alone.
func TestForgetSubServersRestoredAway(t *testing.T) {
	manifest := func(scope string, complete bool) string {
		b, _ := json.Marshal(&BackupManifest{
			Schema:   BackupManifestSchema,
			Scope:    scope,
			Complete: complete,
			Installs: []models.SubServerInstall{{SubServerName: "survival"}},
			Mods:     []BackupManifestMods{{SubServer: "survival", Mods: []BackupManifestMod{}}, {SubServer: "nether"}},
		})
		return string(b)
	}
	yes, no := true, false
	for _, tc := range []struct {
		name     string
		manifest string
		overlay  *bool
		want     []string
	}{
		{"whole-server full restore", manifest("", true), &no, []string{"creative"}},
		{"whole-server restore from before the overlay column", manifest("", true), nil, []string{"creative"}},
		// R45-03/R45-04: a manifest whose sub-server list a store error cut
		// short, or one from before the field, cannot say creative was absent.
		{"incomplete or legacy manifest", manifest("", false), &no, nil},
		{"overlay restore", manifest("", true), &yes, nil},
		{"scoped restore", manifest("survival", true), &no, nil},
		{"no manifest", "", &no, nil},
		{"unreadable manifest", "{not json", &no, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &removedSubsFakeStore{
				manifestFakeStore: manifestFakeStore{
					run: &models.BackupRun{ID: 1, Manifest: tc.manifest},
					// creative was created after the backup; lobby is active, so
					// the node refused the restore unless the archive held it.
					allInstall: []models.SubServerInstall{{SubServerName: "survival"}, {SubServerName: "creative"}, {SubServerName: "lobby"}},
					modSubs:    []string{"nether", "creative", "lobby"},
				},
				server: &models.Server{ID: 7, UUID: "u", ActiveSubServer: "lobby"},
			}
			(&BackupScheduler{store: st}).forgetSubServersRestoredAway(1, 7, tc.overlay)

			for what, got := range map[string][]string{
				"install records": st.installDeleted,
				"modpack snaps":   st.modpackRemoved,
				"mod rows":        st.modsClearedSubs,
			} {
				sort.Strings(got)
				if len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
					t.Errorf("%s removed for %v, want %v", what, got, tc.want)
				}
			}
		})
	}
}
