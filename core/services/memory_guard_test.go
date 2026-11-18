package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type memGuardFakeStore struct {
	srv        models.Server
	desired    []string
	statuses   []string
	crash      string
	crashAt    time.Time
	notes      []models.Notification
	audits     []models.ServerAuditEvent
	auditOn    bool
	nodeToken  string
	getErr     error
	crashErr   error
	nodeErr    error
	desiredErr error
}

func (f *memGuardFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	s := f.srv
	return &s, nil
}
func (f *memGuardFakeStore) GetNodeByID(int) (*models.Node, error) {
	if f.nodeErr != nil {
		return nil, f.nodeErr
	}
	return &models.Node{Token: f.nodeToken}, nil
}
func (f *memGuardFakeStore) UpdateServerStatus(_ int, s string) error {
	f.statuses = append(f.statuses, s)
	return nil
}
func (f *memGuardFakeStore) UpdateServerDesiredState(_ int, s string) error {
	if f.desiredErr != nil {
		return f.desiredErr
	}
	f.desired = append(f.desired, s)
	return nil
}
func (f *memGuardFakeStore) SetServerLastCrash(_ int, reason string, at time.Time) error {
	if f.crashErr != nil {
		return f.crashErr
	}
	f.crash, f.crashAt = reason, at
	return nil
}
func (f *memGuardFakeStore) InsertNotification(n *models.Notification) (int64, error) {
	f.notes = append(f.notes, *n)
	return 1, nil
}
func (f *memGuardFakeStore) GetServerAuditState(int) (bool, bool, int, error) {
	return f.auditOn, false, 0, nil
}
func (f *memGuardFakeStore) InsertServerAudit(ev *models.ServerAuditEvent) error {
	f.audits = append(f.audits, *ev)
	return nil
}

type memGuardFakeQueue struct {
	actions []string
	sendErr error
}

func (q *memGuardFakeQueue) SendCommand(_ context.Context, _ string, action string, _ interface{}, _ interface{}) error {
	if q.sendErr != nil {
		return q.sendErr
	}
	q.actions = append(q.actions, action)
	return nil
}

const memGuardUUID = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"

func newMemGuardTest(t *testing.T, action, status string) (*MemoryGuardService, *memGuardFakeStore, *memGuardFakeQueue, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	st := &memGuardFakeStore{srv: models.Server{ID: 7, UUID: memGuardUUID, Name: "Survival", OwnerID: "owner-1", Status: status, MemoryGuardAction: action}, auditOn: true, nodeToken: "tok"}
	q := &memGuardFakeQueue{}
	return NewMemoryGuardService(st, rdb, q, nil), st, q, rdb
}

func pushMemEvent(t *testing.T, rdb *redis.Client, ev memoryEvent) {
	t.Helper()
	b, _ := json.Marshal(ev)
	if err := rdb.RPush(context.Background(), "dylaris:server:"+memGuardUUID+":memory_events", string(b)).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryGuardCritical(t *testing.T) {
	cases := []struct {
		name, action, status string
		wantCmd              string
		wantDesired          string
		wantStatus           string
		wantTitle            string
	}{
		{"stop sets desired stopped and sends stop", "stop", "online", "stop", "stopped", "stopping", "was stopped to save the world"},
		{"empty value behaves as the default, off", "", "online", "", "", "", "about to run out of memory"},
		{"restart restarts", "restart", "online", "restart", "online", "restarting", "was restarted"},
		{"off only warns", "off", "online", "", "", "", "about to run out of memory"},
		{"not running: no power action", "stop", "installing", "", "", "", "about to run out of memory"},
		// J50: the node fires once and re-arms only below 93%, so a refusal while
		// the readiness gate still says "starting" was the only chance.
		{"still booting: the action runs", "restart", "starting", "restart", "online", "restarting", "was restarted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, st, q, rdb := newMemGuardTest(t, tc.action, tc.status)
			pushMemEvent(t, rdb, memoryEvent{Event: MemoryEventCritical, At: 100, UsedMB: 4400, LimitMB: 4500})
			m.scan(context.Background())

			if got := strings.Join(q.actions, ","); got != tc.wantCmd {
				t.Errorf("commands = %q, want %q", got, tc.wantCmd)
			}
			if got := strings.Join(st.desired, ","); got != tc.wantDesired {
				t.Errorf("desired_state writes = %q, want %q", got, tc.wantDesired)
			}
			if got := strings.Join(st.statuses, ","); got != tc.wantStatus {
				t.Errorf("status writes = %q, want %q", got, tc.wantStatus)
			}
			if tc.wantDesired != "" {
				if v, _ := rdb.Get(context.Background(), DesiredStateKey(memGuardUUID)).Result(); v != tc.wantDesired {
					t.Errorf("published desired_state = %q, want %q (the reconciler reads this)", v, tc.wantDesired)
				}
			}
			if len(st.notes) != 1 || !strings.Contains(st.notes[0].Title, tc.wantTitle) || st.notes[0].UserID != "owner-1" {
				t.Fatalf("notifications = %+v, want one to owner-1 containing %q", st.notes, tc.wantTitle)
			}
			if len(st.audits) != 1 || st.audits[0].EventType != ServerAuditEventMemoryGuard {
				t.Errorf("audits = %+v, want one memory_guard row", st.audits)
			}
			if n, _ := rdb.Exists(context.Background(), "dylaris:server:"+memGuardUUID+":memory_events").Result(); n != 0 {
				t.Error("the event list was not drained")
			}
		})
	}
}

