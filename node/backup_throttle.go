package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// A backup yields to players. While anyone is online on this node, a backup
// run paces its upload to what the uplink can spare and, optionally, its disk
// read (which paces gzip with it, since the uncompressed bytes are what gets
// read). With nobody online it runs at full speed, as it always did.
//
// The policy comes from Core in dylaris:node:<id>:backup_policy, re-published
// every 30 s. A missing key - an older Core, or nothing configured - means no
// policy: no pacing unless NODE_UPLINK_MBIT is set, and never a spool. That is
// the behaviour before this existed, so a node updated ahead of Core changes
// nothing.
//
// No cap here can be 0 B/s. A rate of zero is not a policy, it is a backup that
// never finishes while save-off holds the world unsaved - the same reason the
// beam throttle has none. The upload budget has a floor instead, and "no cap"
// is the absence of a value.

// backupPolicy is what Core publishes. Pointers follow the platform limits
// convention: nil is "not set".
type backupPolicy struct {
	// UplinkMbit is the node's upload capacity as entered in the panel. Nil
	// falls back to NODE_UPLINK_MBIT; neither means the upload is never paced.
	UplinkMbit *int64 `json:"uplinkMbit"`
	// ReadMBps caps the archive's disk read (uncompressed MB/s) while players
	// are online. Nil is no read cap.
	ReadMBps *int64 `json:"readMBps"`
	// HeadroomPct of the uplink is kept free for players on top of what their
	// servers are measured sending.
	HeadroomPct *int64 `json:"headroomPct"`
	// SaveOffMaxMin ends the pacing once world saving has been off this long,
	// so a slow backup cannot hold the world unsaved for hours. Nil is no
	// ceiling; 0 is "never pace while saving is off".
	SaveOffMaxMin *int64 `json:"saveOffMaxMin"`
	// SpoolEnabled archives to a local file first, so saving resumes as soon as
	// the archive is packed and only the upload is slow.
	SpoolEnabled bool `json:"spoolEnabled"`
	// SpoolMaxGB is the largest server (uncompressed) that is spooled; a larger
	// one is uploaded directly.
	SpoolMaxGB int64 `json:"spoolMaxGB"`
}

const (
	defaultBackupHeadroomPct = 30
	defaultSaveOffMaxMin     = 20
	// backupUploadFloorBps is the least the upload is paced to, whatever the
	// budget says (4 Mbit/s). Two parts upload in parallel, and a 64 MiB part
	// must finish inside partPutTimeout; at 2 Mbit/s each it takes under 5
	// minutes.
	backupUploadFloorBps = 4_000_000 / 8
	// backupPaceBurst is the largest single wait. Reads come in 32 KiB pieces.
	backupPaceBurst = 256 * 1024
)

func backupPolicyKey(nodeID string) string { return "dylaris:node:" + nodeID + ":backup_policy" }

// backupEnvKey is where the node tells Core what it knows that Core cannot: the
// env uplink and the spool directory with its free space, for the panel.
func backupEnvKey(nodeID string) string { return "dylaris:node:" + nodeID + ":backup_env" }

// envUplinkMbit reads NODE_UPLINK_MBIT. Anything but a positive integer is
// unset: a typo must not pace every backup to the floor.
func envUplinkMbit() int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("NODE_UPLINK_MBIT")), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// backupPolicyNow is the policy last read from Redis; nil is none.
var backupPolicyNow atomic.Pointer[backupPolicy]

// nodeEnvUplinkMbit is NODE_UPLINK_MBIT, read once at start.
var nodeEnvUplinkMbit int64

// parseBackupPolicy reads Core's JSON. An unparseable value is no policy rather
// than a guess.
func parseBackupPolicy(raw string) *backupPolicy {
	var p backupPolicy
	if raw == "" || json.Unmarshal([]byte(raw), &p) != nil {
		return nil
	}
	return &p
}

// watchBackupPolicy keeps backupPolicyNow current and reports the node's side
// to Core. A Redis error other than a missing key keeps the last policy: an
// unreachable Redis is not an instruction to stop pacing.
func watchBackupPolicy(ctx context.Context, rdb *redis.Client, nodeID, spoolDir string) {
	nodeEnvUplinkMbit = envUplinkMbit()
	reload := func() {
		raw, err := rdb.Get(ctx, backupPolicyKey(nodeID)).Result()
		switch {
		case errors.Is(err, redis.Nil):
			backupPolicyNow.Store(nil)
		case err != nil:
			// NOPERM from an older Core's ACL lands here too, and keeps nil.
		default:
			backupPolicyNow.Store(parseBackupPolicy(raw))
		}
		_, free := getDiskSpace(spoolDir)
		env := map[string]any{"spoolDir": spoolDir, "spoolFreeBytes": free, "uplinkMbit": nil}
		if nodeEnvUplinkMbit > 0 {
			env["uplinkMbit"] = nodeEnvUplinkMbit
		}
		if b, err := json.Marshal(env); err == nil {
			rdb.Set(ctx, backupEnvKey(nodeID), b, time.Minute)
		}
	}
	reload()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reload()
		}
	}
}

