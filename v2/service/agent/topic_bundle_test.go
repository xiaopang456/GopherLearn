package agent

import (
	learningdao "GopherAI/dao/learning"
	"GopherAI/model"
	"context"
	"strings"
	"testing"
	"time"
)

type stubProgress struct {
	progress *model.TopicProgress
	err      error
}

func (s stubProgress) GetTopicProgress(context.Context, int64, string) (*model.TopicProgress, error) {
	return s.progress, s.err
}

type stubAttempts struct {
	rows []learningdao.TopicAttemptRecord
	err  error
}

func (s stubAttempts) ListTopicAttemptsWithQuestions(context.Context, int64, string, int) ([]learningdao.TopicAttemptRecord, error) {
	return s.rows, s.err
}

type stubNotes struct {
	rows []model.LongTermMemory
	err  error
}

func (s stubNotes) ListByTopicKey(context.Context, int64, model.MemoryScope, string, float64, int) ([]model.LongTermMemory, error) {
	return s.rows, s.err
}

func gradedProgress() *model.TopicProgress {
	recent := time.Now().UTC()
	return &model.TopicProgress{
		UserID: 7, TopicKey: "rpc-粘包", Topic: "RPC 粘包",
		Level: model.TopicProgressDeveloping, ValidAttempts: 4,
		WeakPoints: []string{"PacketBuffer 拆包边界"}, LastAttemptAt: &recent,
	}
}

func provider(t *testing.T, progress stubProgress, attempts stubAttempts, notes stubNotes) *TopicBundleProvider {
	t.Helper()
	p, err := NewTopicBundleProvider(progress, attempts, notes, DefaultTopicBundleConfig())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The point of the redesign: one key reaches all three stores, which were
// previously keyed differently and unreachable from one another.
func TestLoadTopicJoinsProgressAttemptsAndNotes(t *testing.T) {
	p := provider(t,
		stubProgress{progress: gradedProgress()},
		stubAttempts{rows: []learningdao.TopicAttemptRecord{
			{
				LearningAttempt: model.LearningAttempt{
					UserID: 7, QuestionOrdinal: 2, EffectiveTotal: 42,
					MissingPoints: []string{"", "混淆了 TCP 流和消息", "Nagle 算法"},
				},
				QuestionText: "粘包与半包分别表现为哪种现象？",
			},
			{
				LearningAttempt: model.LearningAttempt{UserID: 7, QuestionOrdinal: 1, EffectiveTotal: 82},
			},
		}},
		stubNotes{rows: []model.LongTermMemory{
			{ID: "n1", UserID: 7, Status: model.LongTermMemoryActive, Subject: "粘包", Content: "字节数和消息边界无关。"},
		}})

	bundle, err := p.LoadTopic(context.Background(), 7, "rpc-粘包")
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Found || bundle.Topic != "RPC 粘包" {
		t.Fatalf("bundle=%+v", bundle)
	}
	if len(bundle.Notes) != 1 || bundle.Notes[0] != "粘包：字节数和消息边界无关。" {
		t.Fatalf("notes=%v", bundle.Notes)
	}
	if len(bundle.RecentAttempts) != 2 {
		t.Fatalf("attempts=%+v", bundle.RecentAttempts)
	}
	first := bundle.RecentAttempts[0]
	if first.Correct || len(first.Missing) != 2 {
		t.Fatalf("every missing point must survive, blanks aside: %+v", first)
	}
	if first.Question != "粘包与半包分别表现为哪种现象？" {
		t.Fatalf("the question text is what makes an attempt identifiable: %q", first.Question)
	}
	if !bundle.RecentAttempts[1].Correct {
		t.Fatalf("82 of 100 is above the pass mark: %+v", bundle.RecentAttempts[1])
	}
	// An attempt whose question text is missing still has to be usable.
	if bundle.RecentAttempts[1].Question != "question 1" {
		t.Fatalf("a missing question text must fall back to the ordinal: %q", bundle.RecentAttempts[1].Question)
	}
	if !strings.Contains(bundle.MasteryInstruction, "Focus on") {
		t.Fatalf("weak points must reach the instruction: %q", bundle.MasteryInstruction)
	}
	// Assessment numbers must never reach the prompt.
	for _, digit := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9"} {
		if strings.Contains(bundle.MasteryInstruction, digit) {
			t.Fatalf("mastery instruction leaked a number: %q", bundle.MasteryInstruction)
		}
	}
}

// A learner may have written notes on something they were never quizzed on.
// That is not the same as being weak at it.
func TestTopicWithNotesButNoQuizIsStillFound(t *testing.T) {
	p := provider(t,
		stubProgress{err: learningdao.ErrAttemptNotFound},
		stubAttempts{},
		stubNotes{rows: []model.LongTermMemory{
			{ID: "n1", UserID: 7, Status: model.LongTermMemoryActive, Subject: "路由分组", Content: "分组只是前缀。"},
		}})

	bundle, err := p.LoadTopic(context.Background(), 7, "gin-路由分组")
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Found || len(bundle.Notes) != 1 {
		t.Fatalf("bundle=%+v", bundle)
	}
	if bundle.MasteryInstruction != "" || len(bundle.RecentAttempts) != 0 {
		t.Fatalf("never quizzed must not produce an assessment: %+v", bundle)
	}
	if bundle.Topic != "gin-路由分组" {
		t.Fatalf("the key stands in for the name when no quiz named it: %q", bundle.Topic)
	}
}

