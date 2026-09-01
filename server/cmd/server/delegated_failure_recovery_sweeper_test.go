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
	grant     bool
	attempts  atomic.Int32
	held      atomic.Int32
	completed atomic.Int32
	abandoned atomic.Int32
}

func (l *stubSweepLease) Acquire(context.Context) (func(bool), bool) {
	l.attempts.Add(1)
	if !l.grant {
		return nil, false
	}
	l.held.Add(1)
	return func(completed bool) {
		l.held.Add(-1)
		if completed {
			l.completed.Add(1)
		} else {
			l.abandoned.Add(1)
		}
	}, true
}

// cadenceLease models the guarantee the Redis implementation provides: an
// admission that excludes other replicas for the round, and then for the rest
// of the cadence window once the round succeeds. It is what lets the
// multi-replica tests below be deterministic; the redismock tests cover the
// wire protocol that produces this behaviour against a real server.
type cadenceLease struct {
	mu       sync.Mutex
	cooldown time.Duration
	// holder is the replica currently mid-round, empty between rounds.
	holder string
	// coolUntil blocks every replica until the cadence window reopens.
	coolUntil time.Time
	rounds    atomic.Int32
}

func (l *cadenceLease) acquire(who string) (func(bool), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != "" || time.Now().Before(l.coolUntil) {
		return nil, false
	}
	l.holder = who
	l.rounds.Add(1)
	return func(completed bool) {
		l.mu.Lock()
		defer l.mu.Unlock()
		// Token compare: only the holder may finish its own round.
		if l.holder != who {
			return
		}
		l.holder = ""
		if completed {
			l.coolUntil = time.Now().Add(l.cooldown)
		}
	}, true
}

// expire drops the admission the way a missed renewal would.
func (l *cadenceLease) expire() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holder = ""
}

type replicaLease struct {
	shared *cadenceLease
	name   string
}

func (r replicaLease) Acquire(context.Context) (func(bool), bool) { return r.shared.acquire(r.name) }