// serverLoad is what a running server's stats collector last saw.
type serverLoad struct {
	players int
	txBps   int64
}

var (
	serverLoadsMu sync.Mutex
	serverLoads   = map[string]serverLoad{}
)

func noteServerPlayers(uuid string, players int) {
	serverLoadsMu.Lock()
	l := serverLoads[uuid]
	l.players = players
	serverLoads[uuid] = l
	serverLoadsMu.Unlock()
}

func noteServerTx(uuid string, txBps uint64) {
	serverLoadsMu.Lock()
	l := serverLoads[uuid]
	l.txBps = int64(min(txBps, math.MaxInt64))
	serverLoads[uuid] = l
	serverLoadsMu.Unlock()
}

func forgetServerLoad(uuid string) {
	serverLoadsMu.Lock()
	delete(serverLoads, uuid)
	serverLoadsMu.Unlock()
}

// nodeLoad reports whether anyone is online on any server of this node, and
// what those servers are sending - the players' share of the uplink.
func nodeLoad() (playersOnline bool, txBps int64) {
	serverLoadsMu.Lock()
	defer serverLoadsMu.Unlock()
	for _, l := range serverLoads {
		if l.players > 0 {
			playersOnline = true
		}
		txBps += l.txBps
	}
	return playersOnline, txBps
}

// backupRates is the whole decision, in bytes/s, rate.Inf for unpaced.
//
// upload = uplink - headroom - what the servers send, never below the floor
// (and never above the uplink). The headroom is a share, not a fixed number: a
// 40 Mbit/s home uplink has no 50 Mbit/s to spare. What keeps players smooth is
// that the uplink never fills, so no queue builds in front of their packets.
func backupRates(p *backupPolicy, envUplinkMbit int64, playersOnline bool, txBps int64, savesPaused bool, saveOffFor time.Duration) (upload, read rate.Limit) {
	if p == nil {
		p = &backupPolicy{}
	}
	if !playersOnline {
		return rate.Inf, rate.Inf
	}
	ceiling := time.Duration(defaultSaveOffMaxMin) * time.Minute
	if p.SaveOffMaxMin != nil {
		ceiling = time.Duration(*p.SaveOffMaxMin) * time.Minute
	} else if backupPolicyIsSet(p) {
		ceiling = -1 // Core said: no ceiling
	}
	if savesPaused && ceiling >= 0 && saveOffFor >= ceiling {
		return rate.Inf, rate.Inf
	}

	read = rate.Inf
	if p.ReadMBps != nil && *p.ReadMBps > 0 {
		read = rate.Limit(*p.ReadMBps * 1_000_000)
	}

	uplinkMbit := envUplinkMbit
	if p.UplinkMbit != nil && *p.UplinkMbit > 0 {
		uplinkMbit = *p.UplinkMbit
	}
	if uplinkMbit <= 0 {
		return rate.Inf, read
	}
	headroom := int64(defaultBackupHeadroomPct)
	if p.HeadroomPct != nil {
		headroom = min(max(*p.HeadroomPct, 0), 90)
	}
	capBps := uplinkMbit * 1_000_000 / 8
	budget := capBps*(100-headroom)/100 - txBps
	floor := min(max(capBps/10, backupUploadFloorBps), capBps)
	return rate.Limit(max(budget, floor)), read
}

// backupPolicyIsSet tells "Core sent a policy with no ceiling" from "no policy
// at all", which defaults the ceiling. Core always sends the headroom.
func backupPolicyIsSet(p *backupPolicy) bool { return p.HeadroomPct != nil }

