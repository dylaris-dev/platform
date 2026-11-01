package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/storage"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const janitorOwner = "0a1b2c3d-0000-4000-8000-000000000001"

type janitorFakeStore struct {
	rows    map[int]*models.Template
	getErr  error
	connIDs []int
	connErr error
	setting string
	gets    []int
	// onGet runs before each GetTemplate answers, so a test can make a row
	// appear between the first look and the re-read before the delete.
	onGet func(id int)
}

func (f *janitorFakeStore) GetSetting(key string) (string, error) {
	if key == TemplatesStorageConnectionKey {
		return f.setting, nil
	}
	return "", nil
}

func (f *janitorFakeStore) TemplateExists(id int) (bool, error) {
	f.gets = append(f.gets, id)
	if f.onGet != nil {
		f.onGet(id)
	}
	if f.getErr != nil {
		return false, f.getErr
	}
	return f.rows[id] != nil, nil
}

func (f *janitorFakeStore) ListTemplateStorageConnectionIDs() ([]int, error) {
	return f.connIDs, f.connErr
}

type janitorRig struct {
	j      *TemplateJanitorService
	st     *janitorFakeStore
	mr     *miniredis.Miniredis
	base   string
	now    time.Time
	opened []int
}

// newJanitorRig wires a janitor on a local provider (connection 0) and a fresh
// miniredis. extra adds providers for other connection ids.
func newJanitorRig(t *testing.T, st *janitorFakeStore, extra map[int]storage.StorageProvider) *janitorRig {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	r := &janitorRig{st: st, mr: mr, base: t.TempDir(), now: time.Unix(1_800_000_000, 0)}
	core := &storage.LocalProvider{BasePath: r.base}
	r.j = NewTemplateJanitorService(st, rdb, func(connID int) (storage.StorageProvider, error) {
		r.opened = append(r.opened, connID)
		if connID == 0 {
			return core, nil
		}
		if p, ok := extra[connID]; ok {
			return p, nil
		}
		return nil, errors.New("no such connection")
	})
	r.j.now = func() time.Time { return r.now }
	r.j.tempDir = t.TempDir()
	return r
}

