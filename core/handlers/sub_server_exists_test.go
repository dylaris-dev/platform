package handlers

import (
	"context"
	"errors"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// subServerExistsStore answers only ListSubServerInstalls; anything else panics
// on the nil embedded store.Store, and nothing else is reached here.
type subServerExistsStore struct {
	store.Store
	installs []models.SubServerInstall
	err      error
}

func (f *subServerExistsStore) ListSubServerInstalls(serverID int) ([]models.SubServerInstall, error) {
	return f.installs, f.err
}

func existsHandler(t *testing.T, st store.Store, seed string) *ServerHandler {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	if seed != "" {
		mr.Set(invKey, seed)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return &ServerHandler{state: &AppState{Redis: rdb, Store: st}}
}

// The switch guard used to consult ONE source: a node-written Redis cache
// refreshed every 10 seconds with disk quotas available and every 5 MINUTES
// without them. Both of its failure modes were wrong, in opposite directions -
// stale refused a sub-server that exists, absent skipped the check entirely.
//
// Measured on production before the fix: a sub-server created seconds earlier
// was on disk at 122 MB, listed by the files endpoint and by the install
// records, and the switch answered "No sub-server named second on this server"
// for five minutes.
func TestSubServerExists(t *testing.T) {
	populated := `{"total":1,"limit":2,"subServers":{"fromdisk":100}}`
	installs := []models.SubServerInstall{{SubServerName: "fresh"}}

	t.Run("the install record answers before the disk report has caught up", func(t *testing.T) {
		h := existsHandler(t, &subServerExistsStore{installs: installs}, populated)
		exists, answered := h.subServerExists(context.Background(), 1, invUUID, "fresh")
		if !exists || !answered {
			t.Fatalf("exists=%v answered=%v; a sub-server Core just created must be switchable", exists, answered)
		}
	})

	t.Run("a sub-server only the disk knows about still counts", func(t *testing.T) {
		// Older sub-servers predate the install table, so the disk report has
		// to keep its say rather than being replaced.
		h := existsHandler(t, &subServerExistsStore{}, populated)
		exists, answered := h.subServerExists(context.Background(), 1, invUUID, "fromdisk")
		if !exists || !answered {
			t.Fatalf("exists=%v answered=%v; a sub-server on disk must stay reachable", exists, answered)
		}
	})

	t.Run("a name neither source knows is refused", func(t *testing.T) {
		h := existsHandler(t, &subServerExistsStore{installs: installs}, populated)
		exists, answered := h.subServerExists(context.Background(), 1, invUUID, "ghost")
		if exists || !answered {
			t.Fatalf("exists=%v answered=%v; want a definite no", exists, answered)
		}
	})

	// The half that mattered most: "I could not check" used to mean "go ahead".
	t.Run("nothing can answer, so nothing is claimed", func(t *testing.T) {
		h := existsHandler(t, &subServerExistsStore{err: errors.New("db down")}, "")
		exists, answered := h.subServerExists(context.Background(), 1, invUUID, "anything")
		if exists || answered {
			t.Fatalf("exists=%v answered=%v; with both sources silent the caller must not be told anything", exists, answered)
		}
	})

	// A store failure alone must not become a definite no either: the disk
	// report is still a real answer.
	t.Run("the disk report carries it when the store cannot", func(t *testing.T) {
		h := existsHandler(t, &subServerExistsStore{err: errors.New("db down")}, populated)
		if exists, answered := h.subServerExists(context.Background(), 1, invUUID, "fromdisk"); !exists || !answered {
			t.Fatalf("exists=%v answered=%v", exists, answered)
		}
		if exists, answered := h.subServerExists(context.Background(), 1, invUUID, "ghost"); exists || !answered {
			t.Fatalf("exists=%v answered=%v; the disk report answered, so this is a definite no", exists, answered)
		}
	})
}
