package rotation_test

import (
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Pensara/internal/contract"
	"github.com/OxyHQ/Pensara/internal/rotation"
)

// clock is a hand-advanced clock. A breaker's whole behaviour is "after a
// while, try again", and a test that slept through it would be slow, flaky, and
// unable to assert what happens one millisecond either side of the cooldown.
type testClock struct {
	mutex sync.Mutex
	at    time.Time
}

func newClock() *testClock { return &testClock{at: time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)} }

func (c *testClock) now() time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.at
}

func (c *testClock) advance(by time.Duration) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.at = c.at.Add(by)
}

const deployment = contract.DeploymentID("dep_a")

var testPolicy = rotation.Policy{
	FailuresToOpen:   3,
	Cooldown:         5 * time.Second,
	MaxCooldown:      20 * time.Second,
	SuccessesToClose: 1,
	ScoreWeight:      0.5,
}

func admit(t *testing.T, registry *rotation.Registry, id contract.DeploymentID) *rotation.Permit {
	t.Helper()
	permit, ok := registry.Admit(id)
	if !ok {
		t.Fatalf("%s was refused, and this step expects it to be admitted", id)
	}
	return permit
}

func refuse(t *testing.T, registry *rotation.Registry, id contract.DeploymentID) {
	t.Helper()
	if _, ok := registry.Admit(id); ok {
		t.Fatalf("%s was admitted, and this step expects it to be refused", id)
	}
}

func fail(t *testing.T, registry *rotation.Registry, id contract.DeploymentID, times int) {
	t.Helper()
	for range times {
		admit(t, registry, id).Failed()
	}
}

/* -------------------------------------------------------------------------- */
/*  Opening                                                                   */
/* -------------------------------------------------------------------------- */

// TestABreakerOpensOnConsecutiveAttributableFailures, with the control that one
// failure short of the threshold still admits — otherwise "it refused" would be
// what a breaker that opens on the first failure also reports.
func TestABreakerOpensOnConsecutiveAttributableFailures(t *testing.T) {
	clock := newClock()
	registry := rotation.NewRegistry(testPolicy, clock.now)

	fail(t, registry, deployment, testPolicy.FailuresToOpen-1)
	permit, ok := registry.Admit(deployment)
	if !ok {
		t.Fatalf("the breaker opened after %d failures, one short of its threshold", testPolicy.FailuresToOpen-1)
	}
	permit.Failed()

	refuse(t, registry, deployment)
	if state := registry.Project([]contract.DeploymentID{deployment})[0].State; state != rotation.StateOpen {
		t.Errorf("after %d failures the breaker is %q", testPolicy.FailuresToOpen, state)
	}
}

// TestACustomerFaultNeverTripsABreaker is the load-bearing distinction in this
// package. A request the provider cannot express fails identically everywhere,
// so counting it against a deployment would let one customer's malformed
// traffic take a healthy route out of rotation for everyone else.
func TestACustomerFaultNeverTripsABreaker(t *testing.T) {
	clock := newClock()
	registry := rotation.NewRegistry(testPolicy, clock.now)

	for range testPolicy.FailuresToOpen * 5 {
		admit(t, registry, deployment).NotAttributable()
	}

	if _, ok := registry.Admit(deployment); !ok {
		t.Fatalf("%d unattributable failures took the deployment out of rotation", testPolicy.FailuresToOpen*5)
	}
	projected := registry.Project([]contract.DeploymentID{deployment})[0]
	if projected.State != rotation.StateClosed {
		t.Errorf("the breaker is %q after failures that were not its fault", projected.State)
	}
	if projected.ConsecutiveFailures != 0 {
		t.Errorf("the breaker counted %d failures that were not attributable to it", projected.ConsecutiveFailures)
	}
	if projected.Score != 1 {
		t.Errorf("the health score moved to %v on failures that say nothing about this deployment", projected.Score)
	}
}

// TestASuccessEndsTheFailureRun: the threshold is consecutive failures, so a
// deployment that fails intermittently under the threshold stays in rotation
// rather than accumulating its way out over a day.
func TestASuccessEndsTheFailureRun(t *testing.T) {
	clock := newClock()
	registry := rotation.NewRegistry(testPolicy, clock.now)

	for range 5 {
		fail(t, registry, deployment, testPolicy.FailuresToOpen-1)
		admit(t, registry, deployment).Succeeded()
	}
	if _, ok := registry.Admit(deployment); !ok {
		t.Fatal("a deployment that never failed twice in a row was taken out of rotation")
	}
}

