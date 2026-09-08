package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// Resolution is the bucket size recorded at.
//
// One value, because there is one place the statistics can go: a database of
// their own, which is a TimescaleDB in every deployment this is built for.
// Recording into the Core database at hour resolution used to be the
// alternative; it is gone, and with it the only reason this was ever a pair.
const ResolutionDedicated = time.Minute

// FlushInterval is how often accumulated buckets are written.
//
// Well inside both resolutions on purpose: a bucket is written repeatedly while
// it fills, so a Core that restarts mid-bucket loses at most this much rather
// than the whole bucket. It stays one row either way, because the write merges.
const FlushInterval = 5 * time.Minute

// Handle is a configured recorder plus whatever has to be closed with it.
type Handle struct {
	Recorder *Recorder
	// Dedicated is the connection this handle owns and closes.
	Dedicated *sql.DB
	// Resolution is the bucket size actually in use.
	Resolution time.Duration
	// Read is the pool to QUERY through. The same pool as Dedicated now that
	// there is only one backend, and kept as its own field because readers ask
	// for it by that name and a reader should not have to know that.
	Read *sql.DB
}

func (h *Handle) Close() error {
	if h == nil || h.Dedicated == nil {
		return nil
	}
	return h.Dedicated.Close()
}

// Open prepares the recorder against the statistics database.
//
// An empty URL is a caller error rather than a mode: "nothing configured" is
// answered by the manager, which does not open anything at all, so anything
// that reaches here is expected to name a database.
//
// An unreachable database is NOT fatal to the platform. It is reported, the
// manager keeps retrying, and Core starts without long-term metrics - because a
// statistics store must never be a reason Core does not come up.
func Open(ctx context.Context, metricsURL string) (*Handle, error) {
	if metricsURL == "" {
		return nil, fmt.Errorf("no statistics database is configured")
	}

	db, err := sql.Open("postgres", metricsURL)
	if err != nil {
		return nil, fmt.Errorf("open metrics database: %w", err)
	}
	// Small on purpose. This pool serves one writer flushing every few minutes;
	// sizing it like a request-serving pool would hold connections a metrics
	// database has no use for.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)

	ping, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("reach metrics database: %w", err)
	}
	// The statistics database is a Timescale one in every deployment we build
	// for; if the extension turns out to be missing, EnsureSchema logs it and
	// the plain table carries on.
	if err := EnsureSchema(ctx, db, true); err != nil {
		_ = db.Close()
		return nil, err
	}
	log.Printf("metrics: recording into the statistics database at %s resolution", ResolutionDedicated)
	return &Handle{
		Recorder:   NewRecorder(NewSQLStore(db), ResolutionDedicated),
		Dedicated:  db,
		Resolution: ResolutionDedicated,
		Read:       db,
	}, nil
}
