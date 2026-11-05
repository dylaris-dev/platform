package services

import (
	"context"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

type setupBeforeStore struct {
	store.Store
	written []string
	install *models.SubServerInstall // the sub-server's install record
}

func (s *setupBeforeStore) GetSubServerInstall(serverID int, sub string) (*models.SubServerInstall, error) {
	return s.install, nil
}

func (s *setupBeforeStore) UpsertSubServerInstall(in models.SubServerInstall) error {
	s.install = &in
	return nil
}

func (s *setupBeforeStore) DeleteSubServerInstall(serverID int, sub string) error {
	s.install = nil
	return nil
}

func (s *setupBeforeStore) UpdateServerSetup(id int, image, command, active, flags, installer, mc, build string) error {
	s.written = append(s.written, active+"|"+installer+"|"+mc)
	return nil
}

// A failed install left Core on the new sub-server and loader while the node
// never switched. The columns as they were before are put back - only for the
// setup they were recorded for, and only once.
func TestAFailedSetupPutsTheServerBack(t *testing.T) {
	rdb := newQueueTestRedis(t)
	ctx := context.Background()
	st := &setupBeforeStore{}
	srv := &models.Server{ID: 3, UUID: "srv-3", ActiveSubServer: "survival", InstallerType: "paper", MinecraftVersion: "1.21.1"}

	RememberSetupBefore(ctx, rdb, st, srv, "modded")
	if RestoreSetupBefore(ctx, rdb, st, srv, "other") {
		t.Fatal("restored for a setup to a different sub-server")
	}
	if !RestoreSetupBefore(ctx, rdb, st, srv, "modded") {
		t.Fatal("did not restore the setup it recorded")
	}
	if len(st.written) != 1 || st.written[0] != "survival|paper|1.21.1" {
		t.Fatalf("restored %v, want the previous sub-server and loader", st.written)
	}
	if RestoreSetupBefore(ctx, rdb, st, srv, "modded") {
		t.Fatal("restored a second time")
	}
}

// The install record names the version the edit form and the mod checks use,
// and it is written before the command goes out like the servers row. A
// failed install puts it back too, or removes the one the install wrote.
func TestAFailedSetupPutsTheInstallRecordBack(t *testing.T) {
	for _, c := range []struct {
		name   string
		before *models.SubServerInstall
	}{
		{"had a record", &models.SubServerInstall{ServerID: 3, SubServerName: "modded", InstallerType: "paper", McVersion: "1.21.1"}},
		{"had none", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			rdb := newQueueTestRedis(t)
			ctx := context.Background()
			st := &setupBeforeStore{install: c.before}
			srv := &models.Server{ID: 3, UUID: "srv-3", ActiveSubServer: "modded", InstallerType: "paper", MinecraftVersion: "1.21.1"}

			RememberSetupBefore(ctx, rdb, st, srv, "modded")
			st.install = &models.SubServerInstall{ServerID: 3, SubServerName: "modded", InstallerType: "paper", McVersion: "1.21.4"}
			if !RestoreSetupBefore(ctx, rdb, st, srv, "modded") {
				t.Fatal("did not restore the setup it recorded")
			}
			if c.before == nil {
				if st.install != nil {
					t.Fatalf("install record = %+v, want it removed", st.install)
				}
				return
			}
			if st.install == nil || st.install.ServerID != 3 || st.install.McVersion != "1.21.1" {
				t.Fatalf("install record = %+v, want the one from before", st.install)
			}
		})
	}
}
