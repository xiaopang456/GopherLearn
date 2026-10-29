package domain

import (
	"strings"
	"testing"
	"time"
)

func TestLearningSessionStatusSetIsClosed(t *testing.T) {
	statuses := []LearningSessionStatus{
		LearningSessionCreated, LearningSessionAsking, LearningSessionWaitingAnswer,
		LearningSessionGrading, LearningSessionCompleted, LearningSessionCancelled,
		LearningSessionExpired,
	}
	if len(statuses) != 7 {
		t.Fatalf("the quiz has seven states, got %d", len(statuses))
	}
	for _, status := range statuses {
		if !status.IsValid() {
			t.Errorf("%q must be a valid state", status)
		}
	}
	for _, name := range []LearningSessionStatus{"in_progress", "answered", "grading ", "", "FAILED"} {
		if name.IsValid() {
			t.Errorf("%q must not be a learning session state", name)
		}
	}
	// Only three of the seven are exits. A state that is terminal but reported
	// otherwise would keep accepting transitions forever.
	terminal := map[LearningSessionStatus]bool{
		LearningSessionCompleted: true, LearningSessionCancelled: true, LearningSessionExpired: true,
	}
	for _, status := range statuses {
		if status.IsTerminal() != terminal[status] {
			t.Errorf("%q terminal = %v, want %v", status, status.IsTerminal(), terminal[status])
		}
	}
}

func TestLearningSessionTransitionsAreExactlyTheDeclaredEdges(t *testing.T) {
	// The full matrix, asserted pair by pair, so adding a state or an edge cannot
	// pass by being unnoticed. The one backward edge in the machine is the
	// recoverable-grading one, and it is the easiest thing in this file to break.
	all := []LearningSessionStatus{
		LearningSessionCreated, LearningSessionAsking, LearningSessionWaitingAnswer,
		LearningSessionGrading, LearningSessionCompleted, LearningSessionCancelled,
		LearningSessionExpired,
	}
	allowed := map[LearningSessionStatus][]LearningSessionStatus{
		LearningSessionCreated: {LearningSessionAsking, LearningSessionCancelled, LearningSessionExpired},
		LearningSessionAsking:  {LearningSessionCreated, LearningSessionWaitingAnswer, LearningSessionCancelled, LearningSessionExpired},
		LearningSessionWaitingAnswer: {LearningSessionGrading, LearningSessionCompleted,
			LearningSessionCancelled, LearningSessionExpired},
		LearningSessionGrading: {LearningSessionWaitingAnswer, LearningSessionCompleted,
			LearningSessionCancelled, LearningSessionExpired},
		LearningSessionCompleted: nil,
		LearningSessionCancelled: nil,
		LearningSessionExpired:   nil,
	}
	for _, from := range all {
		wanted := map[LearningSessionStatus]bool{}
		for _, to := range allowed[from] {
			wanted[to] = true
		}
		for _, to := range all {
			if from.CanTransitionTo(to) != wanted[to] {
				t.Errorf("%q -> %q = %v, want %v", from, to, from.CanTransitionTo(to), wanted[to])
			}
		}
	}
}

func TestLearningStatusesRefuseUnknownSides(t *testing.T) {
	unknown := LearningSessionStatus("sOMething")
	if unknown.CanTransitionTo(LearningSessionAsking) || LearningSessionCreated.CanTransitionTo(unknown) {
		t.Fatal("an unknown state must not be reachable from anywhere or lead anywhere")
	}
	if LearningSessionCreated.CanTransitionTo(LearningSessionCreated) {
		t.Fatal("a state is not a transition")
	}
	// An expired quiz is an exit: "answer it anyway, it's only a deadline" is the
	// failure this closes.
	if LearningSessionExpired.CanTransitionTo(LearningSessionWaitingAnswer) {
		t.Fatal("an expired quiz cannot be brought back")
	}
}

