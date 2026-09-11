package database

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dylaris-core/config"
	"dylaris-core/models"
	"dylaris-core/store"
)

// Integration tests against a REAL Postgres. Every other test in this repo uses
// fakes, which is why five defects this session were invisible to the suite:
// they were all SQL that only Postgres rejects.
//
//	ticket status / user role   inconsistent types deduced for parameter $1
//	ticket_messages.user_id     NOT NULL with ON DELETE SET NULL -> 23502
//	backup_jobs.*_patterns      nil slice into a NOT NULL text[] -> 23502
//	spark_profiles.requested_by UUID scanned into sql.NullInt64
//
// Skipped unless DYLARIS_TEST_DB_HOST is set, so `go test ./...` stays green on
// a machine with no database. CI provides a postgres service container.
//
// Run locally:
//
//	docker run -d --name dyl-pgtest -e POSTGRES_PASSWORD=testpw \
//	  -e POSTGRES_USER=dylaris -e POSTGRES_DB=dylaris_test -p 55432:5432 postgres:16-alpine
//	DYLARIS_TEST_DB_HOST=127.0.0.1 DYLARIS_TEST_DB_PORT=55432 \
//	  DYLARIS_TEST_DB_USER=dylaris DYLARIS_TEST_DB_PASSWORD=testpw \
//	  DYLARIS_TEST_DB_NAME=dylaris_test go test ./database/ -run Integration -v
var uniqueSuffix atomic.Int64

// runTag makes every generated name unique per test PROCESS, not just per
// call. The tables outlive the run (the CI service container is fresh, a local
// container is not), and a counter restarting at 1 collides with rows a
// previous run left behind on a UNIQUE column.
var runTag = strconv.FormatInt(time.Now().UnixNano(), 36)

// uniqueName returns a name no other test or run will produce.
func uniqueName(prefix string) string {
	return prefix + runTag + "_" + strconv.FormatInt(uniqueSuffix.Add(1), 10)
}

// testDBConfig builds the connection config from the environment, skipping the
// calling test when no database is configured.
func testDBConfig(t *testing.T) config.Config {
	t.Helper()
	host := os.Getenv("DYLARIS_TEST_DB_HOST")
	if host == "" {
		t.Skip("DYLARIS_TEST_DB_HOST not set - skipping the Postgres integration tests")
	}
	env := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}
	return config.Config{
		DBHost:     host,
		DBPort:     env("DYLARIS_TEST_DB_PORT", "5432"),
		DBUser:     env("DYLARIS_TEST_DB_USER", "postgres"),
		DBPassword: os.Getenv("DYLARIS_TEST_DB_PASSWORD"),
		DBName:     env("DYLARIS_TEST_DB_NAME", "postgres"),
		DBSSLMode:  "disable",
		// Plain Postgres, not TimescaleDB: server_stats is then created as an
		// ordinary table, so no extension is needed in the test image.
		DBType: "postgres",
	}
}

// freshSchemaDB creates a scratch database, builds the schema on it EXACTLY
// once and hands back the handle. Dropped again on cleanup.
//
// One boot is what a fresh install is, and it is not the same thing as the
// shared test database: migrateSchema's ADD COLUMN set runs before the later
// phases create their tables, so a column whose table does not exist yet is
// missing after the first boot and present after the second. A test that wants
// to see that has to start from nothing.
func freshSchemaDB(t *testing.T) *sql.DB {
	t.Helper()
	cfg := testDBConfig(t)

	admin, err := sql.Open("postgres", fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName))
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()

	name := "fresh_" + runTag
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create scratch database %s: %v", name, err)
	}

	cfg.DBName = name
	db, err := InitDB(cfg)
	if err != nil {
		t.Fatalf("InitDB on the scratch database: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		dropper, err := sql.Open("postgres", fmt.Sprintf(
			"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
			cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, testDBName()))
		if err != nil {
			return
		}
		defer dropper.Close()
		if _, err := dropper.Exec("DROP DATABASE IF EXISTS " + name); err != nil {
			t.Logf("could not drop the scratch database %s: %v", name, err)
		}
	})
	return db
}

func testDBName() string {
	if v := os.Getenv("DYLARIS_TEST_DB_NAME"); v != "" {
		return v
	}
	return "postgres"
}

