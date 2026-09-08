package database

import (
	"context"
	"database/sql"
	"dylaris-core/config"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq" // Required: Postgres Driver
)

func InitDB(cfg config.Config) (*sql.DB, error) {
	// Postgres Connection String (DSN)
	sslMode := cfg.DBSSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName, sslMode)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("DB Open Error: %w", err)
	}

	// Wait for Postgres to accept connections instead of crashing out —
	// covers the Core booting before the DB in a compose / stack deploy.
	if err := pingWithRetry(db, 60, 2*time.Second); err != nil {
		return nil, fmt.Errorf("DB Ping Error: %w", err)
	}
	log.Println("Postgres DB connection established.")

	useTimescale := config.UsesTimescale(cfg.DBType)
	log.Printf("DB_TYPE=%s (server_stats: %s)", cfg.DBType, map[bool]string{true: "TimescaleDB hypertable + native retention", false: "plain table + hourly retention sweep"}[useTimescale])
	if err := ensureSchemaExclusive(db, useTimescale); err != nil {
		return nil, err
	}

	return db, nil
}

// schemaLockKey is the advisory-lock id every Core replica serialises schema
// creation on. Arbitrary but fixed: only Core uses it.
const schemaLockKey int64 = 0x44594C41 // "DYLA"

// ensureSchemaExclusive runs ensureSchema while holding a Postgres advisory
// lock, so replicas booting at the same time apply the schema one after another.
//
// The individual statements are idempotent, but idempotent is not the same as
// concurrency-safe: two sessions running CREATE TABLE IF NOT EXISTS for the same
// table at the same time can still fail on pg_type's unique index, because the
// existence check and the insert are not atomic against each other. That turns a
// `deploy: mode: global` Core into a boot crash-loop on first deploy, which then
// "fixes itself" on retry - the worst kind of bug to diagnose.
//
// The lock is session-scoped and released automatically if the connection dies,
// so a Core that is killed mid-migration cannot wedge the others permanently.
func ensureSchemaExclusive(db *sql.DB, useTimescale bool) error {
	ctx := context.Background()
	// Lock and unlock must run on the SAME session, so pin one connection
	// instead of letting the pool hand out a different one.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("schema lock: acquire connection: %w", err)
	}
	defer conn.Close()

	// Bounded wait: a healthy peer finishes in seconds, so a multi-minute block
	// means something is wrong and a loud failure beats hanging the boot.
	if _, err := conn.ExecContext(ctx, `SET lock_timeout = '180s'`); err != nil {
		return fmt.Errorf("schema lock: set lock_timeout: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaLockKey); err != nil {
		return fmt.Errorf("schema lock: acquire (another Core may be migrating): %w", err)
	}
	defer func() {
		if _, uerr := conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, schemaLockKey); uerr != nil {
			log.Printf("schema lock: release failed (freed on disconnect anyway): %v", uerr)
		}
	}()

	return ensureSchema(db, useTimescale)
}

// pingWithRetry blocks until the database answers or the attempt budget
// is exhausted.
func pingWithRetry(db *sql.DB, attempts int, delay time.Duration) error {
	var err error
	for i := 1; i <= attempts; i++ {
		if err = db.Ping(); err == nil {
			return nil
		}
		log.Printf("DB not ready (%d/%d): %v", i, attempts, err)
		time.Sleep(delay)
	}
	return err
}

// EnsureSchema provisions the full schema on an arbitrary database. It is the
// exported entry point used by the in-panel DB migration to create every table
// on a fresh TARGET database before the generic row copy. useTimescale controls
// whether server_stats is created as a TimescaleDB hypertable (target backend)
// or a plain table. It is idempotent, like ensureSchema.
func EnsureSchema(db *sql.DB, useTimescale bool) error {
	return ensureSchema(db, useTimescale)
}