func TestLearningAttemptAndQuestionAndLevelSetsAreClosed(t *testing.T) {
	for _, status := range []LearningAttemptStatus{
		LearningAttemptGrading, LearningAttemptCompleted, LearningAttemptFailed, LearningAttemptSkipped,
	} {
		if !status.IsValid() {
			t.Errorf("%q must be a valid attempt state", status)
		}
	}
	if LearningAttemptStatus("graded").IsValid() || LearningAttemptStatus("").IsValid() {
		t.Fatal("an invented attempt state must not compile its way in")
	}
	for _, kind := range []LearningQuestionType{
		LearningQuestionShortAnswer, LearningQuestionConceptComparison, LearningQuestionCodeAnalysis,
	} {
		if !kind.IsValid() {
			t.Errorf("%q must be a valid question type", kind)
		}
	}
	// Multiple choice is deliberately absent: it can be passed by elimination.
	if LearningQuestionType("multiple_choice").IsValid() {
		t.Fatal("multiple choice is not a question type here")
	}
	for _, level := range []TopicProgressLevel{
		TopicProgressInsufficient, TopicProgressDeveloping, TopicProgressMastered,
	} {
		if !level.IsValid() {
			t.Errorf("%q must be a valid level", level)
		}
	}
	// The original defined three levels and validated none, so a fourth could be
	// constructed and stored. That is what this assertion is for.
	if TopicProgressLevel("weak").IsValid() {
		t.Fatal("there is no fourth progress level")
	}
	for _, code := range []LearningFailureCode{
		LearningFailureGradingFailed, LearningFailureInvalidGrade, LearningFailureLeaseExpired,
		LearningFailureModelUnavailable,
	} {
		if !code.IsValid() {
			t.Errorf("%q must be a valid failure code", code)
		}
	}
	if LearningFailureCode("dial tcp: connection refused").IsValid() {
		t.Fatal("a provider's own words are not a failure code")
	}
}

func TestLearningTopicKeyNormalizationIsDeterministic(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Redis 持久化", "redis-持久化"},
		{"  RAG   /  RRF  ", "rag-rrf"},
		{"CAP 定理", "cap-定理"},
		{"---", ""},
		{"Go's slices", "go-s-slices"},
		{"2026-09-26", "2026-09-26"},
		{"中文", "中文"},
		{"", ""},
		{"\t\n  ", ""},
	}
	for _, testCase := range cases {
		if got := NormalizeLearningTopicKey(testCase.in); got != testCase.want {
			t.Errorf("NormalizeLearningTopicKey(%q) = %q, want %q", testCase.in, got, testCase.want)
		}
	}
	// Case and punctuation differences must collapse to one key, or twenty spellings
	// of one subject become twenty unrelated progress records.
	if NormalizeLearningTopicKey("MySQL 主从复制") != NormalizeLearningTopicKey("mysql-主从复制") {
		t.Fatal("two spellings of one topic must share one key")
	}
	long := NormalizeLearningTopicKey(strings.Repeat("长", MaxLearningTopicKeyRunes+40))
	if len([]rune(long)) != MaxLearningTopicKeyRunes {
		t.Fatalf("topic key must truncate at %d runes, got %d", MaxLearningTopicKeyRunes, len([]rune(long)))
	}
	// Truncation must not leave a trailing separator, which would make the stored key
	// differ from the normalized form of itself.
	if strings.HasSuffix(NormalizeLearningTopicKey(strings.Repeat("a", MaxLearningTopicKeyRunes)+"-b"), "-") {
		t.Fatal("a truncated topic key must not end in a separator")
	}
}

func TestLearningSessionValidateForCreate(t *testing.T) {
	valid := func() *LearningSession {
		now := time.Now()
		return &LearningSession{
			ID: "lsn_1", UserID: "usr_1", SessionID: "ses_1",
			Topic: "Redis 持久化", TopicKey: NormalizeLearningTopicKey("Redis 持久化"),
			Status: LearningSessionCreated, CurrentQuestion: 1, TotalQuestions: LearningQuestionCount,
			Version: 1, ExpiresAt: now.Add(LearningDefaultLifetime), CreatedAt: now,
		}
	}
	if err := valid().ValidateForCreate(); err != nil {
		t.Fatalf("the assembled quiz must be lawful: %v", err)
	}
	// The store assigns identifiers, so a caller must be able to describe a new quiz
	// without inventing one it does not own.
	unnamed := valid()
	unnamed.ID = ""
	if err := unnamed.ValidateForCreate(); err != nil {
		t.Fatalf("a quiz without an id must be describable: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*LearningSession)
	}{
		{"no owner", func(s *LearningSession) { s.UserID = "" }},
		{"no chat session", func(s *LearningSession) { s.SessionID = "" }},
		{"no topic", func(s *LearningSession) { s.Topic = "  " }},
		{"no topic key", func(s *LearningSession) { s.TopicKey = "" }},
		{"born asking", func(s *LearningSession) { s.Status = LearningSessionAsking }},
		{"born waiting", func(s *LearningSession) { s.Status = LearningSessionWaitingAnswer }},
		{"born completed", func(s *LearningSession) { s.Status = LearningSessionCompleted }},
		{"version two", func(s *LearningSession) { s.Version = 2 }},
		{"starting on question two", func(s *LearningSession) { s.CurrentQuestion = 2 }},
		{"no questions", func(s *LearningSession) { s.TotalQuestions = 0 }},
		{"five questions", func(s *LearningSession) { s.TotalQuestions = 5 }},
		{"no deadline", func(s *LearningSession) { s.ExpiresAt = time.Time{} }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			session := valid()
			testCase.mutate(session)
			if err := session.ValidateForCreate(); err == nil {
				t.Fatal("an unlawful new quiz must be refused")
			}
		})
	}
}

