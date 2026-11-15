package agenttool

import (
	"context"
	"fmt"
	"strings"

	"GopherAI/common/rag"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

// The search tool for a source session's original words.
//
// It is the last step of the chain the session card opened: the card says
// what a session was about, the summary says what was said in it, and this
// finds the one detail neither of them kept.
//
// The scope is narrow on purpose. The search runs inside one session, and
// that session is either this run's own conversation or one a session card in
// this run pointed at. A model that fills in an arbitrary session id gets
// nothing back, and the reason it gets is a distinct one: "there is no such
// message" and "you may not look there" are different facts, and a model told
// the first will try the same call again.

const SearchSessionHistoryName = "search_session_history"

// The bounds of one search.
const (
	maxSearchSessionResults = 8
	maxSearchSessionSnippet = 300
	// searchSessionCandidateLimit bounds how much of the session the
	// database is asked to look at.
	searchSessionCandidateLimit = 200
)

// searchSessionHistoryDescription is the contract the model reads.
const searchSessionHistoryDescription = "Search one earlier conversation for a word or phrase, when a session card or its summary did not keep the detail you need. Name the session exactly as the card showed it. Returns the matching lines with their message ids; a message id can then be read back in full with read_history_message."

// The states a search can report.
const (
	searchSessionStatusOK          = "ok"
	searchSessionStatusNoEvidence  = "no_evidence"
	searchSessionStatusUnavailable = "unavailable"
)

// The reasons a search gives.
const (
	searchSessionReasonNone         = "none"
	searchSessionReasonNoMatch      = "no_match"
	searchSessionReasonNotAllowed   = "session_not_allowed"
	searchSessionReasonInvalidInput = "invalid_request"
	searchSessionReasonStoreFailure = "store_unavailable"
)

// SearchSessionHistoryInput is what the model asks for.
type SearchSessionHistoryInput struct {
	Session    string `json:"session,omitempty" jsonschema:"description=The conversation to search, exactly as a session card showed it. Leave empty for this conversation."`
	Query      string `json:"query" jsonschema:"required,description=The word or phrase to look for in that conversation's messages."`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"description=How many matching messages to return, 1-8."`
}

// SearchSessionHistoryOutput is what the call reports.
type SearchSessionHistoryOutput struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
	// Matches are the messages that contain the query, oldest first.
	Matches []SearchSessionMatch `json:"matches"`
	// ObservationTokens is what the answer has to spend on this observation.
	ObservationTokens int `json:"observation_tokens"`
	// Truncated says a match was cut short.
	Truncated bool `json:"truncated,omitempty"`
	// Retryable says the store could not answer and the call may be tried
	// again.
	Retryable bool `json:"retryable,omitempty"`
}

// SearchSessionMatch is one matching message.
type SearchSessionMatch struct {
	MessageID int    `json:"message_id"`
	Role      string `json:"role"`
	SentAt    string `json:"sent_at"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// SessionMessageSearcher searches one session's messages.
//
// The implementation is owner-scoped: it takes the user id from the run and
// binds the session in the query, so a session of another account is a lookup
// miss rather than a permission check somebody has to remember.
type SessionMessageSearcher interface {
	SearchOwnedMessagesBySession(context.Context, int64, string, string, int) ([]model.Message, error)
}

// SearchSessionHistoryRegistry builds the search tool.
type SearchSessionHistoryRegistry struct {
	searcher SessionMessageSearcher
	surface  serviceagent.ToolSurface
}

// NewSearchSessionHistoryRegistry builds the registry.
func NewSearchSessionHistoryRegistry(searcher SessionMessageSearcher) (*SearchSessionHistoryRegistry, error) {
	if searcher == nil {
		return nil, fmt.Errorf("%w: session message searcher is required", ErrInvalidKnowledgeSearchConfig)
	}
	return &SearchSessionHistoryRegistry{searcher: searcher}, nil
}

// UseToolSurface changes only the name and description shown to the model.
func (r *SearchSessionHistoryRegistry) UseToolSurface(surface serviceagent.ToolSurface) {
	r.surface = surface
}

// Tools returns the search tool for one run.
func (r *SearchSessionHistoryRegistry) Tools(ctx context.Context, run serviceagent.RunContext) ([]tool.BaseTool, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if run.UserID <= 0 || strings.TrimSpace(run.SessionID) == "" {
		return nil, fmt.Errorf("%w: a trusted user id and session are required", ErrInvalidKnowledgeSearchRequest)
	}
	name := serviceagent.MustToolNameFor(serviceagent.CapabilitySearchSessionHistory, r.resolvedSurface())
	search, err := toolutils.InferTool(name, searchSessionHistoryDescription,
		func(ctx context.Context, input *SearchSessionHistoryInput) (*SearchSessionHistoryOutput, error) {
			return r.search(ctx, run, input)
		})
	if err != nil {
		return nil, fmt.Errorf("build session history search schema: %w", err)
	}
	return []tool.BaseTool{search}, nil
}

func (r *SearchSessionHistoryRegistry) resolvedSurface() serviceagent.ToolSurface {
	if r.surface == "" {
		return serviceagent.SurfaceLegacy
	}
	return r.surface
}

// search runs one call.
func (r *SearchSessionHistoryRegistry) search(ctx context.Context, run serviceagent.RunContext,
	input *SearchSessionHistoryInput) (*SearchSessionHistoryOutput, error) {
	out := &SearchSessionHistoryOutput{Status: searchSessionStatusOK, Reason: searchSessionReasonNone, Matches: []SearchSessionMatch{}}
	if input == nil || strings.TrimSpace(input.Query) == "" {
		out.Status = searchSessionStatusUnavailable
		out.Reason = searchSessionReasonInvalidInput
		return out, nil
	}
	limit := input.MaxResults
	if limit <= 0 {
		limit = maxSearchSessionResults
	}
	if limit > maxSearchSessionResults {
		limit = maxSearchSessionResults
	}
	// The session to search, and the card that authorised it.
	targetSession := strings.TrimSpace(input.Session)
	cardID, cardVersion, watermark := "", uint64(0), uint(0)
	if targetSession == "" {
		targetSession = run.SessionID
	} else if targetSession != strings.TrimSpace(run.SessionID) {
		if run.GrantSet == nil || !run.GrantSet.AuthorisesSession(run, targetSession) {
			out.Status = searchSessionStatusNoEvidence
			out.Reason = searchSessionReasonNotAllowed
			return out, nil
		}
		granted, version, ceiling, ok := run.GrantSet.CardAuthorisation(targetSession)
		if !ok {
			out.Status = searchSessionStatusNoEvidence
			out.Reason = searchSessionReasonNotAllowed
			return out, nil
		}
		cardID, cardVersion, watermark = granted, version, ceiling
	}
	messages, err := r.searcher.SearchOwnedMessagesBySession(ctx, run.UserID, targetSession,
		strings.TrimSpace(input.Query), searchSessionCandidateLimit)
	if err != nil {
		out.Status = searchSessionStatusUnavailable
		out.Reason = searchSessionReasonStoreFailure
		out.Retryable = true
		return out, nil
	}
	grants := make([]serviceagent.HistoryMessageGrant, 0, len(messages))
	tokens := 0
	for _, message := range messages {
		if len(out.Matches) >= limit {
			break
		}
		// The card's own ceiling. The card describes the session as far as it
		// had reached, and a message above that line is not something the
		// card ever saw - so it is not part of what this run may reach.
		if watermark > 0 && message.ID > watermark {
			continue
		}
		text, truncated := boundRunes(message.Content, maxSearchSessionSnippet)
		if text == "" {
			continue
		}
		role := "user"
		if !message.IsUser {
			role = "assistant"
		}
		out.Matches = append(out.Matches, SearchSessionMatch{
			MessageID: int(message.ID), Role: role,
			SentAt: message.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			Text:   text, Truncated: truncated,
		})
		tokens += rag.EstimateTokens(text) + 8
		if cardID != "" {
			grants = append(grants, serviceagent.HistoryMessageGrant{
				MessageID: message.ID, SourceKind: serviceagent.HistorySourceSessionCard,
				SourceSessionID: targetSession,
				CardID:          cardID, CardVersion: cardVersion, CardWatermark: watermark,
			})
		}
	}
	out.ObservationTokens = tokens
	// The ids the search just showed the model are now readable back in full,
	// and the run has to know it: the read-back tool refuses ids it was not
	// given.
	if run.GrantSet != nil && len(grants) > 0 {
		run.GrantSet.Extend(run, grants)
	}
	if len(out.Matches) == 0 {
		out.Status = searchSessionStatusNoEvidence
		out.Reason = searchSessionReasonNoMatch
	}
	return out, nil
}
