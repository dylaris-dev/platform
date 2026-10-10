package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type memGuardFakeStore struct {
	srv       models.Server
	desired   []string
	statuses  []string
	crash     string
	crashAt   time.Time
	notes     []models.Notification
	audits    []models.ServerAuditEvent
	auditOn   bool
	nodeToken string
}

func (f *memGuardFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	s := f.srv
	return &s, nil
}
func (f *memGuardFakeStore) GetNodeByID(int) (*models.Node, error) {
	return &models.Node{Token: f.nodeToken}, nil
}
func (f *memGuardFakeStore) UpdateServerStatus(_ int, s string) error {
	f.statuses = append(f.statuses, s)
	return nil
}
func (f *memGuardFakeStore) UpdateServerDesiredState(_ int, s string) error {
	f.desired = append(f.desired, s)
	return nil
}
func (f *memGuardFakeStore) SetServerLastCrash(_ int, reason string, at time.Time) error {
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

type memGuardFakeQueue struct{ actions []string }

func (q *memGuardFakeQueue) SendCommand(_ context.Context, _ string, action string, _ interface{}, _ interface{}) error {
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
		{"restart restarts", "restart", "online", "restart", "online", "starting", "was restarted"},
		{"off only warns", "off", "online", "", "", "", "about to run out of memory"},
		{"not running: no power action", "stop", "installing", "", "", "", "about to run out of memory"},
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
	pushMemEvent(t, rdb, memoryEvent{Event: MemoryEventOOM, At: 1700000000})
	pushMemEvent(t, rdb, memoryEvent{Event: MemoryEventOOM, At: 1700000060})
	if !m.scan(context.Background()) {
		t.Fatal("scan reported no change after an OOM kill")
	}
	if st.crash != models.CrashReasonOOMKilled || st.crashAt.Unix() != 1700000060 {
		t.Errorf("last crash = %q at %d, want oom_killed at the latest event", st.crash, st.crashAt.Unix())
	}
	if len(q.actions) != 0 {
		t.Errorf("an OOM kill sent commands %v; it only records", q.actions)
	}
	if len(st.notes) != 1 || !strings.Contains(st.notes[0].Body, "killed: out of memory") {
		t.Errorf("notifications = %+v, want exactly one (rate-limited) OOM notice", st.notes)
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