// ensureSchema creates every table, applies column migrations and seeds
// the baseline rows. Every statement is idempotent (CREATE/ALTER ... IF
// NOT EXISTS, conditional inserts), so it is safe to run repeatedly.
func ensureSchema(db *sql.DB, useTimescale bool) error {
	if err := createUsersTable(db); err != nil {
		return err
	}
	if err := createModulesTable(db); err != nil {
		return err
	}
	if err := createNodesTable(db); err != nil {
		return err
	}
	if err := createServersTable(db); err != nil {
		return err
	}
	if err := createSettingsTable(db); err != nil {
		return err
	}
	if err := createServerInvitesTable(db); err != nil {
		return err
	}
	if err := createServerStatsTable(db, useTimescale); err != nil {
		return err
	}
	if err := createGatewayBandwidthStatsTable(db, useTimescale); err != nil {
		return err
	}
	if err := createGatewayTables(db); err != nil {
		return err
	}
	if err := createCustomDomainTables(db); err != nil {
		return err
	}
	if err := createLibraryDisabledTable(db); err != nil {
		return err
	}
	if err := createBackupTables(db); err != nil {
		return err
	}
	if err := createPlatformBackupTables(db); err != nil {
		return err
	}
	if err := createTicketTables(db); err != nil {
		return err
	}
	if err := createMailTemplateTables(db); err != nil {
		return err
	}
	if err := migrateSchema(db); err != nil {
		return err
	}
	if err := applyPhase0a1Schema(db); err != nil {
		return err
	}
	if err := applyPhase8Schema(db); err != nil {
		return err
	}
	if err := applyPhase9Schema(db); err != nil {
		return err
	}
	if err := applyPhase10Schema(db); err != nil {
		return err
	}
	if err := applySubServerInstallSchema(db); err != nil {
		return err
	}
	if err := applyModpackCrosscheckSchema(db); err != nil {
		return err
	}
	if err := applyPhase13Schema(db); err != nil {
		return err
	}
	if err := applyWS5TabProxySchema(db); err != nil {
		return err
	}
	if err := applyPhase14Schema(db); err != nil {
		return err
	}
	if err := applyPhase11Schema(db); err != nil {
		return err
	}
	if err := applyPhase15Schema(db); err != nil {
		return err
	}
	if err := applyPhase16Schema(db); err != nil {
		return err
	}
	if err := applyPhase17Schema(db); err != nil {
		return err
	}
	if err := dropTelemetrySettings(db); err != nil {
		return err
	}
	if err := applyAuthzFoundationSchema(db); err != nil {
		return err
	}
	if err := seedDefaultPanelRoles(db); err != nil {
		return err
	}
	// After the tables exist and beside the other seeds: with none of these the
	// support inbox refuses every ticket rather than showing an empty list.
	if err := seedDefaultTicketCategories(db); err != nil {
		return err
	}
	if err := backfillPanelRoleAssignments(db); err != nil {
		return err
	}
	if err := applyAuthzGrantsSchema(db); err != nil {
		return err
	}
	if err := migrateLegacyServerInvites(db); err != nil {
		return err
	}
	if err := applyWarpSchema(db); err != nil {
		return err
	}
	if err := applyBYONSchema(db); err != nil {
		return err
	}
	if err := applyTrafficSchema(db); err != nil {
		return err
	}
	if err := applyBillingSchema(db); err != nil {
		return err
	}
	if err := applyPlansSchema(db); err != nil {
		return err
	}
	if err := applyUnifiedModpackSchema(db); err != nil {
		return err
	}
	if err := applyBeamChannelSchema(db); err != nil {
		return err
	}
	if err := applyUpdatesSeenSchema(db); err != nil {
		return err
	}
	if err := applyStorageManifestsSchema(db); err != nil {
		return err
	}
	if err := applyStorageConnectionsSchema(db); err != nil {
		return err
	}
	if err := applyInviteAttributionNullable(db); err != nil {
		return err
	}
	if err := applyTicketCategorySnapshot(db); err != nil {
		return err
	}
	if err := applyModpackAuthoringSchema(db); err != nil {
		return err
	}
	if err := applyEntitlementSchema(db); err != nil {
		return err
	}
	if err := applyEntitlementSplitSchema(db); err != nil {
		return err
	}
	if err := applyRconLogFilterSchema(db); err != nil {
		return err
	}
	if err := applyTrafficBillingSchema(db); err != nil {
		return err
	}
	if err := applyTabProxyHostSchema(db); err != nil {
		return err
	}
	if err := applyTabSubServerSchema(db); err != nil {
		return err
	}

	if err := applyRegistryMoveSchema(db); err != nil {
		return err
	}

	if err := applyBackupAllowanceSettingMove(db); err != nil {
		return err
	}

	if err := applySolderTenancySchema(db); err != nil {
		return err
	}

	seedSystemModules(db)

	// After the seed, so every row it owns is guaranteed to exist.
	if err := applyDerivedModuleRows(db); err != nil {
		return err
	}
	return nil
}
