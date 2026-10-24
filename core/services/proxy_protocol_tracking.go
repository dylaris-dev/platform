package services

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Which installs were asked to configure the real-IP setting, and which
// sub-servers are known to expect the header.
//
// The node answers an install that carried the request twice: on its setup
// channel (Pub/Sub, immediate, lost when no Core listens at that moment) and
// in ProxyProtocolResultKey (durable for a day). Core remembers each request
// in proxyProtocolPendingKey until one of the two answers it; the sweep in
// SetupResultService reads the durable one, so a lost message can no longer
// leave a server switched on for a config that was never written.

// proxyProtocolPendingKey is a hash: server uuid -> pendingProxyInstall.
const proxyProtocolPendingKey = "proxy_protocol_default:pending"

// proxyProtocolNoAnswer is how long a request may stay unanswered before it is
// logged and dropped. An install that has not reported in that time crashed
// with the node; the switch is left as it is for the owner to judge.
const proxyProtocolNoAnswer = 6 * time.Hour

type pendingProxyInstall struct {
	Sub string `json:"sub"`
	At  int64  `json:"at"`
	// Default: this install's own setup switched the server on (the new-server
	// default). Only then does a "no" switch it off again. A server that was
	// already on was set up for the header before - a reinstall that failed on
	// a download error says nothing about its config, and switching it off
	// would refuse every player its config still expects through the header.
	Default bool `json:"default,omitempty"`
}

// ProxyProtocolResultKey holds the node's answer: "1" written, "0" not.
func ProxyProtocolResultKey(serverUUID string) string {
	return "dylaris:server:" + serverUUID + ":proxy_protocol_result"
}

// ProxyProtocolSubsKey is the set of sub-servers known to expect the header:
// a node reported writing (or finding) the setting there, or the owner turned
// the switch on while it was active. Switching the server to any other
// sub-server is refused while the switch is on. Losing it only refuses more.
func ProxyProtocolSubsKey(serverUUID string) string {
	return "dylaris:server:" + serverUUID + ":proxy_protocol_subs"
}

// TrackProxyProtocolInstall records that an install carrying the request is
// about to be sent. A stale answer from an earlier install is dropped first.
func TrackProxyProtocolInstall(ctx context.Context, rdb *redis.Client, serverUUID, sub string, isDefault bool) error {
	b, _ := json.Marshal(pendingProxyInstall{Sub: sub, At: time.Now().Unix(), Default: isDefault})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Del(ctx, ProxyProtocolResultKey(serverUUID)).Err(); err != nil {
		return err
	}
	return rdb.HSet(ctx, proxyProtocolPendingKey, serverUUID, string(b)).Err()
}

// ForgetProxyProtocolInstall drops the request and its answer: it has been
// acted on, or something more recent (the owner's own switch) supersedes it.
func ForgetProxyProtocolInstall(ctx context.Context, rdb *redis.Client, serverUUID string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.HDel(ctx, proxyProtocolPendingKey, serverUUID).Err(); err != nil {
		log.Printf("proxy-protocol: forget pending %s: %v", serverUUID, err)
	}
	if err := rdb.Del(ctx, ProxyProtocolResultKey(serverUUID)).Err(); err != nil {
		log.Printf("proxy-protocol: forget result %s: %v", serverUUID, err)
	}
}

// MarkProxyProtocolSubConfigured records that sub expects the header.
func MarkProxyProtocolSubConfigured(ctx context.Context, rdb *redis.Client, serverUUID, sub string) error {
	if sub == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return rdb.SAdd(ctx, ProxyProtocolSubsKey(serverUUID), sub).Err()
}

// ProxyProtocolSubConfigured reports whether sub is known to expect the header.
func ProxyProtocolSubConfigured(ctx context.Context, rdb *redis.Client, serverUUID, sub string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return rdb.SIsMember(ctx, ProxyProtocolSubsKey(serverUUID), sub).Result()
}

// sweepProxyProtocolResults answers every pending request whose durable answer
// is there, and drops the ones nobody answered for proxyProtocolNoAnswer.
func (s *SetupResultService) sweepProxyProtocolResults(ctx context.Context, now time.Time) {
	pending, err := s.redis.HGetAll(ctx, proxyProtocolPendingKey).Result()
	if err != nil {
		log.Printf("proxy-protocol sweep: %v", err)
		return
	}
	for uuid, raw := range pending {
		var p pendingProxyInstall
		_ = json.Unmarshal([]byte(raw), &p)
		res, err := s.redis.Get(ctx, ProxyProtocolResultKey(uuid)).Result()
		if errors.Is(err, redis.Nil) {
			if now.Sub(time.Unix(p.At, 0)) > proxyProtocolNoAnswer {
				// A switch the default turned on has nothing behind it until an
				// install confirms the config, so no answer counts as "no": off
				// is the direction that cannot lock players out. One the owner
				// had on before is theirs, and stays.
				if srv, serr := s.store.GetServerByUUID(uuid); p.Default && serr == nil && srv != nil {
					log.Printf("proxy-protocol sweep: server %q (%s) never answered the real-IP request it was switched on for; switching it back off", uuid, p.Sub)
					s.applyProxyProtocolReport(srv, p.Sub, false)
					continue
				}
				log.Printf("proxy-protocol sweep: server %q (%s) never answered the real-IP request; the switch is left as it is", uuid, p.Sub)
				s.redis.HDel(ctx, proxyProtocolPendingKey, uuid)
			}
			continue
		}
		if err != nil {
			log.Printf("proxy-protocol sweep: %s: %v", uuid, err)
			continue
		}
		srv, serr := s.store.GetServerByUUID(uuid)
		if serr != nil || srv == nil {
			// Deleted meanwhile: nothing left to switch.
			ForgetProxyProtocolInstall(ctx, s.redis, uuid)
			continue
		}
		s.applyProxyProtocolReport(srv, p.Sub, res == "1")
	}
}

// runProxyProtocolSweep sweeps once at start and then every minute, on the
// leader only. Running on every tick covers a leadership change: the new
// leader picks the requests up on its next minute.
func (s *SetupResultService) runProxyProtocolSweep(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		if s.leader == nil || s.leader.IsLeader() {
			s.sweepProxyProtocolResults(ctx, time.Now())
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