func integrationDB(t *testing.T) (*sql.DB, *store.PostgresStore) {
	t.Helper()
	cfg := testDBConfig(t)
	db, err := InitDB(cfg)
	if err != nil {
		t.Fatalf("InitDB against the test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, store.NewPostgresStore(db)
}

// fixture creates the rows the tests hang off: a user, a node and a server.
// Names carry a per-call suffix so a repeated local run does not collide with
// the UNIQUE constraints, and everything is removed again on cleanup.
type fixture struct {
	user   *models.User
	node   *models.Node
	server *models.Server
}

func newFixture(t *testing.T, st *store.PostgresStore) *fixture {
	t.Helper()
	tag := strings.ToLower(strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	if len(tag) > 20 {
		tag = tag[:20]
	}
	suffix := func(p string) string { return uniqueName(p + tag + "_") }

	u := &models.User{Username: suffix("u_"), Password: "x", Email: suffix("e_") + "@example.test"}
	if err := st.CreateUser(u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	n := &models.Node{Name: suffix("n_"), Address: "127.0.0.1", Token: suffix("t_"), Status: "online"}
	if err := st.CreateNode(n); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	srv := &models.Server{
		UUID: suffix("uuid_"), Name: suffix("s_"), NodeID: n.ID, OwnerID: u.ID,
		GameImage: "img", Port: 25600, Memory: 1024, Status: "stopped", ServerType: "game",
	}
	sid, err := st.CreateServer(srv)
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	srv.ID = int(sid)

	t.Cleanup(func() {
		st.DeleteServer(srv.ID)
		st.DeleteNode(n.ID)
		st.DeleteUser(u.ID)
	})
	return &fixture{user: u, node: n, server: srv}
}

// A create request that omits includePatterns/excludePatterns is legal and
// answered 500: a nil slice reaches the driver as NULL, and naming the column
// in the INSERT means its DEFAULT '{}' never applies.
func TestIntegrationBackupJobWithoutPatterns(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	job := &models.BackupJob{ServerID: f.server.ID, Name: "no patterns", Schedule: "manual", RetentionCount: 3, Enabled: true}
	id, err := st.CreateBackupJob(job)
	if err != nil {
		t.Fatalf("CreateBackupJob with nil patterns: %v", err)
	}
	t.Cleanup(func() { st.DeleteBackupJob(id) })

	got, err := st.GetBackupJob(id)
	if err != nil {
		t.Fatalf("GetBackupJob: %v", err)
	}
	if got.IncludePatterns == nil || len(got.IncludePatterns) != 0 {
		t.Errorf("IncludePatterns = %#v, want an empty list", got.IncludePatterns)
	}

	job.ID = id
	job.Name = "still no patterns"
	if err := st.UpdateBackupJob(job); err != nil {
		t.Errorf("UpdateBackupJob with nil patterns: %v", err)
	}
}

// Deleting a user who ever wrote a ticket message hit a not-null violation:
// ticket_messages.user_id was NOT NULL with an ON DELETE SET NULL reference.
//
// The ticket is owned by someone ELSE, which is the case that matters: a
// support agent who replied on a foreign ticket. Deleting the ticket's own
// owner cascades the whole thread away (tickets.user_id is ON DELETE CASCADE),
// so that path never exercises the SET NULL at all.
func TestIntegrationDeleteUserWhoWroteATicketMessage(t *testing.T) {
	_, st := integrationDB(t)
	owner := newFixture(t, st) // owns a server, so it is never the deleted user

	cat := &models.TicketCategory{Name: uniqueName("cat_"), Enabled: true, DefaultPriority: "normal"}
	catID, err := st.CreateTicketCategory(cat)
	if err != nil {
		t.Fatalf("CreateTicketCategory: %v", err)
	}
	t.Cleanup(func() { st.DeleteTicketCategory(catID) })

	// The author owns no server: DeleteUser deliberately refuses to remove a
	// user who still owns one, so the fixture user cannot be the one deleted.
	// The replying agent owns nothing, so DeleteUser's "still owns servers"
	// guard does not fire.
	author := &models.User{
		Username: uniqueName("author_"),
		Password: "x",
		Email:    uniqueName("author_") + "@example.test",
	}
	if err := st.CreateUser(author); err != nil {
		t.Fatalf("CreateUser(author): %v", err)
	}

	tk := &models.Ticket{CategoryID: catID, UserID: owner.user.ID, Title: "help", Status: "open", Priority: "normal"}
	ticketID, err := st.CreateTicket(tk)
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}

	if _, err := st.AddTicketMessage(&models.TicketMessage{TicketID: ticketID, UserID: author.ID, Body: "my message"}); err != nil {
		t.Fatalf("AddTicketMessage: %v", err)
	}

	if err := st.DeleteUser(author.ID); err != nil {
		t.Fatalf("DeleteUser after the user wrote a ticket message: %v", err)
	}

	msgs, err := st.ListTicketMessages(ticketID, true)
	if err != nil {
		t.Fatalf("ListTicketMessages after the author was deleted: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want the anonymised one to survive", len(msgs))
	}
	if msgs[0].Body != "my message" {
		t.Errorf("body = %q, want it unchanged", msgs[0].Body)
	}
	if msgs[0].UserID != "" {
		t.Errorf("userId = %q, want it cleared by the ON DELETE SET NULL", msgs[0].UserID)
	}
}

// Closing a ticket and changing a user's role both failed with "inconsistent
// types deduced for parameter $1" - a defect no fake can reproduce, and one
// that meant neither had ever worked.
func TestIntegrationStatementsPostgresHadRejected(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	cat := &models.TicketCategory{Name: uniqueName("cat_"), Enabled: true, DefaultPriority: "normal"}
	catID, err := st.CreateTicketCategory(cat)
	if err != nil {
		t.Fatalf("CreateTicketCategory: %v", err)
	}
	t.Cleanup(func() { st.DeleteTicketCategory(catID) })

	ticketID, err := st.CreateTicket(&models.Ticket{
		CategoryID: catID, UserID: f.user.ID, Title: "transitions", Status: "open", Priority: "normal",
	})
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}

	for _, status := range []string{"in_progress", "waiting", "resolved", "closed", "open"} {
		if err := st.UpdateTicketStatus(ticketID, status); err != nil {
			t.Errorf("UpdateTicketStatus(%q): %v", status, err)
		}
	}

	for _, role := range []string{"support", "admin", "user"} {
		if err := st.SetUserRole(f.user.ID, role); err != nil {
			t.Errorf("SetUserRole(%q): %v", role, err)
		}
	}
}

// spark_profiles.requested_by is a UUID column. Scanning it into an integer
// target failed for every row, and the scan error was swallowed, so the
// endpoint answered 200 with an empty list.
func TestIntegrationSparkProfileRoundTrip(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)

	if _, err := db.Exec(
		`INSERT INTO spark_profiles (server_id, sub_server_name, url, requested_by) VALUES ($1, $2, $3, $4)`,
		f.server.ID, "survival", "https://spark.example/x", f.user.ID,
	); err != nil {
		t.Fatalf("insert spark profile: %v", err)
	}

	rows, err := db.Query(
		`SELECT id, server_id, sub_server_name, url, requested_by FROM spark_profiles WHERE server_id = $1`,
		f.server.ID)
	if err != nil {
		t.Fatalf("query spark profiles: %v", err)
	}
	defer rows.Close()

	found := 0
	for rows.Next() {
		var id, serverID int
		var subServer, url string
		var requestedBy sql.NullString
		if err := rows.Scan(&id, &serverID, &subServer, &url, &requestedBy); err != nil {
			t.Fatalf("scan: %v - requested_by is a UUID, not an integer", err)
		}
		if !requestedBy.Valid || requestedBy.String != f.user.ID {
			t.Errorf("requested_by = %v, want %q", requestedBy, f.user.ID)
		}
		found++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if found != 1 {
		t.Errorf("got %d profiles, want 1", found)
	}
}

func TestIntegrationGatewayBandwidthStatsTable(t *testing.T) {
	db := freshSchemaDB(t) // full schema on a scratch DB; skips without a test DB
	if _, err := db.Exec(`INSERT INTO gateway_bandwidth_stats (time, component, id, host, region, rx_bps, tx_bps, cap_mbit)
		VALUES (NOW(), 'warp', 'eu-1', 'web-eu-1', 'eu-central', 100, 200, 1000)`); err != nil {
		t.Fatalf("insert into gateway_bandwidth_stats: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM gateway_bandwidth_stats WHERE id='eu-1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("row count = %d, want 1", n)
	}
}

// A BYON node is a tenant's own machine, paired through a single-use enroll
// token. NodeCleanupService sweeps offline server-less nodes every 5 minutes
// with a 24h cutoff, and that sweep used to include owned nodes - so a customer
// who registered a node and then did not create a server for a day (a laptop, a
// home box off over a weekend) lost the pairing. Not just the row: the node's
// scoped Redis ACL users are pruned with it, and on the next boot the node still
// holds its cached .node_secret, so the "paired node with no cached secret"
// guard in redisacl_bootstrap never fires. It reconnects under an identity Core
// has forgotten and loops on a rejected handshake, with no BYON route back in.
//
// Against a real Postgres rather than sqlmock, because the whole change is one
// SQL predicate and NULL semantics are exactly what a query-text assertion
// cannot check: nodes.owner_id is nullable, and the sweep's own
// "id NOT IN (SELECT node_id FROM servers)" sits one nullable column away from
// matching nothing at all.
//
// The same holds for an operator node whose row an admin created. It holds no
// CLUSTER_SECRET, so once swept its cached secret proof names an identity Core
// has forgotten and it cannot pair again without hands on the machine. Only a
// row the cluster proof minted may go.
//
// Each node differs from the swept one in exactly ONE of the two filters, so
// either filter going missing fails its own assertion. The BYON node carries
// the cluster-proof marker it could never get in production for that reason:
// without it, the marker alone would spare it and the owner filter would be
// untested.
func TestIntegrationStaleNodeSweepSparesNodesThatCannotRePair(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st) // supplies the user that owns the BYON node

	stale := time.Now().Add(-48 * time.Hour)

	mkNode := func(prefix string, owner *string, enrolledVia string) *models.Node {
		t.Helper()
		n := &models.Node{Name: uniqueName(prefix), Address: "127.0.0.1", Token: uniqueName(prefix + "t_"), Status: "offline"}
		if err := st.CreateNode(n); err != nil {
			t.Fatalf("CreateNode(%s): %v", prefix, err)
		}
		t.Cleanup(func() { st.DeleteNode(n.ID) })
		if owner != nil {
			if err := st.SetNodeOwner(n.ID, owner); err != nil {
				t.Fatalf("SetNodeOwner(%s): %v", prefix, err)
			}
		}
		if enrolledVia != "" {
			if err := st.SetNodeEnrolledVia(n.ID, enrolledVia); err != nil {
				t.Fatalf("SetNodeEnrolledVia(%s): %v", prefix, err)
			}
		}
		if _, err := db.Exec(`UPDATE nodes SET status = 'offline', last_seen_at = $1 WHERE id = $2`, stale, n.ID); err != nil {
			t.Fatalf("age node %s: %v", prefix, err)
		}
		return n
	}

	byon := mkNode("byon_", &f.user.ID, store.NodeEnrolledViaClusterProof)
	adminCreated := mkNode("admin_", nil, "")
	clusterMinted := mkNode("plat_", nil, store.NodeEnrolledViaClusterProof)

	if _, err := st.DeleteStaleOfflineNodes(time.Now().Add(-24 * time.Hour)); err != nil {
		t.Fatalf("DeleteStaleOfflineNodes: %v", err)
	}

	if got, err := st.GetNodeByID(byon.ID); err != nil || got == nil {
		t.Errorf("the BYON node was swept: a tenant's pairing must survive being offline (err=%v)", err)
	}
	if got, err := st.GetNodeByID(adminCreated.ID); err != nil || got == nil {
		t.Errorf("an admin-created node was swept: it cannot re-pair by itself (err=%v)", err)
	}
	// A cluster-minted node that went away is exactly the churn the sweep
	// exists for, so sparing everything would be the opposite mistake.
	if got, _ := st.GetNodeByID(clusterMinted.ID); got != nil {
		t.Errorf("the cluster-proof node survived the sweep: the cleanup no longer cleans anything up")
	}
}

// The roll-key action arms the admission an approval arms, for a node that has
// not been refused yet. It must be the same door: one-shot, address-bound, and
// never armed for an empty address. Against a real Postgres because the upsert,
// the interval arithmetic and the consume's single UPDATE are the mechanism.
func TestIntegrationRollSecretArmsTheSameOneShotAdmission(t *testing.T) {
	_, st := integrationDB(t)

	fresh := uniqueName("roll_t_")
	t.Cleanup(func() { st.DeleteNodeJoinAttempt(fresh) })

	if ok, err := st.ArmNodeJoinApproval(fresh, "", "admin-1"); err != nil || ok {
		t.Fatalf("armed for an empty address (ok=%v err=%v): that admits the identity from anywhere", ok, err)
	}
	if ok, err := st.ArmNodeJoinApproval(fresh, "203.0.113.7", "admin-1"); err != nil || !ok {
		t.Fatalf("ArmNodeJoinApproval: ok=%v err=%v", ok, err)
	}
	if ok, err := st.ConsumeNodeJoinApproval(fresh, "198.51.100.1"); err != nil || ok {
		t.Errorf("the admission admitted another address (ok=%v err=%v)", ok, err)
	}
	if ok, err := st.ConsumeNodeJoinApproval(fresh, "203.0.113.7"); err != nil || !ok {
		t.Errorf("the admission did not admit its own address (ok=%v err=%v)", ok, err)
	}
	if ok, err := st.ConsumeNodeJoinApproval(fresh, "203.0.113.7"); err != nil || ok {
		t.Errorf("the admission was consumed twice (ok=%v err=%v)", ok, err)
	}

	// A node that was already being refused keeps what was recorded about the
	// refusals; only the admission is written.
	refused := uniqueName("roll_r_")
	t.Cleanup(func() { st.DeleteNodeJoinAttempt(refused) })
	if err := st.RecordNodeJoinAttempt(models.NodeJoinAttempt{NodeToken: refused, PeerIP: "198.51.100.9", Reason: "refused"}); err != nil {
		t.Fatalf("RecordNodeJoinAttempt: %v", err)
	}
	if ok, err := st.ArmNodeJoinApproval(refused, "203.0.113.7", "admin-1"); err != nil || !ok {
		t.Fatalf("ArmNodeJoinApproval over an existing row: ok=%v err=%v", ok, err)
	}
	attempts, err := st.ListNodeJoinAttempts()
	if err != nil {
		t.Fatalf("ListNodeJoinAttempts: %v", err)
	}
	var row *models.NodeJoinAttempt
	for i := range attempts {
		if attempts[i].NodeToken == refused {
			row = &attempts[i]
		}
	}
	if row == nil {
		t.Fatal("the refused row is gone after arming")
	}
	if row.PeerIP != "198.51.100.9" || row.Attempts != 1 || row.Reason != "refused" {
		t.Errorf("arming rewrote the refusal record: %+v", row)
	}
	if row.ApprovedFromIP != "203.0.113.7" || row.ApprovedBy != "admin-1" || row.ApprovedUntil == nil {
		t.Errorf("the admission was not armed as asked: %+v", row)
	}
	// The same window an approval gets, not an open-ended one.
	if d := time.Until(*row.ApprovedUntil); d <= 0 || d > 16*time.Minute {
		t.Errorf("armed for %v, want about fifteen minutes", d)
	}
}

// The address the roll-key admission binds to, empty until the node authenticates.
func TestIntegrationNodeLastAuthPeerIPRoundTrip(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	if ip, err := st.GetNodeLastAuthPeerIP(f.node.ID); err != nil || ip != "" {
		t.Fatalf("a node that never authenticated reads (%q, %v), want ''", ip, err)
	}
	if err := st.SetNodeLastAuthPeerIP(f.node.ID, "203.0.113.7"); err != nil {
		t.Fatalf("SetNodeLastAuthPeerIP: %v", err)
	}
	if ip, err := st.GetNodeLastAuthPeerIP(f.node.ID); err != nil || ip != "203.0.113.7" {
		t.Errorf("read back (%q, %v), want 203.0.113.7", ip, err)
	}
}

// The learned link token rides the shared node scan, so every read path that
// feeds a route (GetNodeByID for CreateServerRoute, ListNodes for the learner)
// sees it. Empty on a new row: the Hub has not answered, routes derive.
func TestIntegrationNodeLinkTokenRoundTrip(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	if n, err := st.GetNodeByID(f.node.ID); err != nil || n.LinkToken != "" {
		t.Fatalf("a new node reads link_token (%+v, %v), want ''", n, err)
	}
	if err := st.SetNodeLinkToken(f.node.ID, "generated-token"); err != nil {
		t.Fatalf("SetNodeLinkToken: %v", err)
	}
	if n, err := st.GetNodeByID(f.node.ID); err != nil || n.LinkToken != "generated-token" {
		t.Errorf("GetNodeByID read back (%v, %v), want generated-token", n, err)
	}
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	for _, n := range nodes {
		if n.ID == f.node.ID && n.LinkToken != "generated-token" {
			t.Errorf("ListNodes read link_token %q, want generated-token", n.LinkToken)
		}
	}
}

// /auth/forgot-password sends mail on an anonymous request and REPLACES the
// reset token every time. Its sibling /auth/resend-verification has enforced a
// per-mailbox cooldown for exactly that reason ("a per-IP limit bounds one
// CALLER, not one MAILBOX"); this endpoint had only the shared per-IP limiter,
// so a caller rotating source addresses could both flood an inbox and keep
// invalidating the link the victim was trying to click.
//
// The window is enforced inside the UPDATE's WHERE, against
// password_reset_expires_at - a reset token's expiry IS its send time plus the
// policy TTL, so no new column was needed. Against a real Postgres because
// that predicate, its NULL branch and RowsAffected are the entire mechanism.
func TestIntegrationPasswordResetTokenCooldown(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	const ttl = 30 * time.Minute
	const cooldown = 60 * time.Second

	issue := func(at time.Time, token string) bool {
		t.Helper()
		expires := at.Add(ttl)
		ok, err := st.SetPasswordResetToken(f.user.ID, token, expires, expires.Add(-cooldown))
		if err != nil {
			t.Fatalf("SetPasswordResetToken: %v", err)
		}
		return ok
	}

	now := time.Now()

	// No token on the row yet: the NULL branch must let the first one through,
	// or password reset would never work at all.
	if !issue(now, "first-token") {
		t.Fatal("the first request was refused; the NULL branch does not let a first token through")
	}
	if issue(now.Add(5*time.Second), "flood-token") {
		t.Error("a second request 5s later was allowed: the mailbox cooldown does not hold")
	}
	if issue(now.Add(30*time.Second), "flood-token-2") {
		t.Error("a request inside the cooldown was allowed")
	}
	if !issue(now.Add(90*time.Second), "second-token") {
		t.Error("a request after the cooldown was refused; a user who never got the first mail could not ask again")
	}

	// A completed reset clears both columns, and the next request must not be
	// held back by the token that was just consumed.
	if err := st.ClearPasswordResetToken(f.user.ID); err != nil {
		t.Fatalf("ClearPasswordResetToken: %v", err)
	}
	if !issue(now.Add(91*time.Second), "after-consume") {
		t.Error("a request right after a completed reset was refused")
	}

	// The token that survives must be the last one actually issued.
	u, err := st.GetUserByPasswordResetToken("after-consume")
	if err != nil || u == nil || u.ID != f.user.ID {
		t.Errorf("the issued token does not resolve back to its user (err=%v)", err)
	}
	if u, _ := st.GetUserByPasswordResetToken("flood-token"); u != nil {
		t.Error("a token from a refused request was stored anyway")
	}
}

// CountPendingNodeEnrollTokens backs the node cap on POST /nodes/enroll-token.
// Against a real Postgres because the whole method is one predicate over three
// nullable columns, and because this file exists for exactly the class of defect
// that only Postgres rejects.
//
// "Pending" means redeemable: not consumed, not expired, and not a recovery
// token. Recovery tokens are excluded deliberately - they re-pair a machine that
// already exists and is therefore already counted, so counting them too would
// refuse a re-pair to a tenant who is legitimately at their limit.
func TestIntegrationCountPendingNodeEnrollTokens(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)

	if n, err := st.CountPendingNodeEnrollTokens(f.user.ID); err != nil || n != 0 {
		t.Fatalf("fresh user: got (%d, %v), want (0, nil)", n, err)
	}

	future := time.Now().Add(24 * time.Hour)
	past := time.Now().Add(-1 * time.Hour)

	if err := st.CreateNodeEnrollToken(f.user.ID, "live-1", "one", &future, ""); err != nil {
		t.Fatalf("CreateNodeEnrollToken: %v", err)
	}
	if err := st.CreateNodeEnrollToken(f.user.ID, "live-2", "no expiry", nil, ""); err != nil {
		t.Fatalf("CreateNodeEnrollToken (no expiry): %v", err)
	}
	if err := st.CreateNodeEnrollToken(f.user.ID, "expired", "stale", &past, ""); err != nil {
		t.Fatalf("CreateNodeEnrollToken (expired): %v", err)
	}
	if err := st.CreateRecoveryToken(f.user.ID, "recovery-1", f.node.Token, &future); err != nil {
		t.Fatalf("CreateRecoveryToken: %v", err)
	}

	// A NULL expiry must count: it never lapses, so it is the most pending of
	// the lot, and a NOT-NULL-only predicate would silently skip it.
	if n, err := st.CountPendingNodeEnrollTokens(f.user.ID); err != nil || n != 2 {
		t.Errorf("got (%d, %v), want (2, nil) - one dated, one open-ended; the expired and recovery tokens must not count", n, err)
	}

	// Redeeming one drops it out of the count, which is what frees the slot.
	if _, _, ok, err := st.ConsumeNodeEnrollToken("live-1"); err != nil || !ok {
		t.Fatalf("ConsumeNodeEnrollToken: ok=%v err=%v", ok, err)
	}
	if n, err := st.CountPendingNodeEnrollTokens(f.user.ID); err != nil || n != 1 {
		t.Errorf("after redeeming one: got (%d, %v), want (1, nil)", n, err)
	}

	// Another tenant's tokens are not this tenant's problem.
	var other string
	if err := db.QueryRow(`SELECT id FROM users WHERE id <> $1 LIMIT 1`, f.user.ID).Scan(&other); err == nil && other != "" {
		if n, err := st.CountPendingNodeEnrollTokens(other); err != nil {
			t.Errorf("counting another tenant: %v", err)
		} else if n != 0 {
			t.Errorf("another tenant sees %d of this one's tokens", n)
		}
	}
}

// GetSFTPAccessByNode feeds the SFTP sync, and what that publishes IS the
// access decision - the node has no second gate. Two things have to be true of
// it and neither was: it must reach account-wide grants (a row with server_id
// NULL, which the old join on si.server_id = s.id skipped entirely, so someone
// granted a whole account had every panel route and no SFTP), and it must mark
// which rows are owners so the caller knows which ones still need resolving.
func TestIntegrationSFTPAccessCoversAccountGrants(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	friend := &models.User{Username: uniqueName("f_sftp_"), Password: "x", Email: uniqueName("fe_") + "@example.test"}
	if err := st.CreateUser(friend); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { st.DeleteUser(friend.ID) })

	rowsFor := func(username string) []store.SFTPAccess {
		t.Helper()
		all, err := st.GetSFTPAccessByNode(f.node.ID)
		if err != nil {
			t.Fatalf("GetSFTPAccessByNode: %v", err)
		}
		var out []store.SFTPAccess
		for _, a := range all {
			if a.Username == username {
				out = append(out, a)
			}
		}
		return out
	}

	t.Run("the owner is present and marked as one", func(t *testing.T) {
		rows := rowsFor(f.user.Username)
		if len(rows) != 1 {
			t.Fatalf("got %d rows for the owner, want 1", len(rows))
		}
		if !rows[0].IsOwner {
			t.Error("the owner's row is not marked IsOwner, so the caller would resolve it needlessly")
		}
		if rows[0].ServerID != f.server.ID || rows[0].UserID != f.user.ID {
			t.Errorf("row = %+v, want serverID %d and userID %s", rows[0], f.server.ID, f.user.ID)
		}
	})

	t.Run("a friend with no grant is absent", func(t *testing.T) {
		if rows := rowsFor(friend.Username); len(rows) != 0 {
			t.Errorf("got %d rows for an unrelated account, want 0", len(rows))
		}
	})

	t.Run("an ACCOUNT-WIDE grant reaches the server", func(t *testing.T) {
		if err := st.UpsertServerGrant(nil, friend.ID, f.user.ID, nil, store.CapOverrides{Grant: []string{"sftp.access"}}, false); err != nil {
			t.Fatalf("UpsertServerGrant (account-wide): %v", err)
		}
		rows := rowsFor(friend.Username)
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1: an account-wide grant used to produce none, so the holder had no SFTP at all", len(rows))
		}
		if rows[0].IsOwner {
			t.Error("a grantee is marked as the owner, which would skip the capability check entirely")
		}
		if rows[0].ServerID != f.server.ID {
			t.Errorf("serverID = %d, want %d", rows[0].ServerID, f.server.ID)
		}
	})

	t.Run("a per-server grant reaches it too, exactly once", func(t *testing.T) {
		sid := f.server.ID
		if err := st.UpsertServerGrant(&sid, friend.ID, f.user.ID, nil, store.CapOverrides{Grant: []string{"sftp.access"}}, false); err != nil {
			t.Fatalf("UpsertServerGrant (per-server): %v", err)
		}
		rows := rowsFor(friend.Username)
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1: holding both grant shapes must not publish the server twice", len(rows))
		}
	})
}

