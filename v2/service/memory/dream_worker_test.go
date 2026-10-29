package memory

import (
	memorydao "GopherAI/dao/memory"
	"context"
	"errors"
	"testing"
	"time"
)

type dreamQueueStub struct {
	owners []int64
	calls  int
	err    error
}

func (q *dreamQueueStub) ListUsersWithPendingCandidates(context.Context, int) ([]int64, error) {
	q.calls++
	return q.owners, q.err
}

// governanceStub replaces GovernanceService.Process. The worker owns none of the
// claiming or validation, so what is worth asserting is which owners it visits
// and when it stops asking.
type governanceStub struct {
	served  []int64
	results map[int64][]error
}

func (g *governanceStub) process(_ context.Context, request DreamRequest) (DreamResult, error) {
	g.served = append(g.served, request.UserID)
	queue := g.results[request.UserID]
	if len(queue) == 0 {
		return DreamResult{}, memorydao.ErrCandidateNotFound
	}
	err := queue[0]
	g.results[request.UserID] = queue[1:]
	return DreamResult{}, err
}

func newTestDreamWorker(t *testing.T, queue DreamQueueRepository, governance *governanceStub, config DreamWorkerConfig) *DreamWorker {
	t.Helper()
	worker, err := NewDreamWorker(&GovernanceService{}, queue, config)
	if err != nil {
		t.Fatal(err)
	}
	worker.process = governance.process
	worker.logf = func(string, ...any) {}
	return worker
}

func TestDreamWorkerServesEveryOwnerAndStopsWhenDrained(t *testing.T) {
	queue := &dreamQueueStub{owners: []int64{7, 9}}
	governance := &governanceStub{results: map[int64][]error{
		7: {nil, nil}, // two candidates, then the owner is drained
		9: {nil},
	}}
	worker := newTestDreamWorker(t, queue, governance, DreamWorkerConfig{ScanInterval: time.Minute, OwnerBatch: 20, PerOwner: 5})

	worker.RunOnce(context.Background())

	// Owner 7: two successes plus the call that reported the backlog empty.
	// Owner 9: one success plus its own empty report. The worker must not keep
	// asking after that, or an idle account costs a query every pass.
	want := []int64{7, 7, 7, 9, 9}
	if len(governance.served) != len(want) {
		t.Fatalf("served=%v want=%v", governance.served, want)
	}
	for index := range want {
		if governance.served[index] != want[index] {
			t.Fatalf("served=%v want=%v", governance.served, want)
		}
	}
}

func TestDreamWorkerYieldsWhenAnotherRunHoldsTheCandidate(t *testing.T) {
	queue := &dreamQueueStub{owners: []int64{7}}
	governance := &governanceStub{results: map[int64][]error{
		7: {memorydao.ErrDreamBusy, nil, nil},
	}}
	worker := newTestDreamWorker(t, queue, governance, DreamWorkerConfig{ScanInterval: time.Minute, OwnerBatch: 20, PerOwner: 5})

	worker.RunOnce(context.Background())

	// Busy means someone else holds the row. Retrying inside the same pass would
	// spin against a lock that only clears when that run finishes or its lease
	// expires, so the pass ends and the next one picks it up.
	if len(governance.served) != 1 {
		t.Fatalf("served=%v want a single attempt", governance.served)
	}
}

func TestDreamWorkerBoundsOneOwnerPerPass(t *testing.T) {
	queue := &dreamQueueStub{owners: []int64{7, 9}}
	governance := &governanceStub{results: map[int64][]error{
		7: {nil, nil, nil, nil, nil, nil, nil}, // a backlog larger than PerOwner
		9: {nil},
	}}
	worker := newTestDreamWorker(t, queue, governance, DreamWorkerConfig{ScanInterval: time.Minute, OwnerBatch: 20, PerOwner: 2})

	worker.RunOnce(context.Background())

	// Two for the backlogged owner, then on to the next: a long queue must not
	// starve the accounts behind it.
	served7 := 0
	for _, id := range governance.served {
		if id == 7 {
			served7++
		}
	}
	if served7 != 2 {
		t.Fatalf("owner 7 served %d times, want the PerOwner bound of 2", served7)
	}
	if len(governance.served) < 3 || governance.served[2] != 9 {
		t.Fatalf("served=%v want owner 9 reached", governance.served)
	}
}

func TestDreamWorkerKeepsRunningAfterAFailedOwner(t *testing.T) {
	queue := &dreamQueueStub{owners: []int64{7, 9}}
	governance := &governanceStub{results: map[int64][]error{
		7: {errors.New("provider unavailable")},
		9: {nil},
	}}
	worker := newTestDreamWorker(t, queue, governance, DreamWorkerConfig{ScanInterval: time.Minute, OwnerBatch: 20, PerOwner: 5})

	worker.RunOnce(context.Background())

	// One owner's outage must not stop the pass: the candidate stays pending and
	// is retried next time, while everyone else is still served.
	reached9 := false
	for _, id := range governance.served {
		if id == 9 {
			reached9 = true
		}
	}
	if !reached9 {
		t.Fatalf("served=%v want owner 9 reached despite owner 7 failing", governance.served)
	}
}

func TestDreamWorkerStartsOnceAndStops(t *testing.T) {
	queue := &dreamQueueStub{}
	governance := &governanceStub{results: map[int64][]error{}}
	worker := newTestDreamWorker(t, queue, governance, DefaultDreamWorkerConfig())

	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(context.Background()); !errors.Is(err, ErrWorkerAlreadyStarted) {
		t.Fatalf("second start returned %v", err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := worker.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}
