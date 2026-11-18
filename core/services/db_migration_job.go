package services

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"dylaris-core/config"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// DBMigrationPhase tracks where a migration job is in its lifecycle.
type DBMigrationPhase string

const (
	PhasePreparing DBMigrationPhase = "preparing"
	PhaseSchema    DBMigrationPhase = "schema"
	PhaseCopying   DBMigrationPhase = "copying"
	PhaseVerifying DBMigrationPhase = "verifying"
	PhaseDone      DBMigrationPhase = "done"
	PhaseFailed    DBMigrationPhase = "failed"
)

const (
	dbMigrationJobKey  = "dylaris:dbmigration:job"
	dbMigrationLockKey = "dylaris:dbmigration:lock"
	dbMigrationLockTTL = 2 * time.Minute
	dbHeartbeat        = 20 * time.Second
	dbStaleAfter       = 75 * time.Second // > heartbeat, so a live job never looks stale
	dbMaxLogLines      = 500
	dbJobTTL           = 7 * 24 * time.Hour
)

// DBTargetInfo is the non-secret description of the migration target shown to
// admins. It deliberately omits the password.
type DBTargetInfo struct {
	Host    string `json:"host"`
	Port    string `json:"port"`
	User    string `json:"user"`
	DBName  string `json:"dbName"`
	SSLMode string `json:"sslMode"`
	DBType  string `json:"dbType"`
}

// DBMigrationJob is the shared, Redis-backed record every admin can poll.
type DBMigrationJob struct {
	ID            string            `json:"id"`
	Phase         DBMigrationPhase  `json:"phase"`
	StartedBy     string            `json:"startedBy"`     // user UUID
	StartedByName string            `json:"startedByName"` // username for display
	StartedAt     time.Time         `json:"startedAt"`
	UpdatedAt     time.Time         `json:"updatedAt"`
	FinishedAt    *time.Time        `json:"finishedAt,omitempty"`
	Target        DBTargetInfo      `json:"target"`
	TablesTotal   int               `json:"tablesTotal"`
	TablesDone    int               `json:"tablesDone"`
	CurrentTable  string            `json:"currentTable"`
	Tables        []TableCopyResult `json:"tables"`
	Log           []string          `json:"log"`
	Error         string            `json:"error,omitempty"`
	Verify        *VerifyReport     `json:"verify,omitempty"`
	MaintenanceOn bool              `json:"maintenanceOn"`
	Stale         bool              `json:"stale"` // computed on read, not persisted authoritatively
}

func (j *DBMigrationJob) appendLog(line string) {
	j.Log = append(j.Log, line)
	if len(j.Log) > dbMaxLogLines {
		j.Log = j.Log[len(j.Log)-dbMaxLogLines:]
	}
}

// snapshot copies the job so it can leave the service's mutex.
//
// Start used to hand its caller the SAME pointer it stored in s.job, which the
// runner goroutine then kept writing under the mutex the caller does not hold.
// Log and Tables are cloned because they are the two the runner keeps appending
// to; the rest is copied by value. FinishedAt and Verify are shared pointers on
// purpose: the runner replaces those pointers, it never writes through them, so
// the pointee a snapshot holds is immutable.
func (j *DBMigrationJob) snapshot() *DBMigrationJob {
	cp := *j
	cp.Log = append([]string(nil), j.Log...)
	cp.Tables = append([]TableCopyResult(nil), j.Tables...)
	return &cp
}

// DBMigrationService orchestrates a single, cluster-wide-exclusive migration
// job. The job state lives in Redis so any Core can serve the status to admins;
// a SetNX lock guarantees only one runs at a time. The SOURCE is the live Core's
// database, opened read-only; the live connection is never switched.
type DBMigrationService struct {
	redis  *redis.Client
	store  store.Store
	source DBConnParams

	mu  sync.Mutex
	job *DBMigrationJob // in-memory copy for the job running on THIS Core
}

// NewDBMigrationService wires the service. source describes the live database so
// the migration can re-open it read-only as the copy source.
func NewDBMigrationService(rdb *redis.Client, st store.Store, source DBConnParams) *DBMigrationService {
	return &DBMigrationService{redis: rdb, store: st, source: source}
}