// TestIntegrationShareExpiryWrites covers the two statements that gave
// server_tabs.share_expires_at a writer at last. The column had been readable,
// gated on and rendered since it was added, and no code path ever set it - so
// a share link could not expire and the 410 branch behind it was unreachable.
//
// Both statements are here because both are things only Postgres can answer:
// whether an untyped nil lands as NULL through the $n::timestamptz cast (the
// same parameter-typing class as the four defects in this file's header), and
// whether the rotate statement's CASE compares against now() the way the
// handler assumes. The SQL mirrors handlers.Update / handlers.RotateShareLink;
// if those change, this is the test that has to be looked at with them.
func TestIntegrationShareExpiryWrites(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)

	var tabID int
	if err := db.QueryRow(`INSERT INTO server_tabs
		(server_id, name, icon, url, position, enabled, open_in_panel,
		 mode, target_port, target_path, surface, visibility, share_token)
		VALUES ($1,'expiry','layout-grid','',0,true,true,'proxied',8100,'/','page','private',$2)
		RETURNING id`, f.server.ID, uniqueName("sh_")).Scan(&tabID); err != nil {
		t.Fatalf("insert tab: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM server_tabs WHERE id=$1`, tabID) })

	// The patch statement, reduced to the one column under test - the rest is
	// COALESCE and cannot express "clear", which is the whole reason this
	// column needs the set-flag.
	patch := func(t *testing.T, set bool, value interface{}) {
		t.Helper()
		if _, err := db.Exec(`UPDATE server_tabs SET
			share_expires_at = CASE WHEN $2::bool THEN $3::timestamptz ELSE share_expires_at END
			WHERE id=$1`, tabID, set, value); err != nil {
			t.Fatalf("patch(set=%v, value=%v): %v", set, value, err)
		}
	}
	read := func(t *testing.T) sql.NullTime {
		t.Helper()
		var got sql.NullTime
		if err := db.QueryRow(`SELECT share_expires_at FROM server_tabs WHERE id=$1`, tabID).Scan(&got); err != nil {
			t.Fatalf("read: %v", err)
		}
		return got
	}

	want := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	t.Run("set stores the instant", func(t *testing.T) {
		patch(t, true, want)
		got := read(t)
		if !got.Valid {
			t.Fatal("share_expires_at is NULL after a set")
		}
		if !got.Time.UTC().Truncate(time.Second).Equal(want) {
			t.Errorf("share_expires_at = %s, want %s", got.Time.UTC(), want)
		}
	})

	t.Run("an untouched field keeps it", func(t *testing.T) {
		patch(t, false, nil)
		if got := read(t); !got.Valid {
			t.Error("a PATCH that never mentioned the expiry cleared it")
		}
	})

	t.Run("an explicit clear nulls it", func(t *testing.T) {
		patch(t, true, nil)
		if got := read(t); got.Valid {
			t.Errorf("share_expires_at = %s after a clear, want NULL", got.Time)
		}
	})

	// The rotate statement: a new slug must not arrive already dead, but a
	// deadline the owner set for the future is their live choice.
	rotate := func(t *testing.T) {
		t.Helper()
		if _, err := db.Exec(`UPDATE server_tabs
			SET share_token=$2,
			    share_expires_at = CASE WHEN share_expires_at <= now() THEN NULL ELSE share_expires_at END
			WHERE id=$1`, tabID, uniqueName("sh_")); err != nil {
			t.Fatalf("rotate: %v", err)
		}
	}

	t.Run("rotate drops an expiry that already passed", func(t *testing.T) {
		patch(t, true, time.Now().Add(-time.Hour).UTC())
		rotate(t)
		if got := read(t); got.Valid {
			t.Errorf("share_expires_at = %s, want NULL - rotating handed back a link that is already expired", got.Time)
		}
	})

	t.Run("rotate keeps a future expiry", func(t *testing.T) {
		patch(t, true, want)
		rotate(t)
		got := read(t)
		if !got.Valid {
			t.Fatal("rotating cleared a future expiry the owner had set")
		}
		if !got.Time.UTC().Truncate(time.Second).Equal(want) {
			t.Errorf("share_expires_at = %s, want %s", got.Time.UTC(), want)
		}
	})
}

// TestIntegrationPasswordChangeSpendsResetLink covers the pairing that makes a
// defensive password change actually defensive.
//
// A user who receives a reset mail they did not request does the right thing:
// they sign in and change the password themselves. That link stayed valid for
// the rest of its hour. Measured on a live stack before this: the owner set a
// new password, the outstanding link then set a different one, and only the
// link-holder's password worked afterwards - the defensive action handed the
// account over.
//
// It lives here because the guard IS the SQL: UpdateUserPassword nulls the
// columns in the same UPDATE, and UpdateUser does it only when the password
// column actually changes, through a CASE over the PRE-update row that no fake
// can answer for.
func TestIntegrationPasswordChangeSpendsResetLink(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)

	hasToken := func(t *testing.T) bool {
		t.Helper()
		var token sql.NullString
		var expires sql.NullTime
		if err := db.QueryRow(`SELECT password_reset_token, password_reset_expires_at FROM users WHERE id=$1`,
			f.user.ID).Scan(&token, &expires); err != nil {
			t.Fatalf("read reset columns: %v", err)
		}
		// Both columns move together; a token with no expiry would never
		// expire, which is the shape worth catching if one is ever missed.
		if token.Valid && token.String != "" && !expires.Valid {
			t.Error("a reset token is stored with no expiry - it would never age out")
		}
		return token.Valid && token.String != ""
	}
	issue := func(t *testing.T, tok string) {
		t.Helper()
		expires := time.Now().Add(time.Hour)
		if _, err := st.SetPasswordResetToken(f.user.ID, tok, expires, expires.Add(-time.Hour)); err != nil {
			t.Fatalf("SetPasswordResetToken: %v", err)
		}
		if !hasToken(t) {
			t.Fatal("the token was not stored, so the rest of this test proves nothing")
		}
	}

	t.Run("UpdateUserPassword spends the link", func(t *testing.T) {
		issue(t, uniqueName("tok_"))
		if err := st.UpdateUserPassword(f.user.ID, "$2a$10$newhashnewhashnewhashnewhashnewhashnewhashnewhashnewhas"); err != nil {
			t.Fatalf("UpdateUserPassword: %v", err)
		}
		if hasToken(t) {
			t.Error("the reset link survived a password change - it can still take the account over")
		}
	})

	t.Run("a profile save that CHANGES the password spends it", func(t *testing.T) {
		issue(t, uniqueName("tok_"))
		u, err := st.GetUserByID(f.user.ID)
		if err != nil || u == nil {
			t.Fatalf("GetUserByID: %v", err)
		}
		u.Password = "$2a$10$profilechangedprofilechangedprofilechangedprofilechang"
		if err := st.UpdateUser(u); err != nil {
			t.Fatalf("UpdateUser: %v", err)
		}
		if hasToken(t) {
			t.Error("the reset link survived a self-service password change")
		}
	})

	t.Run("a profile save that does NOT touch the password keeps it", func(t *testing.T) {
		// The profile save rewrites the password column on every call (the
		// caller round-trips the row), so an unconditional clear would drop a
		// valid link because someone edited their email address.
		issue(t, uniqueName("tok_"))
		u, err := st.GetUserByID(f.user.ID)
		if err != nil || u == nil {
			t.Fatalf("GetUserByID: %v", err)
		}
		u.MinecraftUsername = "Notch"
		if err := st.UpdateUser(u); err != nil {
			t.Fatalf("UpdateUser: %v", err)
		}
		if !hasToken(t) {
			t.Error("editing an unrelated profile field invalidated the reset link the user is waiting on")
		}
	})
}