// The recovery outbox is a crash backstop, not a liveness signal. Leaving it in
// the 30-second loop is what made a scan that usually finds nothing the most
// frequent caller of the most expensive query in the sweeper.
func TestDelegatedFailureRecoveryRunsOutsideTheLivenessLoop(t *testing.T) {
	if delegatedFailureRecoverySweepCadence != 5*time.Minute {
		t.Fatalf("recovery sweep cadence = %s, want 5m", delegatedFailureRecoverySweepCadence)
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

// The saving is a GLOBAL rate, so pin it as one, and pin the poll as finer than
// the cadence — ticking at the cadence would let each replica's phase decide
// who runs and how long an unclaimed window stays unclaimed.
func TestDelegatedFailureRecoveryGlobalCallRate(t *testing.T) {
	const wantScansPerDay = 288
	got := int(24 * time.Hour / delegatedFailureRecoverySweepCadence)
	if got != wantScansPerDay {
		t.Fatalf("recovery scan rate = %d/day for the whole deployment, want %d/day", got, wantScansPerDay)
	}
	if delegatedFailureRecoveryPollInterval >= delegatedFailureRecoverySweepCadence {
		t.Fatalf("poll %s must be finer than the %s cadence",
			delegatedFailureRecoveryPollInterval, delegatedFailureRecoverySweepCadence)
	}
}

// With renewal, the TTL bounds how long the admission survives WITHOUT one, not
// how long a round may take. It has to leave room for a missed renewal to be
// retried, and stay short enough that a replica which died holding the lease
// frees it well inside one cadence window.
func TestDelegatedFailureRecoveryLeaseTimings(t *testing.T) {
	if delegatedFailureRecoveryLeaseRenew > delegatedFailureRecoveryLeaseTTL/2 {
		t.Fatalf("renew %s leaves no room to retry inside a %s TTL",
			delegatedFailureRecoveryLeaseRenew, delegatedFailureRecoveryLeaseTTL)
	}
	if delegatedFailureRecoveryLeaseTTL >= delegatedFailureRecoverySweepCadence {
		t.Fatalf("a dead holder would suppress the next window: TTL %s, cadence %s",
			delegatedFailureRecoveryLeaseTTL, delegatedFailureRecoverySweepCadence)
	}
}

// The bound is six minutes, not the five the cadence suggests, and that is
// accepted rather than papered over (MUL-6883). Pin the composition so that
// moving either input has to move the recorded decision with it: a holder that
// dies straight after a round leaves a cadence-long cooldown behind, and its
// successor only notices the reopening on its next poll.
func TestWorstCaseRecoveryHandoffIsTheConfirmedBound(t *testing.T) {
	const confirmed = 6 * time.Minute
	if delegatedFailureRecoveryWorstCaseHandoff != confirmed {
		t.Fatalf("worst-case handoff = %s, but %s is what was confirmed as acceptable",
			delegatedFailureRecoveryWorstCaseHandoff, confirmed)
	}
	if got := delegatedFailureRecoverySweepCadence + delegatedFailureRecoveryPollInterval; got != delegatedFailureRecoveryWorstCaseHandoff {
		t.Fatalf("cadence %s + poll %s = %s, which no longer matches the documented bound %s",
			delegatedFailureRecoverySweepCadence, delegatedFailureRecoveryPollInterval,
			got, delegatedFailureRecoveryWorstCaseHandoff)
	}
}

// What makes that bound a cadence plus a POLL rather than a cadence plus a
// cadence: the successor is watching finer than the rate it is enforcing. A
// replica ticking at the cadence would miss a reopening it fell a hair short of
// and wait out another full window — which is the shape this measures, by
// setting the successor's phase just before the cooldown begins.
func TestDeadHolderHandsOverWithinOnePollOfTheCooldown(t *testing.T) {
	const (
		cooldown = 400 * time.Millisecond
		poll     = 60 * time.Millisecond
		// Scheduler slack only. The bound under test is cooldown + poll; a
		// cadence-ticking successor would need up to cooldown + cooldown.
		slack = 120 * time.Millisecond
	)

	for attempt := range 3 {
		shared := &cadenceLease{cooldown: cooldown}
		finishA, ok := (replicaLease{shared: shared, name: "replica-a"}).Acquire(context.Background())
		if !ok {
			t.Fatalf("attempt %d: the first replica could not acquire", attempt)
		}

		ctx, cancel := context.WithCancel(context.Background())
		swept := make(chan time.Time, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			runLeasedSweep(ctx, poll, cooldown, replicaLease{shared: shared, name: "replica-b"}, func() {
				select {
				case swept <- time.Now():
				default:
				}
			})
		}()
		// B's startup attempt is refused while A still holds the round, so B's
		// poll phase is fixed just before the cooldown starts.
		time.Sleep(2 * time.Millisecond)
		completedAt := time.Now()
		// A completes and is then never heard from again: its cooldown is
		// shared state and outlives it.
		finishA(true)

		var sweptAt time.Time
		select {
		case sweptAt = <-swept:
		case <-time.After(cooldown + poll + slack + time.Second):
			cancel()
			<-done
			t.Fatalf("attempt %d: the window was never picked up after its holder stopped", attempt)
		}
		cancel()
		<-done

		delay := sweptAt.Sub(completedAt)
		if delay < cooldown {
			t.Fatalf("attempt %d: the next round started %s after the last one, inside the %s cooldown",
				attempt, delay, cooldown)
		}
		if delay > cooldown+poll+slack {
			t.Fatalf("attempt %d: handover took %s, past the cooldown plus one poll (%s)",
				attempt, delay, cooldown+poll)
		}
	}
}

// The point of the gate: the number of scans the deployment performs must be
// set by the cadence, not by how many replicas are running. Deleting the key at
// the end of a round would satisfy the concurrency test below and still fail
// this one, because out-of-phase replicas would take turns.
func TestScanRateDoesNotGrowWithReplicaCount(t *testing.T) {
	const (
		cadence = 60 * time.Millisecond
		poll    = 5 * time.Millisecond
		run     = 420 * time.Millisecond
	)
	countRounds := func(replicas int) int32 {
		shared := &cadenceLease{cooldown: cadence}
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for i := range replicas {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Stagger startups so no two replicas share a tick phase —
				// the rolling-restart shape, and the one a delete-on-finish
				// protocol silently doubles.
				time.Sleep(time.Duration(i) * 17 * time.Millisecond)
				runLeasedSweep(ctx, poll, cadence, replicaLease{shared: shared, name: fmt.Sprint(i)}, func() {})
			}()
		}
		time.Sleep(run)
		cancel()
		wg.Wait()
		return shared.rounds.Load()
	}

	one := countRounds(1)
	many := countRounds(4)
	if one == 0 {
		t.Fatal("a single replica ran no rounds at all")
	}
	// Four replicas may claim a window marginally sooner than one can, but they
	// must not multiply the work. Anything near 4x is the turn-taking bug.
	if many > one+2 {
		t.Fatalf("4 replicas ran %d rounds vs %d for a single replica: the scan rate is scaling with replica count", many, one)
	}
}

// A rolling restart is the case where phases are guaranteed to differ: each
// replica runs its startup round and only then starts its ticker. One effective
// startup scan must cover the deployment.
func TestRollingRestartProducesOneStartupScan(t *testing.T) {
	shared := &cadenceLease{cooldown: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 20 * time.Millisecond)
			// A poll interval no test can wait out, so only the startup round
			// of each replica can fire.
			runLeasedSweep(ctx, time.Hour, time.Hour, replicaLease{shared: shared, name: fmt.Sprint(i)}, func() {})
		}()
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	wg.Wait()

	if got := shared.rounds.Load(); got != 1 {
		t.Fatalf("a rolling restart of 5 replicas produced %d startup scans, want 1", got)
	}
}

