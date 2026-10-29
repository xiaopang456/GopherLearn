package memory

import (
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type workerRepository struct {
	mu         sync.Mutex
	jobs       []model.MemoryExtractionJob
	completed  int
	rejected   int
	rejectCode string
	retried    int
	retryCode  string
	recovered  int
}

func (r *workerRepository) ListReady(context.Context, int, time.Time) ([]model.MemoryExtractionJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.MemoryExtractionJob(nil), r.jobs...), nil
}
func (r *workerRepository) Claim(_ context.Context, id string, _ time.Time, _ time.Duration, _ int) (*model.MemoryExtractionJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.jobs {
		if r.jobs[i].ID == id {
			j := r.jobs[i]
			j.Status = model.MemoryJobProcessing
			j.Attempts++
			j.LeaseToken = "lease"
			r.jobs = nil
			return &j, nil
		}
	}
	return nil, errors.New("conflict")
}
func (r *workerRepository) Complete(_ context.Context, _ *model.MemoryExtractionJob, c *model.MemoryCandidate, status model.MemoryJobStatus, code string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if status == model.MemoryJobCompleted && c != nil {
		r.completed++
	} else if status == model.MemoryJobRejected {
		r.rejected++
		r.rejectCode = code
	}
	return nil
}
func (r *workerRepository) Retry(_ context.Context, _ *model.MemoryExtractionJob, code string, _ time.Time, _ int) error {
	r.mu.Lock()
	r.retried++
	r.retryCode = code
	r.mu.Unlock()
	return nil
}
func (r *workerRepository) RecoverExpired(context.Context, time.Time, int) (int64, error) {
	r.mu.Lock()
	r.recovered++
	r.mu.Unlock()
	return 1, nil
}

type fixedLoader struct{ err error }

func (l fixedLoader) LoadEvidence(context.Context, *model.MemoryExtractionJob) (Evidence, error) {
	return Evidence{EventType: model.MemoryEventAgentRun, Authority: EvidenceAuthoritySystemVerified, UserID: 1, SourceID: "r", SourceRefs: []string{"agent_run:r"}, Content: "verified"}, l.err
}

type fixedExtractor struct {
	candidate *model.MemoryCandidate
	err       error
}

func (e fixedExtractor) Extract(context.Context, Evidence) (*model.MemoryCandidate, error) {
	if e.candidate == nil {
		return nil, e.err
	}
	c := *e.candidate
	return &c, e.err
}

func TestWorkerRecoversAndCompletesPersistedJob(t *testing.T) {
	repository := &workerRepository{jobs: []model.MemoryExtractionJob{{ID: "j", UserID: 1, EventType: model.MemoryEventAgentRun, SourceID: "r", Status: model.MemoryJobPending, AvailableAt: time.Now()}}}
	candidate := &model.MemoryCandidate{ID: "c", UserID: 1, Scope: model.MemoryScopeTopic, Subject: "fact", ProposedContent: "verified fact", ContentHash: model.MemoryContentHash("verified fact"), StructuredJSON: `{}`, Tags: model.StringList{"go", "gopherai", "project"}, SourceRefs: model.StringList{"agent_run:r"}, Confidence: .9, Importance: 7, Sensitivity: model.MemorySensitivitySafe, Status: model.MemoryCandidatePending}
	config := DefaultWorkerConfig()
	config.ScanInterval = time.Millisecond
	config.TaskTimeout = time.Second
	config.Lease = 2 * time.Second
	worker, err := NewWorker(repository, fixedLoader{}, fixedExtractor{candidate: candidate}, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		repository.mu.Lock()
		done := repository.completed == 1
		recovered := repository.recovered
		repository.mu.Unlock()
		if done {
			if recovered != 1 {
				t.Fatalf("recovered=%d", recovered)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := worker.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRejectsSensitiveAndRetriesDependencyFailure(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		loaderErr                 error
		extractorErr              error
		wantRejected, wantRetried int
	}{
		{name: "sensitive", extractorErr: ErrSensitiveMemory, wantRejected: 1},
		{name: "invalid summary", loaderErr: ErrInvalidSummaryEvidence, wantRejected: 1},
		{name: "dependency", loaderErr: errors.New("mysql unavailable"), wantRetried: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := model.MemoryExtractionJob{ID: "j", UserID: 1, EventType: model.MemoryEventAgentRun, SourceID: "r", Status: model.MemoryJobPending, AvailableAt: time.Now()}
			repository := &workerRepository{jobs: []model.MemoryExtractionJob{job}}
			worker, _ := NewWorker(repository, fixedLoader{err: tc.loaderErr}, fixedExtractor{err: tc.extractorErr}, DefaultWorkerConfig())
			worker.process(context.Background(), job)
			repository.mu.Lock()
			defer repository.mu.Unlock()
			if repository.rejected != tc.wantRejected || repository.retried != tc.wantRetried {
				t.Fatalf("rejected=%d retried=%d", repository.rejected, repository.retried)
			}
			if tc.name == "invalid summary" && repository.rejectCode != "invalid_summary_evidence" {
				t.Fatalf("reject code=%q", repository.rejectCode)
			}
		})
	}
}

func TestWorkerPersistsSpecificExtractorContractFailure(t *testing.T) {
	job := model.MemoryExtractionJob{ID: "j", UserID: 1, EventType: model.MemoryEventAgentRun, SourceID: "r", Status: model.MemoryJobPending, AvailableAt: time.Now()}
	repository := &workerRepository{jobs: []model.MemoryExtractionJob{job}}
	worker, _ := NewWorker(repository, fixedLoader{}, fixedExtractor{err: fmt.Errorf("%w: importance", ErrCandidateFieldType)}, DefaultWorkerConfig())
	worker.process(context.Background(), job)
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.retried != 1 || repository.retryCode != "field_type" {
		t.Fatalf("retried=%d code=%q", repository.retried, repository.retryCode)
	}
}
