package episode

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
)

// STABILIZE-065 Phase 2-C1: the tier-1 remediation, T1 to T16.
//
// Every behavioural case enters through worker.Start and the job queue against a
// disposable MySQL, with the production evidence loader, the production job
// repository and the production projector. The one scripted part is the document
// generator, which is the step an LLM performs, and the support verifier, which
// this round implements as a seam only. External calls: none.
//
// These were written and run before the implementation, and the real failures
// are recorded in exp033/stabilize065/phase2/repro/c1-t1t16-red-before.txt. The
// expectations below are the ones preregistered in
// projector-remediation-test-matrix.md; nothing here was relaxed to make the
// fix look better.

// --- disposable store -------------------------------------------------------

func c1DSN() string {
	if dsn := os.Getenv("STABILIZE065_C1_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "root:s065local@tcp(127.0.0.1:33506)/stabilize065_c1?charset=utf8mb4&parseTime=True&loc=Local"
}

// openC1DB connects to the disposable store, or skips honestly.
//
// A skip here is not a pass. It means the tier-1 behaviour is UNPROVEN, and the
// report has to say so rather than counting the run as green.
func openC1DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysqldriver.Open(c1DSN()), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Skipf("the disposable MySQL is unavailable, tier-1 behaviour is UNPROVEN: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil || sqlDB.Ping() != nil {
		t.Skipf("the disposable MySQL is unavailable, tier-1 behaviour is UNPROVEN")
	}
	if err := db.AutoMigrate(&model.Session{}, &model.Message{}, &model.SessionSummary{},
		&model.EpisodeMemory{}, &model.EpisodeProjectionJob{}); err != nil {
		t.Fatalf("migrate the disposable schema: %v", err)
	}
	for _, table := range []string{"episode_projection_jobs", "episode_memories",
		"session_summaries", "messages", "sessions"} {
		if err := db.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	return db
}

const (
	c1Owner     = int64(900000000000000401)
	c1Other     = int64(900000000000000402)
	c1Session   = "sess-c1-subject"
	c1Neighbour = "sess-c1-neighbour"
)

var c1Base = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

// --- the scripted document generator ----------------------------------------

// c1Generator is the stand-in for the model. It returns whatever the fixture
// built, so what is under test is what the server accepts, not what a model
// tends to say.
type c1Generator struct {
	mu       sync.Mutex
	document model.EpisodeDocument
	calls    int
	failWith error
}

func (g *c1Generator) Calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *c1Generator) Generate(_ context.Context,
	evidence ProjectionEvidence) (ProjectionOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.failWith != nil {
		return ProjectionOutcome{}, g.failWith
	}
	// The fixture decides the document; the eligibility rule is the production
	// one, applied here exactly as ModelGenerator applies it.
	if g.document.AssertionCount() == 0 {
		users := 0
		for _, m := range evidence.Messages {
			if m.IsUser {
				users++
			}
		}
		return Abstained(model.AbstainNoDurableAssertion, users), nil
	}
	return Generated(g.document), nil
}

// --- the scripted support verifier ------------------------------------------

type c1Verifier struct {
	mu sync.Mutex
	// verdicts maps an assertion body to what the verifier says about it.
	verdicts map[string]SupportVerdict
	// fallback is used for a body with no explicit verdict.
	fallback SupportVerdict
	fail     bool
	calls    int
	// sawAssistantText records whether the verifier was ever handed text it
	// should not see. It must stay false.
	sawAssistantText bool
	forbidden        []string
}

func (v *c1Verifier) Calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

func (v *c1Verifier) Verify(_ context.Context, request SupportRequest) (SupportVerdict, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	for _, banned := range v.forbidden {
		if strings.Contains(request.AssertionText, banned) {
			v.sawAssistantText = true
		}
		for _, m := range request.UserMessages {
			if strings.Contains(m, banned) {
				v.sawAssistantText = true
			}
		}
	}
	if v.fail {
		return "", fmt.Errorf("verifier unavailable")
	}
	if verdict, ok := v.verdicts[request.AssertionText]; ok {
		return verdict, nil
	}
	if v.fallback == "" {
		return SupportEntailed, nil
	}
	return v.fallback, nil
}

// --- the chain -------------------------------------------------------------