func TestLearningSessionActiveKeyAndExpiry(t *testing.T) {
	session := LearningSession{UserID: "usr_1", SessionID: "ses_1"}
	if session.ActiveKey() != "usr_1:ses_1" {
		t.Fatalf("active key = %q", session.ActiveKey())
	}
	if (&LearningSession{UserID: "usr_1"}).ActiveKey() == session.ActiveKey() {
		t.Fatal("a quiz with no chat session must not collide with one that has one")
	}

	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	open := LearningSession{Status: LearningSessionWaitingAnswer, ExpiresAt: now.Add(time.Second)}
	if open.IsExpired(now) {
		t.Fatal("a quiz is not expired before its deadline")
	}
	if !(LearningSession{Status: LearningSessionWaitingAnswer, ExpiresAt: now}).IsExpired(now) {
		t.Fatal("a quiz is expired at its deadline")
	}
	if !(LearningSession{Status: LearningSessionGrading, ExpiresAt: now.Add(-time.Second)}).IsExpired(now) {
		t.Fatal("a quiz being graded can still expire")
	}
	if (LearningSession{Status: LearningSessionCompleted, ExpiresAt: now.Add(-time.Hour)}).IsExpired(now) {
		t.Fatal("a finished quiz has no deadline left to pass")
	}
	if (LearningSession{Status: LearningSessionWaitingAnswer}).IsExpired(now) {
		t.Fatal("a quiz with no deadline cannot expire")
	}
}

func TestLearningEvidenceRules(t *testing.T) {
	chunk := DocumentChunk{
		ID: "chk_9", DocumentID: "doc_3", CollectionID: "col_1", Version: 2, Order: 4,
		Section: "持久化", Text: "RDB 快照与 AOF 追加日志。", StartLine: 10, EndLine: 18,
	}
	evidence := LearningEvidenceFromChunk(chunk, "Redis 笔记", 1)
	if err := evidence.Validate(); err != nil {
		t.Fatalf("evidence derived from a chunk must be lawful: %v", err)
	}
	if evidence.Identity() != "doc_3:v2:chk_9" {
		t.Fatalf("identity = %q", evidence.Identity())
	}
	if same := LearningEvidenceFromChunk(chunk, "Redis 笔记", 2); same.Identity() != evidence.Identity() {
		t.Fatal("two references to one chunk must share one identity")
	}
	edited := chunk
	edited.ID = "chk_10"
	edited.Version = 3
	if LearningEvidenceFromChunk(edited, "Redis 笔记", 1).Identity() == evidence.Identity() {
		t.Fatal("a new version of a document is not the same evidence")
	}

	cases := []struct {
		name   string
		mutate func(*LearningEvidence)
	}{
		{"zero ordinal", func(e *LearningEvidence) { e.Ordinal = 0 }},
		{"no document", func(e *LearningEvidence) { e.DocumentID = " " }},
		{"no chunk", func(e *LearningEvidence) { e.ChunkID = "" }},
		{"version zero", func(e *LearningEvidence) { e.DocumentVer = 0 }},
		{"inverted lines", func(e *LearningEvidence) { e.EndLine = e.StartLine - 1 }},
		{"no start line", func(e *LearningEvidence) { e.StartLine = 0 }},
		{"empty snapshot", func(e *LearningEvidence) { e.Snapshot = "  " }},
		{"oversized snapshot", func(e *LearningEvidence) { e.Snapshot = strings.Repeat("字", MaxLearningEvidenceRunes+1) }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			item := LearningEvidenceFromChunk(chunk, "Redis 笔记", 1)
			testCase.mutate(&item)
			if err := item.Validate(); err == nil {
				t.Fatal("unlawful evidence must be refused")
			}
		})
	}
}

