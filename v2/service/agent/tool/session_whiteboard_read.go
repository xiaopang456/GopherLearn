package agenttool

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"GopherAI/common/rag"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

// The tool that reads one session's whiteboard.
//
// The board is the session's CURRENT effective state: the goal, the
// constraints in force, the plan and the numbers, and what is still open. It
// is not a transcript and not a summary segment - it is what the session had
// settled on, with every value that has since been replaced removed.
//
// It answers the question the summary cannot: "what was finally agreed". A
// segment says what happened in a stretch of turns; the board says what that
// stretch left behind.
//
// It reads the board and nothing else. The messages behind the board are
// another tool's business, and a read of the current state does not need them.

const ReadSessionWhiteboardName = "read_session_whiteboard"

// The bounds of one read.
const (
	// maxWhiteboardEntryRunes bounds one entry's text.
	maxWhiteboardEntryRunes = 600
	// maxWhiteboardEntries bounds how many entries of one category are
	// shown.
	maxWhiteboardEntries = 20
)

// readSessionWhiteboardDescription is the contract the model reads.
const readSessionWhiteboardDescription = "Read one earlier conversation's whiteboard: the goal, the constraints, the plan and the open items that conversation had settled on. Use it when the learner asks what was finally arranged or decided in a conversation. It reads the conversation's current state only, not its messages, and it reads only a conversation this turn's cards pointed at."

// The states a read can report.
const (
	whiteboardStatusOK          = "ok"
	whiteboardStatusNoEvidence  = "no_evidence"
	whiteboardStatusUnavailable = "unavailable"
)

// The reasons a read gives.
const (
	whiteboardReasonNone         = "none"
	whiteboardReasonNoBoard      = "no_board"
	whiteboardReasonUnreadable   = "unreadable_board"
	whiteboardReasonNotAllowed   = "session_not_allowed"
	whiteboardReasonInvalidInput = "invalid_request"
	whiteboardReasonStoreFailure = "store_unavailable"
)

// ReadSessionWhiteboardInput is what the model asks for.
type ReadSessionWhiteboardInput struct {
	Session string `json:"session" jsonschema:"required,description=The conversation to read, exactly as a session card or a segment search showed it. A conversation no card pointed at cannot be opened."`
}

// WhiteboardEntryView is one entry of the board as the model sees it.
type WhiteboardEntryView struct {
	// Key is what the entry is about, so a changed value replaces the old one
	// under the same key.
	Key string `json:"key,omitempty"`
	// Text is the entry's statement.
	Text string `json:"text"`
}

// ReadSessionWhiteboardOutput is what the call reports.
type ReadSessionWhiteboardOutput struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
	// Session is the conversation that was read.
	Session string `json:"session"`
	// SchemaVersion is the board document's own version.
	SchemaVersion int `json:"schema_version,omitempty"`
	// Goal is the conversation's current goal.
	Goal string `json:"goal,omitempty"`
	// Plan, OpenItems and Constraints are the board's three lists.
	Plan        []WhiteboardEntryView `json:"plan"`
	OpenItems   []WhiteboardEntryView `json:"open_items"`
	Constraints []WhiteboardEntryView `json:"constraints"`
	// ObservationTokens is what the answer has to spend on this observation.
	ObservationTokens int  `json:"observation_tokens"`
	Truncated         bool `json:"truncated,omitempty"`
	Retryable         bool `json:"retryable,omitempty"`
}

// SessionWhiteboardReader reads one session's board.
//
// It is the repository's own read, and it is owner-scoped: a board of another
// account looks exactly like a board that does not exist.
type SessionWhiteboardReader interface {
	GetWhiteboard(ctx context.Context, userID int64, sessionID string) (*model.SessionWhiteboard, error)
}

// ReadSessionWhiteboardRegistry builds the read tool.
type ReadSessionWhiteboardRegistry struct {
	reader  SessionWhiteboardReader
	surface serviceagent.ToolSurface
}

// NewReadSessionWhiteboardRegistry builds the registry.
func NewReadSessionWhiteboardRegistry(reader SessionWhiteboardReader) (*ReadSessionWhiteboardRegistry, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: a session whiteboard reader is required", ErrInvalidKnowledgeSearchConfig)
	}
	return &ReadSessionWhiteboardRegistry{reader: reader}, nil
}

// UseToolSurface changes only the name and description shown to the model.
func (r *ReadSessionWhiteboardRegistry) UseToolSurface(surface serviceagent.ToolSurface) {
	r.surface = surface
}