// A round has no deadline of its own, so a fixed TTL could expire mid-round and
// let a second replica re-run the sweeper's most expensive query exactly when
// the database is already slow. Holding the admission for the whole round
// closes that window; the cooldown then keeps the next replica out afterwards.
func TestSlowRoundExcludesOtherReplicasDuringAndAfter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shared := &cadenceLease{cooldown: time.Hour}

	roundStarted := make(chan struct{})
	finishRound := make(chan struct{})
	var firstRounds atomic.Int32
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		runLeasedSweep(ctx, time.Hour, time.Hour, replicaLease{shared: shared, name: "replica-a"}, func() {
			if firstRounds.Add(1) == 1 {
				close(roundStarted)
			}
			<-finishRound
		})
	}()
	<-roundStarted

	var secondRounds atomic.Int32
	secondCtx, stopSecond := context.WithCancel(ctx)
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		runLeasedSweep(secondCtx, 5*time.Millisecond, time.Hour, replicaLease{shared: shared, name: "replica-b"}, func() {
			secondRounds.Add(1)
		})
	}()
	time.Sleep(100 * time.Millisecond)
	if got := secondRounds.Load(); got != 0 {
		t.Fatalf("second replica ran %d concurrent rounds during a slow round, want 0", got)
	}

	// Finishing must NOT hand the window straight over: the cadence window this
	// round covered is done, and B has to wait for the next one.
	close(finishRound)
	time.Sleep(100 * time.Millisecond)
	if got := secondRounds.Load(); got != 0 {
		t.Fatalf("second replica ran %d rounds inside a window the first replica had already covered, want 0", got)
	}
	stopSecond()
	<-secondDone
	cancel()
	<-firstDone
}