// TestIntegrationUsernameCaseReservation covers the guarantee that only the
// database can give: two names differing solely in case cannot both exist.
//
// Before this, `NewComer` registered happily beside an existing `newcomer` -
// two real accounts, verified live. Not an access-control break, since every
// lookup is exact and a typo therefore fails closed; the harm is that the two
// are indistinguishable at a glance in a member list, an audit log's actor
// column or a ticket thread.
//
// It belongs here rather than in a handler test because the guard IS the unique
// index on LOWER(username) plus a case-folding query, and no fake can answer for
// either.
func TestIntegrationUsernameCaseReservation(t *testing.T) {
	_, st := integrationDB(t)

	base := uniqueName("case_")
	first := &models.User{Username: base, Password: "x", Email: uniqueName("ce_") + "@example.test"}
	if err := st.CreateUser(first); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { st.DeleteUser(first.ID) })

	upper := strings.ToUpper(base)

	t.Run("UsernameTaken folds case", func(t *testing.T) {
		for _, name := range []string{base, upper, strings.Title(base)} {
			taken, err := st.UsernameTaken(name, "")
			if err != nil {
				t.Fatalf("UsernameTaken(%q): %v", name, err)
			}
			if !taken {
				t.Errorf("UsernameTaken(%q) = false; %q exists and differs only by case", name, base)
			}
		}
	})

	t.Run("a free name is free", func(t *testing.T) {
		taken, err := st.UsernameTaken(uniqueName("free_"), "")
		if err != nil {
			t.Fatalf("UsernameTaken: %v", err)
		}
		if taken {
			t.Error("a name nobody holds came back taken")
		}
	})

	t.Run("the holder is excluded, so a rename to your own casing works", func(t *testing.T) {
		taken, err := st.UsernameTaken(upper, first.ID)
		if err != nil {
			t.Fatalf("UsernameTaken: %v", err)
		}
		if taken {
			t.Error("the account's own row blocked it; changing your own capitalisation would be impossible")
		}
	})

	t.Run("the database refuses the collision outright", func(t *testing.T) {
		// The pre-check above is best-effort UX. This is the guarantee: a
		// concurrent claim between check and INSERT still cannot land.
		second := &models.User{Username: upper, Password: "x", Email: uniqueName("ce2_") + "@example.test"}
		err := st.CreateUser(second)
		if err == nil {
			st.DeleteUser(second.ID)
			t.Fatal("a username differing only by case was created; the unique index on LOWER(username) is missing")
		}
	})

	t.Run("collisions are listed rather than fatal", func(t *testing.T) {
		// What the migration asks before creating the index. With the index in
		// place there is nothing to report, which is the point.
		got, err := st.ListUsernameCaseCollisions()
		if err != nil {
			t.Fatalf("ListUsernameCaseCollisions: %v", err)
		}
		for _, set := range got {
			t.Errorf("a collision survived the index: %v", set)
		}
	})
}