func (r *janitorRig) put(t *testing.T, key string) {
	t.Helper()
	p := filepath.Join(r.base, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *janitorRig) exists(key string) bool {
	_, err := os.Stat(filepath.Join(r.base, filepath.FromSlash(key)))
	return err == nil
}

// pass advances both clocks: the local one (temp sweep) and Redis's, which the
// orphan age is read from.
func (r *janitorRig) pass(after time.Duration) {
	r.now = r.now.Add(after)
	r.mr.FastForward(after)
	r.j.runOnce(context.Background())
}

func TestTemplateJanitorDeletesAnOrphanOnlyAfterTwoPassesAnHourApart(t *testing.T) {
	r := newJanitorRig(t, &janitorFakeStore{}, nil)
	r.put(t, janitorOwner+"/7/server.properties")
	key := "dylaris:templates:orphan:0:" + janitorOwner + "/7"

	r.pass(0)
	if !r.exists(janitorOwner + "/7") {
		t.Fatal("deleted on the first sighting")
	}
	if !r.mr.Exists(key) {
		t.Fatalf("first sighting not recorded under %s", key)
	}
	if ttl := r.mr.TTL(key); ttl != 48*time.Hour {
		t.Errorf("ttl = %s, want 48h", ttl)
	}
	r.pass(59 * time.Minute)
	if !r.exists(janitorOwner + "/7") {
		t.Fatal("deleted before an hour had passed")
	}
	r.pass(time.Minute)
	if r.exists(janitorOwner + "/7") {
		t.Fatal("orphan still there an hour after first seen")
	}
	if r.mr.Exists(key) {
		t.Error("redis key kept after the delete")
	}
	if !r.exists(janitorOwner) {
		t.Error("the owner folder went with the template root")
	}
}

// The age is read from Redis alone: a local clock far ahead does not make a
// fresh sighting old enough.
func TestTemplateJanitorAgeComesFromRedisNotTheLocalClock(t *testing.T) {
	r := newJanitorRig(t, &janitorFakeStore{}, nil)
	r.put(t, janitorOwner+"/7/a.txt")
	r.pass(0)
	r.now = r.now.Add(10 * time.Hour)
	r.j.runOnce(context.Background())
	if !r.exists(janitorOwner + "/7") {
		t.Fatal("deleted on a local clock jump with no time passed in Redis")
	}
	r.mr.FastForward(time.Hour)
	r.j.runOnce(context.Background())
	if r.exists(janitorOwner + "/7") {
		t.Fatal("not deleted an hour later by Redis's clock")
	}
}

// Sibling roots whose names share a prefix: only the one without a row goes.
func TestTemplateJanitorSiblingRoots(t *testing.T) {
	r := newJanitorRig(t, &janitorFakeStore{rows: map[int]*models.Template{7: {ID: 7, OwnerID: janitorOwner}}}, nil)
	r.put(t, janitorOwner+"/7/a.txt")
	r.put(t, janitorOwner+"/70/a.txt")
	r.pass(0)
	r.pass(time.Hour)
	if r.exists(janitorOwner + "/70") {
		t.Error("70 (no row) still there after the two-pass window")
	}
	if !r.exists(janitorOwner + "/7/a.txt") {
		t.Error("7 (row present) was touched")
	}
}

// Start must not wait for the first pass: a hung storage would hold up Core's
// startup and with it the HTTP listener.
func TestTemplateJanitorStartDoesNotBlock(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	j := NewTemplateJanitorService(&janitorFakeStore{}, rdb, func(int) (storage.StorageProvider, error) {
		entered <- struct{}{}
		<-release
		return nil, nil
	})
	j.tempDir = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(release)
	done := make(chan struct{})
	go func() { j.Start(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start blocked on the storage")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first pass never ran")
	}
}

func TestTemplateJanitorNeverDeletesARootWithARow(t *testing.T) {
	tenant := 4
	cases := []struct {
		name string
		row  *models.Template
	}{
		{"our storage", &models.Template{ID: 7, OwnerID: janitorOwner}},
		{"row says tenant storage", &models.Template{ID: 7, OwnerID: janitorOwner, BackupStorageID: &tenant}},
		{"row of another owner", &models.Template{ID: 7, OwnerID: "someone-else"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newJanitorRig(t, &janitorFakeStore{rows: map[int]*models.Template{7: tc.row}}, nil)
			r.put(t, janitorOwner+"/7/a.txt")
			for i := 0; i < 4; i++ {
				r.pass(2 * time.Hour)
			}
			if !r.exists(janitorOwner + "/7/a.txt") {
				t.Fatal("a root with a row was deleted")
			}
			if keys := r.mr.Keys(); len(keys) != 0 {
				t.Errorf("redis keys for a live root: %v", keys)
			}
		})
	}
}

// An answer the store could not give counts as "exists".
func TestTemplateJanitorKeepsTheRootWhenTheRowCannotBeRead(t *testing.T) {
	r := newJanitorRig(t, &janitorFakeStore{getErr: errors.New("db down")}, nil)
	r.put(t, janitorOwner+"/7/a.txt")
	for i := 0; i < 3; i++ {
		r.pass(2 * time.Hour)
	}
	if !r.exists(janitorOwner + "/7/a.txt") {
		t.Fatal("deleted on an unreadable row")
	}
}

func TestTemplateJanitorRowThatReappearsClearsTheKey(t *testing.T) {
	st := &janitorFakeStore{rows: map[int]*models.Template{}}
	r := newJanitorRig(t, st, nil)
	r.put(t, janitorOwner+"/7/a.txt")
	r.pass(0)
	if len(r.mr.Keys()) != 1 {
		t.Fatalf("keys after the first pass: %v", r.mr.Keys())
	}
	st.rows[7] = &models.Template{ID: 7, OwnerID: janitorOwner}
	r.pass(2 * time.Hour)
	if !r.exists(janitorOwner + "/7/a.txt") {
		t.Fatal("deleted although the row came back")
	}
	if keys := r.mr.Keys(); len(keys) != 0 {
		t.Errorf("key not cleared: %v", keys)
	}
}

// The row is read again right before the delete; one that appeared after the
// first look in the same pass keeps the files.
func TestTemplateJanitorRereadsTheRowRightBeforeTheDelete(t *testing.T) {
	st := &janitorFakeStore{rows: map[int]*models.Template{}}
	r := newJanitorRig(t, st, nil)
	r.put(t, janitorOwner+"/7/a.txt")
	r.pass(0)
	seen := 0
	st.onGet = func(id int) {
		seen++
		if seen == 2 {
			st.rows[7] = &models.Template{ID: 7, OwnerID: janitorOwner}
		}
	}
	r.pass(2 * time.Hour)
	if seen != 2 {
		t.Fatalf("row read %d times in the deleting pass, want 2", seen)
	}
	if !r.exists(janitorOwner + "/7/a.txt") {
		t.Fatal("deleted although the re-read found the row")
	}
}

func TestTemplateJanitorIgnoresKeysItDoesNotWrite(t *testing.T) {
	r := newJanitorRig(t, &janitorFakeStore{}, nil)
	keep := []string{
		"not-a-uuid/7/a.txt",
		strings.ToUpper(janitorOwner) + "/7/a.txt",
		janitorOwner + "/abc/a.txt",
		janitorOwner + "/007/a.txt",
		janitorOwner + "/0/a.txt",
		janitorOwner + "/-1/a.txt",
		janitorOwner + "/8",   // a file, not a template root
		janitorOwner + ".txt", // a file at the owner level
		"stray.txt",
	}
	for _, k := range keep {
		r.put(t, k)
	}
	for i := 0; i < 3; i++ {
		r.pass(2 * time.Hour)
	}
	for _, k := range keep {
		if !r.exists(k) {
			t.Errorf("%s was deleted", k)
		}
	}
	if len(r.st.gets) != 0 {
		t.Errorf("rows read for keys of the wrong shape: %v", r.st.gets)
	}
	if keys := r.mr.Keys(); len(keys) != 0 {
		t.Errorf("redis keys for keys of the wrong shape: %v", keys)
	}
}

// Core storage, the configured connection and every connection a row names;
// nothing else is ever opened, and a failing one does not stop the others.
func TestTemplateJanitorSweepsEveryConnectionOfOurs(t *testing.T) {
	base5 := t.TempDir()
	p5 := &storage.LocalProvider{BasePath: base5}
	st := &janitorFakeStore{setting: "3", connIDs: []int{5, 3}}
	r := newJanitorRig(t, st, map[int]storage.StorageProvider{5: p5})
	if err := os.MkdirAll(filepath.Join(base5, janitorOwner, "9"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base5, janitorOwner, "9", "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.pass(0)
	got := append([]int(nil), r.opened...)
	sort.Ints(got)
	if want := []int{0, 3, 5}; !equalInts(got, want) {
		t.Errorf("opened %v, want %v", got, want)
	}
	if !r.mr.Exists("dylaris:templates:orphan:5:" + janitorOwner + "/9") {
		t.Errorf("connection 5 not swept after connection 3 failed; keys %v", r.mr.Keys())
	}
	r.pass(2 * time.Hour)
	if _, err := os.Stat(filepath.Join(base5, janitorOwner, "9")); !os.IsNotExist(err) {
		t.Errorf("orphan on connection 5 kept: %v", err)
	}
}

// A store that cannot list the connections still sweeps Core storage and the
// configured one.
func TestTemplateJanitorConnectionListFailure(t *testing.T) {
	r := newJanitorRig(t, &janitorFakeStore{setting: "3", connErr: errors.New("db down")}, nil)
	r.pass(0)
	got := append([]int(nil), r.opened...)
	sort.Ints(got)
	if want := []int{0, 3}; !equalInts(got, want) {
		t.Errorf("opened %v, want %v", got, want)
	}
}

// Core storage never configured: open answers nil, nil and the pass skips it.
func TestTemplateJanitorSkipsAnUnconfiguredStorage(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	j := NewTemplateJanitorService(&janitorFakeStore{}, rdb, func(int) (storage.StorageProvider, error) { return nil, nil })
	j.tempDir = t.TempDir()
	j.runOnce(context.Background())
	if keys := mr.Keys(); len(keys) != 0 {
		t.Errorf("keys %v", keys)
	}
}

func TestTemplateJanitorOnlyTheLeaderSweepsStorage(t *testing.T) {
	r := newJanitorRig(t, &janitorFakeStore{}, nil)
	r.j.SetLeader(fakeLeader{leader: false})
	r.put(t, janitorOwner+"/7/a.txt")
	old := filepath.Join(r.j.tempDir, "template-x.zip")
	if err := os.WriteFile(old, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, r.now.Add(-7*time.Hour), r.now.Add(-7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	r.pass(0)
	r.pass(2 * time.Hour)
	if !r.exists(janitorOwner+"/7/a.txt") || len(r.opened) != 0 {
		t.Fatalf("a follower swept storage (opened %v)", r.opened)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the temp sweep did not run on a follower: %v", err)
	}
}

func TestSweepTemplateTemp(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	files := []struct {
		name     string
		age      time.Duration
		wantGone bool
	}{
		{"template-1.zip", 7 * time.Hour, true},
		{"template-upload-2.zip", 6*time.Hour + time.Minute, true},
		{"template-capture-3.zip", 5 * time.Hour, false},
		{"template-add-123456", 7 * time.Hour, true},
		{"template-add-654321", 5 * time.Hour, false},
		{"template-nested-7.zip", 7 * time.Hour, true},
		{"template-4.txt", 7 * time.Hour, false},
		{"dylaris-pack-5.zip", 7 * time.Hour, false},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-f.age), now.Add(-f.age)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "template-dir.zip"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-7 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "template-dir.zip"), old, old)

	sweepTemplateTemp(dir, now)

	for _, f := range files {
		_, err := os.Stat(filepath.Join(dir, f.name))
		if gone := os.IsNotExist(err); gone != f.wantGone {
			t.Errorf("%s: gone = %v, want %v", f.name, gone, f.wantGone)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "template-dir.zip")); err != nil {
		t.Errorf("a directory was removed: %v", err)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
