package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	redismock "github.com/go-redis/redismock/v9"
)

type stubSweepLease struct {
	grant    bool
	attempts atomic.Int32
	held     atomic.Int32
}

func (l *stubSweepLease) Acquire(context.Context) (func(), bool) {
	l.attempts.Add(1)
	if !l.grant {
		return nil, false
	}
	l.held.Add(1)
	return func() { l.held.Add(-1) }, true
}

// expiringLease models the guarantee the Redis implementation provides —
// exclusive admission that survives exactly as long as it is renewed — without
// depending on wall-clock expiry inside Redis. It is what lets the concurrency
// tests below be deterministic; the redismock tests cover the wire protocol
// that produces this behaviour against a real server.
type expiringLease struct {
	mu     sync.Mutex
	holder string
	// renewals counts successful token-compared renewals.
	renewals atomic.Int32
}

func (l *expiringLease) acquire(who string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != "" {
		return nil, false
	}
	l.holder = who
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		// Token compare: only the holder may release.
		if l.holder == who {
			l.holder = ""
		}
	}, true
}

// expire drops the admission the way a missed renewal would.
func (l *expiringLease) expire() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holder = ""
}

type replicaLease struct {
	shared *expiringLease
	name   string
}

func (r replicaLease) Acquire(context.Context) (func(), bool) { return r.shared.acquire(r.name) }

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

// With renewal, the TTL bounds how long the admission survives WITHOUT one, not
// how long a round may take. It therefore has to leave room for a missed
// renewal to be retried, and stay short enough that a replica which died
// holding the lease frees it well inside one interval.
func TestDelegatedFailureRecoveryLeaseTimings(t *testing.T) {
	if delegatedFailureRecoveryLeaseRenew > delegatedFailureRecoveryLeaseTTL/2 {
		t.Fatalf("renew %s leaves no room to retry inside a %s TTL",
			delegatedFailureRecoveryLeaseRenew, delegatedFailureRecoveryLeaseTTL)
	}
	if delegatedFailureRecoveryLeaseTTL >= delegatedFailureRecoverySweepInterval {
		t.Fatalf("a dead holder would suppress the next round: TTL %s, interval %s",
			delegatedFailureRecoveryLeaseTTL, delegatedFailureRecoverySweepInterval)
	}
}

// The must-fix from review: a round has no deadline of its own, so a fixed TTL
// could expire mid-round and let a second replica re-run the sweeper's most
// expensive query exactly when the database is already slow. Holding the
// admission for the whole round is what closes that window.
func TestSlowRoundKeepsASecondReplicaOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shared := &expiringLease{}

	roundStarted := make(chan struct{})
	finishRound := make(chan struct{})
	var firstRounds atomic.Int32
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		runLeasedSweep(ctx, time.Hour, replicaLease{shared: shared, name: "replica-a"}, func() {
			if firstRounds.Add(1) == 1 {
				close(roundStarted)
			}
			<-finishRound
		})
	}()
	<-roundStarted

	// While replica A is still inside its round, replica B ticks.
	var secondRounds atomic.Int32
	secondCtx, stopSecond := context.WithCancel(ctx)
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		runLeasedSweep(secondCtx, 5*time.Millisecond, replicaLease{shared: shared, name: "replica-b"}, func() {
			secondRounds.Add(1)
		})
	}()
	time.Sleep(100 * time.Millisecond)
	if got := secondRounds.Load(); got != 0 {
		t.Fatalf("second replica ran %d concurrent rounds during a slow round, want 0", got)
	}

	// Once the slow round ends the admission is released, not left to expire,
	// so the next tick can take it immediately.
	close(finishRound)
	deadline := time.After(2 * time.Second)
	for secondRounds.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("released admission was never handed to the waiting replica")
		case <-time.After(time.Millisecond):
		}
	}
	stopSecond()
	<-secondDone
	cancel()
	<-firstDone
}

// A replica that loses its admission mid-round (a renewal that never landed)
// must not delete or extend whatever the next holder wrote.
func TestReleaseAfterLosingTheLeaseDoesNotEvictTheNewHolder(t *testing.T) {
	shared := &expiringLease{}
	releaseA, ok := shared.acquire("replica-a")
	if !ok {
		t.Fatal("first replica could not acquire")
	}
	shared.expire()
	if _, ok := shared.acquire("replica-b"); !ok {
		t.Fatal("second replica could not take the expired admission")
	}

	releaseA()
	if _, ok := shared.acquire("replica-c"); ok {
		t.Fatal("a stale release evicted the new holder")
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
		runLeasedSweep(ctx, time.Hour, lease, func() { swept <- struct{}{} })
	}()

	select {
	case <-swept:
	case <-time.After(2 * time.Second):
		t.Fatal("startup round never ran; the sweep waited for the first tick")
	}
	cancel()
	<-done
	if extra := len(swept); extra != 0 {
		t.Fatalf("%d extra rounds ran before the first tick", extra)
	}
	if held := lease.held.Load(); held != 0 {
		t.Fatalf("%d admissions still held after the sweep stopped", held)
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
		runLeasedSweep(ctx, 5*time.Millisecond, lease, func() { rounds.Add(1) })
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
		runLeasedSweep(ctx, 5*time.Millisecond, lease, func() {})
		close(stopped)
	}()
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("leased sweep did not stop with its context")
	}
}