func TestLearningCodeEvidenceIsAboutActualCode(t *testing.T) {
	withCode := []LearningEvidence{{Snapshot: "```go\nfunc main() {}\n```"}}
	if !HasCodeEvidence(withCode) {
		t.Fatal("a fenced block is code evidence")
	}
	if HasCodeEvidence([]LearningEvidence{{Snapshot: "RDB 快照恢复速度快。"}}) {
		t.Fatal("prose about code is not code")
	}
	if HasCodeEvidence(nil) {
		t.Fatal("no evidence is no code")
	}
}

func TestLearningQuestionValidateForCreate(t *testing.T) {
	valid := func() *LearningQuestion {
		return &LearningQuestion{
			ID: "lq_1", UserID: "usr_1", LearningSessionID: "lsn_1", Ordinal: 1,
			Type: LearningQuestionShortAnswer, Question: "Redis 的 RDB 与 AOF 各自解决什么问题？",
			AssessmentGoal: "说明两种持久化方式的目标差异", ReferenceAnswer: "RDB 面向恢复速度，AOF 面向持久性。",
			Rubric:   []string{"指出 RDB 是快照", "指出 AOF 是追加日志"},
			Evidence: []LearningEvidence{{Ordinal: 1, DocumentID: "doc_1", ChunkID: "chk_1", DocumentVer: 1, StartLine: 1, EndLine: 4, Snapshot: "RDB 与 AOF。"}},
		}
	}
	if err := valid().ValidateForCreate(); err != nil {
		t.Fatalf("the assembled question must be lawful: %v", err)
	}
	unnamed := valid()
	unnamed.ID = ""
	if err := unnamed.ValidateForCreate(); err != nil {
		t.Fatalf("a question without an id must be describable: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*LearningQuestion)
	}{
		{"no owner", func(q *LearningQuestion) { q.UserID = "" }},
		{"no quiz", func(q *LearningQuestion) { q.LearningSessionID = "" }},
		{"ordinal zero", func(q *LearningQuestion) { q.Ordinal = 0 }},
		{"unknown type", func(q *LearningQuestion) { q.Type = "multiple_choice" }},
		{"no text", func(q *LearningQuestion) { q.Question = " " }},
		{"oversized text", func(q *LearningQuestion) { q.Question = strings.Repeat("问", MaxLearningQuestionRunes+1) }},
		{"no goal", func(q *LearningQuestion) { q.AssessmentGoal = "" }},
		{"oversized goal", func(q *LearningQuestion) { q.AssessmentGoal = strings.Repeat("目", MaxLearningGoalRunes+1) }},
		{"no reference answer", func(q *LearningQuestion) { q.ReferenceAnswer = "" }},
		{"oversized reference", func(q *LearningQuestion) { q.ReferenceAnswer = strings.Repeat("答", MaxLearningReferenceRunes+1) }},
		{"empty rubric", func(q *LearningQuestion) { q.Rubric = nil }},
		{"too many rubric items", func(q *LearningQuestion) { q.Rubric = repeatItems(MaxLearningRubricItems + 1) }},
		{"blank rubric item", func(q *LearningQuestion) { q.Rubric = []string{"  "} }},
		{"oversized rubric item", func(q *LearningQuestion) { q.Rubric = []string{strings.Repeat("点", MaxLearningRubricItemRunes+1)} }},
		{"no evidence", func(q *LearningQuestion) { q.Evidence = nil }},
		{"too much evidence", func(q *LearningQuestion) {
			q.Evidence = repeatEvidence(MaxLearningEvidenceItems + 1)
		}},
		{"unlawful evidence", func(q *LearningQuestion) { q.Evidence = []LearningEvidence{{Ordinal: 1}} }},
		// A code question over prose is the shape of a question that cannot be
		// answered from what the learner actually stored.
		{"code question without code", func(q *LearningQuestion) { q.Type = LearningQuestionCodeAnalysis }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			question := valid()
			testCase.mutate(question)
			if err := question.ValidateForCreate(); err == nil {
				t.Fatal("an unlawful question must be refused")
			}
		})
	}

	// The one positive case that is easy to get wrong: a code question is lawful as
	// soon as one evidence item really carries a fenced block.
	code := valid()
	code.Type = LearningQuestionCodeAnalysis
	code.Evidence = []LearningEvidence{{Ordinal: 1, DocumentID: "doc_1", ChunkID: "chk_1", DocumentVer: 1,
		StartLine: 1, EndLine: 6, Snapshot: "```go\nvar x int\n```"}}
	if err := code.ValidateForCreate(); err != nil {
		t.Fatalf("a code question over code must be accepted: %v", err)
	}
}