type c1Chain struct {
	db        *gorm.DB
	repo      *episodedao.Repository
	worker    *Worker
	generator *c1Generator
	verifier  *c1Verifier
	running   bool
}

type c1Message struct {
	id     uint
	isUser bool
	text   string
	at     time.Time
}

// seedSession writes one session and its messages into the disposable store.
func c1SeedSession(t *testing.T, db *gorm.DB, owner int64, sessionID string, msgs []c1Message) {
	t.Helper()
	at := c1Base
	if len(msgs) > 0 {
		at = msgs[0].at
	}
	if err := db.Save(&model.Session{ID: sessionID, UserID: owner, UserName: "c1",
		Mode: model.SessionModeAgent, Title: "c1", CreatedAt: at, UpdatedAt: at}).Error; err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for _, m := range msgs {
		if err := db.Save(&model.Message{ID: m.id, SessionID: sessionID, Content: m.text,
			IsUser: m.isUser, CreatedAt: m.at}).Error; err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
}

func newC1Chain(t *testing.T, msgs []c1Message, document model.EpisodeDocument,
	verifier *c1Verifier) *c1Chain {
	t.Helper()
	db := openC1DB(t)
	c1SeedSession(t, db, c1Owner, c1Session, msgs)

	repo := episodedao.NewRepository(db)
	loader, err := NewDatabaseEvidenceLoader(db, 256)
	if err != nil {
		t.Fatal(err)
	}
	generator := &c1Generator{document: document}
	// C1.1: the verifier is a constructor argument, so a chain that is not
	// about semantic support supplies the permissive test double rather than
	// leaving the projector unverified - which is no longer possible anyway.
	var support SupportVerifier = allowAll()
	if verifier != nil {
		support = verifier
	}
	projector, err := NewProjector(loader, generator, repo, support)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewReconciler(repo, time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultWorkerConfig()
	config.ScanInterval = 40 * time.Millisecond
	config.ReconcileInterval = time.Hour
	config.RecoveryInterval = time.Hour
	config.RetryDelay = 50 * time.Millisecond
	config.QuietWindow = 0
	worker, err := NewWorker(repo, projector, reconciler, config)
	if err != nil {
		t.Fatal(err)
	}
	return &c1Chain{db: db, repo: repo, worker: worker,
		generator: generator, verifier: verifier}
}

func (c *c1Chain) enqueue(t *testing.T, target uint) string {
	t.Helper()
	job, err := c.repo.UpsertDesired(context.Background(), c1Owner, c1Session, target,
		time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return job.ID
}

func (c *c1Chain) run(t *testing.T, jobID string,
	done func(*model.EpisodeProjectionJob) bool) *model.EpisodeProjectionJob {
	t.Helper()
	if !c.running {
		c.running = true
		ctx, cancel := context.WithCancel(context.Background())
		if err := c.worker.Start(ctx); err != nil {
			cancel()
			t.Fatalf("start worker: %v", err)
		}
		t.Cleanup(func() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = c.worker.Stop(stopCtx)
			stopCancel()
			cancel()
		})
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job, err := c.repo.GetJob(context.Background(), c1Owner, jobID)
		if err == nil && job != nil && done(job) {
			return job
		}
		time.Sleep(40 * time.Millisecond)
	}
	job, _ := c.repo.GetJob(context.Background(), c1Owner, jobID)
	t.Fatalf("job never settled; last = %+v", job)
	return nil
}

func (c *c1Chain) settle(t *testing.T, target uint) *model.EpisodeProjectionJob {
	t.Helper()
	id := c.enqueue(t, target)
	return c.run(t, id, func(j *model.EpisodeProjectionJob) bool {
		return j.Status == model.EpisodeProjectionCompleted ||
			j.Status == model.EpisodeProjectionFailed
	})
}

// episodes returns every episode row for the subject session, whatever status.
func (c *c1Chain) episodes(t *testing.T) []model.EpisodeMemory {
	t.Helper()
	var rows []model.EpisodeMemory
	if err := c.db.Where("session_id = ?", c1Session).Order("id asc").
		Find(&rows).Error; err != nil {
		t.Fatalf("read episodes: %v", err)
	}
	return rows
}

// sole returns the single episode row, failing when there is not exactly one.
func (c *c1Chain) sole(t *testing.T) model.EpisodeMemory {
	t.Helper()
	rows := c.episodes(t)
	if len(rows) != 1 {
		t.Fatalf("expected exactly one episode row for the session, found %d", len(rows))
	}
	return rows[0]
}

// assertionKinds lists the non-synopsis assertion kinds on a row.
func c1Kinds(episode model.EpisodeMemory) []string {
	out := []string{}
	for _, a := range model.AssertionsOf(episode) {
		if a.Kind == model.AssertionSynopsis {
			continue
		}
		out = append(out, string(a.Kind))
	}
	return out
}

// --- fixtures ---------------------------------------------------------------

func c1UserOnly(text string) []c1Message {
	return []c1Message{
		{id: 8101, isUser: true, text: text, at: c1Base},
		{id: 8102, isUser: false, text: "助手的回答正文，只应出现在 synopsis 里。", at: c1Base.Add(time.Minute)},
	}
}

func c1Doc(synopsis string) model.EpisodeDocument {
	return model.EpisodeDocument{
		Synopsis:      synopsis,
		Topics:        []model.EpisodeTopic{{Key: "c1", Label: "C1"}},
		SourceHandles: []string{"message:8101", "message:8102"},
	}
}

func c1Entry(text string, ids ...uint) model.EpisodeEvidence {
	return model.EpisodeEvidence{Text: text, EvidenceMessageIDs: ids}
}

// --- T1 / T2 / T3: sessions that hold nothing durable must abstain ----------

func TestC1AbstainsOnSessionsWithNothingDurable(t *testing.T) {
	for name, shape := range map[string]struct {
		userText string
		reason   model.AbstainReason
	}{
		"T1 a plain technical question": {
			"Go 里 defer 的执行顺序是怎样的？", model.AbstainNoDurableAssertion},
		"T2 an operational instruction": {
			"继续执行，给出下一步", model.AbstainNoDurableAssertion},
		"T3 a forwarded agent report": {
			"以下是 ZCode 的报告：本轮完成 A、B、C，门禁全部通过，建议进入下一阶段。",
			model.AbstainNoDurableAssertion},
	} {
		t.Run(name, func(t *testing.T) {
			// The model produced a perfectly good synopsis and no assertions,
			// which the prompt already says is a valid result.
			chain := newC1Chain(t, c1UserOnly(shape.userText), c1Doc("用户提出了一个问题，助手作答。"), nil)
			job := chain.settle(t, 8102)
			if job.Status != model.EpisodeProjectionCompleted {
				t.Fatalf("job ended as %q with code %q", job.Status, job.ErrorCode)
			}
			row := chain.sole(t)
			if row.Status == model.EpisodeStatusActive {
				t.Fatalf("a session with nothing durable produced an ACTIVE episode; " +
					"a synopsis is not a reason to remember a session")
			}
			if row.Status != model.EpisodeStatusAbstained {
				t.Fatalf("expected an abstained row, got status %q", row.Status)
			}
			if row.AbstainReason != shape.reason {
				t.Fatalf("abstain reason is %q, want %q", row.AbstainReason, shape.reason)
			}
			if len(c1Kinds(row)) != 0 {
				t.Fatalf("an abstained row carries assertions: %v", c1Kinds(row))
			}
		})
	}
}

// --- T4 / T5 / T6 / T7 / T8: each durable kind lands in its own slot ---------

func TestC1DurableKindsLandInTheirOwnSlot(t *testing.T) {
	for name, shape := range map[string]struct {
		userText string
		build    func(model.EpisodeDocument, model.EpisodeEvidence) model.EpisodeDocument
		wantKind model.AssertionKind
	}{
		"T4 an explicit standing preference": {
			"请记住：以后代码部分都用 Go 语法讲。",
			func(d model.EpisodeDocument, e model.EpisodeEvidence) model.EpisodeDocument {
				d.Preferences = []model.EpisodeEvidence{e}
				return d
			}, model.AssertionPreference},
		"T5 a settled long-term decision": {
			"那就定 Protobuf 作为 Body 编解码方案。",
			func(d model.EpisodeDocument, e model.EpisodeEvidence) model.EpisodeDocument {
				d.Decisions = []model.EpisodeEvidence{e}
				return d
			}, model.AssertionDecision},
		"T6 a deferral pending load testing": {
			"压缩先不做，等压测出来再说。",
			func(d model.EpisodeDocument, e model.EpisodeEvidence) model.EpisodeDocument {
				d.OpenLoops = []model.EpisodeEvidence{e}
				return d
			}, model.AssertionOpenLoop},
		"T7 a self-correction": {
			"我以前以为 read 返回的字节数等于对方 write 的次数，现在明白不是。",
			func(d model.EpisodeDocument, e model.EpisodeEvidence) model.EpisodeDocument {
				d.UserCorrections = []model.EpisodeEvidence{e}
				return d
			}, model.AssertionCorrection},
		"T8 a stated durable understanding": {
			"我理解 TCP 粘包的本质是字节流没有消息边界。",
			func(d model.EpisodeDocument, e model.EpisodeEvidence) model.EpisodeDocument {
				d.DurableExperiences = []model.EpisodeEvidence{e}
				return d
			}, model.AssertionDurableExperience},
	} {
		t.Run(name, func(t *testing.T) {
			doc := shape.build(c1Doc("用户表达了一条长期内容。"), c1Entry(shape.userText, 8101))
			chain := newC1Chain(t, c1UserOnly(shape.userText), doc, nil)
			job := chain.settle(t, 8102)
			if job.Status != model.EpisodeProjectionCompleted {
				t.Fatalf("job ended as %q with code %q", job.Status, job.ErrorCode)
			}
			row := chain.sole(t)
			if row.Status != model.EpisodeStatusActive {
				t.Fatalf("a session with a durable %s abstained (status %q)",
					shape.wantKind, row.Status)
			}
			kinds := c1Kinds(row)
			if len(kinds) != 1 || kinds[0] != string(shape.wantKind) {
				t.Fatalf("assertion kinds are %v, want exactly [%s]", kinds, shape.wantKind)
			}
			// The slot letter is what a governed citation renders, so a new kind
			// with no letter would be unciteable.
			if shape.wantKind.Letter() == "" {
				t.Fatalf("%s has no slot letter", shape.wantKind)
			}
			// The time comes from the evidence message, and it is persisted.
			for _, a := range model.AssertionsOf(row) {
				if a.Kind == model.AssertionSynopsis {
					continue
				}
				if !a.TimeKnown || a.OccurredAt.IsZero() {
					t.Fatalf("%s carries no occurrence time", a.Kind)
				}
				if !a.OccurredAt.Equal(c1Base.UTC()) {
					t.Fatalf("%s is dated %v, want the evidence message time %v",
						a.Kind, a.OccurredAt, c1Base.UTC())
				}
			}
		})
	}
}

// --- T9: an assistant suggestion the learner never answered ------------------

func TestC1AssistantSuggestionWithoutUserAnswerIsNotRemembered(t *testing.T) {
	msgs := c1UserOnly("粘包是怎么回事？")
	// The model cited the assistant turn, which is where its content came from.
	doc := c1Doc("助手提议演示一个粘包场景，用户没有回应。")
	doc.OpenLoops = []model.EpisodeEvidence{
		c1Entry("用户尚未回应助手提出的演示建议。", 8102)}
	chain := newC1Chain(t, msgs, doc, nil)
	job := chain.settle(t, 8102)
	if job.Status != model.EpisodeProjectionCompleted {
		t.Fatalf("job ended as %q with code %q", job.Status, job.ErrorCode)
	}
	row := chain.sole(t)
	if kinds := c1Kinds(row); len(kinds) != 0 {
		t.Fatalf("an unanswered assistant suggestion became %v", kinds)
	}
	if row.Status != model.EpisodeStatusAbstained {
		t.Fatalf("nothing survived, yet the row is %q rather than abstained", row.Status)
	}
}

// --- T10: the text must be entailed by the user messages it cites ------------

func TestC1AssertionTextMustBeEntailedByTheCitedUserMessages(t *testing.T) {
	unsupported := "用户尚未回应助手提出的演示建议。"
	supported := "用户决定采用 Protobuf。"
	msgs := []c1Message{
		{id: 8101, isUser: true, text: "那就定 Protobuf。", at: c1Base},
		{id: 8102, isUser: false, text: "好的，要不要我演示一下粘包场景？", at: c1Base.Add(time.Minute)},
		{id: 8103, isUser: true, text: "粘包是怎么回事？", at: c1Base.Add(2 * time.Minute)},
	}
	doc := c1Doc("用户定下 Protobuf；助手另外提了一个演示建议。")
	doc.SourceHandles = []string{"message:8101", "message:8102", "message:8103"}
	doc.Decisions = []model.EpisodeEvidence{
		c1Entry(supported, 8101),
		// The pointer is a real user message. The body is about the assistant.
		c1Entry(unsupported, 8103),
	}
	verifier := &c1Verifier{
		verdicts: map[string]SupportVerdict{
			supported:   SupportEntailed,
			unsupported: SupportNotEntailed,
		},
		// The verifier may never be shown assistant text.
		forbidden: []string{"要不要我演示一下"},
	}
	chain := newC1Chain(t, msgs, doc, verifier)
	job := chain.settle(t, 8103)
	if job.Status != model.EpisodeProjectionCompleted {
		t.Fatalf("job ended as %q with code %q", job.Status, job.ErrorCode)
	}
	row := chain.sole(t)
	if row.Status != model.EpisodeStatusActive {
		t.Fatalf("a surviving decision should keep the episode active, got %q", row.Status)
	}
	texts := []string{}
	for _, a := range model.AssertionsOf(row) {
		if a.Kind != model.AssertionSynopsis {
			texts = append(texts, a.Text)
		}
	}
	if len(texts) != 1 || texts[0] != supported {
		t.Fatalf("surviving assertions are %v, want only the entailed one", texts)
	}
	if verifier.Calls() != 2 {
		t.Fatalf("the verifier was called %d times for 2 assertions", verifier.Calls())
	}
	if verifier.sawAssistantText {
		t.Fatal("the verifier was shown assistant text; it may only see the " +
			"assertion body and the user messages that assertion cites")
	}
}

func TestC1VerifierUncertainAndFailureBothTrim(t *testing.T) {
	for name, verifier := range map[string]*c1Verifier{
		"uncertain trims":   {fallback: SupportUncertain},
		"unreachable trims": {fail: true},
	} {
		t.Run(name, func(t *testing.T) {
			doc := c1Doc("用户定下 Protobuf。")
			doc.Decisions = []model.EpisodeEvidence{c1Entry("用户决定采用 Protobuf。", 8101)}
			chain := newC1Chain(t, c1UserOnly("那就定 Protobuf。"), doc, verifier)
			job := chain.settle(t, 8102)
			if job.Status != model.EpisodeProjectionCompleted {
				t.Fatalf("job ended as %q with code %q", job.Status, job.ErrorCode)
			}
			row := chain.sole(t)
			if kinds := c1Kinds(row); len(kinds) != 0 {
				t.Fatalf("an unverified assertion survived: %v", kinds)
			}
			if row.Status != model.EpisodeStatusAbstained {
				t.Fatalf("everything was trimmed, yet the row is %q", row.Status)
			}
		})
	}
}

// --- T11: everything trimmed means abstain, not an empty shell ---------------

func TestC1AllAssertionsTrimmedAbstainsWithItsOwnReason(t *testing.T) {
	doc := c1Doc("一次没有留下持久内容的会话。")
	doc.Decisions = []model.EpisodeEvidence{c1Entry("用户决定采用 Protobuf。", 8101)}
	verifier := &c1Verifier{fallback: SupportNotEntailed}
	chain := newC1Chain(t, c1UserOnly("那就定 Protobuf。"), doc, verifier)
	job := chain.settle(t, 8102)
	if job.Status != model.EpisodeProjectionCompleted {
		t.Fatalf("job ended as %q with code %q", job.Status, job.ErrorCode)
	}
	row := chain.sole(t)
	if row.Status != model.EpisodeStatusAbstained {
		t.Fatalf("status is %q, want abstained", row.Status)
	}
	// The reason has to distinguish "the model produced nothing" from "the
	// server removed everything the model produced"; they are different faults.
	if row.AbstainReason != model.AbstainAllAssertionsTrimmed {
		t.Fatalf("abstain reason is %q, want %q",
			row.AbstainReason, model.AbstainAllAssertionsTrimmed)
	}
}

// --- T12 / T13: a repeat at the same watermark reuses, and never re-asks -----

func TestC1SameWatermarkReusesWithoutCallingTheModel(t *testing.T) {
	for name, shape := range map[string]struct {
		build      func() model.EpisodeDocument
		wantStatus model.EpisodeStatus
	}{
		"a generated episode": {
			func() model.EpisodeDocument {
				d := c1Doc("用户定下 Protobuf。")
				d.Decisions = []model.EpisodeEvidence{c1Entry("用户决定采用 Protobuf。", 8101)}
				return d
			}, model.EpisodeStatusActive},
		"an abstained episode": {
			func() model.EpisodeDocument { return c1Doc("一次普通提问。") },
			model.EpisodeStatusAbstained},
	} {
		t.Run(name, func(t *testing.T) {
			chain := newC1Chain(t, c1UserOnly("那就定 Protobuf。"), shape.build(), nil)
			chain.settle(t, 8102)
			first := chain.sole(t)
			if first.Status != shape.wantStatus {
				t.Fatalf("first pass produced %q, want %q", first.Status, shape.wantStatus)
			}
			callsAfterFirst := chain.generator.Calls()
			if callsAfterFirst != 1 {
				t.Fatalf("the first projection called the model %d times", callsAfterFirst)
			}

			// Re-run the same job at the same watermark, the way a retry does.
			if err := chain.db.Exec(
				"UPDATE episode_projection_jobs SET status = ?, attempts = 0, "+
					"available_at = ?, lease_token = NULL, lease_ends_at = NULL, "+
					"completed_at = NULL, error_code = '' WHERE session_id = ?",
				model.EpisodeProjectionPending, time.Now().Add(-time.Minute),
				c1Session).Error; err != nil {
				t.Fatalf("re-enqueue: %v", err)
			}
			var job model.EpisodeProjectionJob
			if err := chain.db.Where("session_id = ?", c1Session).First(&job).Error; err != nil {
				t.Fatalf("read job: %v", err)
			}
			chain.run(t, job.ID, func(j *model.EpisodeProjectionJob) bool {
				return j.Status == model.EpisodeProjectionCompleted
			})

			if chain.generator.Calls() != callsAfterFirst {
				t.Fatalf("the repeat called the model again: %d -> %d",
					callsAfterFirst, chain.generator.Calls())
			}
			second := chain.sole(t)
			if second.ID != first.ID || second.Status != first.Status ||
				second.Version != first.Version ||
				!second.UpdatedAt.Equal(first.UpdatedAt) {
				t.Fatalf("the repeat changed the row:\n before %+v\n after  %+v",
					first, second)
			}
		})
	}
}

// --- T14: a new watermark updates the same row ------------------------------

func TestC1AdvancingWatermarkUpdatesTheSameRow(t *testing.T) {
	msgs := []c1Message{
		{id: 8101, isUser: true, text: "粘包是怎么回事？", at: c1Base},
		{id: 8102, isUser: false, text: "助手作答。", at: c1Base.Add(time.Minute)},
	}
	chain := newC1Chain(t, msgs, c1Doc("一次普通提问。"), nil)
	chain.settle(t, 8102)
	first := chain.sole(t)
	if first.Status != model.EpisodeStatusAbstained {
		t.Fatalf("first pass produced %q, want abstained", first.Status)
	}

	// A later turn carries a real decision, so the same row must become active.
	if err := chain.db.Save(&model.Message{ID: 8103, SessionID: c1Session,
		Content: "那就定 Protobuf。", IsUser: true,
		CreatedAt: c1Base.Add(2 * time.Minute)}).Error; err != nil {
		t.Fatalf("append message: %v", err)
	}
	next := c1Doc("用户定下 Protobuf。")
	next.SourceHandles = []string{"message:8101", "message:8102", "message:8103"}
	next.Decisions = []model.EpisodeEvidence{c1Entry("用户决定采用 Protobuf。", 8103)}
	chain.generator.mu.Lock()
	chain.generator.document = next
	chain.generator.mu.Unlock()

	chain.settle(t, 8103)
	second := chain.sole(t)
	if second.ID != first.ID {
		t.Fatalf("a new watermark created a second row: %s then %s", first.ID, second.ID)
	}
	if second.Status != model.EpisodeStatusActive {
		t.Fatalf("the updated row is %q, want active", second.Status)
	}
	if second.AbstainReason != "" {
		t.Fatalf("an active row still carries abstain reason %q", second.AbstainReason)
	}
	if second.SourceLastMessageID != 8103 {
		t.Fatalf("watermark is %d, want 8103", second.SourceLastMessageID)
	}
	if kinds := c1Kinds(second); len(kinds) != 1 || kinds[0] != string(model.AssertionDecision) {
		t.Fatalf("the updated row carries %v", kinds)
	}
}

// --- T15: an old document without the new slots is still readable -----------

func TestC1LegacyDocumentWithoutNewSlotsStillReads(t *testing.T) {
	legacy := []byte(`{"synopsis":"旧格式的记录","topics":[{"key":"c1","label":"C1"}],` +
		`"decisions":[{"text":"用户决定采用固定头部方案","evidence_message_ids":[8101]}],` +
		`"open_loops":[],"user_corrections":[],"source_handles":["message:8101"]}`)
	var document model.EpisodeDocument
	if err := document.Scan(legacy); err != nil {
		t.Fatalf("a document written before the new slots existed no longer decodes: %v", err)
	}
	if document.Preferences != nil || document.DurableExperiences != nil {
		t.Fatalf("decoding a legacy document invented new slots: %+v", document)
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("a legacy document no longer validates: %v", err)
	}
	episode := model.EpisodeMemory{ID: "ep-legacy", UserID: c1Owner,
		SessionID: c1Session, Document: document, Status: model.EpisodeStatusActive,
		LastMessageAt: c1Base}
	kinds := c1Kinds(episode)
	if len(kinds) != 1 || kinds[0] != string(model.AssertionDecision) {
		t.Fatalf("a legacy document now flattens to %v", kinds)
	}
	// A legacy entry has no stored time, and that must stay observable rather
	// than being back-filled with the card's own timestamp.
	for _, a := range model.AssertionsOf(episode) {
		if a.Kind == model.AssertionSynopsis {
			continue
		}
		if a.TimeKnown {
			t.Fatalf("a legacy assertion with no stored time now claims one: %v", a.OccurredAt)
		}
	}
}

// --- T16: evidence outside this session or this owner fails closed ----------

func TestC1CrossSessionAndCrossUserEvidenceFailClosed(t *testing.T) {
	for name, foreign := range map[string]struct {
		owner int64
		id    uint
	}{
		"another session of the same learner": {c1Owner, 8001},
		"another learner's session":           {c1Other, 8002},
	} {
		t.Run(name, func(t *testing.T) {
			db := openC1DB(t)
			c1SeedSession(t, db, c1Owner, c1Session, c1UserOnly("那就定 Protobuf。"))
			c1SeedSession(t, db, foreign.owner, c1Neighbour, []c1Message{
				{id: foreign.id, isUser: true, text: "别处的一条用户消息。", at: c1Base}})

			doc := c1Doc("用户定下 Protobuf。")
			doc.Decisions = []model.EpisodeEvidence{
				c1Entry("用户决定采用 Protobuf。", foreign.id)}

			repo := episodedao.NewRepository(db)
			loader, err := NewDatabaseEvidenceLoader(db, 256)
			if err != nil {
				t.Fatal(err)
			}
			projector, err := NewProjector(loader, &c1Generator{document: doc}, repo, allowAll())
			if err != nil {
				t.Fatal(err)
			}
			_, perr := projector.Project(context.Background(), c1Owner, c1Session, 8102)
			if perr == nil {
				t.Fatal("evidence from outside this session was accepted")
			}
			if !strings.Contains(perr.Error(), "source boundary") &&
				!strings.Contains(perr.Error(), ErrEpisodeSourceBoundary.Error()) {
				t.Fatalf("the refusal is not a source-boundary refusal: %v", perr)
			}
			var rows int64
			db.Model(&model.EpisodeMemory{}).Where("session_id = ?", c1Session).Count(&rows)
			if rows != 0 {
				t.Fatalf("a refused projection still wrote %d rows", rows)
			}
		})
	}
}