// A BYON machine must count as ONE machine against the node cap.
//
// It needs two secrets, and the panel that mints them says so: a warp key to
// reach the overlay and an enroll token to become a node. Neither is retired
// when the machine connects - warp re-enrols with that key for the life of the
// box - so a running machine is a nodes row AND a live warp key, and the cap
// was the sum of the two.
//
// Every consumer of that sum was wrong by the same factor: the mint gate
// refused the second machine on a two-node plan, the panel's "used" showed 2
// of 2 for one box, and the over-limit sweep read a tenant who was exactly
// within their plan as over it - which stops everything they own 72 hours
// later, by email, for being compliant.
//
// Against a real database rather than a mocked row, because the defect is in
// what the query MEANS. A test asserting the SQL string would have passed
// against the broken version, which is what "UNREDEEMED" in its comment did.
func TestIntegrationBYONMachineCountsOnceAgainstTheNodeCap(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	if err := st.SetNodeOwner(f.node.ID, &f.user.ID); err != nil {
		t.Fatalf("SetNodeOwner: %v", err)
	}

	mintKey := func(name string) int {
		t.Helper()
		id, err := st.CreateWarpAPIKey(store.WarpAPIKey{
			Name: uniqueName(name), KeyHash: uniqueName("h_"), Policy: "general",
			MaxConns: 1, OnNewConn: "kill_old", NodeID: uniqueName("node-"),
			OwnerID: f.user.ID,
		})
		if err != nil {
			t.Fatalf("CreateWarpAPIKey: %v", err)
		}
		return id
	}
	count := func() int {
		t.Helper()
		nodes, err := st.CountNodesByOwner(f.user.ID)
		if err != nil {
			t.Fatalf("CountNodesByOwner: %v", err)
		}
		keys, err := st.CountNodeWarpKeysByOwner(f.user.ID)
		if err != nil {
			t.Fatalf("CountNodeWarpKeysByOwner: %v", err)
		}
		return nodes + keys
	}

	// The key is minted and the machine has not connected yet. It still has to
	// count, or a one-node plan mints keys without limit.
	keyID := mintKey("k_")
	if got := count(); got != 2 {
		// One node from the fixture (now owned) plus one unredeemed key.
		t.Fatalf("a minted, unconnected key plus one node counted as %d, want 2", got)
	}

	// The machine joins the overlay: the key is now redeemed. Together with the
	// nodes row it is ONE machine, not two.
	if _, err := st.InsertWarpPeer(store.WarpPeer{
		APIKeyID: keyID, Pubkey: uniqueName("pk_"), WGIP: uniqueName("10.44.0."), Region: "leader-01",
	}); err != nil {
		t.Fatalf("InsertWarpPeer: %v", err)
	}
	if got := count(); got != 1 {
		t.Fatalf("one connected BYON machine counted as %d, want 1 - the sweep cuts off a tenant who is within their plan", got)
	}

	// A second machine mid-setup is counted again, which is the behaviour the
	// key half exists for.
	mintKey("k2_")
	if got := count(); got != 2 {
		t.Fatalf("one connected machine plus one being set up counted as %d, want 2", got)
	}
}

