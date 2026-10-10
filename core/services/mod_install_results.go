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

// ModInstallResultService turns a node's report of a mod install into the row
// state the panel shows.
//
// Installing a mod is queued work, so the row was written before the node had
// done anything and was never revisited. A download that 404ed, a jar whose
// hash did not match and a successful install produced the same "installed"
// row, and the panel then offered to update a mod that was never there.
type ModInstallResultService struct {
	store  store.Store
	redis  *redis.Client
	leader LeaderChecker
	events *SystemEventsPublisher
}

// LeaderChecker is the slice of the leader election this service needs. Pub/Sub
// broadcasts to every subscriber, so without it each Core replica would apply
// the same report.
type LeaderChecker interface {
	IsLeader() bool
}

func NewModInstallResultService(st store.Store, rdb *redis.Client, leader LeaderChecker) *ModInstallResultService {
	return &ModInstallResultService{store: st, redis: rdb, leader: leader, events: NewSystemEventsPublisher(rdb)}
}

// Start consumes results until ctx is cancelled.
func (s *ModInstallResultService) Start(ctx context.Context) {
	if s.redis == nil {
		return
	}
	go s.consume(ctx)
}

type modInstallReport struct {
	InstallID string `json:"installId"`
	ServerID  int    `json:"serverId"`
	SubServer string `json:"subServer"`
	ProjectID string `json:"projectId"`
	Status    string `json:"status"`
	Message   string `json:"message"`
}

func (s *ModInstallResultService) consume(ctx context.Context) {
	pubsub := s.redis.PSubscribe(ctx, queue.ModResultsPattern)
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
			var rep modInstallReport
			if err := json.Unmarshal([]byte(msg.Payload), &rep); err != nil {
				log.Printf("mod install result: decode failed: %v", err)
				continue
			}
			s.apply(msg.Channel, rep)
		}
	}
}

// apply writes one report, after checking that the node that sent it is the
// node that hosts the server it is talking about.
//
// Pub/Sub carries no sender identity, so the channel name is the only
// attribution there is - the same reasoning as the backup channels, and the
// same check. Without it any node, a tenant-owned BYON machine included, could
// mark another tenant's mod install failed.
func (s *ModInstallResultService) apply(channel string, rep modInstallReport) {
	token, ok := queue.NodeTokenFromModChannel(channel)
	if !ok {
		log.Printf("mod install result: dropping a message on unattributable channel %q", channel)
		return
	}
	if rep.InstallID == "" || rep.ProjectID == "" {
		log.Printf("mod install result: dropping a message from node %q with no install or project id", token)
		return
	}
	status := rep.Status
	if status != models.ServerModInstalled && status != models.ServerModFailed {
		log.Printf("mod install result: dropping a message from node %q with status %q", token, status)
		return
	}
	srv, err := s.store.GetServerByID(rep.ServerID)
	if err != nil || srv == nil {
		log.Printf("mod install result: dropping a message from node %q: server %d could not be loaded: %v",
			token, rep.ServerID, err)
		return
	}
	node, err := s.store.GetNodeByID(srv.NodeID)
	if err != nil || node == nil {
		log.Printf("mod install result: dropping a message from node %q: node %d could not be loaded: %v",
			token, srv.NodeID, err)
		return
	}
	if node.Token != token {
		log.Printf("mod install result: DROPPED a report from node %q about server %d, which another node hosts",
			token, rep.ServerID)
		return
	}

	applied, err := s.store.SetServerModStatus(rep.ServerID, rep.SubServer, rep.ProjectID,
		rep.InstallID, status, rep.Message)
	if err != nil {
		log.Printf("mod install result: writing the status for server %d project %s failed: %v",
			rep.ServerID, rep.ProjectID, err)
		return
	}
	if applied {
		// The row changing is not enough: the panel only re-reads the list on this
		// event, so without it a finished install stayed "installing" on screen
		// until a reload. Leader-gated like the write, so it fires once.
		s.events.Publish(context.Background(), "server_mods.changed", map[string]interface{}{
			"serverId":  rep.ServerID,
			"projectId": rep.ProjectID,
			"status":    status,
		})
		return
	}
	// Not an error and not worth alarming about: the row has moved on to a
	// newer attempt, so this is a late answer about one that no longer
	// decides anything.
	log.Printf("mod install result: ignoring a late report for server %d project %s (attempt %s is no longer current)",
		rep.ServerID, rep.ProjectID, rep.InstallID)
}
