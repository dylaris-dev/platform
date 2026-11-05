package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

func i64(v int64) *int64 { return &v }

func TestBackupRates(t *testing.T) {
	const mbit = 1_000_000 / 8 // bytes/s per Mbit/s
	set := func(p backupPolicy) *backupPolicy {
		if p.HeadroomPct == nil {
			p.HeadroomPct = i64(30)
		}
		return &p
	}
	tests := []struct {
		name       string
		p          *backupPolicy
		env        int64
		players    bool
		tx         int64
		paused     bool
		saveOffFor time.Duration
		wantUp     rate.Limit
		wantRead   rate.Limit
	}{
		{"nobody online runs at full speed", set(backupPolicy{UplinkMbit: i64(1000), ReadMBps: i64(40)}), 0, false, 0, true, 0, rate.Inf, rate.Inf},
		{"no uplink known is never paced", set(backupPolicy{}), 0, true, 0, true, 0, rate.Inf, rate.Inf},
		{"uplink minus 30 percent headroom", set(backupPolicy{UplinkMbit: i64(1000)}), 0, true, 0, true, 0, rate.Limit(700 * mbit), rate.Inf},
		{"players' measured traffic comes off the budget", set(backupPolicy{UplinkMbit: i64(1000)}), 0, true, 100 * mbit, true, 0, rate.Limit(600 * mbit), rate.Inf},
		{"budget never below 10 percent of the uplink", set(backupPolicy{UplinkMbit: i64(1000)}), 0, true, 900 * mbit, true, 0, rate.Limit(100 * mbit), rate.Inf},
		{"small uplink floors at 4 Mbit/s", set(backupPolicy{UplinkMbit: i64(10)}), 0, true, 9 * mbit, true, 0, rate.Limit(4 * mbit), rate.Inf},
		{"floor never above the uplink", set(backupPolicy{UplinkMbit: i64(2)}), 0, true, 2 * mbit, true, 0, rate.Limit(2 * mbit), rate.Inf},
		{"env uplink when the panel has none", set(backupPolicy{}), 100, true, 0, true, 0, rate.Limit(70 * mbit), rate.Inf},
		{"panel value beats env", set(backupPolicy{UplinkMbit: i64(1000)}), 100, true, 0, true, 0, rate.Limit(700 * mbit), rate.Inf},
		{"read cap applies while players are online", set(backupPolicy{ReadMBps: i64(40)}), 0, true, 0, true, 0, rate.Inf, rate.Limit(40_000_000)},
		{"headroom is clamped to 90 percent", set(backupPolicy{UplinkMbit: i64(1000), HeadroomPct: i64(150)}), 0, true, 0, true, 0, rate.Limit(100 * mbit), rate.Inf},
		{"save-off ceiling reached lifts all pacing", set(backupPolicy{UplinkMbit: i64(1000), ReadMBps: i64(40), SaveOffMaxMin: i64(20)}), 0, true, 0, true, 20 * time.Minute, rate.Inf, rate.Inf},
		{"before the ceiling it still paces", set(backupPolicy{UplinkMbit: i64(1000), SaveOffMaxMin: i64(20)}), 0, true, 0, true, 19 * time.Minute, rate.Limit(700 * mbit), rate.Inf},
		{"ceiling 0 never paces while saving is off", set(backupPolicy{UplinkMbit: i64(1000), SaveOffMaxMin: i64(0)}), 0, true, 0, true, 0, rate.Inf, rate.Inf},
		{"ceiling does not apply once saving resumed (spool upload)", set(backupPolicy{UplinkMbit: i64(1000), SaveOffMaxMin: i64(0)}), 0, true, 0, false, 0, rate.Limit(700 * mbit), rate.Inf},
		{"no ceiling from Core paces for hours", set(backupPolicy{UplinkMbit: i64(1000)}), 0, true, 0, true, 3 * time.Hour, rate.Limit(700 * mbit), rate.Inf},
		// An older Core publishes nothing: env alone paces, with the compiled
		// 20 minute ceiling.
		{"old Core, env set, under default ceiling", nil, 100, true, 0, true, 10 * time.Minute, rate.Limit(70 * mbit), rate.Inf},
		{"old Core, env set, default ceiling reached", nil, 100, true, 0, true, 20 * time.Minute, rate.Inf, rate.Inf},
		{"old Core, no env: behaviour as before", nil, 0, true, 0, true, 0, rate.Inf, rate.Inf},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up, read := backupRates(tt.p, tt.env, tt.players, tt.tx, tt.paused, tt.saveOffFor, 1)
			if up != tt.wantUp || read != tt.wantRead {
				t.Fatalf("got up=%v read=%v, want up=%v read=%v", up, read, tt.wantUp, tt.wantRead)
			}
		})
	}
}

