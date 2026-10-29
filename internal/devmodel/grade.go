package devmodel

import (
	"encoding/json"
	"hash/fnv"
	"strings"
)

// gradingPrompt is the payload internal/service/learning_grade.go builds for one answer.
// Only the question text is read: a script has nothing to judge an answer against, and
// pretending otherwise would put a fake opinion in front of a learner.
//
// DisallowUnknownFields is deliberate even though the caller is this same server. A
// grading payload that grew a field is a change in the workflow's contract, and this
// model is exactly the place where such a change should stop loudly rather than be
// answered as if nothing happened.
type gradingPrompt struct {
	Question        string   `json:"question"`
	ReferenceAnswer string   `json:"reference_answer"`
	Rubric          []string `json:"rubric"`
	Evidence        []string `json:"evidence"`
	StudentAnswer   string   `json:"student_answer"`
}

func parseGradingPrompt(user string) (gradingPrompt, bool) {
	trimmed := strings.TrimSpace(user)
	if !strings.HasPrefix(trimmed, "{") {
		return gradingPrompt{}, false
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var payload gradingPrompt
	if err := decoder.Decode(&payload); err != nil {
		return gradingPrompt{}, false
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err == nil {
		// A second JSON value after the payload means the prompt is not the one object the
		// workflow writes, so guessing which half to answer is the wrong move.
		return gradingPrompt{}, false
	}
	if strings.TrimSpace(payload.Question) == "" {
		return gradingPrompt{}, false
	}
	return payload, true
}

// gradeScript is the fixed verdicts this model returns.
//
// Three rows, chosen by the question text, so the same question always scores the same
// way in every process and a learner cannot raise a score by answering differently. That
// is the whole point of recording it here: the numbers exercise the projection - the
// weighted mean, the confidence over six attempts, the weak-point ranking - and carry no
// claim about the answer. Each row says so in its own feedback text, which is the only
// part of a grade a learner is ever shown.
type gradeScript struct {
	correctness   int
	completeness  int
	clarity       int
	feedback      string
	missingPoints []string
}

var gradeScripts = []gradeScript{
	{
		correctness: 42, completeness: 16, clarity: 11,
		feedback:      "本地脚本判分，不代表任何评价：三个维度按固定分档给出，用于验证记录与投影链路。",
		missingPoints: []string{"没有说出机制的触发条件", "没有区分两种失效路径"},
	},
	{
		correctness: 48, completeness: 20, clarity: 13,
		feedback:      "本地脚本判分，不代表任何评价：分值与回答内容无关。",
		missingPoints: []string{"没有说明代价与前提"},
	},
	{
		correctness: 36, completeness: 12, clarity: 9,
		feedback:      "本地脚本判分，不代表任何评价：这一档分数更低，用来验证掌握度会被多次作答拉动。",
		missingPoints: []string{"缺少结论句", "没有引用材料里的关键设定"},
	},
}

// gradePayload mirrors the grader's contract. There is no total and no verdict field,
// because the workflow refuses either; a field that cannot be named cannot be sent.
type gradePayload struct {
	Correctness   int      `json:"correctness"`
	Completeness  int      `json:"completeness"`
	Clarity       int      `json:"clarity"`
	Feedback      string   `json:"feedback"`
	MissingPoints []string `json:"missing_points"`
	Confidence    float64  `json:"confidence"`
}

// renderGrade answers one grading call. The confidence is 0.8 on purpose: below the
// workflow's 0.5 floor an attempt would be stored but never counted, and the seam exists
// to prove the counting works.
func renderGrade(payload gradingPrompt) string {
	chosen := gradeScripts[questionSlot(payload.Question)]
	encoded, err := json.Marshal(gradePayload{
		Correctness:   chosen.correctness,
		Completeness:  chosen.completeness,
		Clarity:       chosen.clarity,
		Feedback:      chosen.feedback,
		MissingPoints: append([]string(nil), chosen.missingPoints...),
		Confidence:    0.8,
	})
	if err != nil {
		return `{}`
	}
	return string(encoded)
}

// questionSlot maps a question onto one of the fixed verdicts. It depends on the
// question and on nothing else, so a retry of the same answer scores the same way again
// and a restart does not move a learner's record.
func questionSlot(question string) uint32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(question))
	return hash.Sum32() % uint32(len(gradeScripts))
}