// Tools returns the read tool for one run.
func (r *ReadSessionWhiteboardRegistry) Tools(ctx context.Context, run serviceagent.RunContext) ([]tool.BaseTool, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if run.UserID <= 0 || strings.TrimSpace(run.SessionID) == "" {
		return nil, fmt.Errorf("%w: a trusted user id and session are required", ErrInvalidKnowledgeSearchRequest)
	}
	name := serviceagent.MustToolNameFor(serviceagent.CapabilityReadSessionWhiteboard, r.resolvedSurface())
	read, err := toolutils.InferTool(name, readSessionWhiteboardDescription,
		func(ctx context.Context, input *ReadSessionWhiteboardInput) (*ReadSessionWhiteboardOutput, error) {
			return r.read(ctx, run, input)
		})
	if err != nil {
		return nil, fmt.Errorf("build session whiteboard read schema: %w", err)
	}
	return []tool.BaseTool{read}, nil
}

func (r *ReadSessionWhiteboardRegistry) resolvedSurface() serviceagent.ToolSurface {
	if r.surface == "" {
		return serviceagent.SurfaceLegacy
	}
	return r.surface
}

// read runs one call.
func (r *ReadSessionWhiteboardRegistry) read(ctx context.Context, run serviceagent.RunContext,
	input *ReadSessionWhiteboardInput) (*ReadSessionWhiteboardOutput, error) {
	out := &ReadSessionWhiteboardOutput{Status: whiteboardStatusOK, Reason: whiteboardReasonNone,
		Plan: []WhiteboardEntryView{}, OpenItems: []WhiteboardEntryView{}, Constraints: []WhiteboardEntryView{}}
	if input == nil {
		out.Status = whiteboardStatusUnavailable
		out.Reason = whiteboardReasonInvalidInput
		return out, nil
	}
	session := strings.TrimSpace(input.Session)
	if session == "" {
		out.Status = whiteboardStatusUnavailable
		out.Reason = whiteboardReasonInvalidInput
		return out, nil
	}
	out.Session = session
	// The board of another conversation is reachable only because a card
	// pointed at it. The check is made here rather than in the query, because
	// the query is owner-scoped and would happily answer for any session
	// this learner owns - including one the model invented.
	if session != strings.TrimSpace(run.SessionID) {
		if run.GrantSet == nil || !run.GrantSet.AuthorisesSession(run, session) {
			out.Status = whiteboardStatusNoEvidence
			out.Reason = whiteboardReasonNotAllowed
			return out, nil
		}
	}
	board, err := r.reader.GetWhiteboard(ctx, run.UserID, session)
	if err != nil {
		if ctxErr := contextError(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, model.ErrWhiteboardNotFound) {
			out.Status = whiteboardStatusNoEvidence
			out.Reason = whiteboardReasonNoBoard
			return out, nil
		}
		out.Status = whiteboardStatusUnavailable
		out.Reason = whiteboardReasonStoreFailure
		out.Retryable = true
		return out, nil
	}
	if board == nil {
		out.Status = whiteboardStatusNoEvidence
		out.Reason = whiteboardReasonNoBoard
		return out, nil
	}
	document, err := model.DecodeSessionWhiteboardDocument(board.Content)
	if err != nil {
		// The board is there and cannot be read. That is a fact about the
		// board rather than about the store, and it is not retryable.
		out.Status = whiteboardStatusUnavailable
		out.Reason = whiteboardReasonUnreadable
		return out, nil
	}
	out.SchemaVersion = document.SchemaVersion
	tokens := 0
	if document.Goal != nil {
		goal, _ := boundRunes(strings.TrimSpace(document.Goal.Text), maxWhiteboardEntryRunes)
		out.Goal = goal
		tokens += rag.EstimateTokens(goal) + 2
	}
	out.Plan, tokens = whiteboardEntries(document.Plan, tokens)
	out.OpenItems, tokens = whiteboardEntries(document.OpenItems, tokens)
	out.Constraints, tokens = whiteboardEntries(document.Constraints, tokens)
	out.ObservationTokens = tokens
	return out, nil
}

// whiteboardEntries renders one category of the board.
func whiteboardEntries(entries []model.SessionWhiteboardEntry, tokens int) ([]WhiteboardEntryView, int) {
	out := make([]WhiteboardEntryView, 0, len(entries))
	for _, entry := range entries {
		if len(out) >= maxWhiteboardEntries {
			break
		}
		text := strings.TrimSpace(entry.Text)
		if text == "" {
			continue
		}
		bounded, _ := boundRunes(text, maxWhiteboardEntryRunes)
		out = append(out, WhiteboardEntryView{Key: strings.TrimSpace(entry.Key), Text: bounded})
		tokens += rag.EstimateTokens(bounded) + 4
	}
	return out, tokens
}
