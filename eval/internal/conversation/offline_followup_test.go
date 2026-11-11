package conversation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOfflineProductAsksFollowUpsInOneSession is the order rule of an attempt
// that asks several questions.
//
// The questions are asked one after the other in the same session, and not one
// session per question: a follow-up is only a follow-up when the earlier
// question and its answer are still in the conversation. The last question's
// context therefore has to carry the first question and the first answer.
//
// The model is the offline fake: this test is about the order of the
// questions and about what the later question was answered against, not about
// what a model would say.
func TestOfflineProductAsksFollowUpsInOneSession(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires local Docker MySQL image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	repository := filepath.Join("..", "..", "..")
	turns := make([]Turn, 4)
	for i := range turns {
		turns[i] = testTurn(i)
	}
	warmup, err := PrepareOfflineProductReport(ctx, repository, turns, AttemptInput{})
	if err != nil {
		t.Fatal(err)
	}
	questions := []Question{
		{Text: "第一问：这份清单的第一项练什么？", Turn: 30},
		{Text: "第二问：那第三项呢？", Turn: 32},
		{Text: "第三问：它的验收标准是什么？", Turn: 34},
	}
	result, err := RunOfflineProduct(ctx, repository, warmup.Snapshot, AttemptInput{Questions: questions})
	if err != nil {
		t.Fatalf("run: %v; result=%+v", err, result)
	}
	if len(result.Questions) != len(questions) {
		t.Fatalf("questions=%d", len(result.Questions))
	}
	for index, asked := range result.Questions {
		if asked.Question != questions[index].Text {
			t.Fatalf("question %d is %q", index, asked.Question)
		}
		if asked.Answer == "" {
			t.Fatalf("question %d has no answer", index)
		}
		if asked.Index != index {
			t.Fatalf("question %d has the index %d", index, asked.Index)
		}
		// The turn of the question is the round it was asked at, and the
		// scorer reads the attempt by it, so it has to reach the result.
		if asked.Turn != questions[index].Turn {
			t.Fatalf("question %d has the turn %d, want %d", index, asked.Turn, questions[index].Turn)
		}
	}
	// The last question was answered in the context of the earlier ones.
	context := ""
	for _, message := range result.Questions[len(result.Questions)-1].ContextMessages {
		context += message.Content + "\n"
	}
	if !strings.Contains(context, questions[0].Text) {
		t.Fatalf("the first question is not in the last question's context")
	}
	if !strings.Contains(context, questions[1].Text) {
		t.Fatalf("the second question is not in the last question's context")
	}
	if !strings.Contains(context, "synthetic offline answer") {
		t.Fatalf("the earlier answer is not in the last question's context")
	}
	// Every question is its own attempt, and every attempt keeps its own
	// answer.
	if result.Answer != result.Questions[0].Answer {
		t.Fatalf("the primary answer is %q", result.Answer)
	}
}

// TestOfflineProductSeedsTheProfileMemoryBeforeTheQuestion is the rule of a
// seeded profile memory: it has to be in the database the question is asked
// in, because that database is a fresh one, and it has to be there before the
// question is asked.
func TestOfflineProductSeedsTheProfileMemoryBeforeTheQuestion(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires local Docker MySQL image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	repository := filepath.Join("..", "..", "..")
	turns := make([]Turn, 4)
	for i := range turns {
		turns[i] = testTurn(i)
	}
	memories := []SeedMemory{{
		Scope:     "global",
		Subject:   "常用编辑器",
		Content:   "常用编辑器: Neovim",
		Tags:      []string{"工具"},
		Confirmed: true,
		SavedAt:   "2026-01-14",
	}}
	warmup, err := PrepareOfflineProductReport(ctx, repository, turns, AttemptInput{Memories: memories})
	if err != nil {
		t.Fatal(err)
	}
	if warmup.SeededMemories.Written != 1 {
		t.Fatalf("warmup seed=%+v", warmup.SeededMemories)
	}
	result, err := RunOfflineProduct(ctx, repository, warmup.Snapshot, AttemptInput{
		Memories:  memories,
		Questions: []Question{{Text: "我现在用哪个编辑器？"}},
	})
	if err != nil {
		t.Fatalf("run: %v; result=%+v", err, result)
	}
	if result.SeededMemories.Written != 1 {
		t.Fatalf("answer seed=%+v", result.SeededMemories)
	}
	if len(result.SeededMemoryRows) != 1 {
		t.Fatalf("rows=%d", len(result.SeededMemoryRows))
	}
	row := result.SeededMemoryRows[0]
	if row.Content != "常用编辑器: Neovim" || !row.UserConfirmed || row.Scope != "global" {
		t.Fatalf("row=%+v", row)
	}
	want := time.Date(2026, time.January, 14, 0, 0, 0, 0, time.UTC)
	if !row.CreatedAt.Equal(want) || !row.UpdatedAt.Equal(want) {
		t.Fatalf("dates=%v %v", row.CreatedAt, row.UpdatedAt)
	}
}
