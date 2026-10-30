// Package runner executes the retrieval matrix against an index and writes the
// raw records. It scores nothing: scoring is a separate, offline, repeatable step
// over the records a run leaves behind.
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/cloudwego/eino/components/embedding"

	"GopherAI/common/rag"

	"github.com/xiaopang456/GopherLearn/eval/internal/config"
	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
	"github.com/xiaopang456/GopherLearn/eval/internal/index"
	"github.com/xiaopang456/GopherLearn/eval/internal/matrix"
	"github.com/xiaopang456/GopherLearn/eval/internal/safety"
)

// Environment names.
const (
	EnvFake = "fake"
	EnvReal = "real"
)

// Guard is the Redis safety hook of a real run (see package safety).
type Guard interface {
	Check(ctx context.Context, fp safety.Fingerprint, rebuild bool) (safety.Decision, error)
	Begin(ctx context.Context, fp safety.Fingerprint) error
	Commit(ctx context.Context, fp safety.Fingerprint) error
}

// Env is everything a run talks to.
type Env struct {
	// Kind is EnvFake or EnvReal; it is recorded in the manifest so offline
	// results can never be mistaken for quality results.
	Kind string
	// Paid says calls cost money. Limits are enforced only when it is true.
	Paid bool

	Embedder   embedding.Embedder
	Repository rag.ChunkVectorRepository
	Searcher   rag.ActiveChunkSearcher
	// Reranker is used only when the matrix needs it. May be nil.
	Reranker rag.Reranker
	// Guard protects a real Redis; nil for in-memory backends.
	Guard Guard

	Dimension      int
	EmbeddingModel string
	// RerankModel describes the reranker for the manifest ("provider/model").
	RerankModel    string
	Chunker        rag.ChunkerConfig
	MaxSourceBytes int64
}

// Options are the run's own settings.
type Options struct {
	// Split selects the questions: dataset.SplitDev or dataset.SplitSealed.
	Split string
	// Rebuild replaces an evaluation index whose fingerprint differs.
	Rebuild bool
	// Now supplies the clock; nil means time.Now.
	Now func() time.Time
	// Progress receives one line per question; nil discards.
	Progress io.Writer
}

// Result summarises a run.
type Result struct {
	Dir            string
	Records        int
	Errors         int
	EmbeddingCalls int
	RerankCalls    int
	Complete       bool
	Estimate       Estimate
	IndexDecision  string
}

// Manifest is manifest.json: what produced the run directory.
type Manifest struct {
	SchemaVersion int      `json:"schema_version"`
	RunID         string   `json:"run_id"`
	StartedAt     string   `json:"started_at"`
	Environment   string   `json:"environment"`
	Split         string   `json:"split"`
	Commit        string   `json:"commit"`
	ConfigDigest  string   `json:"config_digest"`
	Fingerprint   string   `json:"index_fingerprint"`
	IndexDecision string   `json:"index_decision"`
	Cells         []string `json:"cells"`

	Embedding struct {
		Model          string `json:"model"`
		Dimension      int    `json:"dimension"`
		ContentVersion string `json:"content_version"`
	} `json:"embedding"`
	Rerank struct {
		Attached bool   `json:"attached"`
		Model    string `json:"model,omitempty"`
	} `json:"rerank"`
	Chunker rag.ChunkerConfig `json:"chunker"`
	Corpus  struct {
		Name      string `json:"name"`
		Hash      string `json:"hash"`
		Documents int    `json:"documents"`
	} `json:"corpus"`
	Dataset struct {
		Version   string `json:"version"`
		Split     string `json:"split"`
		Questions int    `json:"questions"`
	} `json:"dataset"`
	HitThreshold float64       `json:"hit_threshold"`
	Limits       config.Limits `json:"limits"`
	Estimate     Estimate      `json:"estimate"`
}

// Summary is summary.json, written when the run ends.
type Summary struct {
	Complete       bool   `json:"complete"`
	Records        int    `json:"records"`
	Errors         int    `json:"errors"`
	EmbeddingCalls int    `json:"embedding_calls"`
	RerankCalls    int    `json:"rerank_calls"`
	StopReason     string `json:"stop_reason,omitempty"`
}

// File names inside a run directory.
const (
	ManifestFile = "manifest.json"
	RecordsFile  = "records.jsonl"
	SummaryFile  = "summary.json"
)