func TestUnknownTopicIsNotFound(t *testing.T) {
	p := provider(t, stubProgress{err: learningdao.ErrAttemptNotFound}, stubAttempts{}, stubNotes{})
	bundle, err := p.LoadTopic(context.Background(), 7, "never-studied")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Found {
		t.Fatalf("nothing in any store must report not found: %+v", bundle)
	}
}

// The query filters on owner; this is the second gate, and it is the one that
// holds when the first is wrong.
func TestForeignRowsAreRejectedEvenIfTheQueryReturnsThem(t *testing.T) {
	foreign := gradedProgress()
	foreign.UserID = 8
	p := provider(t,
		stubProgress{progress: foreign},
		stubAttempts{rows: []learningdao.TopicAttemptRecord{
			{LearningAttempt: model.LearningAttempt{UserID: 8, QuestionOrdinal: 1, EffectiveTotal: 90}},
		}},
		stubNotes{rows: []model.LongTermMemory{
			{ID: "n1", UserID: 8, Status: model.LongTermMemoryActive, Subject: "别人的", Content: "别人的笔记。"},
			{ID: "n2", UserID: 7, Status: model.LongTermMemoryArchived, Subject: "归档的", Content: "已归档。"},
		}})

	bundle, err := p.LoadTopic(context.Background(), 7, "rpc-粘包")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Notes) != 0 || len(bundle.RecentAttempts) != 0 || bundle.MasteryInstruction != "" {
		t.Fatalf("another learner's rows and archived rows must not survive: %+v", bundle)
	}
}

func TestLoadTopicRequiresIdentityAndSurfacesStoreFailures(t *testing.T) {
	p := provider(t, stubProgress{err: learningdao.ErrAttemptNotFound}, stubAttempts{}, stubNotes{})
	if _, err := p.LoadTopic(context.Background(), 0, "rpc-粘包"); err == nil {
		t.Error("a missing user must not silently load a topic")
	}
	if _, err := p.LoadTopic(context.Background(), 7, "   "); err == nil {
		t.Error("a blank key must be rejected")
	}

	failing := provider(t, stubProgress{err: context.DeadlineExceeded}, stubAttempts{}, stubNotes{})
	if _, err := failing.LoadTopic(context.Background(), 7, "rpc-粘包"); err == nil {
		t.Error("a store failure is not an empty topic and must surface")
	}
}

func TestTopicBundleConfigRejectsUnboundedOrImpossibleSettings(t *testing.T) {
	if err := DefaultTopicBundleConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, config := range map[string]TopicBundleConfig{
		"no notes":       {MaxNotes: 0, MaxAttempts: 5, MinConfidence: 0.6, PassMark: 60},
		"unbounded":      {MaxNotes: 500, MaxAttempts: 5, MinConfidence: 0.6, PassMark: 60},
		"bad confidence": {MaxNotes: 8, MaxAttempts: 5, MinConfidence: 2, PassMark: 60},
		"bad pass mark":  {MaxNotes: 8, MaxAttempts: 5, MinConfidence: 0.6, PassMark: 0},
		"pass mark high": {MaxNotes: 8, MaxAttempts: 5, MinConfidence: 0.6, PassMark: 200},
	} {
		if err := config.Validate(); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
	if _, err := NewTopicBundleProvider(nil, stubAttempts{}, stubNotes{}, DefaultTopicBundleConfig()); err == nil {
		t.Error("a provider without a progress reader cannot join anything")
	}
}

// A binary pass mark announced an answer scoring 72 as 答对 and then listed
// three things it missed. The three bands exist to stop the prompt contradicting
// itself.
func TestVerdictSeparatesCleanPassesFromPassesWithGaps(t *testing.T) {
	cases := []struct {
		name    string
		attempt TopicAttempt
		want    string
	}{
		{"clean pass", TopicAttempt{Correct: true}, "答对"},
		{"passed with gaps", TopicAttempt{Correct: true, Missing: []string{"PacketBuffer 的缓冲作用"}}, "答对但有遗漏"},
		{"failed", TopicAttempt{Missing: []string{"拆包机制"}}, "答错"},
		{"failed with nothing recorded", TopicAttempt{}, "答错"},
	}
	for _, c := range cases {
		if got := c.attempt.Verdict(); got != c.want {
			t.Errorf("%s: verdict = %q, want %q", c.name, got, c.want)
		}
	}
}

// Seventy-two out of a hundred is a pass under the old mark and a failure under
// the current one. The grader scores correctness out of sixty alone, so sixty
// overall could be reached while missing most of completeness.
func TestPassMarkRejectsAnswersThatMissMostOfCompleteness(t *testing.T) {
	if mark := DefaultTopicBundleConfig().PassMark; mark != 75 {
		t.Fatalf("pass mark = %d, want 75", mark)
	}
	p := provider(t,
		stubProgress{progress: gradedProgress()},
		stubAttempts{rows: []learningdao.TopicAttemptRecord{
			{LearningAttempt: model.LearningAttempt{UserID: 7, QuestionOrdinal: 1, EffectiveTotal: 72}},
			{LearningAttempt: model.LearningAttempt{UserID: 7, QuestionOrdinal: 2, EffectiveTotal: 82}},
		}},
		stubNotes{})
	bundle, err := p.LoadTopic(context.Background(), 7, "rpc-粘包")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.RecentAttempts[0].Correct {
		t.Error("72 of 100 must not count as a pass")
	}
	if !bundle.RecentAttempts[1].Correct {
		t.Error("82 of 100 must count as a pass")
	}
}