func TestParseBackupPolicyOldCore(t *testing.T) {
	for _, raw := range []string{"", "not json"} {
		if p := parseBackupPolicy(raw); p != nil {
			t.Fatalf("parseBackupPolicy(%q) = %+v, want nil (no policy)", raw, p)
		}
	}
	p := parseBackupPolicy(`{"uplinkMbit":500,"headroomPct":30,"saveOffMaxMin":null,"spoolEnabled":true,"spoolMaxGB":50}`)
	if p == nil || *p.UplinkMbit != 500 || p.SaveOffMaxMin != nil || !p.SpoolEnabled || p.SpoolMaxGB != 50 {
		t.Fatalf("parsed %+v", p)
	}
}

// The pacer must follow players joining and leaving mid-run, not only the
// state at the start.
func TestBackupPacerSwitchesLive(t *testing.T) {
	players := false
	now := time.Unix(1_000_000, 0)
	b := newBackupPacer(0)
	b.inputs = func() (*backupPolicy, int64, bool, int64, time.Time) {
		return &backupPolicy{UplinkMbit: i64(80), ReadMBps: i64(10), HeadroomPct: i64(50)}, 0, players, 0, now
	}
	b.savesPaused(now)
	steps := []struct {
		players        bool
		advance        time.Duration
		wantUp, wantRd rate.Limit
	}{
		{false, 0, rate.Inf, rate.Inf},
		{true, 2 * time.Second, rate.Limit(5_000_000), rate.Limit(10_000_000)},
		{false, 2 * time.Second, rate.Inf, rate.Inf},
		{true, 2 * time.Second, rate.Limit(5_000_000), rate.Limit(10_000_000)},
		// Within a second of the last refresh nothing is re-read.
		{false, 500 * time.Millisecond, rate.Limit(5_000_000), rate.Limit(10_000_000)},
	}
	for i, s := range steps {
		players = s.players
		now = now.Add(s.advance)
		b.refresh(i == 0)
		if b.up.Limit() != s.wantUp || b.read.Limit() != s.wantRd {
			t.Fatalf("step %d: up=%v read=%v, want %v %v", i, b.up.Limit(), b.read.Limit(), s.wantUp, s.wantRd)
		}
	}
}

func TestNodeLoad(t *testing.T) {
	t.Cleanup(func() { forgetServerLoad("a"); forgetServerLoad("b") })
	noteServerTx("a", 1000)
	noteServerPlayers("a", 0)
	noteServerTx("b", 500)
	if on, tx := nodeLoad(); on || tx != 1500 {
		t.Fatalf("nodeLoad = %v %d, want false 1500", on, tx)
	}
	noteServerPlayers("b", 2)
	if on, _ := nodeLoad(); !on {
		t.Fatal("a player on any server of the node must count")
	}
	forgetServerLoad("b")
	if on, tx := nodeLoad(); on || tx != 1000 {
		t.Fatalf("after forget: %v %d", on, tx)
	}
}

// slowPacer paces upload bytes past the first burst to 1 KB/s.
func slowPacer() *backupPacer {
	b := newBackupPacer(0)
	b.inputs = func() (*backupPolicy, int64, bool, int64, time.Time) {
		return nil, 0, false, 0, time.Now()
	}
	b.refresh(true)
	b.up.SetLimit(1000)
	b.last = time.Now().Add(time.Hour) // keep refresh from re-aiming it
	return b
}

// A part sent to a LAN address is not paced: the transfer is classified by the
// connection actually used. httptest listens on loopback.
func TestPutToLANIsNotPaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	defer srv.Close()
	u := &partUploader{client: srv.Client(), pace: slowPacer()}
	data := bytes.Repeat([]byte{1}, 1<<20) // 17 minutes at the slow rate
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // before srv.Close, which waits for the request
	done := make(chan error, 1)
	go func() { done <- u.put(ctx, srv.URL, data) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a LAN upload was paced")
	}
}

func TestUploadWaitPacesInternet(t *testing.T) {
	b := slowPacer()
	defer b.done()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	wait := b.uploadWait(ctx, func() bool { return false })
	if err := wait(backupPaceBurst); err != nil { // the burst
		t.Fatal(err)
	}
	if err := wait(100_000); err == nil {
		t.Fatal("an internet upload was not paced")
	}
	if err := b.uploadWait(ctx, func() bool { return true })(100_000); err != nil {
		t.Fatalf("LAN: %v", err)
	}
}

