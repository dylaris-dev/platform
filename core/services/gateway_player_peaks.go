package services

import (
	"context"
	"log"
	"time"

	"dylaris-core/pkg/leader"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

const (
	playerPeakSampleEvery = 60 * time.Second
	// playerPeakKeepDays is retention, not a limit: 13 months lets a month be
	// read against the same month last year, and a row is a few dozen bytes.
	playerPeakKeepDays = 400
)

// GatewayPlayerPeaksService records the daily peak of concurrent players per
// route-only account (Java, Bedrock and total, UTC days) from the per-address
// counts the edges publish. Leader-gated, and idle unless the gateway mode is
// known and local.
//
// A peak is a LOWER BOUND: a sample that is missing or stale (edge update,
// Core restart, leader change) writes nothing, and a peak shorter than the
// sample interval can be missed. Nothing is interpolated or carried over.
type GatewayPlayerPeaksService struct {
	store         store.Store
	redis         *redis.Client
	leader        leader.Election
	gatewayRemote func() (remote, known bool)
	now           func() time.Time
	players       func(context.Context) map[string]RoutePlayers
	done          chan struct{} // closed when the loop ends; tests only

	// Touched by the sampling goroutine only. written is what THIS process last
	// wrote per owner for day, so the steady state costs no write; it holds
	// only owners seen with players today and is dropped at midnight and with
	// the lease. GREATEST in SQL keeps the row right while it is cold.
	day     time.Time
	written map[string]store.PlayerPeak
}

func NewGatewayPlayerPeaksService(st store.Store, rdb *redis.Client) *GatewayPlayerPeaksService {
	return &GatewayPlayerPeaksService{
		store: st,
		redis: rdb,
		now:   time.Now,
		players: func(ctx context.Context) map[string]RoutePlayers {
			return GatewayPlayersByRoute(ctx, rdb)
		},
	}
}

// SetLeader wires the leader-election gate. Call once at boot.
func (s *GatewayPlayerPeaksService) SetLeader(l leader.Election) { s.leader = l }

// SetGatewayRemote wires the gateway provider mode: unless it is known and
// local, the sampler idles (a remote-mode Core runs no edges of its own).
func (s *GatewayPlayerPeaksService) SetGatewayRemote(fn func() (remote, known bool)) {
	s.gatewayRemote = fn
}

func (s *GatewayPlayerPeaksService) Start(ctx context.Context) {
	if s.redis == nil {
		return
	}
	go func() {
		if s.done != nil {
			defer close(s.done)
		}
		t := time.NewTicker(playerPeakSampleEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sample(ctx)
			}
		}
	}()
}

func (s *GatewayPlayerPeaksService) sample(ctx context.Context) {
	if s.leader != nil && !s.leader.IsLeader() {
		s.day, s.written = time.Time{}, nil
		return
	}
	if LocalGatewayErr(s.gatewayRemote) != nil {
		return
	}
	now := s.now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if !day.Equal(s.day) {
		s.day, s.written = day, map[string]store.PlayerPeak{}
		if n, err := s.store.PurgePlayerPeaks(day.AddDate(0, 0, -playerPeakKeepDays)); err != nil {
			log.Printf("gateway player peaks: purge before %s: %v", day.AddDate(0, 0, -playerPeakKeepDays).Format("2006-01-02"), err)
		} else if n > 0 {
			log.Printf("gateway player peaks: purged %d rows older than %d days", n, playerPeakKeepDays)
		}
	}

	counts := s.players(ctx)
	if len(counts) == 0 {
		return
	}
	routes, err := s.store.ListCoreLinkRoutes()
	if err != nil {
		log.Printf("gateway player peaks: list routes: %v", err)
		return
	}
	perOwner := map[string]store.PlayerPeak{}
	for _, rt := range routes {
		p, ok := counts[rt.Domain]
		// A suspended route is left out, as in the admin view's total.
		if !ok || rt.OwnerID == "" || rt.SuspendedAt != nil {
			continue
		}
		sum := perOwner[rt.OwnerID]
		sum.Java += p.Java
		sum.Bedrock += p.Bedrock
		perOwner[rt.OwnerID] = sum
	}
	for owner, p := range perOwner {
		p.Total = p.Java + p.Bedrock
		if p.Total == 0 {
			continue
		}
		last := s.written[owner]
		if p.Java <= last.Java && p.Bedrock <= last.Bedrock && p.Total <= last.Total {
			continue
		}
		if err := s.store.RaisePlayerPeak(owner, day, p.Java, p.Bedrock, p.Total); err != nil {
			log.Printf("gateway player peaks: raise %s on %s: %v", owner, day.Format("2006-01-02"), err)
			continue
		}
		s.written[owner] = store.PlayerPeak{Day: day, Java: max(last.Java, p.Java),
			Bedrock: max(last.Bedrock, p.Bedrock), Total: max(last.Total, p.Total)}
	}
}