// backupPacer paces one backup run. The limiters are re-aimed at most once a
// second from the current policy and load, so a player joining or leaving
// mid-run changes the pace within the stats collector's ping interval.
type backupPacer struct {
	// up paces uploads that share the uplink with the players, upLAN the rest.
	// Both carry the storage's own cap; up also the players' budget.
	up, upLAN, read *rate.Limiter
	// storageCap is the destination's upload cap (rate.Inf for none). It holds
	// whether or not anyone is online and past the save-off ceiling: it is a
	// property of where the bytes go, a slow NAS or a metered link.
	storageCap rate.Limit
	// pausedSince is when world saving was paused, unix nanoseconds; 0 while
	// it is not.
	pausedSince atomic.Int64
	// inputs is a variable so tests can drive the load and the clock.
	inputs func() (p *backupPolicy, envUplink int64, players bool, txBps int64, now time.Time)

	mu   sync.Mutex
	last time.Time
}

func newBackupPacer(storageCapMbit int64) *backupPacer {
	storageCap := rate.Inf
	if storageCapMbit > 0 {
		storageCap = rate.Limit(storageCapMbit * 1_000_000 / 8)
	}
	return &backupPacer{
		up:         rate.NewLimiter(storageCap, backupPaceBurst),
		upLAN:      rate.NewLimiter(storageCap, backupPaceBurst),
		read:       rate.NewLimiter(rate.Inf, backupPaceBurst),
		storageCap: storageCap,
		inputs: func() (*backupPolicy, int64, bool, int64, time.Time) {
			players, tx := nodeLoad()
			return backupPolicyNow.Load(), nodeEnvUplinkMbit, players, tx, time.Now()
		},
	}
}

func (b *backupPacer) savesPaused(at time.Time) { b.pausedSince.Store(at.UnixNano()) }
func (b *backupPacer) savesResumed()            { b.pausedSince.Store(0) }

func (b *backupPacer) refresh(force bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, env, players, tx, now := b.inputs()
	if !force && now.Sub(b.last) < time.Second {
		return
	}
	b.last = now
	var saveOffFor time.Duration
	since := b.pausedSince.Load()
	if since != 0 {
		saveOffFor = now.Sub(time.Unix(0, since))
	}
	up, read := backupRates(p, env, players, tx, since != 0, saveOffFor)
	up = min(up, b.storageCap)
	if b.up.Limit() != up {
		b.up.SetLimitAt(now, up)
	}
	if b.read.Limit() != read {
		b.read.SetLimitAt(now, read)
	}
}

func (b *backupPacer) wait(ctx context.Context, lim *rate.Limiter, n int) error {
	if b == nil {
		return nil
	}
	b.refresh(false)
	for n > 0 {
		step := min(n, backupPaceBurst)
		if err := lim.WaitN(ctx, step); err != nil {
			return err
		}
		n -= step
	}
	return nil
}

// reader paces the archive's source bytes.
func (b *backupPacer) reader(ctx context.Context, r io.Reader) io.Reader {
	if b == nil {
		return r
	}
	return &pacedReader{r: r, wait: func(n int) error { return b.wait(ctx, b.read, n) }}
}

// uploadWait paces upload bytes. A LAN destination does not share the uplink
// with the players, so only the storage's own cap applies to it.
func (b *backupPacer) uploadWait(ctx context.Context, lan func() bool) func(int) error {
	return func(n int) error {
		if b == nil {
			return nil
		}
		if lan() {
			return b.wait(ctx, b.upLAN, n)
		}
		return b.wait(ctx, b.up, n)
	}
}

// capReader paces a filesystem destination (shared, local, node-local) to the
// storage's cap alone: those writes do not leave through the uplink.
func (b *backupPacer) capReader(ctx context.Context, r io.Reader) io.Reader {
	if b == nil || b.storageCap == rate.Inf {
		return r
	}
	return &pacedReader{r: r, wait: func(n int) error { return b.wait(ctx, b.upLAN, n) }}
}

// pacedReader waits for the bytes it has just read before handing them on.
type pacedReader struct {
	r    io.Reader
	wait func(n int) error
}

func (p *pacedReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	if n > 0 {
		if werr := p.wait(n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func logBackupPace(runID int, b *backupPacer) {
	if b == nil {
		return
	}
	b.refresh(true)
	up, read := b.up.Limit(), b.read.Limit()
	if up == rate.Inf && read == rate.Inf {
		return
	}
	players, _ := nodeLoad()
	log.Printf("backup %d: pacing upload %s (storage cap %s), read %s, players online: %v", runID, fmtRate(up), fmtRate(b.storageCap), fmtRate(read), players)
}

func fmtRate(l rate.Limit) string {
	if l == rate.Inf {
		return "unpaced"
	}
	return strconv.FormatFloat(float64(l)*8/1_000_000, 'f', 1, 64) + " Mbit/s"
}