// TestIntegrationTabPatchWritesEveryParameter runs handlers.Update's real
// UPDATE, whole, against Postgres.
//
// TestEverySQLStatementPrepares already proves the statement parses and that
// every column exists, but PREPARE never sees the Go argument list - so a
// seventeen-parameter statement whose args drifted out of order, or lost one,
// passes it and fails on first use. This executes it with the arguments the
// handler passes, in the handler's order.
//
// The sub-server column is the reason it is worth the space. Like
// share_expires_at above it needs three states, because "" is a LEGITIMATE
// value ("every sub-server") and COALESCE(NULLIF(...)) cannot tell "keep" from
// "clear". Getting that wrong does not error: it silently pins a tab to a world
// nobody chose, or refuses to unpin one, and the tab then hides itself whenever
// another sub-server is running.
func TestIntegrationTabPatchWritesEveryParameter(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)

	var tabID int
	if err := db.QueryRow(`INSERT INTO server_tabs
		(server_id, name, icon, url, position, enabled, open_in_panel,
		 mode, target_port, target_path, surface, visibility, share_token,
		 proxy_host_label, sub_server_name)
		VALUES ($1,'patch','layout-grid','',0,true,true,'proxied',8100,'/','tab','private',$2,$3,'survival')
		RETURNING id`, f.server.ID, uniqueName("pt_"), uniqueName("lbl")).Scan(&tabID); err != nil {
		t.Fatalf("insert tab: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM server_tabs WHERE id=$1`, tabID) })

	// Byte-for-byte the statement in handlers.Update, with its argument list in
	// the handler's order. If that one changes, this has to change with it.
	patch := func(t *testing.T, setSub bool, sub string) {
		t.Helper()
		_, err := db.Exec(`UPDATE server_tabs SET
			name           = COALESCE(NULLIF($3, ''), name),
			icon           = COALESCE(NULLIF($4, ''), icon),
			url            = COALESCE(NULLIF($5, ''), url),
			position       = COALESCE($6, position),
			enabled        = COALESCE($7, enabled),
			open_in_panel  = COALESCE($8, open_in_panel),
			mode           = COALESCE(NULLIF($9, ''),  mode),
			target_port    = COALESCE($10, target_port),
			target_path    = COALESCE(NULLIF($11, ''), target_path),
			surface        = COALESCE(NULLIF($12, ''), surface),
			visibility     = COALESCE(NULLIF($13, ''), visibility),
			share_expires_at = CASE WHEN $14::bool THEN $15::timestamptz ELSE share_expires_at END,
			sub_server_name = CASE WHEN $16::bool THEN $17::text ELSE sub_server_name END
			WHERE id=$1 AND server_id=$2`,
			tabID, f.server.ID,
			"", "", "", nil, nil, nil, "", nil, "", "", "",
			false, nil,
			setSub, sub)
		if err != nil {
			t.Fatalf("patch(setSub=%v, sub=%q): %v", setSub, sub, err)
		}
	}
	read := func(t *testing.T) string {
		t.Helper()
		var got string
		if err := db.QueryRow(`SELECT sub_server_name FROM server_tabs WHERE id=$1`, tabID).Scan(&got); err != nil {
			t.Fatalf("read: %v", err)
		}
		return got
	}

	t.Run("absent keeps the stored pin", func(t *testing.T) {
		patch(t, false, "")
		if got := read(t); got != "survival" {
			t.Errorf("sub_server_name = %q, want survival - an unrelated edit moved the pin", got)
		}
	})
	t.Run("a name pins the tab", func(t *testing.T) {
		patch(t, true, "creative")
		if got := read(t); got != "creative" {
			t.Errorf("sub_server_name = %q, want creative", got)
		}
	})
	t.Run("empty unpins it", func(t *testing.T) {
		patch(t, true, "")
		if got := read(t); got != "" {
			t.Errorf("sub_server_name = %q, want empty - the tab cannot be unpinned", got)
		}
	})
	t.Run("and absent still keeps that", func(t *testing.T) {
		patch(t, false, "ignored")
		if got := read(t); got != "" {
			t.Errorf("sub_server_name = %q, want empty - a later edit re-pinned it", got)
		}
	})
}