func TestSpoolBudget(t *testing.T) {
	const gb = 1_000_000_000
	on := &backupPolicy{SpoolEnabled: true, SpoolMaxGB: 50}
	tests := []struct {
		name   string
		p      *backupPolicy
		src    int64
		free   uint64
		wantOK bool
	}{
		{"no policy (old Core)", nil, gb, 100 * gb, false},
		{"spool off", &backupPolicy{SpoolMaxGB: 50}, gb, 100 * gb, false},
		{"fits", on, 30 * gb, 40 * gb, true},
		{"over the size limit uploads directly", on, 51 * gb, 500 * gb, false},
		{"not enough free space with the 5 GB reserve", on, 30 * gb, 30*gb + spoolReserveBytes - 1, false},
		{"exactly enough", on, 30 * gb, 30*gb + spoolReserveBytes, true},
		{"no size limit set", &backupPolicy{SpoolEnabled: true}, gb, 100 * gb, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limit, why := spoolBudget(tt.p, tt.src, tt.free)
			if (limit > 0) != tt.wantOK {
				t.Fatalf("limit=%d why=%q, want ok=%v", limit, why, tt.wantOK)
			}
			if tt.wantOK && limit != int64(tt.free-spoolReserveBytes) {
				t.Fatalf("limit=%d, want free minus reserve", limit)
			}
		})
	}
}

func TestSpoolArchiveStopsAtItsLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), spoolFileName(7))
	var counter countingWriter
	f, _, err := spoolArchive(path, 10, &counter, func(w io.Writer) (bool, error) {
		_, err := w.Write(make([]byte, 11))
		return true, err
	})
	if f != nil {
		f.Close()
	}
	if !errors.Is(err, errSpoolFull) {
		t.Fatalf("err = %v, want errSpoolFull", err)
	}

	path2 := filepath.Join(t.TempDir(), spoolFileName(8))
	f, added, err := spoolArchive(path2, 100, &counter, func(w io.Writer) (bool, error) {
		_, err := w.Write([]byte("archive"))
		return true, err
	})
	if err != nil || !added {
		t.Fatal(err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != "archive" {
		t.Fatalf("spooled file reads %q, want it rewound", got)
	}
}

func TestClearBackupSpoolRemovesOnlyRunFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{spoolFileName(1), spoolFileName(2), "keep.txt"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600)
	}
	clearBackupSpool(dir)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "keep.txt" {
		t.Fatalf("left %v", entries)
	}
}

// With the spool on, a running server's saving resumes once the archive is
// packed, BEFORE any part is uploaded, and the spool file is gone afterwards.
func TestRunBackupSpoolResumesSavingBeforeUpload(t *testing.T) {
	spool := t.TempDir()
	prevDir, prevPause, prevPolicy := globalSpoolDir, pauseSaves, backupPolicyNow.Load()
	t.Cleanup(func() {
		globalSpoolDir, pauseSaves = prevDir, prevPause
		backupPolicyNow.Store(prevPolicy)
	})
	globalSpoolDir = spool
	backupPolicyNow.Store(&backupPolicy{HeadroomPct: i64(30), SpoolEnabled: true, SpoolMaxGB: 1})

	core := newFakeCore(t, 1024)
	partsAtSaveOn := -1
	pauseSaves = func(_ context.Context, _ *redis.Client, _ *DockerManager, uuid string) *worldSaveGuard {
		return &worldSaveGuard{uuid: uuid, send: func(cmd string) {
			if cmd == "save-on" && partsAtSaveOn < 0 {
				core.bucket.mu.Lock()
				partsAtSaveOn = len(core.bucket.parts)
				core.bucket.mu.Unlock()
			}
		}}
	}

	report := runBackupWithFakeCore(t, core, nil)
	if report["status"] != "success" {
		t.Fatalf("report = %v, want success", report)
	}
	if partsAtSaveOn != 0 {
		t.Fatalf("saving resumed after %d parts, want before the first", partsAtSaveOn)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Fatalf("spool left %v behind", entries)
	}
}