func TestLearningGradeTotalAndBounds(t *testing.T) {
	grade := LearningGrade{
		Correctness: 48, Completeness: 20, Clarity: 12,
		Feedback: "两个目标都答到了，但没说清取舍。", MissingPoints: []string{"恢复速度与写入开销的取舍"},
		Confidence: 0.8,
	}
	if grade.Total() != 80 {
		t.Fatalf("total = %d, want 80", grade.Total())
	}
	if err := grade.Validate(); err != nil {
		t.Fatalf("the assembled grade must be lawful: %v", err)
	}
	// The ceilings sum to 100, which is what a mastery score means.
	if MaxLearningCorrectness+MaxLearningCompleteness+MaxLearningClarity != 100 {
		t.Fatal("the three dimensions must add up to a hundred")
	}

	cases := []struct {
		name   string
		mutate func(*LearningGrade)
	}{
		{"correctness above the ceiling", func(g *LearningGrade) { g.Correctness = MaxLearningCorrectness + 1 }},
		{"correctness negative", func(g *LearningGrade) { g.Correctness = -1 }},
		{"completeness above the ceiling", func(g *LearningGrade) { g.Completeness = MaxLearningCompleteness + 1 }},
		{"clarity above the ceiling", func(g *LearningGrade) { g.Clarity = MaxLearningClarity + 1 }},
		{"confidence above one", func(g *LearningGrade) { g.Confidence = 1.01 }},
		{"confidence negative", func(g *LearningGrade) { g.Confidence = -0.5 }},
		{"no feedback", func(g *LearningGrade) { g.Feedback = " " }},
		{"oversized feedback", func(g *LearningGrade) { g.Feedback = strings.Repeat("点", MaxLearningFeedbackRunes+1) }},
		{"too many missing points", func(g *LearningGrade) { g.MissingPoints = repeatItems(MaxLearningMissingPoints + 1) }},
		{"blank missing point", func(g *LearningGrade) { g.MissingPoints = []string{"   "} }},
		{"oversized missing point", func(g *LearningGrade) {
			g.MissingPoints = []string{strings.Repeat("缺", MaxLearningMissingPointRunes+1)}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			item := grade
			item.MissingPoints = append([]string(nil), grade.MissingPoints...)
			testCase.mutate(&item)
			if err := item.Validate(); err == nil {
				t.Fatal("an unlawful grade must be refused")
			}
		})
	}
	// A model that returns a score of "100" for correctness is out of range even
	// though the total would look plausible: the dimensions are the contract.
	if (LearningGrade{Correctness: 100, Feedback: "ok"}).Validate() == nil {
		t.Fatal("a single dimension cannot carry the whole score")
	}
}

func TestLearningAttemptLeaseRules(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	later := now.Add(30 * time.Second)
	earlier := now.Add(-30 * time.Second)
	completed := now.Add(-time.Minute)

	live := LearningAttempt{Status: LearningAttemptGrading, LeaseExpiresAt: &later}
	if !live.HoldsLease(now) {
		t.Fatal("a grading attempt with a future deadline holds a lease")
	}
	if live.LeaseExpired(now) {
		t.Fatal("a live lease has not expired")
	}

	stale := LearningAttempt{Status: LearningAttemptGrading, LeaseExpiresAt: &earlier}
	if stale.HoldsLease(now) || !stale.LeaseExpired(now) {
		t.Fatal("a grading attempt past its deadline is reclaimable")
	}

	// A row that claims to be grading but cannot say until when is abandoned work,
	// not live work. Treating the missing deadline as "never expires" would strand
	// the quiz forever.
	unknown := LearningAttempt{Status: LearningAttemptGrading}
	if unknown.HoldsLease(now) || !unknown.LeaseExpired(now) {
		t.Fatal("a grading attempt with no lease deadline is expired")
	}

	finished := LearningAttempt{Status: LearningAttemptCompleted, LeaseExpiresAt: &earlier, CompletedAt: &completed}
	if finished.HoldsLease(now) || finished.LeaseExpired(now) {
		t.Fatal("a finished attempt holds no lease whatever its columns say")
	}
}

