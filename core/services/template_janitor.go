package services

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"dylaris-core/pkg/leader"
	"dylaris-core/storage"

	"github.com/redis/go-redis/v9"
)

// The template janitor removes template folders on OUR storage (Core storage
// and the operator's storage connections) that no template row names any more:
// a delete whose file removal failed, or an account removal that could not
// reach the storage. It never sees a tenant's own bucket - it opens storage by
// connection id only, and a tenant storage is not a connection.
//
// A root is deleted only on proof gathered over two passes: first seen without
// a row, recorded in Redis, and still without a row at least an hour later,
// read again immediately before the delete. A row is created before any of its
// files are written, so a template being created never looks like an orphan;
// the hour covers a row that was briefly unreadable or a replica that saw an
// older database state.
const (
	TemplateOrphanGrace   = time.Hour
	templateOrphanTTL     = 48 * time.Hour
	templateTempMaxAge    = 6 * time.Hour
	templateOrphanKeyBase = "dylaris:templates:orphan:"
)

// TemplateStageAddPattern is the os.CreateTemp pattern of one staged add-files
// part. It has no .zip suffix: the part is whatever file the user picked.
const TemplateStageAddPattern = "template-add-*"

// templateTempPatterns is every name template code stages under in the temp
// dir: uploads, captures, nested archives and installs are template-*.zip.
// A new staging name must be added here, or a crashed request leaks it.
var templateTempPatterns = []string{"template-*.zip", TemplateStageAddPattern}

var (
	// Postgres prints users.id in this form, and templateRootKey uses it as is.
	templateOwnerRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	templateIDRe    = regexp.MustCompile(`^[1-9][0-9]*$`)
)

type templateJanitorStore interface {
	GetSetting(key string) (string, error)
	TemplateExists(id int) (bool, error)
	ListTemplateStorageConnectionIDs() ([]int, error)
}

// TemplateJanitorService runs hourly. The storage sweep is leader-gated; the
// temp-file sweep runs on every replica, because each one has its own temp dir.
type TemplateJanitorService struct {
	store    templateJanitorStore
	rdb      *redis.Client
	open     func(connID int) (storage.StorageProvider, error)
	leader   leader.Election
	interval time.Duration
	now      func() time.Time
	tempDir  string
}

// NewTemplateJanitorService takes open, which builds the template storage of a
// connection (0 = Core storage) and answers nil, nil when that storage was
// never configured and so cannot hold anything.
func NewTemplateJanitorService(st templateJanitorStore, rdb *redis.Client, open func(connID int) (storage.StorageProvider, error)) *TemplateJanitorService {
	return &TemplateJanitorService{store: st, rdb: rdb, open: open, interval: time.Hour, now: time.Now, tempDir: os.TempDir()}
}

func (s *TemplateJanitorService) SetLeader(l leader.Election) { s.leader = l }

