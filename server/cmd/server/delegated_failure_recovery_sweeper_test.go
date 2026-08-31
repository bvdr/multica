package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	redismock "github.com/go-redis/redismock/v9"
)

type stubSweepLease struct {
	grant    bool
	attempts atomic.Int32
	lastTTL  atomic.Int64
}

func (l *stubSweepLease) Acquire(_ context.Context, ttl time.Duration) bool {
	l.attempts.Add(1)
	l.lastTTL.Store(int64(ttl))
	return l.grant
}

// The recovery outbox is a crash backstop, not a liveness signal. Leaving it in
// the 30-second loop is what made a scan that usually finds nothing the most
// frequent caller of the most expensive query in the sweeper.
func TestDelegatedFailureRecoveryRunsOutsideTheLivenessLoop(t *testing.T) {
	if delegatedFailureRecoverySweepInterval != 5*time.Minute {
		t.Fatalf("recovery sweep interval = %s, want 5m", delegatedFailureRecoverySweepInterval)
	}

	source, err := os.ReadFile("runtime_sweeper.go")
	if err != nil {
		t.Fatalf("read runtime_sweeper.go: %v", err)
	}
	start := strings.Index(string(source), "func runRuntimeSweeper(")
	end := strings.Index(string(source), "func runRuntimeGCSweeper(")
	if start < 0 || end <= start {
		t.Fatal("could not isolate runtime sweeper loops")
	}
	if strings.Contains(string(source[start:end]), "sweepPendingDelegatedFailureRecoveries(") {
		t.Fatal("delegated failure recovery is still invoked from the 30-second liveness loop")
	}
}

// The per-replica call rate is the whole point of the change, so pin it rather
// than leaving it to be re-derived from a constant someone may retune.
func TestDelegatedFailureRecoveryDailyCallRate(t *testing.T) {
	const wantCallsPerDay = 288
	got := int(24 * time.Hour / delegatedFailureRecoverySweepInterval)
	if got != wantCallsPerDay {
		t.Fatalf("recovery sweep rate = %d/day/replica, want %d/day/replica", got, wantCallsPerDay)
	}
}

// A lease that outlives its round would let a replica that died holding it
// suppress the next one; a lease far shorter than the round would let a second
// replica start the same work while the first is still in it.
func TestDelegatedFailureRecoveryLeaseTTLFitsInsideTheInterval(t *testing.T) {
	if delegatedFailureRecoveryLeaseTTL >= delegatedFailureRecoverySweepInterval {
		t.Fatalf("lease TTL %s must be shorter than the %s interval, or a dead holder suppresses the next round",
			delegatedFailureRecoveryLeaseTTL, delegatedFailureRecoverySweepInterval)
	}
	if delegatedFailureRecoveryLeaseTTL < delegatedFailureRecoverySweepInterval/2 {
		t.Fatalf("lease TTL %s is short enough that a slow round could run twice concurrently", delegatedFailureRecoveryLeaseTTL)
	}
}

// The startup scan is the one that matters: a process that just started is the
// aftermath of the exit this sweep repairs. Waiting a full interval for it
// would make the new cadence strictly worse than the old one.
func TestLeasedSweepRunsAtStartupBeforeTheFirstTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lease := &stubSweepLease{grant: true}
	swept := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// An interval no test can wait out, so only the startup round can fire.
		runLeasedSweep(ctx, time.Hour, delegatedFailureRecoveryLeaseTTL, lease, func() {
			swept <- struct{}{}
		})
	}()

	select {
	case <-swept:
	case <-time.After(2 * time.Second):
		t.Fatal("startup round never ran; the sweep waited for the first tick")
	}
	if got := lease.lastTTL.Load(); time.Duration(got) != delegatedFailureRecoveryLeaseTTL {
		t.Fatalf("startup round took the lease for %s, want %s", time.Duration(got), delegatedFailureRecoveryLeaseTTL)
	}
	cancel()
	<-done
	if extra := len(swept); extra != 0 {
		t.Fatalf("%d extra rounds ran before the first tick", extra)
	}
}

// Losing the lease must skip the work, not just the logging.
func TestLeasedSweepSkipsRoundsItDoesNotWin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lease := &stubSweepLease{grant: false}
	var rounds atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLeasedSweep(ctx, 5*time.Millisecond, delegatedFailureRecoveryLeaseTTL, lease, func() {
			rounds.Add(1)
		})
	}()

	deadline := time.After(time.Second)
	for lease.attempts.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d lease attempts observed", lease.attempts.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	if got := rounds.Load(); got != 0 {
		t.Fatalf("ran %d rounds without holding the lease, want 0", got)
	}
}

func TestLeasedSweepStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	lease := &stubSweepLease{grant: true}
	stopped := make(chan struct{})
	go func() {
		runLeasedSweep(ctx, 5*time.Millisecond, delegatedFailureRecoveryLeaseTTL, lease, func() {})
		close(stopped)
	}()
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("leased sweep did not stop with its context")
	}
}

func TestRedisSweepLeaseAdmitsExactlyTheClaimingReplica(t *testing.T) {
	client, mock := redismock.NewClientMock()
	lease := newRedisSweepLease(client, "test:lease")

	mock.ExpectSetNX("test:lease", lease.holder, time.Minute).SetVal(true)
	if !lease.Acquire(context.Background(), time.Minute) {
		t.Fatal("claiming replica was not admitted")
	}
	mock.ExpectSetNX("test:lease", lease.holder, time.Minute).SetVal(false)
	if lease.Acquire(context.Background(), time.Minute) {
		t.Fatal("replica was admitted to a round another replica already holds")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("redis expectations: %v", err)
	}
}

// Failing closed here would let a Redis outage silently switch off crash
// recovery. Running the round twice only repeats work that is already
// idempotent; never running it strands an obligation.
func TestRedisSweepLeaseFailsOpen(t *testing.T) {
	client, mock := redismock.NewClientMock()
	lease := newRedisSweepLease(client, "test:lease")
	mock.ExpectSetNX("test:lease", lease.holder, time.Minute).SetErr(errors.New("redis is down"))
	if !lease.Acquire(context.Background(), time.Minute) {
		t.Fatal("an unreachable Redis switched off the recovery round")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("redis expectations: %v", err)
	}
}

// No Redis is the Helm default. It must keep the pre-change behaviour of every
// replica running every round, at the new cadence.
func TestUnleasedAlwaysRuns(t *testing.T) {
	if !(unleased{}).Acquire(context.Background(), time.Minute) {
		t.Fatal("the no-Redis deployment stopped running the recovery sweep")
	}
}
