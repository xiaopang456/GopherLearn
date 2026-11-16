package conversation

// A paid run has to wait long enough for the memory to be indexed, and the
// product's projection worker has to be allowed to take that long.
//
// The ninety seconds the arm used to wait were the local stand-in's
// timescale: the stand-in answers in milliseconds. A real model does not.
// Publishing the projection is only the start - the product's worker then
// generates the document (one call for a card, one call per assertion for an
// Episode), embeds it and writes it to the index.
//
// Two timeouts are in the way, and they are different:
//
//   - the ARM's wait: when it expires, the attempt tears the product down
//     while the projection is still running, and the worker records a
//     CANCELLED projection rather than a timeout. The arm has to be able to
//     wait longer;
//   - the PRODUCT's task timeout: the worker kills a projection that runs
//     past it. Thirty seconds is not enough for a real model's generation
//     and verification chain, so the evaluation has to hand the product a
//     configuration that allows for it - the same kind of setting as the
//     quiet window the evaluation already overrides.
//
// The lease has to outlast the task timeout, because the configuration
// refuses a lease that is not longer.

import (
	"path/filepath"
	"testing"
	"time"
)

// The arm's wait comes from the environment, so a paid run can say how long
// it waited.
func TestTheArmWaitComesFromTheEnvironment(t *testing.T) {
	env := RealEnvironment{MemoryWaitTimeout: 20 * time.Minute}
	arm := CardArmForEnvironment(env)
	if arm.Wait() != 20*time.Minute {
		t.Fatalf("the arm waits %v, want the environment's 20m", arm.Wait())
	}
}

// A run that says nothing gets the paid run's default, not the stand-in's.
//
// The shorter default is the one that cancels a projection, so it is the one
// that must not be used when a real model is answering.
func TestAPaidRunWaitsLongerThanTheStandInByDefault(t *testing.T) {
	arm := CardArmForEnvironment(RealEnvironment{})
	if arm.Wait() <= cardArmDefaultWait {
		t.Fatalf("the paid run waits %v, want more than the stand-in's %v", arm.Wait(), cardArmDefaultWait)
	}
	if arm.Wait() < realProjectionTaskTimeoutSeconds*time.Second {
		t.Fatalf("the arm waits %v, want at least the projection's task timeout %v",
			arm.Wait(), realProjectionTaskTimeoutSeconds*time.Second)
	}
}

// The two arms use the same wait.
//
// The Episode arm is the slower one - it verifies every assertion separately
// - so a shorter wait for it would be wrong in the other direction too. The
// comparison is only fair if both arms wait the same.
func TestBothArmsWaitTheSame(t *testing.T) {
	env := RealEnvironment{}
	old := CardArmForEnvironment(env)
	env.SessionCardMode = CardArmModeOn
	card := CardArmForEnvironment(env)
	if old.Wait() != card.Wait() {
		t.Fatalf("the old arm waits %v and the card arm %v, want the same", old.Wait(), card.Wait())
	}
}

// The product's worker is allowed to take as long as the arm waits.
//
// A worker that is killed first produces a projection that never lands, and
// the arm then waits for a row that will not appear.
//
// The numbers are checked against the PRODUCT's own configuration validation,
// not against a copy of its bounds: the evaluation hands the product a
// configuration, and a configuration the product refuses is a run that does
// not start.
func TestTheProductWorkerIsAllowedToTakeAsLongAsTheArmWaits(t *testing.T) {
	arm := CardArmForEnvironment(RealEnvironment{})
	task := time.Duration(realProjectionTaskTimeoutSeconds) * time.Second
	lease := time.Duration(realProjectionLeaseSeconds) * time.Second
	if task <= 0 || lease <= 0 {
		t.Fatalf("the task timeout %v and the lease %v have to be positive", task, lease)
	}
	if lease <= task {
		t.Fatalf("the lease %v does not outlast the task timeout %v", lease, task)
	}
	if task >= arm.Wait() {
		t.Fatalf("the task timeout %v is not shorter than the arm's wait %v", task, arm.Wait())
	}
	// The product's own validation has to accept the numbers. It refuses a
	// task timeout above two minutes and a lease that does not outlast it -
	// so the evaluation stays inside the product's bound instead of moving it.
	//
	// The configuration is the one the evaluation actually hands the
	// product, read from the product's own example file and overridden with
	// the two numbers under test. A hand-built one would be refused for a
	// field that has nothing to do with these numbers.
	repository := filepath.Join("..", "..", "..")
	storage := t.TempDir()
	settings, err := offlineSettings(repository, storage)
	if err != nil {
		t.Fatalf("read the product's configuration: %v", err)
	}
	settings.Config.AgentConfig.EpisodeTaskTimeoutSeconds = realProjectionTaskTimeoutSeconds
	settings.Config.AgentConfig.EpisodeLeaseSeconds = realProjectionLeaseSeconds
	if err := settings.Validate(); err != nil {
		t.Fatalf("the product refuses the configuration the evaluation hands it: %v", err)
	}
}