// ErrMigrationRunning is returned when a job is already active cluster-wide.
var ErrMigrationRunning = errors.New("a database migration is already running")

// Start validates the target, enables maintenance, and launches the copy in a
// background goroutine on this Core. Returns the initial job record.
func (s *DBMigrationService) Start(ctx context.Context, target DBConnParams, startedBy, startedByName string) (*DBMigrationJob, error) {
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	// Refuse to migrate a database onto itself.
	if sameEndpoint(s.source, target) {
		return nil, errors.New("target host/port/dbname is identical to the source database")
	}

	// Fail fast if the target is unreachable.
	probe, err := target.Open(ctx, 8*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot reach target: %w", err)
	}
	probe.Close()

	// Single active job cluster-wide.
	jobID := newJobID()
	ok, err := s.redis.SetNX(ctx, dbMigrationLockKey, jobID, dbMigrationLockTTL).Result()
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	if !ok {
		return nil, ErrMigrationRunning
	}

	// Refuse to start if the platform is not actually blocked. Copying a live
	// database while users keep writing to it is the failure this job exists to
	// avoid, and the lock has to go back or nobody can retry until it expires.
	prevBlockLevel, err := s.enableMaintenance(startedBy)
	if err != nil {
		releaseJobLock(context.Background(), s.redis, dbMigrationLockKey, jobID)
		return nil, fmt.Errorf("enable maintenance mode: %w", err)
	}

	now := time.Now()
	job := &DBMigrationJob{
		ID:            jobID,
		Phase:         PhasePreparing,
		StartedBy:     startedBy,
		StartedByName: startedByName,
		StartedAt:     now,
		UpdatedAt:     now,
		Target: DBTargetInfo{
			Host: target.Host, Port: target.Port, User: target.User,
			DBName: target.DBName, SSLMode: target.SSLMode,
			DBType: config.NormalizeDBType(target.DBType),
		},
		MaintenanceOn: true,
	}
	job.appendLog(fmt.Sprintf("Migration started by %s. Maintenance mode enabled (block_all).", displayName(startedByName, startedBy)))
	job.appendLog(fmt.Sprintf("Target: %s:%s/%s (%s)", target.Host, target.Port, target.DBName, job.Target.DBType))

	s.mu.Lock()
	s.job = job
	s.persistLocked(job)
	out := job.snapshot()
	s.mu.Unlock()

	go s.run(target, startedBy, prevBlockLevel)
	return out, nil
}

// GetJob reads the shared job record from Redis (any Core can answer). The
// boolean is false when no job has ever run.
func (s *DBMigrationService) GetJob(ctx context.Context) (*DBMigrationJob, bool, error) {
	raw, err := s.redis.Get(ctx, dbMigrationJobKey).Result()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var job DBMigrationJob
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		return nil, false, err
	}
	// Compute staleness: an in-progress job whose heartbeat stopped (e.g. the
	// Core running it died) is flagged so admins aren't misled.
	if isInProgress(job.Phase) && time.Since(job.UpdatedAt) > dbStaleAfter {
		job.Stale = true
	}
	return &job, true, nil
}

