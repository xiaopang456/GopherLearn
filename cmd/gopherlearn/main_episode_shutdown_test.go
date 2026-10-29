package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The shutdown regression: the worker's stop context is built at exit time, so
// a process that has been running longer than the grace window can still wait
// for its worker's loops to exit. The startup-time context the first wiring
// used was already expired by the first real shutdown of a process that lived
// past the window, and Stop then reported expiry while the loops - including
// the persistence write that releases a lease - were still running.

// slowExitRunner is a projection that finishes its exit only after Stop's
// cancellation reaches it. The delay stands in for the persistence paths
// (releasing a lease, recording the retry) that run after cancellation and that
// the grace window exists to cover.
type slowExitRunner struct {
	entered chan struct{}
	once    sync.Once
}

func (r *slowExitRunner) Project(ctx context.Context, _, _, _ string) (*domain.Episode, error) {
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
	// The loops do not stop the instant cancellation lands; finishing takes a
	// bounded, observable moment.
	time.Sleep(60 * time.Millisecond)
	return nil, ctx.Err()
}

// newStartedWorker builds a real background worker on the in-memory store and
// starts it, so the stop path exercises the actual goroutines and their done
// channel rather than a stub.
func newStartedWorker(t *testing.T, store repository.Repository, runner service.EpisodeProjectionRunner) *service.EpisodeWorker {
	t.Helper()
	reconciler, err := service.NewEpisodeReconciler(store, 0, 10)
	if err != nil {
		t.Fatalf("build reconciler: %v", err)
	}
	worker, err := service.NewEpisodeWorker(store, runner, reconciler, service.EpisodeWorkerConfig{
		WorkerCount: 1, QueueSize: 4, ScanInterval: 5 * time.Millisecond,
		ReconcileInterval: 40 * time.Millisecond, TaskTimeout: 10 * time.Second,
		Lease: 30 * time.Second, RecoveryInterval: 40 * time.Millisecond,
		RetryDelay: 10 * time.Millisecond, MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("build worker: %v", err)
	}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	return worker
}

// enqueueInFlightProjection publishes one due job and waits until the runner is
// inside it, so the shutdown below arrives mid-projection.
func enqueueInFlightProjection(t *testing.T, store repository.Repository, runner *slowExitRunner) {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateUser(ctx, &domain.User{Email: "stop@example.com", DisplayName: "Stop"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "s", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	message, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "assistant", Content: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertEpisodeDesired(ctx, user.ID, session.ID, message.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-runner.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the projection never started")
	}
}

// TestStopEpisodeWorkerAfterOutlivingTheGraceWindow is the headline case: the
// worker has been running longer than the grace window the old wiring spent at
// startup, a projection is in flight, and the stop must still wait for the
// loops to exit and report success. The windows here are milliseconds, so the
// default suite never waits five real seconds.
func TestStopEpisodeWorkerAfterOutlivingTheGraceWindow(t *testing.T) {
	store := repository.NewInMemoryRepository()
	runner := &slowExitRunner{entered: make(chan struct{})}
	worker := newStartedWorker(t, store, runner)
	enqueueInFlightProjection(t, store, runner)

	// The process outlives the startup-time window with a projection in
	// flight, then stops with a fresh exit-time deadline.
	time.Sleep(120 * time.Millisecond)
	started := time.Now()
	if err := stopEpisodeWorker(worker, time.Second); err != nil {
		t.Fatalf("stop with a fresh exit-time deadline = %v, want nil", err)
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Fatalf("stop returned after %v: it did not wait for the loops to exit", elapsed)
	}
	// A second stop is a no-op, not an error: the shutdown path may race the
	// drain path to the same call.
	if err := stopEpisodeWorker(worker, time.Second); err != nil {
		t.Fatalf("second stop = %v, want nil", err)
	}
}

// TestWorkerStopWithStartupTimeDeadlineReports pins the contract that makes
// the exit-time deadline necessary. A deadline that was already expired when
// the stop ran - exactly what a startup-time context is on every shutdown of a
// long-lived process - reports instead of waiting, and the report must be an
// error rather than a fake success.
func TestWorkerStopWithStartupTimeDeadlineReports(t *testing.T) {
	store := repository.NewInMemoryRepository()
	runner := &slowExitRunner{entered: make(chan struct{})}
	worker := newStartedWorker(t, store, runner)
	enqueueInFlightProjection(t, store, runner)

	// The old wiring's shape: the deadline is spent before the stop runs.
	startupTimeContext, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	time.Sleep(120 * time.Millisecond)

	stopStarted := time.Now()
	err := worker.Stop(startupTimeContext)
	if elapsed := time.Since(stopStarted); elapsed > 30*time.Millisecond {
		t.Fatalf("the expired deadline waited %v; it should have reported immediately", elapsed)
	}
	if err == nil {
		t.Fatal("an expired deadline reported success while the loops were still running")
	}
	// The report does not lose the worker: a fresh deadline still stops it.
	if err := stopEpisodeWorker(worker, time.Second); err != nil {
		t.Fatalf("cleanup stop = %v, want nil", err)
	}
	if err := stopEpisodeWorker(worker, time.Second); err != nil {
		t.Fatalf("second stop = %v, want nil", err)
	}
}
