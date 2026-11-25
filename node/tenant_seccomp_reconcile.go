package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"
)

// seccompRecreatePause spaces the recreates of ReconcileSeccomp. Each one is a
// world save and a world load; 30s lets one server's load get through most of
// its burst before the next one saves, so a node with many servers rolls them
// at a steady pace instead of all at once.
const seccompRecreatePause = 30 * time.Second

// onTenantSeccomp reports whether a container runs the current tenant profile.
// Docker keeps HostConfig.SecurityOpt as it was sent (moby parseSecurityOpt
// only reads it), so the option is compared whole. Any later change to the
// profile therefore reads as stale too, and the next node start recreates every
// running server again to roll it out.
func onTenantSeccomp(securityOpt []string) bool {
	return slices.Contains(securityOpt, "seccomp="+tenantSeccompProfile)
}

// seccompRecreateHold names why a running server is not to be recreated now,
// "" when it is. A server held here keeps its old profile until its next start
// or the next node start.
func seccompRecreateHold(ctx context.Context, rdb *redis.Client, storage *StorageManager, uuid string) string {
	if desired, err := rdb.Get(ctx, desiredStateKey(uuid)).Result(); err == nil && desired != "online" {
		return "meant to be stopped (desired state " + desired + ")"
	}
	if backupsInFlight.has("server:" + uuid) {
		return "a backup is running"
	}
	if restoreServersInFlight.has(uuid) {
		return "a restore is running"
	}
	for _, k := range []string{"setup:", "reinstall:", "version_update:", "copy_sub:", "migrate_storage:", softwareKey("")} {
		if commandsInFlight.has(k + uuid) {
			return "a " + k[:len(k)-1] + " command is running"
		}
	}
	if restartBlocked(ctx, rdb, storage, uuid) {
		return "held (protected status, node_busy, disk full, stopped by itself or a staged move)"
	}
	return ""
}

// ReconcileSeccomp brings every running server of this node onto the tenant
// seccomp profile (R54-06). The profile is set at container create, so a server
// created before it kept a profile that lets it move files into another
// server's quota project. Owner decision: they are restarted, players on them
// included. A stopped one is not touched here; PowerAction "start" recreates it.
//
// Startup only, one server at a time, through the same graceful stop and
// recreate a panel restart takes.
func (dm *DockerManager) ReconcileSeccomp(rdb *redis.Client) {
	infoCtx, cancel := context.WithTimeout(dm.ctx, 10*time.Second)
	info, err := dm.cli.Info(infoCtx)
	cancel()
	if err != nil || !daemonHasSeccomp(info.SecurityOptions) {
		return
	}
	running, err := dm.ListRunningMCContainers()
	if err != nil {
		log.Printf("ReconcileSeccomp: failed to list containers: %v", err)
		return
	}
	var recreated, skipped, failed int
	for _, mc := range running {
		release, ok := waitServerPower(dm.ctx, mc.UUID)
		if !ok {
			return
		}
		outcome, err := dm.reconcileSeccompOne(rdb, mc.UUID)
		release()
		switch {
		case err != nil:
			failed++
			log.Printf("ReconcileSeccomp: %s failed: %v", mc.ContainerName, err)
		case outcome == "recreated":
			recreated++
			log.Printf("ReconcileSeccomp: %s recreated with the tenant seccomp profile", mc.ContainerName)
		case outcome != "":
			skipped++
			log.Printf("ReconcileSeccomp: %s skipped, %s", mc.ContainerName, outcome)
		}
		if outcome == "recreated" || err != nil {
			select {
			case <-dm.ctx.Done():
				return
			case <-time.After(seccompRecreatePause):
			}
		}
	}
	if recreated+skipped+failed > 0 {
		log.Printf("ReconcileSeccomp: %d running servers, %d recreated, %d skipped, %d failed, %d already current",
			len(running), recreated, skipped, failed, len(running)-recreated-skipped-failed)
	}
}

// reconcileSeccompOne runs under the server's power lock. Its outcome is
// "recreated", a skip reason, or "" for a container already on the profile.
func (dm *DockerManager) reconcileSeccompOne(rdb *redis.Client, uuid string) (string, error) {
	ctx := dm.ctx
	info, err := dm.cli.ContainerInspect(ctx, "mc_"+uuid)
	if err != nil {
		return "", fmt.Errorf("inspect: %w", err)
	}
	if info.HostConfig != nil && onTenantSeccomp(info.HostConfig.SecurityOpt) {
		return "", nil
	}
	// The list is from before the wait for the lock. A server stopped since is
	// recreated by its next start (PowerAction "start", or a panel start).
	if info.State == nil || !info.State.Running {
		return "no longer running, its next start recreates it", nil
	}
	if hold := seccompRecreateHold(ctx, rdb, dm.storageMgr, uuid); hold != "" {
		return hold, nil
	}

	statusKey := fmt.Sprintf("dylaris:server:%s:status", uuid)
	// Kill takes no lock: one that lands during the restart leaves the server
	// stopped, as in ReconcileRedisEnv.
	stops, _ := lastStopIssued.Load(uuid)
	leaveStopped := func() bool {
		if now, _ := lastStopIssued.Load(uuid); now != stops {
			return true
		}
		desired, err := rdb.Get(ctx, desiredStateKey(uuid)).Result()
		return err == nil && desired != "online"
	}

	// The panel restart's steps (main.go "restart"), without clearRunBlockers:
	// this is not a user's retry.
	log.Printf("ReconcileSeccomp: mc_%s predates the tenant seccomp profile, restarting", uuid)
	if err := gracefulStop(rdb, uuid, dm); err != nil {
		rdb.Set(ctx, statusKey, statusAfterFailedStop(dm, uuid), 30*time.Second)
		return "", fmt.Errorf("stop for the restart: %w", err)
	}
	rdb.Set(ctx, statusKey, "restarting", 30*time.Second)
	rdb.Del(ctx, fmt.Sprintf("dylaris:server:%s:stop-requested", uuid))
	time.Sleep(2 * time.Second)
	defer startGuard(uuid, func() bool {
		if !leaveStopped() {
			return false
		}
		log.Printf("ReconcileSeccomp: mc_%s left stopped, a stop or kill came in during its restart", uuid)
		rdb.Set(ctx, statusKey, "stopped", 30*time.Second)
		return true
	})()
	if err := dm.RestartContainer(uuid); errors.Is(err, errStartAbandoned) {
		return "recreated", nil
	} else if err != nil {
		rdb.Set(ctx, statusKey, "stopped", 30*time.Second)
		return "", fmt.Errorf("restart: %w", err)
	}
	rdb.Set(ctx, statusKey, "starting", 30*time.Second)
	return "recreated", nil
}
