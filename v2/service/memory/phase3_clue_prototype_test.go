package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"GopherAI/model"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// 第三阶段线索原型（评测树 test-only，不进产品）：
//
// 现有白板里唯一没有送进提取请求的既有线索是 Unverified——学习者的原话在
// 当时无法确认成当前事实时被保留下来，每条都带 MessageID。它已经满足本
// 轮线索的三条要求，不需要任何新表或新字段：
//
//   - 接入点最小：直接复用 SessionWhiteboardDocument.Unverified（V3 已有）；
//   - 可追到原话：每条线索带 MessageID，收尾时按现有证据校验复核（引用必须
//     在可见集合里，且是学习者原话）；
//   - 流程不变：候选仍进现有待确认草稿，用户确认后才写长期记忆。
//
// 原型形态是一个装饰器：只把这段既有状态渲染进模型请求，产品代码零改动。
// 对照实验（评测窗口）用 withUnverified 开/关跑同一批样本，比较候选发现率、
// 误提与可追溯性，再决定是否把这段渲染并入产品。

// clueInjectingChat appends a clue block to the last user message before the
// inner model sees it. It touches nothing else: tools, streaming and the
// request's own content pass through unchanged.
type clueInjectingChat struct {
	inner modelcomponent.ToolCallingChatModel
	clues string
}

func (c *clueInjectingChat) Generate(ctx context.Context, in []*schema.Message, opts ...modelcomponent.Option) (*schema.Message, error) {
	augmented := append([]*schema.Message(nil), in...)
	if c.clues != "" {
		for i := len(augmented) - 1; i >= 0; i-- {
			if augmented[i].Role == schema.User {
				augmented[i] = schema.UserMessage(augmented[i].Content + "\n\n" + c.clues)
				break
			}
		}
	}
	return c.inner.Generate(ctx, augmented, opts...)
}

func (c *clueInjectingChat) Stream(ctx context.Context, in []*schema.Message, opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return c.inner.Stream(ctx, in, opts...)
}

func (c *clueInjectingChat) WithTools(tools []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	bound, err := c.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &clueInjectingChat{inner: bound, clues: c.clues}, nil
}

// clueCarryingProposer is the prototype seam: the same extraction, with the
// session's own unverified statements rendered as clues.
type clueCarryingProposer struct {
	chat           modelcomponent.ToolCallingChatModel
	withUnverified bool
}

func (p *clueCarryingProposer) Propose(ctx context.Context, input *SessionExtractionInput) ([]SessionRawProposal, error) {
	chat := p.chat
	if p.withUnverified {
		if clues := renderUnverifiedClues(input.Board); clues != "" {
			chat = &clueInjectingChat{inner: chat, clues: clues}
		}
	}
	proposer, err := NewModelSessionProposer(chat)
	if err != nil {
		return nil, err
	}
	return proposer.Propose(ctx, input)
}

// renderUnverifiedClues renders the board's kept-back learner statements as
// clues. Every line carries its message id, so a candidate built from a clue
// can always be traced back to the words - and re-checked at finalize.
func renderUnverifiedClues(board model.SessionWhiteboardDocument) string {
	if len(board.Unverified) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("Unverified learner statements from this session")
	builder.WriteString(" (clues only; a candidate built from one must cite its message id as evidence):")
	for _, anchor := range board.Unverified {
		fmt.Fprintf(&builder, "\n- message %d: %q", anchor.MessageID, anchor.Text)
		if anchor.Key != "" {
			fmt.Fprintf(&builder, " key=%q", anchor.Key)
		}
		if anchor.Value != "" {
			fmt.Fprintf(&builder, " value=%q", anchor.Value)
		}
		fmt.Fprintf(&builder, " reason=%s", anchor.Reason)
	}
	return builder.String()
}

