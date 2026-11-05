package agenttool

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

// The browse tool for the session's summary segments.
//
// The window carries the newest segments and the few the server believes the
// question is about. Everything older is one tool call away: this is how the
// model reaches a stretch of the conversation that was compressed long ago
// and is not in front of it.
//
// The model never names an internal id. It names a segment number, which is
// an ordinal inside this session and means nothing outside it, and the
// server takes the session and the owner from the run. A tool that read
// whatever id it was handed would let the model enumerate the learner's
// whole session by counting.

const BrowseSummarySegmentsName = "browse_summary_segments"

// The bounds of one browse call.
//
// Five segments and three hundred tokens each are the plan's numbers. The
// per-segment bound is what keeps one long segment from spending the whole
// observation on a single stretch of conversation.
const (
	maxBrowseSummarySegments = 5
	maxBrowseSummaryTokens   = 300
	// browseSummaryCandidateLimit bounds how many segments the browse tool
	// looks at when it searches. A session with more segments than this has
	// a tail the browse tool does not reach by search; the model can still
	// name a segment number directly.
	browseSummaryCandidateLimit = 512
)

// browseSummarySegmentsDescription is the contract the model reads.
const browseSummarySegmentsDescription = "Browse this session's earlier summary segments. Use it when the segments already in front of you do not cover what the learner is asking about, or when they are too compressed to answer from. A segment is a compressed record of earlier turns, not a transcript: the words the learner used are read back with read_history_message. Segment numbers are counted from 1 within this session."

// The states a browse call can report.
const (
	browseStatusOK          = "ok"
	browseStatusNoEvidence  = "no_evidence"
	browseStatusUnavailable = "unavailable"
)

// The reasons a browse call gives.
const (
	browseReasonNone             = "none"
	browseReasonNoCandidate      = "no_candidate"
	browseReasonIndexUnavailable = "index_unavailable"
	browseReasonCancelled        = "cancelled"
	browseReasonInvalidRequest   = "invalid_request"
)