/* -------------------------------------------------------------------------- */
/*  Probing back in                                                           */
/* -------------------------------------------------------------------------- */

// TestAnOpenBreakerProbesBackInAfterItsCooldown is how a deployment returns:
// one real request, not a synthetic one — a synthetic probe proves the provider
// answers some other request than the one it is failing, and Pensara would be
// paying for it.
func TestAnOpenBreakerProbesBackInAfterItsCooldown(t *testing.T) {
	clock := newClock()
	registry := rotation.NewRegistry(testPolicy, clock.now)
	fail(t, registry, deployment, testPolicy.FailuresToOpen)

	// One millisecond short of the cooldown it is still refusing, so the
	// admission below is the clock and not an unconditional retry.
	clock.advance(testPolicy.Cooldown - time.Millisecond)
	refuse(t, registry, deployment)

	clock.advance(time.Millisecond)
	permit := admit(t, registry, deployment)
	if !permit.Trial() {
		t.Error("the first request after a cooldown is not marked as the trial the deployment's return turns on")
	}
	if state := registry.Project([]contract.DeploymentID{deployment})[0].State; state != rotation.StateHalfOpen {
		t.Errorf("the breaker is %q while its trial is in flight", state)
	}

	permit.Succeeded()
	projected := registry.Project([]contract.DeploymentID{deployment})[0]
	if projected.State != rotation.StateClosed {
		t.Errorf("after a successful trial the breaker is %q", projected.State)
	}
	if projected.ProbesAt != nil {
		t.Error("a closed breaker still advertises a next probe time")
	}
}

// TestHalfOpenAdmitsExactlyOneTrial: everything that arrives the moment a
// cooldown expires would be a thundering herd onto the provider that has just
// stopped failing, which is the failure a breaker exists to prevent.
func TestHalfOpenAdmitsExactlyOneTrial(t *testing.T) {
	clock := newClock()
	registry := rotation.NewRegistry(testPolicy, clock.now)
	fail(t, registry, deployment, testPolicy.FailuresToOpen)
	clock.advance(testPolicy.Cooldown)

	first := admit(t, registry, deployment)
	refuse(t, registry, deployment)
	refuse(t, registry, deployment)

	// The slot is released when the trial reports, and not before.
	first.NotAttributable()
	second := admit(t, registry, deployment)
	if !second.Trial() {
		t.Error("the request after a released trial slot is not itself a trial; the breaker closed on a result it never got")
	}
}

// TestAFailedTrialReopensWithALongerCooldown: a provider that is still down
// must not be probed at the same rate forever, and a provider that recovers
// must not wait a day.
func TestAFailedTrialReopensWithALongerCooldown(t *testing.T) {
	clock := newClock()
	registry := rotation.NewRegistry(testPolicy, clock.now)
	fail(t, registry, deployment, testPolicy.FailuresToOpen)

	clock.advance(testPolicy.Cooldown)
	admit(t, registry, deployment).Failed()

	// The first cooldown has elapsed again and it is still refusing: the second
	// wait is longer than the first.
	clock.advance(testPolicy.Cooldown)
	refuse(t, registry, deployment)

	clock.advance(testPolicy.Cooldown)
	admit(t, registry, deployment).Failed()

	// And it is capped, so a long outage does not push the next probe past any
	// useful horizon.
	for range 10 {
		clock.advance(testPolicy.MaxCooldown)
		admit(t, registry, deployment).Failed()
	}
	clock.advance(testPolicy.MaxCooldown)
	admit(t, registry, deployment).Succeeded()
}

// TestReportingAPermitTwiceCountsOnce: a caller that reports on the happy path
// and again in a deferred cleanup must not double-count its way into opening a
// breaker.
func TestReportingAPermitTwiceCountsOnce(t *testing.T) {
	clock := newClock()
	registry := rotation.NewRegistry(testPolicy, clock.now)

	for range testPolicy.FailuresToOpen {
		permit := admit(t, registry, deployment)
		permit.NotAttributable()
		permit.Failed()
	}
	if _, ok := registry.Admit(deployment); !ok {
		t.Fatal("a second report on an already-reported permit counted against the breaker")
	}
}

