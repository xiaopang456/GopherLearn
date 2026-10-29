package responder

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

var _ Responder = EchoResponder{}

func TestEchoResponderAnswersEveryStrategyVerbatim(t *testing.T) {
	// The echo responder is a seam placeholder: resolving knowledge_qa must not
	// make it claim retrieval or invent citations.
	for _, strategy := range []domain.AnswerStrategy{
		domain.AnswerStrategyDirectChat,
		domain.AnswerStrategyKnowledgeQA,
		domain.AnswerStrategyAgent,
	} {
		request := Request{
			UserID:                 "user-1",
			SessionID:              "session-1",
			Content:                "解释一下 Redis 持久化",
			SessionMode:            domain.SessionModeChat,
			AnswerStrategy:         strategy,
			KnowledgeEnabled:       strategy == domain.AnswerStrategyKnowledgeQA,
			KnowledgeCollectionIDs: []string{"collection-1"},
		}
		answer, err := EchoResponder{}.Respond(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if answer.Content != request.Content {
			t.Fatalf("strategy %q answer = %q, want the verbatim content %q", strategy, answer.Content, request.Content)
		}
		// Nothing was retrieved, so nothing may be cited.
		if len(answer.Sources) != 0 {
			t.Fatalf("strategy %q fabricated %d sources", strategy, len(answer.Sources))
		}
		if strings.Contains(strings.ToLower(answer.Content), "citation") || strings.Contains(answer.Content, "引用") {
			t.Fatalf("strategy %q fabricated references: %q", strategy, answer.Content)
		}
	}
}

func TestEchoResponderRespondHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (EchoResponder{}).Respond(ctx, Request{Content: "question"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestEchoResponderStreamReassemblesToTheSameContent(t *testing.T) {
	content := strings.Repeat("长文本", 20)
	var chunks []string
	answer, err := EchoResponder{}.Stream(context.Background(), Request{
		Content:        content,
		SessionMode:    domain.SessionModeChat,
		AnswerStrategy: domain.AnswerStrategyDirectChat,
	}, func(delta string) error {
		chunks = append(chunks, delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Content != content || strings.Join(chunks, "") != content {
		t.Fatalf("streamed answer mismatch: answer=%q joined=%q", answer.Content, strings.Join(chunks, ""))
	}
	if len(chunks) < 2 {
		t.Fatalf("expected the stream to be split into %d+ chunks, got %d", 2, len(chunks))
	}
}

func TestEchoResponderStreamStopsAtTheFirstError(t *testing.T) {
	sentinel := errors.New("client disconnected")
	emitted := 0
	_, err := EchoResponder{}.Stream(context.Background(), Request{Content: "abcdef"}, func(string) error {
		emitted++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want %v", err, sentinel)
	}
	if emitted != 1 {
		t.Fatalf("emit calls = %d, want 1", emitted)
	}
}