func (s *TemplateJanitorService) Start(ctx context.Context) {
	log.Printf("Template janitor started (interval: %s)", s.interval)
	ticker := time.NewTicker(s.interval)
	go func() {
		defer ticker.Stop()
		// The first pass runs in here, not before the goroutine: a slow or
		// unreachable storage would otherwise hold up Core's startup.
		for {
			s.runOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *TemplateJanitorService) runOnce(ctx context.Context) {
	sweepTemplateTemp(s.tempDir, s.now())
	if s.leader != nil && !s.leader.IsLeader() {
		return
	}
	if s.rdb == nil {
		// Without Redis there is no first sighting to compare against.
		return
	}
	ignored := 0
	for _, conn := range s.connections() {
		prov, err := s.open(conn)
		if err != nil {
			logErrf("template-janitor", "connection %d: cannot open storage, skipped this pass: %v", conn, err)
			continue
		}
		if prov == nil {
			continue
		}
		n, err := s.sweep(ctx, conn, prov)
		ignored += n
		if err != nil {
			logErrf("template-janitor", "connection %d: %v, skipped the rest of this storage this pass", conn, err)
		}
	}
	if ignored > 0 {
		log.Printf("template-janitor: ignored %d entr(ies) that are not template folders", ignored)
	}
}

// connections is Core storage, the configured template connection and every
// connection a template row names. A failed list still sweeps the first two.
func (s *TemplateJanitorService) connections() []int {
	set := map[int]bool{0: true}
	if v, _ := s.store.GetSetting(TemplatesStorageConnectionKey); v != "" {
		if id, err := strconv.Atoi(v); err == nil && id > 0 {
			set[id] = true
		}
	}
	ids, err := s.store.ListTemplateStorageConnectionIDs()
	if err != nil {
		logErrf("template-janitor", "list template storage connections: %v", err)
	}
	for _, id := range ids {
		if id > 0 {
			set[id] = true
		}
	}
	out := make([]int, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// sweep walks <owner>/<id>/ of one storage and returns how many entries had a
// shape the template code never writes.
func (s *TemplateJanitorService) sweep(ctx context.Context, conn int, prov storage.StorageProvider) (int, error) {
	owners, err := prov.ListFiles(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("list owners: %w", err)
	}
	ignored := 0
	for _, o := range owners {
		if !o.IsDir || !templateOwnerRe.MatchString(o.Name) {
			ignored++
			continue
		}
		roots, err := prov.ListFiles(ctx, o.Name)
		if err != nil {
			return ignored, fmt.Errorf("list %s: %w", o.Name, err)
		}
		for _, r := range roots {
			id, err := strconv.Atoi(r.Name)
			if !r.IsDir || !templateIDRe.MatchString(r.Name) || err != nil {
				ignored++
				continue
			}
			if err := s.checkRoot(ctx, conn, prov, o.Name, id); err != nil {
				return ignored, err
			}
		}
	}
	return ignored, nil
}

// checkRoot records, keeps or deletes one template root. An error is a Redis
// failure: without it there is no proof, so the storage pass stops.
func (s *TemplateJanitorService) checkRoot(ctx context.Context, conn int, prov storage.StorageProvider, owner string, id int) error {
	// Both segments are validated by the caller, so root is never the area root.
	root := owner + "/" + strconv.Itoa(id)
	key := templateOrphanKeyBase + strconv.Itoa(conn) + ":" + root
	if s.rowExists(id) {
		if err := s.rdb.Del(ctx, key).Err(); err != nil {
			return fmt.Errorf("clear %s: %w", key, err)
		}
		return nil
	}
	// The age comes from the key's own TTL, which Redis counts down, so no
	// replica's clock takes part in it.
	ttl, err := s.rdb.TTL(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("read %s: %w", key, err)
	}
	switch {
	case ttl == -2: // no key: first sighting
		if err := s.rdb.SetNX(ctx, key, 1, templateOrphanTTL).Err(); err != nil {
			return fmt.Errorf("record %s: %w", key, err)
		}
		return nil
	case ttl < 0: // a key without expiry is not ours; restart its clock
		if err := s.rdb.Expire(ctx, key, templateOrphanTTL).Err(); err != nil {
			return fmt.Errorf("expire %s: %w", key, err)
		}
		return nil
	}
	age := templateOrphanTTL - ttl
	if age < TemplateOrphanGrace {
		return nil
	}
	if s.rowExists(id) {
		if err := s.rdb.Del(ctx, key).Err(); err != nil {
			return fmt.Errorf("clear %s: %w", key, err)
		}
		return nil
	}
	if err := prov.DeletePath(ctx, root); err != nil {
		logErrf("template-janitor", "connection %d: delete orphan %s: %v", conn, root, err)
		return nil
	}
	log.Printf("template-janitor: connection %d: deleted orphan template folder %s (no row for %s)", conn, root, age.Round(time.Minute))
	if err := s.rdb.Del(ctx, key).Err(); err != nil {
		logErrf("template-janitor", "clear %s: %v", key, err)
	}
	return nil
}

// rowExists treats an answer the store could not give as "exists".
func (s *TemplateJanitorService) rowExists(id int) bool {
	ok, err := s.store.TemplateExists(id)
	if err != nil {
		logErrf("template-janitor", "read template %d: %v, keeping its files", id, err)
		return true
	}
	return ok
}

// sweepTemplateTemp removes the template staging files a crashed request left
// in this replica's temp dir. Six hours is far beyond any upload, capture,
// add-files or install.
func sweepTemplateTemp(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		logErrf("template-janitor", "read temp dir: %v", err)
		return
	}
	for _, e := range entries {
		if !templateTempName(e.Name()) || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < templateTempMaxAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
			logErrf("template-janitor", "remove temp %s: %v", e.Name(), err)
		}
	}
}

func templateTempName(name string) bool {
	for _, p := range templateTempPatterns {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}
