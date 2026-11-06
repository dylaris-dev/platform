package services

import (
	"context"
	"encoding/json"
	"log"
	"net"

	"dylaris-core/models"
	"dylaris-core/store"
	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"
)

// ServerAuditEventSFTPChanges is the server audit row for what one SFTP session
// changed on a server.
const ServerAuditEventSFTPChanges = "sftp.changes"

// ServerAuditEventBeamChanges is the same for a change made through Beam.
const ServerAuditEventBeamChanges = "beam.changes"

// maxSFTPAuditPaths bounds what one record may put in the audit trail, whatever
// the node sent.
const maxSFTPAuditPaths = 50

// SFTPAuditService writes the nodes' SFTP audit records into the server audit
// trail. Leader-gated like the other result consumers: Pub/Sub reaches every
// Core, and one row per record is wanted.
type SFTPAuditService struct {
	store  store.Store
	redis  *redis.Client
	leader LeaderChecker
}

func NewSFTPAuditService(st store.Store, rdb *redis.Client, leader LeaderChecker) *SFTPAuditService {
	return &SFTPAuditService{store: st, redis: rdb, leader: leader}
}

func (s *SFTPAuditService) Start(ctx context.Context) {
	if s.redis == nil {
		return
	}
	go func() {
		pubsub := s.redis.PSubscribe(ctx, queue.SFTPAuditPattern)
		defer pubsub.Close()
		ch := pubsub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				if s.leader != nil && !s.leader.IsLeader() {
					continue
				}
				var rec queue.SFTPAuditRecord
				if err := json.Unmarshal([]byte(msg.Payload), &rec); err != nil {
					continue
				}
				s.apply(msg.Channel, rec)
			}
		}
	}()
}

// auditIP turns what a node sent into something the INET column accepts, or
// "" (stored as NULL). Older nodes send a Beam peer as "ip:port",
// which Postgres refuses, and one bad field must not cost the whole row.
func auditIP(s string) string {
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	if net.ParseIP(s) != nil {
		return s
	}
	if _, _, err := net.ParseCIDR(s); err == nil {
		return s
	}
	return ""
}

// apply writes one record after checking that the node that sent it hosts the
// server it names. An SFTP record must also come from one of ours: a customer's
// machine serves no SFTP, so such a record is not a record of anything. It does
// serve Beam - the only file path it has - so its Beam records are kept.
func (s *SFTPAuditService) apply(channel string, rec queue.SFTPAuditRecord) {
	token, ok := queue.NodeTokenFromSFTPAuditChannel(channel)
	if !ok || rec.ServerUUID == "" || rec.Username == "" {
		return
	}
	srv, err := s.store.GetServerByUUID(rec.ServerUUID)
	if err != nil || srv == nil {
		return
	}
	node, err := s.store.GetNodeByID(srv.NodeID)
	if err != nil || node == nil || node.Token != token || (node.IsExternal() && rec.Via != "beam") {
		log.Printf("sftp audit: DROPPED a record from node %q about server %s, which it does not serve file access for", token, rec.ServerUUID)
		return
	}
	enabled, force, _, err := s.store.GetServerAuditState(srv.ID)
	if err != nil || (!enabled && !force) {
		return
	}
	if len(rec.Paths) > maxSFTPAuditPaths {
		rec.Paths, rec.Truncated = rec.Paths[:maxSFTPAuditPaths], true
	}
	eventType, agent := ServerAuditEventSFTPChanges, "sftp"
	if rec.Via == "beam" {
		eventType, agent = ServerAuditEventBeamChanges, "beam"
	}
	ev := &models.ServerAuditEvent{
		ServerID:  srv.ID,
		EventType: eventType,
		IPAddress: auditIP(rec.RemoteIP),
		UserAgent: agent,
		Metadata: map[string]interface{}{
			"username": rec.Username, "writes": rec.Writes, "deletes": rec.Deletes,
			"renames": rec.Renames, "mkdirs": rec.Mkdirs, "paths": rec.Paths, "truncated": rec.Truncated,
		},
	}
	// A customer's machine checks the Beam ticket itself, so the name in its
	// record is its own claim: kept as the node's report, never resolved to a
	// platform user it could otherwise put on any change.
	if node.IsExternal() {
		ev.Metadata["reported_by"] = "external-node"
	} else if u, err := s.store.GetUserByUsername(rec.Username); err == nil && u != nil {
		id := u.ID
		ev.ActorUserID = &id
	}
	if err := s.store.InsertServerAudit(ev); err != nil {
		log.Printf("sftp audit: insert for server %d: %v", srv.ID, err)
	}
}