func (s *DBMigrationService) run(target DBConnParams, actorID, prevBlockLevel string) {
	ctx := context.Background()
	jobID := s.jobID()
	defer func() {
		if rec := recover(); rec != nil {
			s.fail(fmt.Sprintf("panic during migration: %v", rec), actorID, prevBlockLevel)
		}
		releaseJobLock(context.Background(), s.redis, dbMigrationLockKey, jobID)
	}()

	stopHeartbeat := s.startHeartbeat()
	defer stopHeartbeat()

	src, err := s.source.Open(ctx, 10*time.Second)
	if err != nil {
		s.fail("open source database: "+err.Error(), actorID, prevBlockLevel)
		return
	}
	defer src.Close()
	tgt, err := target.Open(ctx, 10*time.Second)
	if err != nil {
		s.fail("open target database: "+err.Error(), actorID, prevBlockLevel)
		return
	}
	defer tgt.Close()

	// Total table count for progress.
	if ordered, derr := discoverTablesOrdered(ctx, src); derr == nil {
		s.update(func(j *DBMigrationJob) { j.TablesTotal = len(ordered) })
	}

	s.update(func(j *DBMigrationJob) {
		j.Phase = PhaseSchema
		j.appendLog("Provisioning schema on the target database...")
	})
	if err := EnsureTargetSchema(tgt, config.UsesTimescale(target.DBType)); err != nil {
		s.fail("provision target schema: "+err.Error(), actorID, prevBlockLevel)
		return
	}

	s.update(func(j *DBMigrationJob) {
		j.Phase = PhaseCopying
		j.appendLog("Copying tables (parents before children)...")
	})
	_, err = CopyAllTables(ctx, src, tgt, func(table string, rows int64) {
		s.update(func(j *DBMigrationJob) {
			j.TablesDone++
			j.CurrentTable = table
			j.Tables = append(j.Tables, TableCopyResult{Table: table, Rows: rows})
			j.appendLog(fmt.Sprintf("copied %s: %d rows", table, rows))
		})
	})
	if err != nil {
		s.fail("copy tables: "+err.Error(), actorID, prevBlockLevel)
		return
	}

	s.update(func(j *DBMigrationJob) {
		j.Phase = PhaseVerifying
		j.appendLog("Auto-verifying: comparing table presence and row counts...")
	})
	report, err := VerifyCopy(ctx, src, tgt, time.Now())
	if err != nil {
		s.fail("verify copy: "+err.Error(), actorID, prevBlockLevel)
		return
	}

	finished := time.Now()
	s.update(func(j *DBMigrationJob) {
		j.Verify = &report
		j.Phase = PhaseDone
		j.FinishedAt = &finished
		for _, line := range report.Log {
			j.appendLog("verify: " + line)
		}
		if report.OK {
			j.appendLog("DONE. Verification passed. Next: restart Core with the new DB_* (+ DB_TYPE) env vars, then disable maintenance.")
		} else {
			j.appendLog("WARNING: copy finished but verification FAILED. Do NOT switch yet. Maintenance left ON; investigate and re-run.")
		}
	})
	log.Printf("db-migration: job %s finished (verify ok=%v)", s.jobID(), report.OK)
}

// fail records a terminal failure, re-enables normal operation (the source was
// never modified, so it is safe to lift maintenance), and persists the job.
func (s *DBMigrationService) fail(msg, actorID, prevBlockLevel string) {
	log.Printf("db-migration: FAILED: %s", msg)
	s.disableMaintenance(actorID, prevBlockLevel)
	finished := time.Now()
	s.update(func(j *DBMigrationJob) {
		j.Phase = PhaseFailed
		j.Error = msg
		j.FinishedAt = &finished
		j.MaintenanceOn = false
		j.appendLog("FAILED: " + msg)
		j.appendLog("The source database was NOT modified. Maintenance disabled; the platform is back online.")
	})
}

// startHeartbeat keeps the lock alive and refreshes the job's UpdatedAt so a
// long-running single-table copy is not mistaken for a dead job. Returns a stop
// function.
func (s *DBMigrationService) startHeartbeat() func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(dbHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if id := s.jobID(); id != "" {
					held, err := refreshOrReclaimJobLock(context.Background(), s.redis, dbMigrationLockKey, id, dbMigrationLockTTL)
					if err == nil && !held {
						log.Printf("db migration %s: the migration lock is held by another job - a second migration is running against the same target", id)
					}
				}
				s.update(func(j *DBMigrationJob) {}) // touch UpdatedAt + re-persist
			}
		}
	}()
	return func() { close(done) }
}

// update mutates the in-memory job under lock, bumps UpdatedAt, and persists.
func (s *DBMigrationService) update(mut func(*DBMigrationJob)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return
	}
	mut(s.job)
	s.job.UpdatedAt = time.Now()
	s.persistLocked(s.job)
}

// persistLocked writes the job JSON to Redis. Caller holds s.mu.
func (s *DBMigrationService) persistLocked(job *DBMigrationJob) {
	b, err := json.Marshal(job)
	if err != nil {
		return
	}
	s.redis.Set(context.Background(), dbMigrationJobKey, b, dbJobTTL)
}

func (s *DBMigrationService) jobID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return ""
	}
	return s.job.ID
}