func TestMemoryGuardCriticalNotificationIsThrottledButActionRepeats(t *testing.T) {
	m, st, q, rdb := newMemGuardTest(t, "restart", "online")
	for i := 0; i < 3; i++ {
		pushMemEvent(t, rdb, memoryEvent{Event: MemoryEventCritical, At: int64(i), UsedMB: 4450, LimitMB: 4500})
		m.scan(context.Background())
	}
	if got := strings.Join(q.actions, ","); got != "restart,restart,restart" {
		t.Errorf("commands = %q, want a restart for every critical event", got)
	}
	if len(st.notes) != 1 {
		t.Errorf("notifications = %d, want 1 per %s", len(st.notes), memoryCriticalNotifyEvery)
	}
}

func TestMemoryGuardOOMKilledRecordsCrash(t *testing.T) {
	m, st, q, rdb := newMemGuardTest(t, "stop", "online")
	latest := time.Now().Add(-time.Minute).Unix()
	pushMemEvent(t, rdb, memoryEvent{Event: MemoryEventOOM, At: latest - 60})
	pushMemEvent(t, rdb, memoryEvent{Event: MemoryEventOOM, At: latest})
	if !m.scan(context.Background()) {
		t.Fatal("scan reported no change after an OOM kill")
	}
	if st.crash != models.CrashReasonOOMKilled || st.crashAt.Unix() != latest {
		t.Errorf("last crash = %q at %d, want oom_killed at the latest event", st.crash, st.crashAt.Unix())
	}
	if len(q.actions) != 0 {
		t.Errorf("an OOM kill sent commands %v; it only records", q.actions)
	}
	if len(st.notes) != 1 || !strings.Contains(st.notes[0].Body, "killed: out of memory") {
		t.Errorf("notifications = %+v, want exactly one (rate-limited) OOM notice", st.notes)
	}
}

// T11: the kill time is the node host's clock. A BYON machine running fast put
// it in the future, which the panel's banner window hides; one far off put it
// before anything the node could still have queued. Either is Core's clock.
func TestMemoryGuardOOMTimeFromASkewedNode(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		at       int64
		wantNode bool
	}{
		{"in sync", now.Add(-2 * time.Minute).Unix(), true},
		{"node clock 5 minutes fast", now.Add(5 * time.Minute).Unix(), false},
		{"node clock a day slow", now.Add(-24 * time.Hour).Unix(), false},
		{"no time", 0, false},
	}
	for _, c := range cases {
		m, st, _, rdb := newMemGuardTest(t, "off", "online")
		pushMemEvent(t, rdb, memoryEvent{Event: MemoryEventOOM, At: c.at})
		before := time.Now()
		m.scan(context.Background())
		if c.wantNode {
			if st.crashAt.Unix() != c.at {
				t.Errorf("%s: crash at %v, want the node's %v", c.name, st.crashAt, time.Unix(c.at, 0))
			}
			continue
		}
		if st.crashAt.Before(before.Truncate(time.Second)) || st.crashAt.After(time.Now()) {
			t.Errorf("%s: crash at %v, want Core's clock (%v)", c.name, st.crashAt, before)
		}
	}
}

func TestMemoryGuardWarningIsRateLimited(t *testing.T) {
	m, st, q, rdb := newMemGuardTest(t, "stop", "online")
	for i := 0; i < 3; i++ {
		pushMemEvent(t, rdb, memoryEvent{Event: MemoryEventWarning, At: int64(i), UsedMB: 4200, LimitMB: 4500})
		m.scan(context.Background())
	}
	if len(st.notes) != 1 {
		t.Errorf("notifications = %d, want 1 per %s", len(st.notes), memoryWarnNotifyEvery)
	}
	if len(q.actions) != 0 || len(st.desired) != 0 {
		t.Error("a warning took a power action")
	}
}

