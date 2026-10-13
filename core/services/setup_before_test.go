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

	RememberSetupBefore(ctx, rdb, srv, "modded")
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