// The other direction: a round cut short by shutdown did NOT cover its window,
// so it must be handed back rather than cooled down.
func TestAbandonedRoundIsHandedBackImmediately(t *testing.T) {
	shared := &cadenceLease{cooldown: time.Hour}
	lease := replicaLease{shared: shared, name: "replica-a"}

	finish, ok := lease.Acquire(context.Background())
	if !ok {
		t.Fatal("first replica could not acquire")
	}
	finish(false)

	if _, ok := (replicaLease{shared: shared, name: "replica-b"}).Acquire(context.Background()); !ok {
		t.Fatal("an abandoned round was cooled down instead of handed back")
	}
}

// A replica that loses its admission mid-round (a renewal that never landed)
// must not cool down or delete whatever the next holder wrote.
func TestFinishAfterLosingTheLeaseDoesNotAffectTheNewHolder(t *testing.T) {
	shared := &cadenceLease{cooldown: time.Hour}
	finishA, ok := (replicaLease{shared: shared, name: "replica-a"}).Acquire(context.Background())
	if !ok {
		t.Fatal("first replica could not acquire")
	}
	shared.expire()
	if _, ok := (replicaLease{shared: shared, name: "replica-b"}).Acquire(context.Background()); !ok {
		t.Fatal("second replica could not take the expired admission")
	}

	finishA(true)
	if _, ok := (replicaLease{shared: shared, name: "replica-c"}).Acquire(context.Background()); ok {
		t.Fatal("a stale finish evicted the new holder")
	}
}

// The startup scan is the one that matters: a process that just started is the
// aftermath of the exit this sweep repairs. Waiting a full poll for it would
// make the new cadence strictly worse than the old one.
func TestLeasedSweepRunsAtStartupBeforeTheFirstTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lease := &stubSweepLease{grant: true}
	swept := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLeasedSweep(ctx, time.Hour, time.Hour, lease, func() { swept <- struct{}{} })
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
	if lease.completed.Load() != 1 {
		t.Fatalf("startup round finished as completed %d times, want 1", lease.completed.Load())
	}
}

// The local floor is what bounds the no-Redis deployment, where there is no
// shared cooldown to consult. Without it a poll finer than the cadence would
// turn fail-open into a hot loop against the database.
func TestLocalCadenceFloorHoldsWithoutALease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var rounds atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLeasedSweep(ctx, time.Millisecond, time.Hour, unleased{}, func() { rounds.Add(1) })
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
	if got := rounds.Load(); got != 1 {
		t.Fatalf("an ungated replica ran %d rounds in one cadence window, want 1", got)
	}
}

// Losing the lease must skip the work, not just the logging.
func TestLeasedSweepSkipsWindowsItDoesNotWin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lease := &stubSweepLease{grant: false}
	var rounds atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLeasedSweep(ctx, 5*time.Millisecond, time.Hour, lease, func() { rounds.Add(1) })
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
		runLeasedSweep(ctx, 5*time.Millisecond, time.Hour, lease, func() {})
		close(stopped)
	}()
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("leased sweep did not stop with its context")
	}
}