/* -------------------------------------------------------------------------- */
/*  Ordering and projection                                                   */
/* -------------------------------------------------------------------------- */

// TestOrderingPrefersAdmittingThenHealthier: the breaker decides what may be
// tried, the score decides what is tried first.
func TestOrderingPrefersAdmittingThenHealthier(t *testing.T) {
	clock := newClock()
	registry := rotation.NewRegistry(testPolicy, clock.now)

	const (
		healthy = contract.DeploymentID("dep_healthy")
		flaky   = contract.DeploymentID("dep_flaky")
		broken  = contract.DeploymentID("dep_broken")
	)
	admit(t, registry, healthy).Succeeded()
	// Under the threshold, so it stays in rotation and only its score suffers.
	admit(t, registry, flaky).Failed()
	admit(t, registry, flaky).Succeeded()
	fail(t, registry, broken, testPolicy.FailuresToOpen)

	if registry.Rank(broken).Admitting {
		t.Error("an open breaker ranks as admitting")
	}
	if registry.Rank(healthy).Score <= registry.Rank(flaky).Score {
		t.Error("a deployment that has only succeeded does not score above one that has failed")
	}
	if !registry.Rank(healthy).Before(registry.Rank(flaky)) {
		t.Error("the healthiest deployment does not sort first")
	}
	if !registry.Rank(flaky).Before(registry.Rank(broken)) {
		t.Error("a deployment still in rotation does not sort ahead of one that is out of it")
	}
	// The open breaker sorts last whatever its score, which is why a candidate
	// list is ordered before it is walked rather than filtered as it is walked.
	if registry.Rank(broken).Before(registry.Rank(flaky)) {
		t.Error("an open breaker sorts ahead of an admitting one")
	}
}

// TestSoonestProbeReportsAFactRatherThanAGuess: it is what the executor puts on
// the retry hint of a refusal, so a client backing off is told when the breaker
// will really admit a request again.
func TestSoonestProbeReportsAFactRatherThanAGuess(t *testing.T) {
	clock := newClock()
	registry := rotation.NewRegistry(testPolicy, clock.now)

	const later = contract.DeploymentID("dep_later")
	fail(t, registry, deployment, testPolicy.FailuresToOpen)
	clock.advance(2 * time.Second)
	fail(t, registry, later, testPolicy.FailuresToOpen)

	wait, known := registry.SoonestProbe([]contract.DeploymentID{deployment, later}, clock.now())
	if !known {
		t.Fatal("no probe time is known for two open breakers")
	}
	if wait != testPolicy.Cooldown-2*time.Second {
		t.Errorf("the soonest probe is in %s, expected %s", wait, testPolicy.Cooldown-2*time.Second)
	}

	if _, known := registry.SoonestProbe([]contract.DeploymentID{"dep_never_seen"}, clock.now()); known {
		t.Error("a probe time was reported for a deployment nothing has ever routed to")
	}
}

// TestANewDeploymentIsAssumedHealthy: the alternative sorts a deployment nobody
// has used permanently last, so it never receives the traffic that would prove
// otherwise.
func TestANewDeploymentIsAssumedHealthy(t *testing.T) {
	registry := rotation.NewRegistry(testPolicy, newClock().now)
	projected := registry.Project([]contract.DeploymentID{"dep_fresh"})
	if len(projected) != 1 {
		t.Fatalf("the projection carries %d entries for one deployment", len(projected))
	}
	if projected[0].State != rotation.StateClosed || projected[0].Score != 1 {
		t.Errorf("a deployment nothing has routed to projects as %+v", projected[0])
	}
}

// TestRetainForgetsDeploymentsThatLeftTheInventory: a long-running process that
// has seen many snapshots must not accumulate the history of every deployment
// that ever existed.
func TestRetainForgetsDeploymentsThatLeftTheInventory(t *testing.T) {
	clock := newClock()
	registry := rotation.NewRegistry(testPolicy, clock.now)

	const departed = contract.DeploymentID("dep_departed")
	fail(t, registry, departed, testPolicy.FailuresToOpen)
	refuse(t, registry, departed)

	registry.Retain([]contract.DeploymentID{deployment})

	// Its state is gone, which for a breaker means it starts closed again. That
	// is correct: a deployment that has left the inventory and come back is a
	// different route as far as this process knows.
	if _, ok := registry.Admit(departed); !ok {
		t.Error("a forgotten deployment kept its open breaker")
	}
}