// captureToken records the holder token the lease generated, so the renew and
// release expectations can assert the SAME token is compared.
func captureToken(into *string) func(expected, actual []interface{}) error {
	return func(_, actual []interface{}) error {
		for _, arg := range actual {
			s, ok := arg.(string)
			if ok && len(s) == 36 {
				*into = s
				return nil
			}
		}
		return fmt.Errorf("no holder token in %v", actual)
	}
}

// The Redis half of the guarantee tested above: while the round runs the lease
// is renewed with a token compare, and when it ends the lease is released with
// the same compare rather than being left to expire.
func TestRedisSweepLeaseRenewsDuringTheRoundAndReleasesAfter(t *testing.T) {
	client, mock := redismock.NewClientMock()
	const ttl = 300 * time.Millisecond
	const renew = 10 * time.Millisecond
	lease := newRedisSweepLease(client, "test:lease", ttl, renew)

	var token string
	mock.CustomMatch(captureToken(&token)).ExpectSetNX("test:lease", "", ttl).SetVal(true)

	renewed := make(chan struct{}, 16)
	for range 3 {
		mock.CustomMatch(func(_, actual []interface{}) error {
			if !containsString(actual, token) {
				return fmt.Errorf("renewal did not compare the holder token %q: %v", token, actual)
			}
			select {
			case renewed <- struct{}{}:
			default:
			}
			return nil
		}).ExpectEval(redisRenewSweepLeaseSource, []string{"test:lease"}, "", ttl.Milliseconds()).SetVal(int64(1))
	}
	released := make(chan struct{}, 1)
	mock.CustomMatch(func(_, actual []interface{}) error {
		if !containsString(actual, token) {
			return fmt.Errorf("release did not compare the holder token %q: %v", token, actual)
		}
		released <- struct{}{}
		return nil
	}).ExpectEval(redisReleaseSweepLeaseSource, []string{"test:lease"}, "").SetVal(int64(1))

	release, ok := lease.Acquire(context.Background())
	if !ok {
		t.Fatal("claiming replica was not admitted")
	}
	if token == "" {
		t.Fatal("no holder token was written")
	}
	// Stay inside the round past several renew intervals, the way a slow round
	// would, then end it.
	for range 3 {
		select {
		case <-renewed:
		case <-time.After(2 * time.Second):
			t.Fatal("the admission was not renewed while the round was still running")
		}
	}
	release()
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("the admission was not released when the round ended")
	}
}

func containsString(args []interface{}, want string) bool {
	for _, arg := range args {
		if s, ok := arg.(string); ok && s == want {
			return true
		}
	}
	return false
}

func TestRedisSweepLeaseRefusesARoundAnotherReplicaHolds(t *testing.T) {
	client, mock := redismock.NewClientMock()
	lease := newRedisSweepLease(client, "test:lease", time.Minute, 20*time.Second)
	mock.CustomMatch(func(_, _ []interface{}) error { return nil }).
		ExpectSetNX("test:lease", "", time.Minute).SetVal(false)

	if release, ok := lease.Acquire(context.Background()); ok {
		release()
		t.Fatal("replica was admitted to a round another replica already holds")
	}
}

// Failing closed here would let a Redis outage silently switch off crash
// recovery. Running the round twice only repeats work that is already
// idempotent; never running it strands an obligation.
func TestRedisSweepLeaseFailsOpen(t *testing.T) {
	client, mock := redismock.NewClientMock()
	lease := newRedisSweepLease(client, "test:lease", time.Minute, 20*time.Second)
	mock.CustomMatch(func(_, _ []interface{}) error { return nil }).
		ExpectSetNX("test:lease", "", time.Minute).SetErr(errors.New("redis is down"))

	release, ok := lease.Acquire(context.Background())
	if !ok {
		t.Fatal("an unreachable Redis switched off the recovery round")
	}
	// There is no admission to give back, so release must be a safe no-op
	// rather than a compare-and-delete against a key this replica never wrote.
	release()
}

// No Redis is the Helm default. It must keep the pre-change behaviour of every
// replica running every round, at the new cadence.
func TestUnleasedAlwaysRuns(t *testing.T) {
	release, ok := (unleased{}).Acquire(context.Background())
	if !ok {
		t.Fatal("the no-Redis deployment stopped running the recovery sweep")
	}
	release()
}