func captureToken(into *string) func(expected, actual []interface{}) error {
	return func(_, actual []interface{}) error {
		for _, arg := range actual {
			if s, ok := arg.(string); ok && len(s) == 36 {
				*into = s
				return nil
			}
		}
		return fmt.Errorf("no holder token in %v", actual)
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

// The Redis half of the guarantee: while the round runs the lease is renewed
// with a token compare, and when it succeeds the key is CONVERTED to a cooldown
// rather than deleted — deleting is what would let the next replica start
// another round inside the same window.
func TestRedisSweepLeaseRenewsDuringTheRoundAndCoolsDownAfter(t *testing.T) {
	client, mock := redismock.NewClientMock()
	const (
		ttl      = 300 * time.Millisecond
		renew    = 10 * time.Millisecond
		cooldown = 5 * time.Second
	)
	lease := newRedisSweepLease(client, "test:lease", ttl, renew, cooldown)

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
	cooled := make(chan struct{}, 1)
	mock.CustomMatch(func(_, actual []interface{}) error {
		if !containsString(actual, token) {
			return fmt.Errorf("cooldown did not compare the holder token %q: %v", token, actual)
		}
		if !containsString(actual, sweepCooldownMarker) {
			return fmt.Errorf("cooldown did not write the marker: %v", actual)
		}
		cooled <- struct{}{}
		return nil
	}).ExpectEval(redisCooldownSweepLeaseSource, []string{"test:lease"}, "", sweepCooldownMarker, cooldown.Milliseconds()).SetVal(int64(1))

	finish, ok := lease.Acquire(context.Background())
	if !ok {
		t.Fatal("claiming replica was not admitted")
	}
	if token == "" {
		t.Fatal("no holder token was written")
	}
	for range 3 {
		select {
		case <-renewed:
		case <-time.After(2 * time.Second):
			t.Fatal("the admission was not renewed while the round was still running")
		}
	}
	finish(true)
	select {
	case <-cooled:
	case <-time.After(2 * time.Second):
		t.Fatal("a completed round did not convert its admission into a cadence cooldown")
	}
}

// A round cut short releases instead, so the window is contested again at once.
func TestRedisSweepLeaseReleasesAnAbandonedRound(t *testing.T) {
	client, mock := redismock.NewClientMock()
	lease := newRedisSweepLease(client, "test:lease", time.Minute, 30*time.Second, 5*time.Minute)

	var token string
	mock.CustomMatch(captureToken(&token)).ExpectSetNX("test:lease", "", time.Minute).SetVal(true)
	released := make(chan struct{}, 1)
	mock.CustomMatch(func(_, actual []interface{}) error {
		if !containsString(actual, token) {
			return fmt.Errorf("release did not compare the holder token %q: %v", token, actual)
		}
		released <- struct{}{}
		return nil
	}).ExpectEval(redisReleaseSweepLeaseSource, []string{"test:lease"}, "").SetVal(int64(1))

	finish, ok := lease.Acquire(context.Background())
	if !ok {
		t.Fatal("claiming replica was not admitted")
	}
	finish(false)
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("an abandoned round did not release its admission")
	}
}

func TestRedisSweepLeaseRefusesACoveredWindow(t *testing.T) {
	client, mock := redismock.NewClientMock()
	lease := newRedisSweepLease(client, "test:lease", time.Minute, 20*time.Second, 5*time.Minute)
	mock.CustomMatch(func(_, _ []interface{}) error { return nil }).
		ExpectSetNX("test:lease", "", time.Minute).SetVal(false)

	if finish, ok := lease.Acquire(context.Background()); ok {
		finish(false)
		t.Fatal("replica was admitted to a window another replica already covered")
	}
}

// Failing closed here would let a Redis outage silently switch off crash
// recovery. Running the round twice only repeats work that is already
// idempotent; never running it strands an obligation.
func TestRedisSweepLeaseFailsOpen(t *testing.T) {
	client, mock := redismock.NewClientMock()
	lease := newRedisSweepLease(client, "test:lease", time.Minute, 20*time.Second, 5*time.Minute)
	mock.CustomMatch(func(_, _ []interface{}) error { return nil }).
		ExpectSetNX("test:lease", "", time.Minute).SetErr(errors.New("redis is down"))

	finish, ok := lease.Acquire(context.Background())
	if !ok {
		t.Fatal("an unreachable Redis switched off the recovery round")
	}
	// There is no admission to convert or drop, so finish must be a safe no-op
	// rather than a compare-and-write against a key this replica never wrote.
	finish(true)
}

// No Redis is the Helm default. It must keep the pre-change behaviour of every
// replica running on its own cadence.
func TestUnleasedAlwaysRuns(t *testing.T) {
	finish, ok := (unleased{}).Acquire(context.Background())
	if !ok {
		t.Fatal("the no-Redis deployment stopped running the recovery sweep")
	}
	finish(true)
}
