package agenttool

import (
	"context"
	"fmt"
	"strings"
	"time"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

// The tool that asks the learner which conversation they meant.
//
// It is the third step of the cross-session chain, and it exists because the
// first two can end in a tie: four conversations of the same learner each
// arranged three exercises, and the question names none of them. The card
// search returns all four, and no ranking of them is right - the learner is
// the only one who knows which one they mean.
//
// So the model asks, and this tool is what it asks with. It carries the
// question that is waiting for an answer and the conversations that were
// found, and the runner turns them into a card the learner can answer.
//
// The candidates are checked against the card index before they are offered:
// a conversation the learner does not own, or one that has no card, is not
// offered - offering it would invite the model to open a conversation the
// boundaries refuse.

// AskWhichConversationName is the tool's name.
const AskWhichConversationName = "ask_which_conversation"

// The bounds of one question.
const (
	// maxAskWhichConversationCandidates is how many conversations may be
	// offered at once.
	//
	// It is the card search's own ceiling: the candidates came from there,
	// and a list longer than the search could return is a list the model
	// assembled by hand.
	maxAskWhichConversationCandidates = 8
)

// askWhichConversationDescription is the contract the model reads.
//
// It carries the routing rule, because the routing is the model's to make:
// the server does not read the question and decide that the turn is about
// another conversation, because the server cannot see the whole conversation
// and the model can. A rule the server applied to the question's words would
// refuse exactly the questions that need it - "what did we talk about last
// week in the other chat" is a bare-looking question that is not about this
// conversation at all.
//
// The candidates are optional on purpose. A search that found nothing
// credible is exactly the case where the learner has to be asked, and a tool
// that refuses to ask without candidates makes that case unreachable: the
// learner is left with a model that lists possibilities in its own words, and
// a list of possibilities is not a question. With no candidates the
// clarification offers the learner's OWN conversation list instead, and a
// target that never made it into a candidate list is still reachable.
const askWhichConversationDescription = "Ask the learner which conversation they mean, when the candidates you found cannot be told apart, or when no candidate is credible. Use it when the question is about an earlier conversation and the search returned several equally plausible ones - several conversations of the same family, each with a similar title - or returned nothing at all. Carry the question the learner asked, because the next turn answers THAT question in the conversation they pick; carry the conversation ids you found, and carry none when the search found none. The learner may pick one of them, pick one from their own conversation list, or write their own clue, and their own clue may reach conversations you did not offer. Do not use it to ask a question you could answer from this conversation, and do not open the candidates one by one to read their summaries first - ask, and let the learner decide."

// The states a question can report.
const (
	askWhichStatusOK          = "ok"
	askWhichStatusNoEvidence  = "no_evidence"
	askWhichStatusUnavailable = "unavailable"
)

// The reasons a question gives.
const (
	askWhichReasonNone         = "none"
	askWhichReasonInvalidInput = "invalid_request"
	askWhichReasonNoCandidate  = "no_candidate"
	askWhichReasonStoreFailure = "store_unavailable"
)

// AskWhichConversationInput is what the model asks with.
type AskWhichConversationInput struct {
	// Question is the question that is waiting for an answer.
	//
	// It is the learner's own question, in the learner's own words. The
	// next turn answers it, and a question that has to be reconstructed
	// is a question that was already answered wrongly once.
	Question string `json:"question" jsonschema:"required,description=The question you are asking about, in the learner's own words. It will be answered in the conversation they pick."`
	// Candidates are the conversations you found and are offering.
	//
	// They are the conversation ids the search returned. One that the
	// learner does not own, or that has no card, is dropped. Leave the list
	// empty when the search found nothing credible: the learner is then
	// offered their own conversation list instead.
	Candidates []string `json:"candidates,omitempty" jsonschema:"description=The conversation ids you found and are offering, best first. Leave it empty when the search found nothing credible."`
	// Prompt says what the learner may do, in your own words.
	Prompt string `json:"prompt,omitempty" jsonschema:"description=What the learner may answer: they can pick one of the candidates, or write their own clue."`
}

// AskWhichConversationOutput is what the call reports.
type AskWhichConversationOutput struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
	// Clarification is the question, with the conversations that were
	// offered.
	Clarification *model.SessionClarification `json:"clarification,omitempty"`
	// Rendered is the list as the learner reads it.
	Rendered string `json:"rendered,omitempty"`
	// ObservationTokens is what the answer has to spend on this
	// observation.
	ObservationTokens int `json:"observation_tokens"`
	// Retryable says the store could not answer and the call may be tried
	// again.
	Retryable bool `json:"retryable,omitempty"`
}

