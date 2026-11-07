package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// The spool packs a backup into a local file before uploading it. World saving
// is off only while the archive is packed; the slow, paced upload then runs
// with saving back on. It costs disk space the size of the archive, which is
// why it is opt-in per node and checked before every run. Never memory.

// spoolReserveBytes is left free on the spool's filesystem, on top of the
// archive: the servers on the same disk keep writing while it is packed.
const spoolReserveBytes = 5 << 30

// backupSpoolDir is where spooled archives go: NODE_BACKUP_SPOOL_DIR, else a
// hidden directory in the first storage path. That is the disk the node's data
// is mounted on, where a path elsewhere in the container could be its writable
// layer. The leading dot keeps every server-directory scan away from it.
func backupSpoolDir(storagePaths []string) string {
	if d := strings.TrimSpace(os.Getenv("NODE_BACKUP_SPOOL_DIR")); d != "" {
		return d
	}
	if len(storagePaths) == 0 {
		return ""
	}
	return filepath.Join(storagePaths[0], ".dylaris-backup-spool")
}

const spoolFilePrefix = "run-"

// globalSpoolDir is backupSpoolDir, set once at start.
var globalSpoolDir string

func spoolFileName(runID int) string { return spoolFilePrefix + strconv.Itoa(runID) + ".tar.gz" }

// clearBackupSpool removes archives left by runs that a node restart ended.
// No run survives a restart, so every spool file of THIS node found at start is
// debris. On storage another node also uses (the default spool sits in the
// first storage path), a spool file may be that node's live upload, and the
// names do not tell the two apart: left alone, like the other boot sweeps.
func clearBackupSpool(dir string) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("backup spool: cannot create %s: %v", dir, err)
		return
	}
	if sharedWithAnotherNode(filepath.Dir(dir)) {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), spoolFilePrefix) {
			if os.Remove(filepath.Join(dir, e.Name())) == nil {
				log.Printf("backup spool: removed %s, left by an interrupted run", e.Name())
			}
		}
	}
}

// spoolBudget decides whether a run spools, and how many bytes the spool file
// may grow to. 0 means upload directly. sourceBytes is the uncompressed size
// of what is archived, an upper bound for the archive (gzip adds well under a
// percent on incompressible data, covered by the reserve).
func spoolBudget(p *backupPolicy, sourceBytes int64, freeBytes uint64) (int64, string) {
	switch {
	case p == nil || !p.SpoolEnabled:
		return 0, ""
	case p.SpoolMaxGB <= 0 || sourceBytes > p.SpoolMaxGB*1_000_000_000:
		return 0, fmt.Sprintf("the server (%d MB) is larger than the spool limit of %d GB", sourceBytes/1_000_000, p.SpoolMaxGB)
	case freeBytes < uint64(sourceBytes)+spoolReserveBytes:
		return 0, fmt.Sprintf("the spool disk has %d MB free, %d MB needed with the 5 GB reserve", freeBytes/1_000_000, (uint64(sourceBytes)+spoolReserveBytes)/1_000_000)
	}
	// Everything but the reserve: the archive may outgrow its estimate a
	// little (a log written mid-run), and never into the last 5 GB.
	return int64(freeBytes - spoolReserveBytes), ""
}

// spoolReserveMu makes reading the shared budget and holding a run's share of
// it one step, so two runs started together cannot both see the space free.
var spoolReserveMu sync.Mutex

// spoolSlackBytes is what a spool may write past its source size: tar headers,
// gzip framing, a log growing mid-run.
var spoolSlackBytes int64 = 256 << 20

// reserveSpool is spoolBudget against the node's shared write budget: free
// less what other writes (restores, copies, other spools) hold. A spool that is
// granted holds its source size plus slack in inflightWrites until release, so
// the next run, restore or copy sees it as taken. release is idempotent; landed is
// what the spool file holds on the disk at that moment, booked as landed so a
// budget measured before those bytes were there still counts them.
func reserveSpool(p *backupPolicy, sourceBytes int64, freeBytes uint64) (int64, func(landed int64), string) {
	spoolReserveMu.Lock()
	defer spoolReserveMu.Unlock()
	if held := inflightWrites.Load(); held > 0 {
		freeBytes -= min(freeBytes, uint64(held))
	}
	limit, why := spoolBudget(p, sourceBytes, freeBytes)
	if limit <= 0 {
		return 0, func(int64) {}, why
	}
	// The file is bound by what it holds, not by the free space: the size is
	// du's, which does not follow a link the archive takes as its target, and
	// every byte past the reservation was invisible to the writes beside it.
	held := min(limit, sourceBytes+spoolSlackBytes)
	inflightWrites.Add(held)
	var once sync.Once
	return held, func(landed int64) {
		once.Do(func() {
			inflightWrites.Add(-held)
			landedWrites.Add(max(landed, 0))
		})
	}, ""
}

var errSpoolFull = errors.New("the archive outgrew the space reserved for the spool")

// boundedWriter refuses to write past max bytes.
type boundedWriter struct {
	w   io.Writer
	max int64
	n   int64
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if b.n+int64(len(p)) > b.max {
		return 0, errSpoolFull
	}
	n, err := b.w.Write(p)
	b.n += int64(n)
	return n, err
}

// spoolArchive writes the archive to path, through counter, and returns the
// file rewound for the upload. The file is returned whenever it was created,
// so the caller removes it on every path.
func spoolArchive(path string, limit int64, counter io.Writer, write func(io.Writer) (bool, error)) (*os.File, bool, error) {
	// The name is this run's alone (run ids are Core-wide, a run goes to one
	// node, backupsInFlight holds it here), so a file under it is what this
	// run left when the node died. The boot sweep skips storage another node
	// shares, and the redelivery failed on it for good.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, err
	}
	added, err := write(io.MultiWriter(&boundedWriter{w: f, max: limit}, counter))
	if err != nil {
		return f, added, err
	}
	if err := f.Sync(); err != nil {
		return f, added, fmt.Errorf("flush the spool file: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return f, added, err
	}
	return f, added, nil
}