func TestLearningAttemptValidateForClaim(t *testing.T) {
	now := time.Now()
	valid := func() *LearningAttempt {
		return &LearningAttempt{
			ID: "lat_1", UserID: "usr_1", SessionID: "ses_1", LearningSessionID: "lsn_1",
			QuestionID: "lq_1", QuestionOrdinal: 1, SubmissionID: "sub-1", TopicKey: "redis-持久化",
			Status: LearningAttemptGrading, Answer: "RDB 快照、AOF 追加日志。",
			Rubric: []string{"指出 RDB 是快照"}, Evidence: []LearningEvidence{{
				Ordinal: 1, DocumentID: "doc_1", ChunkID: "chk_1", DocumentVer: 1, StartLine: 1, EndLine: 3, Snapshot: "RDB。",
			}},
			LeaseToken: "lease-1", LeaseExpiresAt: &now, CreatedAt: now,
		}
	}
	if err := valid().ValidateForClaim(); err != nil {
		t.Fatalf("the assembled claim must be lawful: %v", err)
	}
	unnamed := valid()
	unnamed.ID = ""
	if err := unnamed.ValidateForClaim(); err != nil {
		t.Fatalf("an attempt without an id must be describable: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*LearningAttempt)
	}{
		{"no owner", func(a *LearningAttempt) { a.UserID = "" }},
		{"no chat session", func(a *LearningAttempt) { a.SessionID = "" }},
		{"no quiz", func(a *LearningAttempt) { a.LearningSessionID = "" }},
		{"no question", func(a *LearningAttempt) { a.QuestionID = "" }},
		{"ordinal zero", func(a *LearningAttempt) { a.QuestionOrdinal = 0 }},
		{"no submission id", func(a *LearningAttempt) { a.SubmissionID = "" }},
		{"no topic key", func(a *LearningAttempt) { a.TopicKey = "" }},
		{"born completed", func(a *LearningAttempt) { a.Status = LearningAttemptCompleted }},
		{"empty answer", func(a *LearningAttempt) { a.Answer = " " }},
		{"oversized answer", func(a *LearningAttempt) { a.Answer = strings.Repeat("答", MaxLearningAnswerRunes+1) }},
		{"no lease token", func(a *LearningAttempt) { a.LeaseToken = "" }},
		{"no lease deadline", func(a *LearningAttempt) { a.LeaseExpiresAt = nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			attempt := valid()
			testCase.mutate(attempt)
			if err := attempt.ValidateForClaim(); err == nil {
				t.Fatal("an unlawful claim must be refused")
			}
		})
	}
}

func TestTopicProgressProjectionMatchesTheDeclaredArithmetic(t *testing.T) {
	// Three attempts scoring 90, 60 and 30 newest first carry weights 3, 2 and 1:
	// (270 + 120 + 30) / 6 = 70. The numbers are chosen so every part of the sum is
	// visible, and the result lands below the mastered bar so this case cannot be
	// satisfied by a formula that only looks at the maximum.
	completed := []LearningAttempt{
		{EffectiveTotal: 90, CompletedAt: timePointer(time.Unix(300, 0))},
		{EffectiveTotal: 60, CompletedAt: timePointer(time.Unix(200, 0))},
		{EffectiveTotal: 30, CompletedAt: timePointer(time.Unix(100, 0))},
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	progress := ProjectTopicProgress("usr_1", "redis-持久化", "Redis 持久化", completed, now)

	if progress.Mastery != 70 {
		t.Fatalf("mastery = %d, want 70", progress.Mastery)
	}
	if progress.Confidence != 0.5 {
		t.Fatalf("confidence = %v, want 0.5", progress.Confidence)
	}
	if progress.ValidCount != 3 {
		t.Fatalf("valid attempts = %d, want 3", progress.ValidCount)
	}
	if progress.Level != TopicProgressDeveloping {
		t.Fatalf("level = %q, want developing", progress.Level)
	}
	if progress.LastAttempt == nil || !progress.LastAttempt.Equal(time.Unix(300, 0).UTC()) {
		t.Fatalf("last attempt = %v, want the newest one", progress.LastAttempt)
	}
	if !progress.CreatedAt.Equal(now) || !progress.UpdatedAt.Equal(now) {
		t.Fatalf("the projection must record the clock it was given: %+v", progress)
	}
	if err := progress.Validate(); err != nil {
		t.Fatalf("the projection must be storable: %v", err)
	}

	// Order is part of the arithmetic: feeding the same attempts oldest first gives
	// (270 + 120 + 30) with weights reversed, which is 50, not 70.
	reversed := []LearningAttempt{completed[2], completed[1], completed[0]}
	if got := ProjectTopicProgress("usr_1", "k", "K", reversed, now).Mastery; got == 70 {
		t.Fatalf("a differently ordered input gave the same mastery (%d): the weights must follow recency", got)
	}
}

func TestTopicProgressLevelsAtTheDeclaredBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		totals      []int
		wantMastery int
		wantLevel   TopicProgressLevel
	}{
		// Fewer than three valid answers is never a description of a person.
		{"no attempts", nil, 0, TopicProgressInsufficient},
		{"one attempt", []int{90}, 90, TopicProgressInsufficient},
		{"two attempts", []int{90, 80}, 87, TopicProgressInsufficient},
		// Three attempts is where confidence reaches 0.5, so the mastered bar can
		// finally be cleared - but only at 85, and 85 clears it.
		{"three at the bar", []int{85, 85, 85}, 85, TopicProgressMastered},
		{"three just below", []int{80, 85, 90}, 83, TopicProgressDeveloping},
		{"three weak", []int{40, 40, 40}, 40, TopicProgressDeveloping},
		// Six attempts saturate confidence; a strong recent answer is weighted up
		// and the plateau of ordinary answers is weighted down.
		{"six mixed", []int{100, 60, 60, 60, 60, 60}, 71, TopicProgressDeveloping},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			attempts := make([]LearningAttempt, 0, len(testCase.totals))
			for index, total := range testCase.totals {
				attempt := LearningAttempt{EffectiveTotal: total}
				attempt.CompletedAt = timePointer(time.Unix(int64(1000-index), 0).UTC())
				attempts = append(attempts, attempt)
			}
			progress := ProjectTopicProgress("usr_1", "k", "K", attempts, now)
			if progress.Mastery != testCase.wantMastery {
				t.Fatalf("mastery = %d, want %d", progress.Mastery, testCase.wantMastery)
			}
			if progress.Level != testCase.wantLevel {
				t.Fatalf("level = %q, want %q", progress.Level, testCase.wantLevel)
			}
			if progress.ValidCount != len(testCase.totals) {
				t.Fatalf("valid attempts = %d, want %d", progress.ValidCount, len(testCase.totals))
			}
		})
	}
}