// AskWhichConversationRegistry builds the tool.
type AskWhichConversationRegistry struct {
	cards   ThinCardRetriever
	surface serviceagent.ToolSurface
}

// NewAskWhichConversationRegistry builds the registry.
//
// The card retriever is required: a candidate is offered only when the
// learner owns it and it has a card, and the retriever is what says so.
func NewAskWhichConversationRegistry(cards ThinCardRetriever) (*AskWhichConversationRegistry, error) {
	if cards == nil {
		return nil, fmt.Errorf("%w: a thin card retriever is required", ErrInvalidKnowledgeSearchConfig)
	}
	return &AskWhichConversationRegistry{cards: cards}, nil
}

// UseToolSurface changes only the name and description shown to the model.
func (r *AskWhichConversationRegistry) UseToolSurface(surface serviceagent.ToolSurface) {
	r.surface = surface
}

// Tools returns the tool for one run.
func (r *AskWhichConversationRegistry) Tools(ctx context.Context, run serviceagent.RunContext) ([]tool.BaseTool, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if run.UserID <= 0 || strings.TrimSpace(run.SessionID) == "" {
		return nil, fmt.Errorf("%w: a trusted user id and session are required", ErrInvalidKnowledgeSearchRequest)
	}
	name := serviceagent.MustToolNameFor(serviceagent.CapabilityAskWhichConversation, r.resolvedSurface())
	ask, err := toolutils.InferTool(name, askWhichConversationDescription,
		func(ctx context.Context, input *AskWhichConversationInput) (*AskWhichConversationOutput, error) {
			return r.ask(ctx, run, input)
		})
	if err != nil {
		return nil, fmt.Errorf("build the ask-which-conversation schema: %w", err)
	}
	return []tool.BaseTool{ask}, nil
}

func (r *AskWhichConversationRegistry) resolvedSurface() serviceagent.ToolSurface {
	if r.surface == "" {
		return serviceagent.SurfaceLegacy
	}
	return r.surface
}

// ask runs one call.
func (r *AskWhichConversationRegistry) ask(ctx context.Context, run serviceagent.RunContext,
	input *AskWhichConversationInput) (*AskWhichConversationOutput, error) {
	out := &AskWhichConversationOutput{Status: askWhichStatusOK, Reason: askWhichReasonNone}
	if input == nil || strings.TrimSpace(input.Question) == "" {
		out.Status = askWhichStatusUnavailable
		out.Reason = askWhichReasonInvalidInput
		return out, nil
	}
	clarification := &model.SessionClarification{
		ClarificationID: serviceagent.NewClarificationID(),
		Question:        strings.TrimSpace(input.Question),
		Prompt:          strings.TrimSpace(input.Prompt),
		AllowFreeText:   true,
		// The learner's own conversation list is always reachable from the
		// clarification. It is the ground truth: the candidates are what one
		// search found, and a target that never made it into them is still
		// the learner's own conversation.
		AllowHistoryBrowse: true,
		CreatedAt:          time.Now().UTC(),
	}
	for _, session := range input.Candidates {
		session = strings.TrimSpace(session)
		if session == "" {
			continue
		}
		if len(clarification.Candidates) >= maxAskWhichConversationCandidates {
			break
		}
		// The candidate is checked against the card index: the learner has
		// to own it, and it has to have a card - a conversation the card
		// index does not describe cannot be opened, so offering it would
		// invite the model to try and fail.
		candidate, found, err := r.cards.CardOfSession(ctx, run.UserID, session)
		if err != nil {
			out.Status = askWhichStatusUnavailable
			out.Reason = askWhichReasonStoreFailure
			out.Retryable = true
			return out, nil
		}
		if !found {
			continue
		}
		clarification.Candidates = append(clarification.Candidates, model.SessionClarificationCandidate{
			SessionID:    candidate.SessionID,
			Title:        candidate.Title,
			Topics:       append([]string{}, candidate.Topics...),
			LastTalkedAt: candidate.LastTalkedAt,
		})
	}
	if len(clarification.Candidates) == 0 {
		// No candidate survived. The question is still a question: the
		// learner is offered their own conversation list, and a target that
		// never made it into a candidate list is still reachable through it.
		//
		// Refusing here is what left the learner with a model that lists
		// possibilities in its own words: a list of possibilities is not a
		// question, and nobody was ever asked.
		out.Reason = askWhichReasonNoCandidate
	}
	if err := clarification.Validate(); err != nil {
		out.Status = askWhichStatusUnavailable
		out.Reason = askWhichReasonInvalidInput
		return out, nil
	}
	out.Clarification = clarification
	out.Rendered = clarification.Rendered()
	out.ObservationTokens = serviceagent.ClarificationTokens(clarification)
	return out, nil
}