// Spool off (or an older Core): saving stays paused until the upload is done.
func TestRunBackupWithoutSpoolResumesSavingAfterUpload(t *testing.T) {
	prevPause, prevPolicy := pauseSaves, backupPolicyNow.Load()
	t.Cleanup(func() { pauseSaves = prevPause; backupPolicyNow.Store(prevPolicy) })
	backupPolicyNow.Store(nil)

	core := newFakeCore(t, 1024)
	partsAtSaveOn := -1
	pauseSaves = func(_ context.Context, _ *redis.Client, _ *DockerManager, uuid string) *worldSaveGuard {
		return &worldSaveGuard{uuid: uuid, send: func(cmd string) {
			if cmd == "save-on" {
				core.bucket.mu.Lock()
				partsAtSaveOn = len(core.bucket.parts)
				core.bucket.mu.Unlock()
			}
		}}
	}
	if report := runBackupWithFakeCore(t, core, nil); report["status"] != "success" {
		t.Fatalf("report = %v", report)
	}
	if partsAtSaveOn < 1 {
		t.Fatalf("saving resumed after %d parts, want after the upload", partsAtSaveOn)
	}
}

// The storage's own cap holds whether anyone plays or not, past the save-off
// ceiling, and for a LAN destination too; with players the lower of the two.
func TestBackupPacerStorageCap(t *testing.T) {
	players := false
	now := time.Unix(2_000_000, 0)
	b := newBackupPacer(80) // 10 MB/s
	b.inputs = func() (*backupPolicy, int64, bool, int64, time.Time) {
		return &backupPolicy{UplinkMbit: i64(1000), HeadroomPct: i64(30), SaveOffMaxMin: i64(20)}, 0, players, 0, now
	}
	b.savesPaused(now)
	steps := []struct {
		name          string
		players       bool
		saveOff       time.Duration
		wantUp, wantL rate.Limit
	}{
		{"nobody online: the cap", false, 0, 10_000_000, 10_000_000},
		{"players: lower of budget (87.5 MB/s) and cap", true, 0, 10_000_000, 10_000_000},
		{"ceiling reached: the cap still holds", true, 25 * time.Minute, 10_000_000, 10_000_000},
	}
	for _, s := range steps {
		players = s.players
		now = time.Unix(2_000_000, 0).Add(s.saveOff)
		b.refresh(true)
		if b.up.Limit() != s.wantUp || b.upLAN.Limit() != s.wantL {
			t.Fatalf("%s: up=%v lan=%v, want %v %v", s.name, b.up.Limit(), b.upLAN.Limit(), s.wantUp, s.wantL)
		}
	}

	// A budget below the cap wins.
	small := newBackupPacer(800)
	small.inputs = func() (*backupPolicy, int64, bool, int64, time.Time) {
		return &backupPolicy{UplinkMbit: i64(40), HeadroomPct: i64(30)}, 0, true, 0, time.Now()
	}
	small.refresh(true)
	if small.up.Limit() != rate.Limit(28*1_000_000/8) || small.upLAN.Limit() != rate.Limit(100_000_000) {
		t.Fatalf("budget below cap: up=%v lan=%v", small.up.Limit(), small.upLAN.Limit())
	}

	if newBackupPacer(0).capReader(context.Background(), bytes.NewReader(nil)) == nil {
		t.Fatal("capReader returned nil")
	}
}

// A LAN upload ignores the players but not the storage's cap.
func TestPutToLANIsPacedByStorageCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	defer srv.Close()
	b := newBackupPacer(1) // 125 KB/s
	u := &partUploader{client: srv.Client(), pace: b}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := u.put(ctx, srv.URL, bytes.Repeat([]byte{1}, 4<<20)); err == nil {
		t.Fatal("4 MiB to a LAN storage capped at 1 Mbit/s finished within a second")
	}
}

// Two spooled runs started together each saw the whole free space and
// together filled the disk the servers run on. The first one's archive is
// held against the shared budget until its spool file is gone.
func TestSpoolReservationIsSharedAcrossRuns(t *testing.T) {
	const gb = 1_000_000_000
	on := &backupPolicy{SpoolEnabled: true, SpoolMaxGB: 50}
	t.Cleanup(func() { inflightWrites.Store(0) })
	inflightWrites.Store(0)
	tests := []struct {
		name     string
		inflight int64
		wantOK   bool
	}{
		{"alone", 0, true},
		{"another write holds most of the disk", 20 * gb, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inflightWrites.Store(tt.inflight)
			limit, release, why := reserveSpool(on, 30*gb, 50*gb)
			defer release()
			if (limit > 0) != tt.wantOK {
				t.Fatalf("limit=%d why=%q, want ok=%v", limit, why, tt.wantOK)
			}
		})
	}

	inflightWrites.Store(0)
	limit, release, _ := reserveSpool(on, 30*gb, 50*gb)
	if limit <= 0 {
		t.Fatal("first run refused")
	}
	if l2, r2, why := reserveSpool(on, 30*gb, 50*gb); l2 > 0 {
		r2()
		t.Fatalf("second run of 30 GB spooled into 50 GB beside the first (limit %d, %q)", l2, why)
	}
	release()
	release() // idempotent
	if got := inflightWrites.Load(); got != 0 {
		t.Fatalf("inflightWrites = %d after release, want 0", got)
	}
}

