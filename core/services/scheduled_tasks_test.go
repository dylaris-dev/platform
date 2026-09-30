package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// schedTaskFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods ScheduledTaskService touches
// are overridden. Any other call would panic - the tests never make one.
type schedTaskFakeStore struct {
	store.Store

	due    []models.ScheduledTask
	dueErr error

	servers map[int]models.Server
	nodes   map[int]models.Node

	billing    map[string]*store.UserBilling
	billingErr error

	desiredCalls []schedDesiredCall
	statusCalls  []schedStatusCall
	enabledCalls []schedEnabledCall
	runRecords   []schedRunRecord

	// claimLost makes ClaimScheduledTaskRun answer "another replica won".
	claimLost map[int]bool
	claims    []int
}

type schedDesiredCall struct {
	id    int
	state string
}
type schedStatusCall struct {
	id     int
	status string
}
type schedEnabledCall struct {
	id      int
	enabled bool
	nextRun *time.Time
}
type schedRunRecord struct {
	id      int
	status  string
	errMsg  string
	nextRun *time.Time
}

func (f *schedTaskFakeStore) ListDueScheduledTasks(now time.Time, limit int) ([]models.ScheduledTask, error) {
	return f.due, f.dueErr
}

func (f *schedTaskFakeStore) GetServerByID(id int) (*models.Server, error) {
	if s, ok := f.servers[id]; ok {
		return &s, nil
	}
	return nil, errors.New("server not found")
}

func (f *schedTaskFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if n, ok := f.nodes[id]; ok {
		return &n, nil
	}
	return nil, errors.New("node not found")
}

// GetUserBilling backs the suspension gate on a restart task. An absent entry
// mirrors the real store, which answers "active" for a user with no billing row
// at all - so only an explicit entry or billingErr changes the outcome.
func (f *schedTaskFakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	if f.billingErr != nil {
		return nil, f.billingErr
	}
	if b, ok := f.billing[userID]; ok {
		return b, nil
	}
	return &store.UserBilling{UserID: userID, Status: "active"}, nil
}

func (f *schedTaskFakeStore) UpdateServerDesiredState(id int, state string) error {
	f.desiredCalls = append(f.desiredCalls, schedDesiredCall{id, state})
	return nil
}

func (f *schedTaskFakeStore) UpdateServerStatus(id int, status string) error {
	f.statusCalls = append(f.statusCalls, schedStatusCall{id, status})
	return nil
}

func (f *schedTaskFakeStore) SetScheduledTaskEnabled(id int, enabled bool, nextRun *time.Time) error {
	f.enabledCalls = append(f.enabledCalls, schedEnabledCall{id, enabled, nextRun})
	return nil
}

func (f *schedTaskFakeStore) ClaimScheduledTaskRun(id int, dueAt, next time.Time) (bool, error) {
	f.claims = append(f.claims, id)
	return !f.claimLost[id], nil
}

func (f *schedTaskFakeStore) RecordScheduledTaskRun(id int, ranAt time.Time, status, errMsg string, nextRun *time.Time) error {
	f.runRecords = append(f.runRecords, schedRunRecord{id, status, errMsg, nextRun})
	return nil
}

func newSchedTestService(t *testing.T, fs *schedTaskFakeStore) (*ScheduledTaskService, func()) {
	t.Helper()
	rdb := newQueueTestRedis(t)
	svc := &ScheduledTaskService{store: fs, redis: rdb, queue: NewQueueService(rdb)}
	return svc, func() {}
}

func TestComputeNextRun(t *testing.T) {
	from := time.Date(2026, 7, 15, 10, 15, 30, 0, time.UTC)

	cases := []struct {
		name    string
		cron    string
		want    time.Time
		wantErr bool
	}{
		{"every minute", "* * * * *", time.Date(2026, 7, 15, 10, 16, 0, 0, time.UTC), false},
		{"hourly descriptor", "@hourly", time.Date(2026, 7, 15, 11, 0, 0, 0, time.UTC), false},
		{"daily descriptor", "@daily", time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC), false},
		{"explicit daily", "0 0 * * *", time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC), false},
		{"invalid cron", "not a cron", time.Time{}, true},
		{"wrong field count", "* * * *", time.Time{}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ComputeNextRun(c.cron, from)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got next=%v", c.cron, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ComputeNextRun(%q): %v", c.cron, err)
			}
			if !got.Equal(c.want) {
				t.Errorf("ComputeNextRun(%q) = %v, want %v", c.cron, got, c.want)
			}
		})
	}
}