// enableMaintenance puts the platform into block_all and reports whether it
// actually got there.
//
// The result is not optional. Every one of these writes used to be discarded,
// so a failed write still produced a job recorded as MaintenanceOn:true, a job
// log line stating "Maintenance mode enabled (block_all)" as fact, and a
// migration copying the database WHILE users kept writing to it. The caller now
// refuses to start on an error - a migration without maintenance is worse than
// no migration.
//
// A missing maintenance.block_level row is the normal first-run state, not a
// failure: GetSetting reports it as sql.ErrNoRows, and it simply means there is
// no previous level to restore afterwards.
func (s *DBMigrationService) enableMaintenance(actorID string) (prevBlockLevel string, err error) {
	prevBlockLevel, err = s.store.GetSetting("maintenance.block_level")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read current block level: %w", err)
	}

	if err := s.store.SetSettingBy("maintenance.block_level", "block_all", actorID); err != nil {
		return "", fmt.Errorf("set block level: %w", err)
	}
	if err := s.store.SetSettingBy("maintenance.active", "true", actorID); err != nil {
		// Undo the level we just raised, or the platform is left at block_all
		// with maintenance inactive - a state nobody asked for and nothing else
		// clears.
		if prevBlockLevel != "" {
			if rerr := s.store.SetSettingBy("maintenance.block_level", prevBlockLevel, actorID); rerr != nil {
				log.Printf("db migration: could not restore block level %q after a failed activation: %v", prevBlockLevel, rerr)
			}
		}
		return "", fmt.Errorf("activate maintenance: %w", err)
	}
	return prevBlockLevel, nil
}

// disableMaintenance lifts the block after the migration finishes.
//
// This one cannot abort anything - the migration is already over - but it must
// not fail silently either. A lost "false" write leaves the whole platform in
// block_all indefinitely, which is a self-inflicted outage that an operator can
// only clear by hand, and previously there was nothing in the log to point at.
func (s *DBMigrationService) disableMaintenance(actorID, prevBlockLevel string) {
	if err := s.store.SetSettingBy("maintenance.active", "false", actorID); err != nil {
		log.Printf("db migration: FAILED to lift maintenance mode - the platform stays in block_all until this is cleared manually: %v", err)
	}
	if prevBlockLevel != "" && prevBlockLevel != "block_all" {
		if err := s.store.SetSettingBy("maintenance.block_level", prevBlockLevel, actorID); err != nil {
			log.Printf("db migration: could not restore the previous block level %q: %v", prevBlockLevel, err)
		}
	}
}

// VerifyTarget opens the configured target connection and compares it to the
// live source, read-only. Used by the manual "Verify" button. (Wave 3 wires the
// endpoint; the comparison itself lives in VerifyCopy.)
func (s *DBMigrationService) VerifyTarget(ctx context.Context, target DBConnParams) (VerifyReport, error) {
	if err := validateTarget(target); err != nil {
		return VerifyReport{}, err
	}
	src, err := s.source.Open(ctx, 10*time.Second)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	tgt, err := target.Open(ctx, 10*time.Second)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("open target: %w", err)
	}
	defer tgt.Close()
	return VerifyCopy(ctx, src, tgt, time.Now())
}

// --- helpers ---

func validateTarget(t DBConnParams) error {
	missing := []string{}
	if strings.TrimSpace(t.Host) == "" {
		missing = append(missing, "host")
	}
	if strings.TrimSpace(t.Port) == "" {
		missing = append(missing, "port")
	}
	if strings.TrimSpace(t.User) == "" {
		missing = append(missing, "user")
	}
	if strings.TrimSpace(t.DBName) == "" {
		missing = append(missing, "dbName")
	}
	if len(missing) > 0 {
		return fmt.Errorf("target connection missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

func sameEndpoint(a, b DBConnParams) bool {
	return strings.EqualFold(a.Host, b.Host) && a.Port == b.Port && a.DBName == b.DBName
}

func isInProgress(p DBMigrationPhase) bool {
	switch p {
	case PhasePreparing, PhaseSchema, PhaseCopying, PhaseVerifying:
		return true
	}
	return false
}

func newJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func displayName(name, id string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return id
}
