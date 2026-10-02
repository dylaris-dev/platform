package services

import (
	"context"
	"encoding/json"
	"log"

	"dylaris-core/models"
	"dylaris-core/store"
	"dylaris-pkg/queue"

	"github.com/redis/go-redis/v9"
)

// ServerAuditEventSFTPChanges is the server audit row for what one SFTP session
// changed on a server.
const ServerAuditEventSFTPChanges = "sftp.changes"

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

// apply writes one record after checking that the node that sent it hosts the
// server it names, and is one of ours: a customer's machine serves no SFTP, so
// a record from one is not a record of anything.
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
	if err != nil || node == nil || node.Token != token || node.IsExternal() {
		log.Printf("sftp audit: DROPPED a record from node %q about server %s, which it does not serve SFTP for", token, rec.ServerUUID)
		return
	}
	enabled, force, _, err := s.store.GetServerAuditState(srv.ID)
	if err != nil || (!enabled && !force) {
		return
	}
	if len(rec.Paths) > maxSFTPAuditPaths {
		rec.Paths, rec.Truncated = rec.Paths[:maxSFTPAuditPaths], true
	}
	ev := &models.ServerAuditEvent{
		ServerID:  srv.ID,
		EventType: ServerAuditEventSFTPChanges,
		IPAddress: rec.RemoteIP,
		UserAgent: "sftp",
		Metadata: map[string]interface{}{
			"username": rec.Username, "writes": rec.Writes, "deletes": rec.Deletes,
			"renames": rec.Renames, "mkdirs": rec.Mkdirs, "paths": rec.Paths, "truncated": rec.Truncated,
		},
	}
	if u, err := s.store.GetUserByUsername(rec.Username); err == nil && u != nil {
		id := u.ID
		ev.ActorUserID = &id
	}
	if err := s.store.InsertServerAudit(ev); err != nil {
		log.Printf("sftp audit: insert for server %d: %v", srv.ID, err)
	}
}
