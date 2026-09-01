package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// sweepLease enforces a global cadence for a sweep that only needs to happen
// once across the deployment, no matter how many replicas are running.
//
// Two separate exclusions are needed, and getting only the first is the trap:
//
//   - while a round runs, no other replica may join it
//   - after a round SUCCEEDS, no replica may start another until the cadence
//     window reopens
//
// Holding a key for the duration of the round and deleting it at the end gives
// only the first. Replicas whose ticks are out of phase — which they always are
// after a rolling restart, because each starts its ticker when its own startup
// round finishes — then simply take turns, and the deployment still runs one
// scan per replica per window. The admission therefore does not end at DEL; on
// success it converts into a cooldown that blocks everyone.
//
// It is an efficiency gate, not a correctness one: the work behind it is
// idempotent, so a round run twice costs duplicated database work while a round
// never run loses a recovery. Every implementation therefore fails OPEN.
type sweepLease interface {
	// Acquire reports whether this replica should run the round now. The
	// returned finish must be called when the round ends and is never nil when
	// ok is true.
	//
	// finish(true) means the round completed, and starts the cadence cooldown.
	// finish(false) means it was cut short — a shutdown mid-round — and hands
	// the window back so another replica can cover it immediately. Both stay
	// safe to call after the caller's context is cancelled.
	//
	// The cooldown is shared state, so it deliberately outlives the replica
	// that set it: a holder that dies right after completing a round keeps
	// everyone else out for the rest of that window. That is the cost of making
	// the cadence global, and it is what makes the caller's worst-case handoff
	// a cadence plus a poll rather than a cadence
	// (delegatedFailureRecoveryWorstCaseHandoff records the accepted bound).
	Acquire(ctx context.Context) (finish func(completed bool), ok bool)
}

// unleased is the no-Redis deployment. There is no cross-replica state to
// consult, so every replica runs on its own cadence — which is what the sweep
// did before it was gated, and is exactly one runner on the Helm default of a
// single backend replica. The caller's local cadence guard is what keeps this
// from degenerating into a poll.
type unleased struct{}

func (unleased) Acquire(context.Context) (func(bool), bool) { return func(bool) {}, true }

// Every mutation compares the holder token before acting, so a replica can only
// extend, convert or drop the admission it still owns. Without that compare, a
// replica whose lease had already expired and been taken by someone else would
// renew, cool down or delete the new holder's key.
//
// Sent as plain EVAL rather than a cached script: at one call per renew
// interval the extra script bytes are irrelevant, and it keeps each lease
// mutation a single observable command with no NOSCRIPT fallback path.
const redisRenewSweepLeaseSource = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`

// Converting rather than deleting is what makes this a cadence gate. The key
// stays present, now holding the cooldown marker instead of a holder token, so
// the next replica to poll is refused until it expires.
const redisCooldownSweepLeaseSource = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
  return 1
end
return 0
`

const redisReleaseSweepLeaseSource = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`

// sweepCooldownMarker is what the key holds between rounds. Holder tokens are
// UUIDs, so this can never be mistaken for one.
const sweepCooldownMarker = "cooldown"

// finishTimeout bounds the compare-and-write on the shutdown path, where the
// caller's context is already cancelled. Failing here is not harmful — the key
// expires on its own — so it only decides how long shutdown waits.
const finishTimeout = 5 * time.Second

// redisSweepLease admits the first replica to claim the key, holds the
// admission for as long as the round runs, and converts it into a cadence
// cooldown when the round succeeds.
//
// A fixed TTL cannot cover the round: it has no deadline of its own, since its
// batch size bounds how many recoveries it dispatches and not how long the
// database takes to answer. So the admission is renewed on a timer while the
// round runs. The cooldown then bounds the other direction — how soon anyone
// may start the next round — which the renewal alone does not.
type redisSweepLease struct {
	client   *redis.Client
	key      string
	ttl      time.Duration
	renew    time.Duration
	cooldown time.Duration
}

func newRedisSweepLease(client *redis.Client, key string, ttl, renew, cooldown time.Duration) *redisSweepLease {
	return &redisSweepLease{client: client, key: key, ttl: ttl, renew: renew, cooldown: cooldown}
}

func (l *redisSweepLease) Acquire(ctx context.Context) (func(bool), bool) {
	// A fresh token per round: nothing here re-acquires an admission it already
	// holds, so reusing one across rounds would only make a stale renewal look
	// legitimate.
	token := uuid.NewString()
	acquired, err := l.client.SetNX(ctx, l.key, token, l.ttl).Result()
	if err != nil {
		// Fail open. Redis being unreachable must not silently switch off a
		// recovery path; the cost of guessing wrong in this direction is a
		// duplicate scan that finds the same rows already handled. There is no
		// admission to hold, so nothing to renew, cool down or release. The
		// caller's local cadence guard still bounds how often this happens.
		slog.Warn("sweep lease: redis unavailable, running this round unguarded",
			"key", l.key, "error", err)
		return func(bool) {}, true
	}
	if !acquired {
		// Either another replica is mid-round, or the cadence window this
		// round belongs to has already been covered.
		return nil, false
	}

	renewCtx, stopRenewing := context.WithCancel(ctx)
	renewalStopped := make(chan struct{})
	go func() {
		defer close(renewalStopped)
		l.keepAlive(renewCtx, token)
	}()

	return func(completed bool) {
		stopRenewing()
		<-renewalStopped
		// Detached from the caller's context so a shutdown still records the
		// outcome instead of leaving the key to expire on its own.
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		defer cancel()

		if !completed {
			// The round was cut short, so this window is NOT covered. Drop the
			// admission rather than cooling down, and let another replica take
			// it on its next poll.
			if err := l.client.Eval(finishCtx, redisReleaseSweepLeaseSource, []string{l.key}, token).Err(); err != nil && err != redis.Nil {
				slog.Warn("sweep lease: release failed; the round will be contested again when the key expires",
					"key", l.key, "error", err)
			}
			return
		}
		if err := l.client.Eval(finishCtx, redisCooldownSweepLeaseSource,
			[]string{l.key}, token, sweepCooldownMarker, l.cooldown.Milliseconds()).Err(); err != nil && err != redis.Nil {
			// The admission expires on its own, so the worst case is that the
			// next window opens early and one extra scan runs.
			slog.Warn("sweep lease: cooldown failed; another replica may repeat this round",
				"key", l.key, "error", err)
		}
	}, true
}

// keepAlive extends the admission until the round ends or the lease is lost.
func (l *redisSweepLease) keepAlive(ctx context.Context, token string) {
	ticker := time.NewTicker(l.renew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewed, err := l.client.Eval(ctx, redisRenewSweepLeaseSource, []string{l.key}, token, l.ttl.Milliseconds()).Int64()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// Transient: the admission is still ours until the key
				// expires, so keep trying on the next tick.
				slog.Warn("sweep lease: renewal failed, retrying", "key", l.key, "error", err)
				continue
			}
			if renewed == 0 {
				// The key is gone or now belongs to someone else. The round
				// keeps running — stopping it half-done would strand the work
				// it has already started, and the dispatch it performs is
				// idempotent — but it is no longer the single holder, and its
				// finish will no longer match the token.
				slog.Warn("sweep lease: lost mid-round; another replica may run this round concurrently",
					"key", l.key)
				return
			}
		}
	}
}

// delegatedFailureRecoveryLeaseKey is versioned so a future change to the round
// protocol cannot be confused by a key left behind by an older replica.
const delegatedFailureRecoveryLeaseKey = "multica:sweep-lease:v1:delegated-failure-recovery"