// Every run used to be paced to the whole uplink budget, so two at once sent
// twice what the players had left them.
func TestConcurrentUploadsSplitTheUplink(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	inputs := func() (*backupPolicy, int64, bool, int64, time.Time) {
		return &backupPolicy{UplinkMbit: i64(80), HeadroomPct: i64(50)}, 0, true, 0, now
	}
	a, b := newBackupPacer(0), newBackupPacer(0)
	a.inputs, b.inputs = inputs, inputs
	t.Cleanup(a.done)
	t.Cleanup(b.done)
	internet := func() bool { return false }
	for _, p := range []*backupPacer{a, b} {
		if err := p.uploadWait(context.Background(), internet)(1); err != nil {
			t.Fatal(err)
		}
	}
	steps := []struct {
		name string
		do   func()
		want rate.Limit
	}{
		{"two uploading split 5 MB/s", func() {}, 2_500_000},
		{"one left takes it all", b.done, 5_000_000},
	}
	for _, s := range steps {
		s.do()
		a.refresh(true)
		if a.up.Limit() != s.want {
			t.Fatalf("%s: up=%v, want %v", s.name, a.up.Limit(), s.want)
		}
	}
}

// Once the spool file is written its bytes are already gone from the free
// space, so holding the reservation through the paced upload counted them
// twice and refused restores and copies on a disk with room for them.
func TestSpoolReservationEndsOnceTheArchiveIsOnDisk(t *testing.T) {
	spool := t.TempDir()
	prevDir, prevPause, prevPolicy := globalSpoolDir, pauseSaves, backupPolicyNow.Load()
	t.Cleanup(func() {
		globalSpoolDir, pauseSaves = prevDir, prevPause
		backupPolicyNow.Store(prevPolicy)
		inflightWrites.Store(0)
	})
	globalSpoolDir = spool
	backupPolicyNow.Store(&backupPolicy{HeadroomPct: i64(30), SpoolEnabled: true, SpoolMaxGB: 1})
	inflightWrites.Store(0)

	core := newFakeCore(t, 1024)
	heldAtSaveOn := int64(-1)
	pauseSaves = func(_ context.Context, _ *redis.Client, _ *DockerManager, uuid string) *worldSaveGuard {
		return &worldSaveGuard{uuid: uuid, send: func(cmd string) {
			if cmd == "save-on" && heldAtSaveOn < 0 {
				heldAtSaveOn = inflightWrites.Load()
			}
		}}
	}
	if report := runBackupWithFakeCore(t, core, nil); report["status"] != "success" {
		t.Fatalf("report = %v, want success", report)
	}
	if heldAtSaveOn != 0 {
		t.Fatalf("inflightWrites = %d while the spooled archive uploads, want 0", heldAtSaveOn)
	}
}

// Dividing the floor too left each of seven runs at the 4 Mbit/s floor with
// about 36 KB/s per part, and a 64 MiB part then outlasts partPutTimeout.
func TestConcurrentUploadsKeepTheFloorEach(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	inputs := func() (*backupPolicy, int64, bool, int64, time.Time) {
		// 10 Mbit/s with the players sending 9: the budget is below the floor.
		return &backupPolicy{UplinkMbit: i64(10), HeadroomPct: i64(30)}, 0, true, 9_000_000 / 8, now
	}
	internet := func() bool { return false }
	pacers := make([]*backupPacer, 7)
	for i := range pacers {
		pacers[i] = newBackupPacer(0)
		pacers[i].inputs = inputs
		t.Cleanup(pacers[i].done)
		if err := pacers[i].uploadWait(context.Background(), internet)(1); err != nil {
			t.Fatal(err)
		}
	}
	for i, p := range pacers {
		p.refresh(true)
		if got := p.up.Limit(); got < backupUploadFloorBps {
			t.Fatalf("run %d of 7: up=%v, want at least the floor %d", i, got, backupUploadFloorBps)
		}
	}
}
