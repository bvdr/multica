package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// sweepLease admits one replica per round to a sweep that only needs to happen
// once across the deployment, and keeps that admission for as long as the round
// actually runs.
//
// It is an efficiency gate, not a correctness one: the work behind it is
// idempotent, so a round run twice costs duplicated database work while a round
// never run loses a recovery. Every implementation therefore fails OPEN.
type sweepLease interface {
	// Acquire reports whether this replica should run the round. The returned
	// release must be called when the round ends and is never nil when ok is
	// true; it stays safe to call after the caller's context is cancelled,
	// which is what lets a shutdown hand the next round over immediately.
	Acquire(ctx context.Context) (release func(), ok bool)
}

// unleased is the no-Redis deployment: every replica runs every round, which is
// what the sweep did before it was gated. A single-replica install — the Helm
// default — is already exactly one runner, and a multi-replica install without
// Redis keeps the amplification it has today at the new, lower cadence.
type unleased struct{}

func (unleased) Acquire(context.Context) (func(), bool) { return func() {}, true }

// Renew and release compare the holder token before acting, so a replica can
// only extend or drop the admission it still owns. Without that compare, a
// replica whose lease had already expired and been taken by someone else would
// renew or delete the new holder's key.
//
// Sent as plain EVAL rather than a cached script: at one renewal per renew
// interval the extra script bytes are irrelevant, and it keeps each lease
// mutation a single observable command with no NOSCRIPT fallback path.
const redisRenewSweepLeaseSource = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`

const redisReleaseSweepLeaseSource = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`

// releaseTimeout bounds the compare-and-delete on the shutdown path, where the
// caller's context is already cancelled. Failing to release is not harmful —
// the key expires on its own — so this only decides how long shutdown waits
// before giving up on handing the round over early.
const releaseTimeout = 5 * time.Second

// redisSweepLease admits the first replica to claim the key each round and
// holds the admission for the whole round.
//
// A fixed TTL cannot do that. The round has no deadline of its own: its batch
// size bounds how many recoveries it dispatches, not how long the database
// takes to answer, so a slow round can outlive any TTL picked in advance —
// and precisely when the database is already struggling, a second replica
// would then enter and re-run the sweeper's most expensive query. The lease is
// therefore renewed on a timer while the round runs, and released when it ends
// so the next round is contested immediately rather than after a timeout.
//
// Every mutation compares a per-round holder token, so a replica that lost its
// lease (a renewal that arrived too late, a pause long enough for the key to
// expire) can neither extend nor delete whatever the new holder wrote.
type redisSweepLease struct {
	client *redis.Client
	key    string
	ttl    time.Duration
	renew  time.Duration
}

func newRedisSweepLease(client *redis.Client, key string, ttl, renew time.Duration) *redisSweepLease {
	return &redisSweepLease{client: client, key: key, ttl: ttl, renew: renew}
}

func (l *redisSweepLease) Acquire(ctx context.Context) (func(), bool) {
	// A fresh token per round: nothing here re-acquires an admission it already
	// holds, so reusing one across rounds would only make a stale renewal look
	// legitimate.
	token := uuid.NewString()
	acquired, err := l.client.SetNX(ctx, l.key, token, l.ttl).Result()
	if err != nil {
		// Fail open. Redis being unreachable must not silently switch off a
		// recovery path; the cost of guessing wrong in this direction is a
		// duplicate scan that finds the same rows already handled. There is no
		// admission to hold, so nothing to renew or release either.
		slog.Warn("sweep lease: redis unavailable, running this round unguarded",
			"key", l.key, "error", err)
		return func() {}, true
	}
	if !acquired {
		return nil, false
	}

	renewCtx, stopRenewing := context.WithCancel(ctx)
	renewalStopped := make(chan struct{})
	go func() {
		defer close(renewalStopped)
		l.keepAlive(renewCtx, token)
	}()

	return func() {
		stopRenewing()
		<-renewalStopped
		// Detached from the caller's context so a shutdown still hands the
		// round over instead of leaving the key to expire on its own.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		if err := l.client.Eval(releaseCtx, redisReleaseSweepLeaseSource, []string{l.key}, token).Err(); err != nil && err != redis.Nil {
			slog.Warn("sweep lease: release failed; the round will be contested again when the key expires",
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
				// idempotent — but it is no longer the single holder.
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
