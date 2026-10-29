package learning

import (
	"GopherAI/model"
	"math"
	"testing"
	"time"
)

func TestCalculateTopicProgressNeedsEnoughEvidence(t *testing.T) {
	now := time.Now()
	progress := calculateTopicProgress(7, "redis", "Redis", []model.LearningAttempt{{EffectiveTotal: 100, CompletedAt: &now}})
	if progress.MasteryScore != 100 || progress.Level != model.TopicProgressInsufficient {
		t.Fatalf("progress = %#v, one perfect answer must still be insufficient", progress)
	}
	if math.Abs(progress.Confidence-1.0/6.0) > 0.0001 {
		t.Fatalf("confidence = %v, want 1/6", progress.Confidence)
	}
}

func TestCalculateTopicProgressIsDeterministicAndRecentWeighted(t *testing.T) {
	now := time.Now()
	attempts := []model.LearningAttempt{
		{EffectiveTotal: 90, MissingPoints: []string{"Deadline 传播"}, CompletedAt: &now},
		{EffectiveTotal: 60, MissingPoints: []string{"Deadline 传播", "错误码"}, CompletedAt: &now},
		{EffectiveTotal: 30, MissingPoints: []string{"错误码"}, CompletedAt: &now},
	}
	first := calculateTopicProgress(7, "rpc", "RPC", attempts)
	second := calculateTopicProgress(7, "rpc", "RPC", attempts)
	// Input is newest first, so weights are 3,2,1: (270+120+30)/6 = 70.
	if first.MasteryScore != 70 || second.MasteryScore != 70 || first.Level != model.TopicProgressDeveloping {
		t.Fatalf("progress = %#v / %#v", first, second)
	}
	if len(first.WeakPoints) != 2 || first.WeakPoints[0] != "Deadline 传播" || first.WeakPoints[1] != "错误码" {
		t.Fatalf("weak points = %#v", first.WeakPoints)
	}
}

func TestValidateGradeCommitRejectsModelOwnedTotalBoundaries(t *testing.T) {
	valid := GradeCommit{Correctness: 60, Completeness: 25, Clarity: 15, Feedback: "完整", Confidence: 1}
	if err := validateGradeCommit(valid); err != nil {
		t.Fatal(err)
	}
	valid.Correctness = 61
	if err := validateGradeCommit(valid); err == nil {
		t.Fatal("expected out-of-range correctness to fail")
	}
}
