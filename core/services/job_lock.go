package services

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// refreshOrReclaimJobLock keeps a long-running job's cluster-wide lock alive.
//
// The lock is taken once with SETNX and then has to survive the whole job,
// which for a database or storage migration is hours. A bare EXPIRE cannot do
// that: EXPIRE on a missing key is a no-op that reports 0 and creates nothing,
// so once the key is gone the refresh is silently dead for the rest of the run.
//
// The key does go missing in normal operation. Redis is a coordination bus
// here, deliberately run without persistence (save "", appendonly no), so a
// restart wipes it while the job keeps running in this process. Everything else
// about the job survives that, because the job record is re-persisted with SET;
// only the lock was refreshed with EXPIRE, and so only the lock stayed gone -
// leaving nothing to stop a second migration against the same target.
//
// Reclaiming must never steal a lock another job legitimately holds, so the GET
// and the write are a single Lua step, the same atomicity the leader lease uses
// in core/pkg/leader:
//
//	missing        -> take it back; this job is still running, so it is ours
//	ours           -> extend it
//	someone else's -> return false; this job has been superseded
func refreshOrReclaimJobLock(ctx context.Context, rdb *redis.Client, key, owner string, ttl time.Duration) (bool, error) {
	const script = `
		local v = redis.call('get', KEYS[1])
		if v == false then
			redis.call('set', KEYS[1], ARGV[1], 'PX', ARGV[2])
			return 1
		elseif v == ARGV[1] then
			redis.call('pexpire', KEYS[1], ARGV[2])
			return 1
		end
		return 0
	`
	res, err := rdb.Eval(ctx, script, []string{key}, owner, ttl.Milliseconds()).Result()
	if err != nil {
		return false, err
	}
	n, _ := res.(int64)
	return n == 1, nil
}

// releaseJobLock deletes key only while it still holds owner, in one Lua step.
// A job that was superseded after a Redis restart must not drop the lock (or
// the cancel flag, whose value is also a job id) of the job that replaced it.
func releaseJobLock(ctx context.Context, rdb *redis.Client, key, owner string) error {
	const script = `
		if redis.call('get', KEYS[1]) == ARGV[1] then
			return redis.call('del', KEYS[1])
		end
		return 0
	`
	return rdb.Eval(ctx, script, []string{key}, owner).Err()
}
