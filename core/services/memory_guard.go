package services

// Landing the node's memory guard events (node/memory_guard.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/pkg/leader"

	"github.com/redis/go-redis/v9"
)

// The node appends JSON events to dylaris:server:<uuid>:memory_events, a list
// under the per-server prefix its Redis ACL already covers. A node that
// predates this writes nothing; a Core that predates it leaves the list to
// expire (the node caps and expires it).
const (
	memoryEventsPattern = "dylaris:server:*:memory_events"

	MemoryEventWarning  = "memory_warning"
	MemoryEventCritical = "memory_critical"
	MemoryEventOOM      = "oom_killed"

	// ServerAuditEventMemoryGuard is the audit row for every action taken here.
	ServerAuditEventMemoryGuard = "memory_guard"
	// NotifyTypeServerMemory is the notification type for all three events.
	NotifyTypeServerMemory = "server_memory"

	// A warning repeats every 5 minutes on the node while memory stays high;
	// the owner hears about it at most this often.
	memoryWarnNotifyEvery = 6 * time.Hour
	// An OOM loop (the log-shipper restarts Java up to three times) is one
	// incident, not three notifications. The crash is recorded every time.
	memoryOOMNotifyEvery = 30 * time.Minute
	// memory_critical repeats once memory drops and climbs again, and with
	// 'restart' that can be every few minutes.
	memoryCriticalNotifyEvery = 6 * time.Hour
)

// memoryEvent mirrors the node's struct.
type memoryEvent struct {
	Event   string `json:"event"`
	At      int64  `json:"at"`
	UsedMB  int64  `json:"usedMB"`
	LimitMB int64  `json:"limitMB"`
}

// memoryGuardStore is the slice of store.Store this needs.
type memoryGuardStore interface {
	GetServerByUUID(uuid string) (*models.Server, error)
	GetNodeByID(id int) (*models.Node, error)
	UpdateServerStatus(id int, status string) error
	UpdateServerDesiredState(id int, desiredState string) error
	SetServerLastCrash(id int, reason string, at time.Time) error
	InsertNotification(n *models.Notification) (int64, error)
	GetServerAuditState(serverID int) (enabled, forceOn bool, count int, err error)
	InsertServerAudit(ev *models.ServerAuditEvent) error
}

type nodeCommander interface {
	SendCommand(ctx context.Context, nodeToken string, action string, config interface{}, installer interface{}) error
}

type MemoryGuardService struct {
	store  memoryGuardStore
	redis  *redis.Client
	queue  nodeCommander
	events *SystemEventsPublisher
	leader leader.Election
}

func NewMemoryGuardService(s memoryGuardStore, r *redis.Client, q nodeCommander, ev *SystemEventsPublisher) *MemoryGuardService {
	return &MemoryGuardService{store: s, redis: r, queue: q, events: ev}
}

func (m *MemoryGuardService) SetLeader(l leader.Election) { m.leader = l }

// Start drains the event lists every 5 seconds on the leader. Leader-only
// because a drain is a read-and-delete: two Cores would split one server's
// events between them and both act.
func (m *MemoryGuardService) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if m.leader != nil && !m.leader.IsLeader() {
					continue
				}
				if m.scan(ctx) {
					m.events.Publish(ctx, "servers.changed", nil)
				}
			}
		}
	}()
}

// scan lands every pending event. Returns true when a server row changed.
func (m *MemoryGuardService) scan(ctx context.Context) bool {
	changed := false
	var cursor uint64
	for {
		keys, next, err := m.redis.Scan(ctx, cursor, memoryEventsPattern, 100).Result()
		if err != nil {
			return changed
		}
		for _, key := range keys {
			parts := strings.Split(key, ":")
			if len(parts) != 4 {
				continue
			}
			for _, ev := range m.drain(ctx, key) {
				if m.handle(ctx, parts[2], ev) {
					changed = true
				}
			}
		}
		cursor = next
		if cursor == 0 {
			return changed
		}
	}
}