func TestTopicProgressCountsWeakPointsByFrequency(t *testing.T) {
	now := time.Now()
	attempt := func(total int, missing ...string) LearningAttempt {
		return LearningAttempt{
			EffectiveTotal: total,
			Grade:          LearningGrade{MissingPoints: missing, Feedback: "ok"},
			CompletedAt:    timePointer(time.Unix(int64(total), 0).UTC()),
		}
	}
	completed := []LearningAttempt{
		attempt(60, "缓存击穿的判定", "  一致性哈希的   虚拟节点  ", "锁粒度"),
		attempt(55, "缓存击穿的判定", "回源风暴"),
		// The blanks are what an untrimmed model output looks like; they are not
		// weak points and must not be counted as ones.
		attempt(50, "", "   ", "虚拟节点 的 一致性哈希"),
	}

	progress := ProjectTopicProgress("usr_1", "redis", "Redis", completed, now)

	// "缓存击穿的判定" appears twice, so it leads. The two virtual-node wordings
	// differ as text, so they are counted separately: no semantic clustering happens
	// here, and a future reader must not assume "缓存击穿" and "缓存击穿与穿透" are one
	// idea.
	want := []string{"缓存击穿的判定", "一致性哈希的   虚拟节点", "锁粒度", "回源风暴", "虚拟节点 的 一致性哈希"}
	if strings.Join(progress.WeakPoints, "|") != strings.Join(want, "|") {
		t.Fatalf("weak points = %q, want %q", progress.WeakPoints, want)
	}

	// Blanks are not weak points.
	for _, point := range progress.WeakPoints {
		if strings.TrimSpace(point) == "" {
			t.Fatal("a blank weak point must not be stored")
		}
	}

	// The cap is on distinct counted points, and the most frequent survive.
	//
	// Eight is stated as a literal rather than as MaxLearningWeakPoints on both sides: a
	// test that compares a value to the constant that produced it stays green no matter
	// where the constant is moved, and this one is a product decision, not an
	// implementation detail. It bounds how many gaps a learner is shown at once, how many
	// phrases the mastery note may name, and the size of the JSON blob written on every
	// graded answer. Raising it should be a deliberate edit that breaks something here.
	const weakPointCap = 8
	if MaxLearningWeakPoints != weakPointCap {
		t.Fatalf("the weak-point cap is %d, but %d is what the progress view, the mastery note and the stored blob are sized for",
			MaxLearningWeakPoints, weakPointCap)
	}
	many := make([]string, 0, weakPointCap+6)
	for index := range weakPointCap + 6 {
		many = append(many, strings.Repeat("缺", index+1))
	}
	capped := ProjectTopicProgress("usr_1", "redis", "Redis",
		[]LearningAttempt{attempt(40, many...)}, now)
	if len(capped.WeakPoints) != weakPointCap {
		t.Fatalf("weak points = %d, want the cap %d", len(capped.WeakPoints), weakPointCap)
	}
	if err := capped.Validate(); err != nil {
		t.Fatalf("a capped projection must still be storable: %v", err)
	}
}