// Execute runs the matrix over the questions of one split.
//
// Everything that can refuse does so before the first paid call and before the
// run directory exists: an unsafe path, a foreign Redis database, a plan over the
// limits. The run directory is created fresh and never overwritten. No call is
// retried; a failure is recorded and the run moves on.
func Execute(ctx context.Context, cfg *config.Config, corpus *dataset.Corpus, ds *dataset.Dataset, env Env, opts Options) (*Result, error) {
	if err := cfg.CheckPaths(); err != nil {
		return nil, err
	}
	if env.Kind != EnvFake && env.Kind != EnvReal {
		return nil, fmt.Errorf("runner: environment must be %q or %q", EnvFake, EnvReal)
	}
	questions := ds.Filter(opts.Split)
	if len(questions) == 0 {
		return nil, fmt.Errorf("runner: the %q split has no questions", opts.Split)
	}
	cells := matrix.Expand(cfg.Matrix)
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	fp := safety.NewFingerprint(corpus.Hash(), env.EmbeddingModel, env.Dimension, env.Chunker, cfg.Services.EvalUserID)

	decision := safety.Build
	if env.Guard != nil {
		var err error
		if decision, err = env.Guard.Check(ctx, fp, opts.Rebuild); err != nil {
			return nil, err
		}
	}
	estimate, err := MakePlan(ctx, PlanInput{
		Config: cfg, Corpus: corpus, Questions: questions, Chunker: env.Chunker,
		MaxSourceBytes: env.MaxSourceBytes, IndexExists: decision == safety.Reuse,
	})
	if err != nil {
		return nil, err
	}
	if env.Paid {
		if err := estimate.CheckLimits(cfg.Limits); err != nil {
			return nil, err
		}
	}
	if matrix.NeedsReranker(cells) && env.Reranker == nil {
		return nil, errors.New("runner: the matrix includes hybrid+rerank but no reranker is configured")
	}

	started := now().UTC()
	runID := started.Format("20060102T150405Z") + "-" + cfg.Digest()[:8]
	if err := os.MkdirAll(cfg.Paths.ResultsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create results directory: %w", err)
	}
	dir := filepath.Join(cfg.Paths.ResultsDir, runID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create run directory (an existing run is never overwritten): %w", err)
	}

	manifest := buildManifest(runID, started, cfg, corpus, ds, env, opts, fp, decision, cells, estimate, len(questions))
	if err := writeJSON(filepath.Join(dir, ManifestFile), manifest); err != nil {
		return nil, err
	}
	recordsFile, err := os.OpenFile(filepath.Join(dir, RecordsFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer recordsFile.Close()
	records := bufio.NewWriter(recordsFile)

	budget := NewBudget(env.Paid, cfg.Limits.MaxEmbeddingCalls, cfg.Limits.MaxRerankCalls)
	result := &Result{Dir: dir, Estimate: estimate, IndexDecision: decision.String()}
	finish := func(complete bool, reason string) (*Result, error) {
		if err := records.Flush(); err != nil {
			return result, err
		}
		result.Complete = complete
		result.EmbeddingCalls, result.RerankCalls = budget.Used()
		err := writeJSON(filepath.Join(dir, SummaryFile), Summary{
			Complete: complete, Records: result.Records, Errors: result.Errors,
			EmbeddingCalls: result.EmbeddingCalls, RerankCalls: result.RerankCalls, StopReason: reason,
		})
		return result, err
	}

	var ix *index.Index
	if decision == safety.Reuse {
		ix = index.Attach(cfg.Services.EvalUserID, corpus)
	} else {
		if env.Guard != nil {
			if err := env.Guard.Begin(ctx, fp); err != nil {
				_, _ = finish(false, "could not claim the Redis database")
				return result, fmt.Errorf("claim the Redis database: %w", err)
			}
		}
		ix, err = index.Build(ctx, index.Params{
			UserID: cfg.Services.EvalUserID, Corpus: corpus,
			Embedder: countingEmbedder{inner: env.Embedder, budget: budget}, Repository: env.Repository,
			Dimension: env.Dimension, EmbeddingModel: env.EmbeddingModel, Chunker: env.Chunker,
			MaxSourceBytes: env.MaxSourceBytes,
		})
		if err != nil {
			_, _ = finish(false, "indexing failed")
			return result, fmt.Errorf("build the index: %w", err)
		}
		if env.Guard != nil {
			if err := env.Guard.Commit(ctx, fp); err != nil {
				_, _ = finish(false, "could not record the index fingerprint")
				return result, fmt.Errorf("record the index fingerprint: %w", err)
			}
		}
	}

	queryEmbedder := newCachingEmbedder(env.Embedder, budget)
	service, err := rag.NewRAGService(ctx, queryEmbedder, env.Searcher, ix.Loader(), env.Dimension)
	if err != nil {
		_, _ = finish(false, "retrieval service did not start")
		return result, fmt.Errorf("start the retrieval service: %w", err)
	}
	if matrix.NeedsReranker(cells) {
		if err := service.UseReranker(ctx, newCachingReranker(env.Reranker, budget)); err != nil {
			_, _ = finish(false, "reranker check failed")
			return result, fmt.Errorf("check the reranker: %w", err)
		}
	}

	encoder := json.NewEncoder(records)
	for qi, q := range questions {
		for _, cell := range cells {
			embedBefore, rerankBefore := budget.Used()
			began := time.Now()
			outcome, retrieveErr := service.RetrieveWithOptions(ctx, cfg.Services.EvalUserID, q.Question, cell.Options())
			elapsed := time.Since(began)
			embedAfter, rerankAfter := budget.Used()

			if ctx.Err() != nil {
				return finish(false, "interrupted")
			}
			rec := Record{
				QuestionID: q.ID, Cell: cell.ID(), Method: cell.Method, Rule: cell.Rule, TopK: cell.TopK,
				EffectiveDepth: outcome.EffectiveCandidateDepth, RerankStatus: string(outcome.RerankStatus),
				Results: hitsOf(outcome.Results), Pool: hitsOf(outcome.Candidates),
				EmbeddingCalls: embedAfter - embedBefore, RerankCalls: rerankAfter - rerankBefore,
				ElapsedMs: float64(elapsed.Microseconds()) / 1000,
			}
			if retrieveErr != nil {
				rec.ErrorKind = errorKind(retrieveErr)
				result.Errors++
			}
			if err := encoder.Encode(rec); err != nil {
				_, _ = finish(false, "could not write records")
				return result, err
			}
			result.Records++
			if errors.Is(retrieveErr, ErrBudgetExhausted) {
				return finish(false, "paid-call limit reached")
			}
		}
		if opts.Progress != nil {
			fmt.Fprintf(opts.Progress, "question %d/%d done\n", qi+1, len(questions))
		}
	}
	return finish(true, "")
}

func buildManifest(runID string, started time.Time, cfg *config.Config, corpus *dataset.Corpus, ds *dataset.Dataset, env Env, opts Options,
	fp safety.Fingerprint, decision safety.Decision, cells []matrix.Cell, estimate Estimate, questions int) Manifest {
	m := Manifest{
		SchemaVersion: 1, RunID: runID, StartedAt: started.Format(time.RFC3339), Environment: env.Kind, Split: opts.Split,
		Commit: vcsRevision(), ConfigDigest: cfg.Digest(), Fingerprint: fp.Hash(), IndexDecision: decision.String(),
		Chunker: env.Chunker, HitThreshold: cfg.Metrics.HitThreshold, Limits: cfg.Limits, Estimate: estimate,
	}
	for _, c := range cells {
		m.Cells = append(m.Cells, c.ID())
	}
	m.Embedding.Model, m.Embedding.Dimension, m.Embedding.ContentVersion = env.EmbeddingModel, env.Dimension, rag.EmbeddingContentVersion
	m.Rerank.Attached = matrix.NeedsReranker(cells)
	if m.Rerank.Attached {
		m.Rerank.Model = env.RerankModel
	}
	m.Corpus.Name, m.Corpus.Hash, m.Corpus.Documents = corpus.Name, corpus.Hash(), len(corpus.Documents)
	m.Dataset.Version, m.Dataset.Split, m.Dataset.Questions = ds.Version, opts.Split, questions
	return m
}

// vcsRevision is the commit the binary was built from, when the toolchain
// recorded it. "go build" does; "go run" and "go test" do not.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	revision, dirty := "unknown", ""
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			if setting.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	return revision + dirty
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