// BrowseSummarySegmentsInput is what the model asks for.
type BrowseSummarySegmentsInput struct {
	Query      string `json:"query" jsonschema:"required,description=Search terms to match against the segment topics, points, names and numbers."`
	Segments   []int  `json:"segments,omitempty" jsonschema:"description=Optional specific segment numbers to read directly, counted from 1 within this session. At most five."`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"description=How many segments to return, 1-5."`
}

// SummarySegmentView is one segment as the model sees it.
type SummarySegmentView struct {
	Seq            int      `json:"seq"`
	StartMessageID int      `json:"start_message_id"`
	EndMessageID   int      `json:"end_message_id"`
	Topic          string   `json:"topic"`
	Points         []string `json:"points"`
	// NamesAndNumbers are the exact terms the segment recorded.
	NamesAndNumbers []string `json:"names_and_numbers,omitempty"`
	// WhiteboardStatus names the values of this segment that the board has
	// since replaced. A value listed here is not the current one.
	WhiteboardStatus []string `json:"whiteboard_status,omitempty"`
}

// BrowseSummarySegmentsOutput is what the call reports.
type BrowseSummarySegmentsOutput struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
	// Segments are the segments that were found, oldest first.
	Segments []SummarySegmentView `json:"segments"`
	// ObservationTokens is what the answer has to spend on this observation.
	ObservationTokens int    `json:"observation_tokens"`
	Truncated         bool   `json:"truncated,omitempty"`
	Retryable         bool   `json:"retryable,omitempty"`
	NextStep          string `json:"next_step,omitempty"`
}

// SummarySegmentBrowser reads the segments of one session.
//
// The browser is owner-scoped: it takes the user id and the session id from
// the run, never from the model. A segment from another conversation is not
// reachable by naming a number, because the number is looked up inside the
// session the run is in.
type SummarySegmentBrowser interface {
	GetSummarySegment(ctx context.Context, userID int64, sessionID string, seq int) (*model.SessionSummarySegment, error)
	ListSummarySegments(ctx context.Context, userID int64, sessionID string, limit int) ([]model.SessionSummarySegment, error)
	GetWhiteboard(ctx context.Context, userID int64, sessionID string) (*model.SessionWhiteboard, error)
}

// SummarySegmentStore is the storage the segment verifier reads.
//
// It is the narrow read the session repository already offers, and it is
// owner-scoped: a segment of another session is a lookup miss, not a
// permission check somebody has to remember.
type SummarySegmentStore interface {
	GetSummarySegment(ctx context.Context, userID int64, sessionID string, seq int) (*model.SessionSummarySegment, error)
}

// summarySegmentEvidenceAdapter turns the store's row into the facts the
// verifier checks.
type summarySegmentEvidenceAdapter struct {
	store SummarySegmentStore
}

// NewSummarySegmentEvidenceReader wires the store-side verifier for the
// summary segments' citations.
func NewSummarySegmentEvidenceReader(store SummarySegmentStore) SummarySegmentEvidenceReader {
	return summarySegmentEvidenceAdapter{store: store}
}

// LoadSummarySegmentEvidence reads one segment's facts.
func (a summarySegmentEvidenceAdapter) LoadSummarySegmentEvidence(ctx context.Context, userID int64,
	sessionID string, seq int) (SummarySegmentEvidenceFacts, error) {
	if a.store == nil {
		return SummarySegmentEvidenceFacts{}, fmt.Errorf("summary segment store is not wired")
	}
	segment, err := a.store.GetSummarySegment(ctx, userID, sessionID, seq)
	if err != nil {
		return SummarySegmentEvidenceFacts{}, err
	}
	return SummarySegmentEvidenceFacts{
		Seq: segment.Seq, UserID: segment.UserID, SessionID: segment.SessionID,
		EndMessageID: segment.EndMessageID,
	}, nil
}

// BrowseSummarySegmentsRegistry builds the browse tool.
type BrowseSummarySegmentsRegistry struct {
	browser SummarySegmentBrowser
	surface serviceagent.ToolSurface
}

// NewBrowseSummarySegmentsRegistry builds the registry.
func NewBrowseSummarySegmentsRegistry(browser SummarySegmentBrowser) (*BrowseSummarySegmentsRegistry, error) {
	if browser == nil {
		return nil, fmt.Errorf("%w: summary segment browser is required", ErrInvalidKnowledgeSearchConfig)
	}
	return &BrowseSummarySegmentsRegistry{browser: browser}, nil
}

// UseToolSurface changes only the name and description shown to the model.
func (r *BrowseSummarySegmentsRegistry) UseToolSurface(surface serviceagent.ToolSurface) {
	r.surface = surface
}

// Tools returns the browse tool for one run.
func (r *BrowseSummarySegmentsRegistry) Tools(ctx context.Context, run serviceagent.RunContext) ([]tool.BaseTool, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if run.UserID <= 0 || strings.TrimSpace(run.SessionID) == "" {
		return nil, fmt.Errorf("%w: a trusted user id and session are required", ErrInvalidKnowledgeSearchRequest)
	}
	name := serviceagent.MustToolNameFor(serviceagent.CapabilityBrowseSummarySegments, r.resolvedSurface())
	browse, err := toolutils.InferTool(name, browseSummarySegmentsDescription,
		func(ctx context.Context, input *BrowseSummarySegmentsInput) (*BrowseSummarySegmentsOutput, error) {
			return r.browse(ctx, run, input)
		})
	if err != nil {
		return nil, fmt.Errorf("build summary segment browse schema: %w", err)
	}
	return []tool.BaseTool{browse}, nil
}

func (r *BrowseSummarySegmentsRegistry) resolvedSurface() serviceagent.ToolSurface {
	if r.surface == "" {
		return serviceagent.SurfaceLegacy
	}
	return r.surface
}

// browse runs one call.
func (r *BrowseSummarySegmentsRegistry) browse(ctx context.Context, run serviceagent.RunContext,
	input *BrowseSummarySegmentsInput) (*BrowseSummarySegmentsOutput, error) {
	out := &BrowseSummarySegmentsOutput{Status: browseStatusOK, Reason: browseReasonNone, Segments: []SummarySegmentView{}}
	if input == nil {
		out.Status = browseStatusUnavailable
		out.Reason = browseReasonInvalidRequest
		return out, nil
	}
	limit := input.MaxResults
	if limit <= 0 {
		limit = maxBrowseSummarySegments
	}
	if limit > maxBrowseSummarySegments {
		limit = maxBrowseSummarySegments
	}
	segments, err := r.resolve(ctx, run, input, limit)
	if err != nil {
		if ctxErr := contextError(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, model.ErrSummarySegmentNotFound) {
			out.Status = browseStatusNoEvidence
			out.Reason = browseReasonNoCandidate
			return out, nil
		}
		out.Status = browseStatusUnavailable
		out.Reason = browseReasonIndexUnavailable
		out.Retryable = true
		return out, nil
	}
	if len(segments) == 0 {
		out.Status = browseStatusNoEvidence
		out.Reason = browseReasonNoCandidate
		return out, nil
	}
	document := r.boardState(ctx, run)
	grants := make([]serviceagent.HistoryMessageGrant, 0, 8)
	tokens := 0
	for _, segment := range segments {
		view, cost, segmentGrants := r.render(run, segment, document)
		if cost > maxBrowseSummaryTokens {
			// A segment that does not fit inside one segment's budget is
			// trimmed rather than dropped: the first points are the ones the
			// generator put first, and they are the ones about the topic.
			view = trimSummarySegmentView(view, maxBrowseSummaryTokens-tokens)
			cost = estimateViewTokens(view)
			out.Truncated = true
		}
		if tokens+cost > maxBrowseSummaryTokens*maxBrowseSummarySegments {
			out.Truncated = true
			break
		}
		tokens += cost
		out.Segments = append(out.Segments, view)
		grants = append(grants, segmentGrants...)
	}
	out.ObservationTokens = tokens
	// The ids the browse tool just showed the model are now readable, and the
	// run has to know it: the read-back tool refuses ids it was not given.
	if run.GrantSet != nil {
		run.GrantSet.Extend(run, grants)
	}
	if len(out.Segments) == 0 {
		out.Status = browseStatusNoEvidence
		out.Reason = browseReasonNoCandidate
		return out, nil
	}
	return out, nil
}

// resolve finds the segments the call asked for.
//
// Named segments are read directly, because a model that names a segment
// knows which one it wants. Otherwise the question is matched against the
// session's segments, and the best matches are returned.
func (r *BrowseSummarySegmentsRegistry) resolve(ctx context.Context, run serviceagent.RunContext,
	input *BrowseSummarySegmentsInput, limit int) ([]model.SessionSummarySegment, error) {
	if len(input.Segments) > 0 {
		named := input.Segments
		if len(named) > maxBrowseSummarySegments {
			named = named[:maxBrowseSummarySegments]
		}
		segments := make([]model.SessionSummarySegment, 0, len(named))
		for _, seq := range named {
			if seq <= 0 {
				continue
			}
			segment, err := r.browser.GetSummarySegment(ctx, run.UserID, run.SessionID, seq)
			if err != nil {
				if errors.Is(err, model.ErrSummarySegmentNotFound) {
					continue
				}
				return nil, err
			}
			segments = append(segments, *segment)
		}
		return segments, nil
	}
	all, err := r.browser.ListSummarySegments(ctx, run.UserID, run.SessionID, browseSummaryCandidateLimit)
	if err != nil {
		return nil, err
	}
	ranked := serviceagent.RankSummarySegmentsForQuestion(input.Query, all)
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked, nil
}

// boardState reads the board.
//
// The board may be missing or unreadable, and then the segments are shown
// without the supersession notes: a note that cannot be verified is not
// written, because a wrong note would mark a current value as superseded.
func (r *BrowseSummarySegmentsRegistry) boardState(ctx context.Context, run serviceagent.RunContext) model.SessionWhiteboardDocument {
	board, err := r.browser.GetWhiteboard(ctx, run.UserID, run.SessionID)
	if err != nil || board == nil {
		return model.SessionWhiteboardDocument{}
	}
	document, err := model.DecodeSessionWhiteboardDocument(board.Content)
	if err != nil {
		return model.SessionWhiteboardDocument{}
	}
	return document
}

// render turns one stored segment into the view the model reads.
func (r *BrowseSummarySegmentsRegistry) render(run serviceagent.RunContext, segment model.SessionSummarySegment,
	document model.SessionWhiteboardDocument) (SummarySegmentView, int, []serviceagent.HistoryMessageGrant) {
	content, err := model.DecodeSessionSummarySegmentContent(segment.Content)
	if err != nil {
		return SummarySegmentView{}, 0, nil
	}
	view := SummarySegmentView{
		Seq: segment.Seq, StartMessageID: int(segment.StartMessageID), EndMessageID: int(segment.EndMessageID),
		Topic: content.Topic, Points: []string{},
	}
	for _, point := range content.Points {
		view.Points = append(view.Points, serviceagent.RenderSummarySegmentPoint(point))
	}
	view.NamesAndNumbers = append(view.NamesAndNumbers, content.Names...)
	view.NamesAndNumbers = append(view.NamesAndNumbers, content.Numbers...)
	for _, note := range serviceagent.SummarySegmentSupersessionNotes(segment, document) {
		view.WhiteboardStatus = append(view.WhiteboardStatus,
			fmt.Sprintf("%s: %s -> %s（第 %d 轮，以当前状态为准）", note.Key, note.OldValue, note.CurrentValue,
				note.ChangedAtMessageID))
	}
	grants := make([]serviceagent.HistoryMessageGrant, 0, 8)
	for _, id := range content.EvidenceMessageIDs() {
		if id == 0 || id > segment.EndMessageID {
			continue
		}
		grants = append(grants, serviceagent.HistoryMessageGrant{
			MessageID: id, SourceKind: serviceagent.HistorySourceSummarySegment,
			SourceSessionID: run.SessionID, SegmentSeq: segment.Seq,
			SegmentEndMessageID: segment.EndMessageID,
		})
	}
	return view, estimateViewTokens(view), grants
}

// trimSummarySegmentView drops the tail of a view until it fits.
func trimSummarySegmentView(view SummarySegmentView, budget int) SummarySegmentView {
	if budget <= 0 {
		view.Points = []string{}
		return view
	}
	for len(view.Points) > 0 {
		candidate := view
		candidate.Points = view.Points[:len(view.Points)-1]
		if estimateViewTokens(candidate) <= budget {
			return candidate
		}
		view.Points = candidate.Points
	}
	return view
}

// estimateViewTokens is what one view costs the answer.
func estimateViewTokens(view SummarySegmentView) int {
	total := len(view.Topic) / 2
	for _, point := range view.Points {
		total += len(point) / 2
	}
	for _, term := range view.NamesAndNumbers {
		total += len(term) / 2
	}
	for _, status := range view.WhiteboardStatus {
		total += len(status) / 2
	}
	return total + 8
}