// drain reads and removes the list in one transaction, so an event the node
// appends in between is either read now or left for the next tick.
func (m *MemoryGuardService) drain(ctx context.Context, key string) []memoryEvent {
	pipe := m.redis.TxPipeline()
	rng := pipe.LRange(ctx, key, 0, -1)
	pipe.Del(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil
	}
	var out []memoryEvent
	for _, raw := range rng.Val() {
		var ev memoryEvent
		if json.Unmarshal([]byte(raw), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out
}

// handle acts on one event. Returns true when the server row changed.
func (m *MemoryGuardService) handle(ctx context.Context, uuid string, ev memoryEvent) bool {
	srv, err := m.store.GetServerByUUID(uuid)
	if err != nil || srv == nil {
		return false
	}
	switch ev.Event {
	case MemoryEventWarning:
		if !m.once(ctx, uuid, "memory_warning_notified", memoryWarnNotifyEvery) {
			return false
		}
		m.notify(srv, "Server "+srv.Name+" is close to its memory limit",
			fmt.Sprintf("Memory reached %s of its limit, so the world was saved as a precaution. "+
				"If this keeps happening, give it more RAM or RAM headroom.", memPct(ev)))
		return false

	case MemoryEventCritical:
		action := srv.MemoryGuardAction
		if !models.ValidMemoryGuardAction(action) {
			action = models.MemoryGuardOff
		}
		taken := action
		// Only a running server. Anything else is already on its way down or
		// is being worked on (install, move), and a stop or restart would undo
		// or interrupt that.
		if action != models.MemoryGuardOff && srv.Status != "online" {
			taken = "none"
		}
		if taken == models.MemoryGuardStop || taken == models.MemoryGuardRestart {
			if err := m.power(ctx, srv, taken); err != nil {
				log.Printf("memory guard: %s on server %s failed: %v", taken, uuid, err)
				taken = "failed"
			}
		}
		log.Printf("memory guard: server %s held at %s of its memory limit, action %s (configured %s)", uuid, memPct(ev), taken, action)
		m.audit(srv.ID, ev, map[string]interface{}{"action": action, "taken": taken})
		acted := taken == models.MemoryGuardStop || taken == models.MemoryGuardRestart
		// The action runs every time; the owner hears about it once per window,
		// or a server that hits the wall after every restart floods the inbox.
		if !m.once(ctx, uuid, "memory_critical_notified", memoryCriticalNotifyEvery) {
			return acted
		}
		switch taken {
		case models.MemoryGuardStop:
			m.notify(srv, "Server "+srv.Name+" was stopped to save the world",
				fmt.Sprintf("Server %s was stopped to save the world: memory reached %s of its limit. "+
					"Give it more RAM or headroom.", srv.Name, memPct(ev)))
		case models.MemoryGuardRestart:
			m.notify(srv, "Server "+srv.Name+" was restarted to save the world",
				fmt.Sprintf("Server %s was restarted to save the world: memory reached %s of its limit. "+
					"Give it more RAM or headroom.", srv.Name, memPct(ev)))
		default:
			m.notify(srv, "Server "+srv.Name+" is about to run out of memory",
				fmt.Sprintf("Server %s has been at %s of its memory limit for 30 seconds and may be killed "+
					"without saving. Give it more RAM or headroom.", srv.Name, memPct(ev)))
		}
		return acted

	case MemoryEventOOM:
		at := time.Unix(ev.At, 0)
		if ev.At <= 0 {
			at = time.Now()
		}
		if err := m.store.SetServerLastCrash(srv.ID, models.CrashReasonOOMKilled, at); err != nil {
			log.Printf("memory guard: record OOM kill of %s: %v", uuid, err)
			return false
		}
		log.Printf("memory guard: server %s was OOM-killed", uuid)
		m.audit(srv.ID, ev, nil)
		if m.once(ctx, uuid, "oom_notified", memoryOOMNotifyEvery) {
			m.notify(srv, "Server "+srv.Name+" was killed: out of memory",
				fmt.Sprintf("Server %s was killed: out of memory. It needs more RAM or RAM headroom.", srv.Name))
		}
		return true
	}
	return false
}

// power stops or restarts the server the way the panel's power buttons do.
// desired_state goes first: the node reconciler reads it, and a stop it does
// not know about is undone by a restart.
func (m *MemoryGuardService) power(ctx context.Context, srv *models.Server, action string) error {
	node, err := m.store.GetNodeByID(srv.NodeID)
	if err != nil || node == nil {
		return fmt.Errorf("node %d: %v", srv.NodeID, err)
	}
	desired, status := "stopped", "stopping"
	if action == models.MemoryGuardRestart {
		desired, status = "online", "starting"
	}
	if err := m.store.UpdateServerDesiredState(srv.ID, desired); err != nil {
		return fmt.Errorf("desired_state: %w", err)
	}
	PublishDesiredState(ctx, m.redis, srv.UUID, desired)
	if err := m.store.UpdateServerStatus(srv.ID, status); err != nil {
		log.Printf("memory guard: status for %s: %v", srv.UUID, err)
	}
	return m.queue.SendCommand(ctx, node.Token, action, map[string]interface{}{"uuid": srv.UUID}, nil)
}

// once reports whether this is the first call for (uuid, what) within every.
// A Redis error answers true: a duplicate notification beats a silent one.
func (m *MemoryGuardService) once(ctx context.Context, uuid, what string, every time.Duration) bool {
	ok, err := m.redis.SetNX(ctx, "dylaris:server:"+uuid+":"+what, "1", every).Result()
	return ok || err != nil
}

func (m *MemoryGuardService) notify(srv *models.Server, title, body string) {
	if srv.OwnerID == "" {
		return
	}
	if _, err := m.store.InsertNotification(&models.Notification{
		UserID: srv.OwnerID,
		Type:   NotifyTypeServerMemory,
		Title:  title,
		Body:   body,
		Link:   fmt.Sprintf("/servers/%d", srv.ID),
	}); err != nil {
		log.Printf("memory guard: notify owner of server %d: %v", srv.ID, err)
	}
}

// audit mirrors handlers.LogServerAudit's gate: rows only where audit is on.
func (m *MemoryGuardService) audit(serverID int, ev memoryEvent, extra map[string]interface{}) {
	enabled, force, _, err := m.store.GetServerAuditState(serverID)
	if err != nil || (!enabled && !force) {
		return
	}
	meta := map[string]interface{}{"event": ev.Event}
	if ev.LimitMB > 0 {
		meta["usedMB"], meta["limitMB"] = ev.UsedMB, ev.LimitMB
	}
	for k, v := range extra {
		meta[k] = v
	}
	if err := m.store.InsertServerAudit(&models.ServerAuditEvent{ServerID: serverID, EventType: ServerAuditEventMemoryGuard, Metadata: meta}); err != nil {
		log.Printf("memory guard: audit for server %d: %v", serverID, err)
	}
}

func memPct(ev memoryEvent) string {
	if ev.LimitMB <= 0 {
		return "the limit"
	}
	return fmt.Sprintf("%d%% (%d of %d MB)", ev.UsedMB*100/ev.LimitMB, ev.UsedMB, ev.LimitMB)
}
