package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// sweepLease admits one replica per round to a sweep that only needs to happen
// once across the deployment. It is an efficiency gate, not a correctness one:
// the work behind it is idempotent, so a round run twice costs duplicated
// database work while a round never run loses a recovery. Every implementation
// therefore fails OPEN.
type sweepLease interface {
	// Acquire reports whether this replica should run the round. ttl bounds
	// how long the admission is held, so a replica that dies mid-round cannot
	// suppress later rounds.
	Acquire(ctx context.Context, ttl time.Duration) bool
}

// unleased is the no-Redis deployment: every replica runs every round, which is
// what the sweep did before it was gated. A single-replica install — the Helm
// default — is already exactly one runner, and a multi-replica install without
// Redis keeps the amplification it has today at the new, lower cadence.
type unleased struct{}

func (unleased) Acquire(context.Context, time.Duration) bool { return true }

// redisSweepLease admits the first replica to claim the key each round.
//
// SET NX PX is the whole protocol: there is no renewal and no release. The
// admission simply expires, so the next round is contested from scratch and a
// replica that crashes holding it delays at most one round. That is the right
// shape for a rare, bounded, idempotent sweep — a renewed lease would add a
// failure mode (a live holder that has stopped sweeping) with nothing to gain.
type redisSweepLease struct {
	client *redis.Client
	key    string
	// holder identifies this process only for operators reading the key; the
	// protocol never compares it, because nothing here releases the lease.
	holder string
}

func newRedisSweepLease(client *redis.Client, key string) *redisSweepLease {
	return &redisSweepLease{client: client, key: key, holder: uuid.NewString()}
}

func (l *redisSweepLease) Acquire(ctx context.Context, ttl time.Duration) bool {
	acquired, err := l.client.SetNX(ctx, l.key, l.holder, ttl).Result()
	if err != nil {
		// Fail open. Redis being unreachable must not silently switch off a
		// recovery path; the cost of guessing wrong in this direction is a
		// duplicate scan that finds the same rows already handled.
		slog.Warn("sweep lease: redis unavailable, running this round unguarded",
			"key", l.key, "error", err)
		return true
	}
	return acquired
}

// delegatedFailureRecoveryLeaseKey is versioned so a future change to the round
// protocol cannot be confused by a key left behind by an older replica.
const delegatedFailureRecoveryLeaseKey = "multica:sweep-lease:v1:delegated-failure-recovery"