// W13: the drain deletes the list before handle runs, and the node fires
// memory_critical once per climb. A transient DB error put nothing back, so the
// configured stop never happened; a server that is gone has nothing to retry.
func TestMemoryGuardDBErrorKeepsTheEvent(t *testing.T) {
	key := "dylaris:server:" + memGuardUUID + ":memory_events"
	cases := []struct {
		name             string
		event            string
		getErr, crashErr error
		wantKept         bool
	}{
		{"critical, server lookup fails", MemoryEventCritical, errors.New("pgbouncer: connection reset"), nil, true},
		{"oom, crash write fails", MemoryEventOOM, nil, errors.New("pgbouncer: connection reset"), true},
		{"server deleted: dropped", MemoryEventCritical, sql.ErrNoRows, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, st, q, rdb := newMemGuardTest(t, "stop", "online")
			st.getErr, st.crashErr = tc.getErr, tc.crashErr
			pushMemEvent(t, rdb, memoryEvent{Event: tc.event, At: time.Now().Unix(), UsedMB: 4400, LimitMB: 4500})
			m.scan(context.Background())

			n, _ := rdb.LLen(context.Background(), key).Result()
			if (n == 1) != tc.wantKept {
				t.Fatalf("events left after a failed handle = %d, want kept=%v", n, tc.wantKept)
			}
			if !tc.wantKept {
				return
			}
			if ttl, _ := rdb.TTL(context.Background(), key).Result(); ttl <= 0 {
				t.Errorf("requeued list TTL = %v, want one (the node's lists expire)", ttl)
			}
			// The DB is back: the next tick acts on it.
			st.getErr, st.crashErr = nil, nil
			m.scan(context.Background())
			if tc.event == MemoryEventCritical && strings.Join(q.actions, ",") != "stop" {
				t.Errorf("commands after recovery = %v, want stop", q.actions)
			}
			if tc.event == MemoryEventOOM && st.crash != models.CrashReasonOOMKilled {
				t.Errorf("crash after recovery = %q, want oom_killed", st.crash)
			}
			if n, _ := rdb.Exists(context.Background(), key).Result(); n != 0 {
				t.Error("the event was not drained once handled")
			}
		})
	}
}

// X12: power() failing on the database (node lookup, desired_state write) lost
// the event exactly like the server lookup did before W13. A send that fails
// after desired_state landed is not retried: the reconciler acts on that row.
func TestMemoryGuardPowerDBErrorKeepsTheEvent(t *testing.T) {
	key := "dylaris:server:" + memGuardUUID + ":memory_events"
	dbErr := errors.New("pgbouncer: connection reset")
	cases := []struct {
		name                       string
		nodeErr, desiredErr, sendE error
		wantKept                   bool
	}{
		{"node lookup fails", dbErr, nil, nil, true},
		{"desired_state write fails", nil, dbErr, nil, true},
		{"node gone: dropped", sql.ErrNoRows, nil, nil, false},
		{"send fails after desired_state landed: dropped", nil, nil, errors.New("queue down"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, st, q, rdb := newMemGuardTest(t, "stop", "online")
			st.nodeErr, st.desiredErr, q.sendErr = tc.nodeErr, tc.desiredErr, tc.sendE
			pushMemEvent(t, rdb, memoryEvent{Event: MemoryEventCritical, At: time.Now().Unix(), UsedMB: 4400, LimitMB: 4500})
			m.scan(context.Background())

			n, _ := rdb.LLen(context.Background(), key).Result()
			if (n == 1) != tc.wantKept {
				t.Fatalf("events left after a failed power action = %d, want kept=%v", n, tc.wantKept)
			}
			if !tc.wantKept {
				return
			}
			st.nodeErr, st.desiredErr = nil, nil
			m.scan(context.Background())
			if got := strings.Join(q.actions, ","); got != "stop" {
				t.Errorf("commands after recovery = %q, want stop", got)
			}
			if len(st.audits) != 1 {
				t.Errorf("audits = %d, want one row for the action that landed, not one per attempt", len(st.audits))
			}
		})
	}
}

// X13: every requeue reset the list's one-hour TTL, so an event held back by a
// long DB outage stopped or restarted a server that had long recovered. Core
// stamps the first requeue and drops an event that has waited past the bound.
func TestMemoryGuardStaleRequeuedEventIsDropped(t *testing.T) {
	key := "dylaris:server:" + memGuardUUID + ":memory_events"
	m, st, q, rdb := newMemGuardTest(t, "restart", "online")
	st.getErr = errors.New("pgbouncer: connection reset")
	pushMemEvent(t, rdb, memoryEvent{Event: MemoryEventCritical, At: time.Now().Unix(), UsedMB: 4400, LimitMB: 4500})
	m.scan(context.Background())
	m.scan(context.Background())

	raw, err := rdb.LRange(context.Background(), key, 0, -1).Result()
	if err != nil || len(raw) != 1 {
		t.Fatalf("requeued events = %v (%v), want one", raw, err)
	}
	var ev memoryEvent
	if err := json.Unmarshal([]byte(raw[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.QueuedAt <= 0 || time.Since(time.Unix(ev.QueuedAt, 0)) > time.Minute {
		t.Fatalf("queuedAt = %d, want Core's first-requeue time", ev.QueuedAt)
	}

	// The outage outlasted the bound: the same event, first held back 2 h ago.
	rdb.Del(context.Background(), key)
	ev.QueuedAt = time.Now().Add(-2 * time.Hour).Unix()
	pushMemEvent(t, rdb, ev)
	m.scan(context.Background())
	if n, _ := rdb.Exists(context.Background(), key).Result(); n != 0 {
		t.Error("a stale event was put back again")
	}
	st.getErr = nil
	m.scan(context.Background())
	if len(q.actions) != 0 || len(st.desired) != 0 {
		t.Errorf("a 2 h old memory_critical still acted: commands %v, desired %v", q.actions, st.desired)
	}
}