func TestRunDue_NoDueTasks_NoOp(t *testing.T) {
	fs := &schedTaskFakeStore{}
	svc, done := newSchedTestService(t, fs)
	defer done()

	svc.runDue(context.Background())

	if len(fs.runRecords) != 0 {
		t.Fatalf("expected no run records, got %+v", fs.runRecords)
	}
}

func TestRunDue_RestartTask_Success(t *testing.T) {
	fs := &schedTaskFakeStore{
		servers: map[int]models.Server{1: {ID: 1, UUID: "srv-1", NodeID: 5, Status: "online"}},
		nodes:   map[int]models.Node{5: {ID: 5, Token: "node-tok-5"}},
		due: []models.ScheduledTask{
			{ID: 100, ServerID: 1, TaskType: "restart", ScheduleCron: "* * * * *"},
		},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()

	svc.runDue(context.Background())

	if len(fs.desiredCalls) != 1 || fs.desiredCalls[0] != (schedDesiredCall{1, "online"}) {
		t.Errorf("desiredCalls = %+v, want [{1 online}]", fs.desiredCalls)
	}
	if len(fs.statusCalls) != 1 || fs.statusCalls[0] != (schedStatusCall{1, "starting"}) {
		t.Errorf("statusCalls = %+v, want [{1 starting}]", fs.statusCalls)
	}

	payload := readStreamPayload(t, svc.redis, "dylaris:node:node-tok-5:cmds")
	if payload["action"] != "restart" {
		t.Errorf("action = %v, want restart", payload["action"])
	}
	cfg, _ := payload["config"].(map[string]interface{})
	if cfg["uuid"] != "srv-1" {
		t.Errorf("config.uuid = %v, want srv-1", cfg["uuid"])
	}

	if len(fs.runRecords) != 1 {
		t.Fatalf("runRecords = %+v, want 1 entry", fs.runRecords)
	}
	rr := fs.runRecords[0]
	if rr.status != "ok" || rr.errMsg != "" {
		t.Errorf("run record = %+v, want status=ok errMsg=empty", rr)
	}
	if rr.nextRun == nil {
		t.Errorf("expected nextRun to be set for a valid cron")
	}
	if len(fs.enabledCalls) != 0 {
		t.Errorf("expected no SetScheduledTaskEnabled calls for a valid cron, got %+v", fs.enabledCalls)
	}
}

func TestRunDue_SayTask_Success(t *testing.T) {
	fs := &schedTaskFakeStore{
		// Status matters now: a say is only queued to a server that is
		// running. This test never named a state, which is how it passed
		// while the executor queued to stopped servers too.
		servers: map[int]models.Server{2: {ID: 2, UUID: "srv-2", Status: "online"}},
		due: []models.ScheduledTask{
			{ID: 200, ServerID: 2, TaskType: "say", ScheduleCron: "@hourly", Payload: "hello world"},
		},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()

	svc.runDue(context.Background())

	vals, err := svc.redis.LRange(context.Background(), "dylaris:server:srv-2:input", 0, -1).Result()
	if err != nil {
		t.Fatalf("LRange: %v", err)
	}
	if len(vals) != 1 || vals[0] != "say hello world" {
		t.Fatalf("stdin queue = %v, want [say hello world]", vals)
	}

	if len(fs.runRecords) != 1 || fs.runRecords[0].status != "ok" {
		t.Fatalf("run record = %+v, want status=ok", fs.runRecords)
	}
}

func TestRunDue_SayTask_EmptyPayload_RecordsError(t *testing.T) {
	fs := &schedTaskFakeStore{
		servers: map[int]models.Server{2: {ID: 2, UUID: "srv-2"}},
		due: []models.ScheduledTask{
			{ID: 201, ServerID: 2, TaskType: "say", ScheduleCron: "@hourly", Payload: ""},
		},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()

	svc.runDue(context.Background())

	if len(fs.runRecords) != 1 {
		t.Fatalf("runRecords = %+v, want 1 entry", fs.runRecords)
	}
	rr := fs.runRecords[0]
	if rr.status != "error" || rr.errMsg == "" {
		t.Errorf("run record = %+v, want status=error with a message", rr)
	}
}

func TestRunDue_UnknownTaskType_RecordsError(t *testing.T) {
	fs := &schedTaskFakeStore{
		servers: map[int]models.Server{2: {ID: 2, UUID: "srv-2"}},
		due: []models.ScheduledTask{
			{ID: 202, ServerID: 2, TaskType: "bogus", ScheduleCron: "@hourly"},
		},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()

	svc.runDue(context.Background())

	if len(fs.runRecords) != 1 || fs.runRecords[0].status != "error" {
		t.Fatalf("runRecords = %+v, want single error entry", fs.runRecords)
	}
}

func TestRunDue_ServerNotFound_RecordsError(t *testing.T) {
	fs := &schedTaskFakeStore{
		due: []models.ScheduledTask{
			{ID: 300, ServerID: 999, TaskType: "restart", ScheduleCron: "@hourly"},
		},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()

	svc.runDue(context.Background())

	if len(fs.runRecords) != 1 || fs.runRecords[0].status != "error" {
		t.Fatalf("runRecords = %+v, want single error entry", fs.runRecords)
	}
}

func TestRunDue_RestartTask_NodeNotFound_RecordsError(t *testing.T) {
	fs := &schedTaskFakeStore{
		servers: map[int]models.Server{1: {ID: 1, UUID: "srv-1", NodeID: 5, Status: "online"}},
		// nodes map deliberately empty -> GetNodeByID fails
		due: []models.ScheduledTask{
			{ID: 301, ServerID: 1, TaskType: "restart", ScheduleCron: "@hourly"},
		},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()

	svc.runDue(context.Background())

	if len(fs.runRecords) != 1 || fs.runRecords[0].status != "error" {
		t.Fatalf("runRecords = %+v, want single error entry", fs.runRecords)
	}
}

// TestRunDue_RestartTask_QueueNil pins that the "queue not available" guard
// fires BEFORE the desired-state/status writes - a restart that can't reach
// the node must not leave the server looking like it is starting.
func TestRunDue_RestartTask_QueueNil(t *testing.T) {
	fs := &schedTaskFakeStore{
		servers: map[int]models.Server{1: {ID: 1, UUID: "srv-1", NodeID: 5, Status: "online"}},
		nodes:   map[int]models.Node{5: {ID: 5, Token: "node-tok-5"}},
		due: []models.ScheduledTask{
			{ID: 302, ServerID: 1, TaskType: "restart", ScheduleCron: "@hourly"},
		},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()
	svc.queue = nil

	svc.runDue(context.Background())

	if len(fs.desiredCalls) != 0 || len(fs.statusCalls) != 0 {
		t.Errorf("expected no state-update calls when queue is nil, got desired=%+v status=%+v", fs.desiredCalls, fs.statusCalls)
	}
	if len(fs.runRecords) != 1 || fs.runRecords[0].status != "error" {
		t.Fatalf("runRecords = %+v, want single error entry", fs.runRecords)
	}
}

// TestRunDue_InvalidCron_DisablesTaskAndOverridesOKStatus pins that a bad cron
// string wins over an otherwise-successful execute: the task is disabled and
// the recorded status/error reflect the parse failure, not "ok".
func TestRunDue_InvalidCron_DisablesTaskAndOverridesOKStatus(t *testing.T) {
	fs := &schedTaskFakeStore{
		servers: map[int]models.Server{2: {ID: 2, UUID: "srv-2"}},
		due: []models.ScheduledTask{
			{ID: 400, ServerID: 2, TaskType: "say", ScheduleCron: "garbage cron", Payload: "hi"},
		},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()

	svc.runDue(context.Background())

	if len(fs.enabledCalls) != 1 {
		t.Fatalf("enabledCalls = %+v, want exactly one disable call", fs.enabledCalls)
	}
	ec := fs.enabledCalls[0]
	if ec.id != 400 || ec.enabled != false || ec.nextRun != nil {
		t.Errorf("disable call = %+v, want {400 false <nil>}", ec)
	}

	if len(fs.runRecords) != 1 {
		t.Fatalf("runRecords = %+v, want 1 entry", fs.runRecords)
	}
	rr := fs.runRecords[0]
	if rr.status != "error" || rr.errMsg == "" || rr.nextRun != nil {
		t.Errorf("run record = %+v, want status=error, non-empty errMsg, nil nextRun", rr)
	}
}

func TestRunDue_PublishesServersChangedOnlyOnRestart(t *testing.T) {
	fs := &schedTaskFakeStore{
		servers: map[int]models.Server{1: {ID: 1, UUID: "srv-1", NodeID: 5, Status: "online"}},
		nodes:   map[int]models.Node{5: {ID: 5, Token: "node-tok-5"}},
		due: []models.ScheduledTask{
			{ID: 500, ServerID: 1, TaskType: "restart", ScheduleCron: "@hourly"},
		},
	}
	rdb := newQueueTestRedis(t)
	svc := &ScheduledTaskService{store: fs, redis: rdb, queue: NewQueueService(rdb), events: NewSystemEventsPublisher(rdb)}

	ctx := context.Background()
	sub := rdb.Subscribe(ctx, SystemEventsChannel)
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ch := sub.Channel()

	svc.runDue(ctx)

	got := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case msg := <-ch:
			var ev SystemEvent
			if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
				t.Fatalf("unmarshal event: %v", err)
			}
			got[ev.Type] = true
		case <-deadline:
			t.Fatalf("timed out waiting for events, got so far: %v", got)
		}
	}
	if !got["servers.changed"] || !got["scheduled_tasks.changed"] {
		t.Errorf("events = %v, want both servers.changed and scheduled_tasks.changed", got)
	}
}

func TestRunDue_SayOnly_DoesNotPublishServersChanged(t *testing.T) {
	fs := &schedTaskFakeStore{
		servers: map[int]models.Server{2: {ID: 2, UUID: "srv-2"}},
		due: []models.ScheduledTask{
			{ID: 501, ServerID: 2, TaskType: "say", ScheduleCron: "@hourly", Payload: "hi"},
		},
	}
	rdb := newQueueTestRedis(t)
	svc := &ScheduledTaskService{store: fs, redis: rdb, queue: NewQueueService(rdb), events: NewSystemEventsPublisher(rdb)}

	ctx := context.Background()
	sub := rdb.Subscribe(ctx, SystemEventsChannel)
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ch := sub.Channel()

	svc.runDue(ctx)

	// Expect exactly one event: scheduled_tasks.changed.
	select {
	case msg := <-ch:
		var ev SystemEvent
		if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		if ev.Type != "scheduled_tasks.changed" {
			t.Fatalf("first event = %q, want scheduled_tasks.changed", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scheduled_tasks.changed event")
	}

	select {
	case msg := <-ch:
		var ev SystemEvent
		_ = json.Unmarshal([]byte(msg.Payload), &ev)
		t.Fatalf("unexpected extra event published: %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// expected: no second event
	}
}

// --- suspension parity with the HTTP power gate ---
//
// handlers/servers_lifecycle.go refuses start/restart while the owner's billing
// is suspended. This executor took no such check, so a tenant's own nightly
// "restart" task handed their server back after the billing lifecycle had
// stopped it, until the next hourly enforcement pass stopped it again.

func TestRunDue_RestartTask_SuspendedOwner_IsSkipped(t *testing.T) {
	fs := &schedTaskFakeStore{
		servers: map[int]models.Server{1: {ID: 1, UUID: "srv-1", NodeID: 5, OwnerID: "owner-1", Status: "online"}},
		nodes:   map[int]models.Node{5: {ID: 5, Token: "node-tok-5"}},
		billing: map[string]*store.UserBilling{"owner-1": {UserID: "owner-1", Status: "suspended"}},
		due: []models.ScheduledTask{
			{ID: 100, ServerID: 1, TaskType: "restart", ScheduleCron: "* * * * *"},
		},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()

	svc.runDue(context.Background())

	// The DB write is the part that actually defeats the suspension: the node
	// reconciler reads desired_state, so flipping it back to online restarts the
	// server even if the queued command were lost.
	if len(fs.desiredCalls) != 0 {
		t.Errorf("desiredCalls = %+v, want none for a suspended owner", fs.desiredCalls)
	}
	if len(fs.statusCalls) != 0 {
		t.Errorf("statusCalls = %+v, want none for a suspended owner", fs.statusCalls)
	}
	msgs, err := svc.redis.XRange(context.Background(), "dylaris:node:node-tok-5:cmds", "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("queued %d command(s) for a suspended owner, want 0", len(msgs))
	}

	// Recorded as an error, not silently dropped: the tenant sees why on the
	// task's last run, and next_run still advances so it resumes on its own once
	// payment is settled.
	if len(fs.runRecords) != 1 {
		t.Fatalf("runRecords = %+v, want 1 entry", fs.runRecords)
	}
	// "skipped", not "error": nothing is broken, the owner is suspended.
	if fs.runRecords[0].status != "skipped" {
		t.Errorf("status = %q, want skipped", fs.runRecords[0].status)
	}
	if !strings.Contains(fs.runRecords[0].errMsg, "suspended") {
		t.Errorf("errMsg = %q, want it to mention the suspension", fs.runRecords[0].errMsg)
	}
	if fs.runRecords[0].nextRun == nil {
		t.Error("nextRun = nil, want the schedule to keep advancing through a suspension")
	}
	if len(fs.enabledCalls) != 0 {
		t.Errorf("enabledCalls = %+v, want the task left enabled", fs.enabledCalls)
	}
}

// Fails CLOSED, unlike the HTTP gate: the real store answers "active" for a user
// with no billing row, so an error is a genuine database failure and skipping
// one firing costs a deferred restart the next tick retries.
func TestRunDue_RestartTask_BillingUnreadable_IsSkipped(t *testing.T) {
	fs := &schedTaskFakeStore{
		servers:    map[int]models.Server{1: {ID: 1, UUID: "srv-1", NodeID: 5, OwnerID: "owner-1", Status: "online"}},
		nodes:      map[int]models.Node{5: {ID: 5, Token: "node-tok-5"}},
		billingErr: errors.New("db down"),
		due: []models.ScheduledTask{
			{ID: 100, ServerID: 1, TaskType: "restart", ScheduleCron: "* * * * *"},
		},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()

	svc.runDue(context.Background())

	if len(fs.desiredCalls) != 0 {
		t.Errorf("desiredCalls = %+v, want none when the billing status cannot be read", fs.desiredCalls)
	}
	if len(fs.runRecords) != 1 || fs.runRecords[0].status != "error" {
		t.Fatalf("runRecords = %+v, want a single error record", fs.runRecords)
	}
}

// Scope pin: the console "send command" handler has no suspension gate, so the
// say task must not grow one either. The executor mirrors the HTTP contract
// exactly - no stricter, no looser.
func TestRunDue_SayTask_SuspendedOwner_StillRuns(t *testing.T) {
	fs := &schedTaskFakeStore{
		servers: map[int]models.Server{2: {ID: 2, UUID: "srv-2", OwnerID: "owner-1", Status: "online"}},
		billing: map[string]*store.UserBilling{"owner-1": {UserID: "owner-1", Status: "suspended"}},
		due: []models.ScheduledTask{
			{ID: 501, ServerID: 2, TaskType: "say", ScheduleCron: "@hourly", Payload: "hi"},
		},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()

	svc.runDue(context.Background())

	got, err := svc.redis.LRange(context.Background(), "dylaris:server:srv-2:input", 0, -1).Result()
	if err != nil {
		t.Fatalf("LRange: %v", err)
	}
	if len(got) != 1 || got[0] != "say hi" {
		t.Errorf("stdin queue = %v, want [\"say hi\"]", got)
	}
}

// The stdin queue is an uncapped Redis list with no expiry, and a say task
// used to push into it whatever state the server was in, recording "ok".
// Measured on a live instance: a minutely task on a stopped server added an
// entry a minute, and the next start drained the whole backlog into a server
// that was still booting - "Command exception: /say ..." for the ones that
// landed too early and a wall of stale messages for the rest.
func TestRunDue_SayTask_OnlyQueuesToARunningServer(t *testing.T) {
	tests := []struct {
		status    string
		wantQueue bool
		why       string
	}{
		{status: "online", wantQueue: true, why: "the whole point is that a running server still gets its message"},
		{status: "stopped", wantQueue: false, why: "this is the case that accumulated forever and reported ok every minute"},
		{status: "starting", wantQueue: false, why: "this is exactly the state that answered with Command exception"},
		{status: "offline", wantQueue: false, why: "the node is gone; nothing will drain this until it returns"},
		{status: "pending_setup", wantQueue: false, why: "there is no server behind it yet"},
		{status: "installing", wantQueue: false, why: "the container is being rebuilt under it"},
		{status: "", wantQueue: false, why: "an unknown state is not a running one"},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			fs := &schedTaskFakeStore{
				servers: map[int]models.Server{2: {ID: 2, UUID: "srv-2", Status: tt.status}},
				due: []models.ScheduledTask{
					{ID: 600, ServerID: 2, TaskType: "say", ScheduleCron: "@hourly", Payload: "hi"},
				},
			}
			svc, done := newSchedTestService(t, fs)
			defer done()

			svc.runDue(context.Background())

			got, err := svc.redis.LRange(context.Background(), "dylaris:server:srv-2:input", 0, -1).Result()
			if err != nil {
				t.Fatalf("LRange: %v", err)
			}
			if tt.wantQueue && len(got) != 1 {
				t.Fatalf("stdin queue = %v, want one message: %s", got, tt.why)
			}
			if !tt.wantQueue && len(got) != 0 {
				t.Fatalf("stdin queue = %v, want nothing queued: %s", got, tt.why)
			}

			if len(fs.runRecords) != 1 {
				t.Fatalf("runRecords = %+v, want exactly one", fs.runRecords)
			}
			rr := fs.runRecords[0]
			wantStatus := "ok"
			if !tt.wantQueue {
				wantStatus = "skipped"
			}
			if rr.status != wantStatus {
				t.Errorf("recorded status = %q, want %q: a firing that was not delivered must not read as ok", rr.status, wantStatus)
			}
			if !tt.wantQueue && rr.errMsg == "" {
				t.Error("no reason was recorded, so the owner cannot tell why the message never arrived")
			}
			// The task must keep its schedule either way - a server that is
			// down for an hour must not lose its task.
			if rr.nextRun == nil {
				t.Error("next run was cleared; the task would stop firing once the server came back")
			}
		})
	}
}

// A scheduled restart restarts a RUNNING server and nothing else, the rule the
// power-action handler answers with a 409. It used to start a server its owner
// had stopped on purpose, writing desired_state=online, and to run against a
// server in setup or out of disk.
func TestRunDue_RestartTask_OnlyRestartsARunningServer(t *testing.T) {
	for _, status := range []string{"stopped", "offline", "pending_setup", "disk_full", "installing", ""} {
		t.Run("status="+status, func(t *testing.T) {
			fs := &schedTaskFakeStore{
				servers: map[int]models.Server{1: {ID: 1, UUID: "srv-1", NodeID: 5, Status: status}},
				nodes:   map[int]models.Node{5: {ID: 5, Token: "node-tok-5"}},
				due:     []models.ScheduledTask{{ID: 100, ServerID: 1, TaskType: "restart", ScheduleCron: "* * * * *"}},
			}
			svc, done := newSchedTestService(t, fs)
			defer done()

			svc.runDue(context.Background())

			if len(fs.desiredCalls) != 0 || len(fs.statusCalls) != 0 {
				t.Errorf("a %q server was touched: desired=%+v status=%+v", status, fs.desiredCalls, fs.statusCalls)
			}
			if n := svc.redis.XLen(context.Background(), "dylaris:node:node-tok-5:cmds").Val(); n != 0 {
				t.Errorf("a restart was queued for a %q server", status)
			}
			if len(fs.runRecords) != 1 || fs.runRecords[0].status != "skipped" {
				t.Fatalf("run records = %+v, want one skipped", fs.runRecords)
			}
			if fs.runRecords[0].nextRun == nil {
				t.Error("a skipped firing must still move next_run on")
			}
		})
	}
}

// The handler refuses a power action inside the post-install cooldown (429);
// the schedule has to as well.
func TestRunDue_RestartTask_SkippedDuringInstallCooldown(t *testing.T) {
	fs := &schedTaskFakeStore{
		servers: map[int]models.Server{1: {ID: 1, UUID: "srv-1", NodeID: 5, Status: "online"}},
		nodes:   map[int]models.Node{5: {ID: 5, Token: "node-tok-5"}},
		due:     []models.ScheduledTask{{ID: 100, ServerID: 1, TaskType: "restart", ScheduleCron: "* * * * *"}},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()
	svc.redis.Set(context.Background(), "dylaris:server:srv-1:install-start", "1", 30*time.Second)

	svc.runDue(context.Background())

	if n := svc.redis.XLen(context.Background(), "dylaris:node:node-tok-5:cmds").Val(); n != 0 {
		t.Error("a restart was queued inside the install cooldown")
	}
	if len(fs.runRecords) != 1 || fs.runRecords[0].status != "skipped" {
		t.Fatalf("run records = %+v, want one skipped", fs.runRecords)
	}
}

// Two Core replicas can list the same due row across a leader handover. Only
// the one whose claim moves next_run fires; the other does nothing at all.
func TestRunDue_LostClaimDoesNotFire(t *testing.T) {
	due := time.Date(2026, 9, 30, 4, 0, 0, 0, time.UTC)
	fs := &schedTaskFakeStore{
		servers:   map[int]models.Server{1: {ID: 1, UUID: "srv-1", NodeID: 5, Status: "online"}},
		nodes:     map[int]models.Node{5: {ID: 5, Token: "node-tok-5"}},
		due:       []models.ScheduledTask{{ID: 100, ServerID: 1, TaskType: "restart", ScheduleCron: "* * * * *", NextRun: &due}},
		claimLost: map[int]bool{100: true},
	}
	svc, done := newSchedTestService(t, fs)
	defer done()

	svc.runDue(context.Background())

	if len(fs.claims) != 1 {
		t.Fatalf("claims = %v, want one attempt", fs.claims)
	}
	if n := svc.redis.XLen(context.Background(), "dylaris:node:node-tok-5:cmds").Val(); n != 0 {
		t.Error("a task whose claim was lost still fired")
	}
	if len(fs.runRecords) != 0 || len(fs.desiredCalls) != 0 {
		t.Errorf("a lost claim still wrote: records=%+v desired=%+v", fs.runRecords, fs.desiredCalls)
	}
}

// Schedules run in UTC whatever the host's zone is. robfig evaluates a spec in
// time.Local unless it names a zone, so this held only because the production
// image happens to have no TZ set.
func TestComputeNextRun_IsUTCRegardlessOfHostZone(t *testing.T) {
	orig := time.Local
	time.Local = time.FixedZone("UTC+5", 5*3600)
	defer func() { time.Local = orig }()

	from := time.Date(2026, 7, 15, 10, 15, 30, 0, time.UTC)
	got, err := ComputeNextRun("@daily", from)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("@daily from %v = %v, want %v (UTC midnight)", from, got, want)
	}
}

// A zone cannot be chosen, and "TZ=UTC" with no space after it used to panic
// inside the parser mid-request; every one of these is a plain error now.
func TestComputeNextRun_RefusesZonesAndOverlongSpecs(t *testing.T) {
	from := time.Date(2026, 7, 15, 10, 15, 30, 0, time.UTC)
	long := strings.Repeat("0,", 70) + "0 * * * *"
	for _, spec := range []string{"TZ=UTC", "TZ=Europe/Berlin 0 4 * * *", "CRON_TZ=UTC @daily", "", "   ", long} {
		if _, err := ComputeNextRun(spec, from); err == nil {
			t.Errorf("ComputeNextRun(%q) was accepted", spec)
		}
	}
	// Surrounding whitespace is not a different schedule.
	if _, err := ComputeNextRun("  0 4 * * *  ", from); err != nil {
		t.Errorf("a padded schedule was refused: %v", err)
	}
}

// "@every" is the one form that can go below a minute; the executor ticks every
// 30s, so a shorter one just fired on every tick.
func TestComputeNextRun_MinimumInterval(t *testing.T) {
	from := time.Date(2026, 7, 15, 10, 15, 30, 0, time.UTC)
	for _, spec := range []string{"@every 1s", "@every 30s", "@every 59s"} {
		if _, err := ComputeNextRun(spec, from); err == nil {
			t.Errorf("%q was accepted", spec)
		}
	}
	for _, spec := range []string{"@every 1m", "@every 90s", "@every 2h"} {
		if _, err := ComputeNextRun(spec, from); err != nil {
			t.Errorf("%q was refused: %v", spec, err)
		}
	}
}
