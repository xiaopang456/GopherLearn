package learning

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestModelAnswerGraderAcceptsOnlyDimensionSchema(t *testing.T) {
	grader, err := NewModelAnswerGrader(fakeQuestionModel{content: `{"correctness":50,"completeness":20,"clarity":12,"feedback":"核心正确，但缺少并发边界。","missing_points":["说明并发冲突"],"confidence":0.9}`})
	if err != nil {
		t.Fatal(err)
	}
	result, err := grader.Grade(context.Background(), GradeRequest{Question: "问题", ReferenceAnswer: "参考", Rubric: []string{"要点"}, Evidence: testEvidence(), Answer: "回答"})
	if err != nil || result.Correctness+result.Completeness+result.Clarity != 82 {
		t.Fatalf("result = %#v, err = %v", result, err)
	}

	withTotal, _ := NewModelAnswerGrader(fakeQuestionModel{content: `{"correctness":50,"completeness":20,"clarity":12,"total":82,"feedback":"反馈","missing_points":[],"confidence":0.9}`})
	if _, err := withTotal.Grade(context.Background(), GradeRequest{Question: "问题", ReferenceAnswer: "参考", Rubric: []string{"要点"}, Evidence: testEvidence(), Answer: "回答"}); !errors.Is(err, ErrInvalidGradeOutput) {
		t.Fatalf("model-owned total error = %v", err)
	}

	outOfRange, _ := NewModelAnswerGrader(fakeQuestionModel{content: strings.Replace(`{"correctness":50,"completeness":20,"clarity":12,"feedback":"反馈","missing_points":[],"confidence":0.9}`, `50`, `61`, 1)})
	if _, err := outOfRange.Grade(context.Background(), GradeRequest{Question: "问题", ReferenceAnswer: "参考", Rubric: []string{"要点"}, Evidence: testEvidence(), Answer: "回答"}); !errors.Is(err, ErrInvalidGradeOutput) {
		t.Fatalf("range error = %v", err)
	}
}