func TestTopicProgressValidate(t *testing.T) {
	now := time.Now()
	valid := TopicProgress{UserID: "usr_1", TopicKey: "k", Topic: "K", Mastery: 70,
		Confidence: 0.5, ValidCount: 3, Level: TopicProgressDeveloping, WeakPoints: []string{"a"},
		CreatedAt: now, UpdatedAt: now}
	if err := valid.Validate(); err != nil {
		t.Fatalf("the assembled projection must be storable: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*TopicProgress)
	}{
		{"no owner", func(p *TopicProgress) { p.UserID = "" }},
		{"no topic key", func(p *TopicProgress) { p.TopicKey = "" }},
		{"mastery above a hundred", func(p *TopicProgress) { p.Mastery = 101 }},
		{"mastery negative", func(p *TopicProgress) { p.Mastery = -1 }},
		{"confidence above one", func(p *TopicProgress) { p.Confidence = 1.5 }},
		{"negative count", func(p *TopicProgress) { p.ValidCount = -1 }},
		{"unknown level", func(p *TopicProgress) { p.Level = "weak" }},
		{"too many weak points", func(p *TopicProgress) { p.WeakPoints = repeatItems(MaxLearningWeakPoints + 1) }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			item := valid
			item.WeakPoints = append([]string(nil), valid.WeakPoints...)
			testCase.mutate(&item)
			if err := item.Validate(); err == nil {
				t.Fatal("an unlawful projection must be refused")
			}
		})
	}
}

func TestTopicProgressIsReliableOnlyPastTheStatedBar(t *testing.T) {
	now := time.Now()
	// The gate that lets a weak point be re-tested and lets the agent read a mastery
	// note is one decision with one owner, so a projection that cannot carry either
	// claim must say so identically.
	weak := ProjectTopicProgress("usr_1", "k", "K", []LearningAttempt{{EffectiveTotal: 95}}, now)
	if weak.IsReliable() {
		t.Fatal("one attempt is never a reliable picture of a learner")
	}
	strong := ProjectTopicProgress("usr_1", "k", "K", []LearningAttempt{
		{EffectiveTotal: 95}, {EffectiveTotal: 90}, {EffectiveTotal: 92},
	}, now)
	if !strong.IsReliable() {
		t.Fatalf("three valid attempts must be reliable: %+v", strong)
	}
	if (TopicProgress{UserID: "u", TopicKey: "k", Level: TopicProgressInsufficient, ValidCount: 9}).IsReliable() {
		t.Fatal("a level that says insufficient cannot be treated as a fact, whatever the count")
	}
	if (TopicProgress{UserID: "u", TopicKey: "k", ValidCount: 3}).IsReliable() {
		t.Fatal("an unset level is not a valid level")
	}
	// The count has to be able to refuse on its own. Every other case here is decided by
	// the level, so a projection that inherited a "developing" label from an older run -
	// or had one written by anything but this function - would still be treated as a
	// settled picture of the learner.
	if (TopicProgress{UserID: "u", TopicKey: "k", Level: TopicProgressDeveloping, ValidCount: LearningReliableAttemptCount - 1}).IsReliable() {
		t.Fatal("the attempt count is a separate bar from the level, not a restatement of it")
	}
}

func repeatItems(count int) []string {
	items := make([]string, 0, count)
	for index := range count {
		items = append(items, "条目"+strings.Repeat("x", index%3+1))
	}
	return items
}

func repeatEvidence(count int) []LearningEvidence {
	items := make([]LearningEvidence, 0, count)
	for index := range count {
		items = append(items, LearningEvidence{Ordinal: index + 1, DocumentID: "doc_1", ChunkID: "chk_" + string(rune('a'+index)),
			DocumentVer: 1, StartLine: 1, EndLine: 2, Snapshot: "证据"})
	}
	return items
}

func timePointer(value time.Time) *time.Time { return &value }
