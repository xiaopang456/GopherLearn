package service

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// The learner-facing response surface, stated as a field list.
//
// A comment that says "the answer key is not included" is worth nothing the day someone
// adds a convenient field to the view - and the convenient field is exactly the kind that
// gets added, because reference answers and rubrics are already sitting on the struct one
// hop away. So the shape is asserted as data: these keys, in this set, and nothing else.
// Adding a field to LearningQuizView fails here until the list below is edited on purpose,
// which is the moment to ask whether a learner should see it.

// jsonKeys marshals a value and returns its top-level JSON keys, sorted. Marshalling these
// view types cannot fail: they hold strings, numbers and slices of strings.
func jsonKeys(t *testing.T, value any) []string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func assertExactKeys(t *testing.T, label string, got []string, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s keys = [%s], want [%s]", label, strings.Join(got, ","), strings.Join(want, ","))
	}
}

func TestLearningQuizViewSerializesOnlyWhatALearnerMaySee(t *testing.T) {
	view := LearningQuizView{
		ID: "lsn-1", SessionID: "session-1", Topic: "Redis 持久化",
		Status: string(domain.LearningSessionWaitingAnswer), CurrentQuestion: 2, TotalQuestions: 3,
		Question: &LearningQuestionView{
			Ordinal: 2, Type: string(domain.LearningQuestionShortAnswer),
			Question: "第二种情况该怎么处理？",
		},
		ExpiresAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
	}

	assertExactKeys(t, "quiz", jsonKeys(t, view), []string{
		"current_question", "expires_at", "id", "question", "session_id", "status", "topic",
		"total_questions",
	})
	// The nested question is a card to display: position, kind, text. Nothing that tells a
	// learner how it will be scored.
	encoded, err := json.Marshal(view.Question)
	if err != nil {
		t.Fatal(err)
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &nested); err != nil {
		t.Fatal(err)
	}
	nestedKeys := make([]string, 0, len(nested))
	for key := range nested {
		nestedKeys = append(nestedKeys, key)
	}
	sort.Strings(nestedKeys)
	assertExactKeys(t, "question", nestedKeys, []string{"ordinal", "question", "type"})

	// The names of everything that must never appear, checked against the whole document
	// rather than only against the key list, so a nested object cannot smuggle one in.
	body := string(mustJSON(t, view))
	for _, forbidden := range []string{
		"reference_answer", "rubric", "evidence", "snapshot", "assessment_goal",
		"content_hash", "chunk_id", "doc_version", "document_version", "title_path",
		"grading_confidence", "lease_token", "lease_expires_at", "auto_total", "auto_correctness",
		"effective_total", "answer", "submission_id", "failure_code", "correctness",
		"completeness", "clarity", "missing_points", "progress", "mastery", "confidence",
		"topic_key", "version", "user_id", "agent_run_id", "grade", "prompt",
	} {
		if strings.Contains(body, `"`+forbidden+`"`) {
			t.Fatalf("the quiz response carries %q:\n%s", forbidden, body)
		}
	}
}

func TestLearningQuizViewHasNoScoreFieldToForget(t *testing.T) {
	// The answer path used to attach the projection to this response. The field is gone,
	// so a future edit has to reintroduce it deliberately - and this case is what notices
	// even if whoever adds it forgets to say so anywhere else.
	//
	// A quiz with no card to show is the seven always-present keys: `question` is the only
	// optional one, and it disappears rather than arriving as null, because a closed quiz
	// has no question rather than a question whose text is unknown.
	var view LearningQuizView
	assertExactKeys(t, "empty quiz", jsonKeys(t, view), []string{
		"current_question", "expires_at", "id", "session_id", "status", "topic",
		"total_questions",
	})
}

func TestLearningProgressViewIsTheOwnersOwnRecord(t *testing.T) {
	stamped := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	progress := LearningProgressView{
		TopicKey: "redis-持久化", Topic: "Redis 持久化", Mastery: 77, Confidence: 0.5,
		ValidCount: 3, Level: string(domain.TopicProgressDeveloping),
		WeakPoints: []string{"没说出重写触发条件"}, LastAttempt: stamped,
	}
	assertExactKeys(t, "progress", jsonKeys(t, progress), []string{
		"confidence", "last_attempt", "level", "mastery", "topic", "topic_key",
		"valid_attempts", "weak_points",
	})
	// A projection with no attempt yet is not a record with a zero date on it.
	undated := progress
	undated.LastAttempt = time.Time{}
	body := string(mustJSON(t, undated))
	if strings.Contains(body, "last_attempt") {
		t.Fatalf("an attempt that never happened was given a date:\n%s", body)
	}
	// The internal identifiers stay out even here: this response is about a topic, and a
	// row id is nothing a client can act on.
	for _, forbidden := range []string{`"id"`, `"user_id"`, `"weak_point"`, `"effective_total"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the progress response carries %q:\n%s", forbidden, body)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