func cluePrototypeBoard(t *testing.T) string {
	t.Helper()
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "teaching-preference", Text: "讲新概念先举例再讲定义", Value: "先举例再讲定义",
			SourceMessageIDs: []uint{3}, Origin: model.SessionWhiteboardOriginUser, UpdatedAtMessageID: 3,
		}},
		// 当时没能确认成当前事实的学习者原话：保留下来，带 message id。
		Unverified: []model.SessionWhiteboardAnchor{{
			Key: "teaching-preference", Value: "先举例再讲定义", MessageID: 3,
			Text:     "以后你给我讲新的概念，都先举一个我熟悉的例子再讲定义。",
			Complete: true, Reason: model.SessionWhiteboardAnchorUnsettled, UpdatedAtMessageID: 3,
		}},
	}
	encoded, err := model.MarshalSessionWhiteboardDocument(document)
	if err != nil {
		t.Fatalf("marshal board: %v", err)
	}
	return encoded
}

func TestCluePrototypeUnverifiedAnchorReachesRequest(t *testing.T) {
	board := cluePrototypeBoard(t)
	call := `{"scope":"global","class":"teaching_preference","subject":"讲解偏好",` +
		`"proposed_content":"讲新概念时先举一个我熟悉的例子，再讲正式定义。","durability":"stable",` +
		`"evidence_ids":[3],"statement_evidence_id":3}`
	newService := func(chat *memoryChat, withUnverified bool) *SessionFinalizeService {
		service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
			nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
			&clueCarryingProposer{chat: chat, withUnverified: withUnverified},
			&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "new"}}, 0)
		return service
	}

	// 开启线索：请求同时带未确认语句与对应原话，候选通过原话校验。
	withClues := &memoryChat{response: toolMessage(finalizeToolProfile, call)}
	result, err := newService(withClues, true).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-clue-on"}})
	if err != nil {
		t.Fatalf("clue-on extract: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals=%d skipped=%+v, want the candidate proposed", len(result.Proposals), result.Skipped)
	}
	payload := withClues.messages[1].Content
	if !strings.Contains(payload, "Unverified learner statements") || !strings.Contains(payload, "message 3") {
		t.Fatalf("the clue block is missing from the request: %s", payload)
	}
	if !strings.Contains(payload, "以后你给我讲新的概念，都先举一个我熟悉的例子再讲定义。") {
		t.Fatalf("the clue lost the learner's original words: %s", payload)
	}
	if !strings.Contains(payload, "以后你给我讲新的概念") {
		t.Fatalf("the visible utterance the clue points at is missing")
	}

	// 对照：关闭线索时请求里没有这段内容（产品的现状）。
	without := &memoryChat{response: toolMessage(finalizeToolProfile, call)}
	if _, err := newService(without, false).
		Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-clue-off"}}); err != nil {
		t.Fatalf("clue-off extract: %v", err)
	}
	if strings.Contains(without.messages[1].Content, "Unverified learner statements") {
		t.Fatalf("the control request must not carry the clue block")
	}
}

func TestCluePrototypeKeepsUtteranceValidation(t *testing.T) {
	board := cluePrototypeBoard(t)
	fabricated := `{"scope":"global","class":"teaching_preference","subject":"讲解偏好",` +
		`"proposed_content":"讲新概念时先举一个我熟悉的例子。","durability":"stable",` +
		`"evidence_ids":[99],"statement_evidence_id":99}`
	chat := &memoryChat{response: toolMessage(finalizeToolProfile, fabricated)}
	service := newFinalizeService(t, fakeFinalizeBoards{content: &board}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&clueCarryingProposer{chat: chat, withUnverified: true},
		&scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 0)
	result, err := service.Extract(context.Background(), FinalizeRequest{UserID: 7, Cursor: FinalizeCursor{SessionID: "S-clue-fake"}})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// 线索只是线索：引用可见集之外的候选照样被拒。
	if len(result.Proposals) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != FinalizeSkipEvidenceMissing {
		t.Fatalf("proposals=%+v skipped=%+v, want the out-of-set citation refused", result.Proposals, result.Skipped)
	}
}
