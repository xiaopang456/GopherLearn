package memory

import (
	agentdao "GopherAI/dao/agent"
	learningdao "GopherAI/dao/learning"
	"GopherAI/model"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

type DatabaseEvidenceLoader struct {
	db       *gorm.DB
	agent    *agentdao.Repository
	learning *learningdao.Repository
}

func NewDatabaseEvidenceLoader(db *gorm.DB) (*DatabaseEvidenceLoader, error) {
	if db == nil {
		return nil, fmt.Errorf("memory evidence database is required")
	}
	return &DatabaseEvidenceLoader{db: db, agent: agentdao.NewRepository(db), learning: learningdao.NewRepository(db)}, nil
}

func (l *DatabaseEvidenceLoader) LoadEvidence(ctx context.Context, job *model.MemoryExtractionJob) (Evidence, error) {
	if job == nil || job.UserID <= 0 {
		return Evidence{}, fmt.Errorf("invalid memory evidence job")
	}
	switch job.EventType {
	case model.MemoryEventLearningComplete:
		return l.learningEvidence(ctx, job)
	case model.MemoryEventAgentRun, model.MemoryEventExplicitRemember:
		return l.agentEvidence(ctx, job)
	case model.MemoryEventSummaryCompacted:
		return l.summaryEvidence(ctx, job)
	default:
		return Evidence{}, fmt.Errorf("unsupported memory event %q", job.EventType)
	}
}

func (l *DatabaseEvidenceLoader) summaryEvidence(ctx context.Context, job *model.MemoryExtractionJob) (Evidence, error) {
	var count int64
	if err := l.db.WithContext(ctx).Model(&model.Session{}).Where("id = ? AND user_id = ?", job.SourceSessionID, job.UserID).Count(&count).Error; err != nil {
		return Evidence{}, fmt.Errorf("verify summary session: %w", err)
	}
	if count != 1 || strings.TrimSpace(job.EventPayload) == "" {
		return Evidence{}, ErrNoDurableMemory
	}
	content, err := durableSummaryEvidence(job.EventPayload)
	if err != nil {
		return Evidence{}, err
	}
	return Evidence{
		EventType: job.EventType, Authority: EvidenceAuthorityUserSummary, UserID: job.UserID, SourceID: job.SourceID, SourceSessionID: job.SourceSessionID,
		SourceRefs: []string{"session_summary:" + job.SourceID}, Content: content,
	}, nil
}

// sessionScopedSummaryFields describe what a session is doing, not what is true
// of the learner, and must not reach the extractor.
//
// `current_goal` is the summarizer's reading of the conversation in progress.
// Handing it over produced exactly the memory that reading predicts: asked three
// questions about serialization history, the system stored "The user's current
// goal is to clarify prior decisions and discussion history regarding
// serialization" as a global, stable fact — which would then be injected into
// every future conversation, forever, describing an intent that expired when
// that session ended.
//
// This is the same boundary EvidenceAuthority already draws elsewhere: an
// assistant inference is not evidence about the user. A goal the learner states
// in their own words still reaches memory through the agent-run path, where it
// carries user authority instead of being inferred.
//
// The list covers the summary's three progress fields, which say where the
// conversation had got to: what it is trying to do, what it has not finished,
// and what it has. Removing only current_goal was tried first and was not
// enough — the very next session summarised as
//
//	open_loops: ["How PacketBuffer handles sticky packets (粘包)", ...]
//
// and the extractor stored "The learner is currently investigating how
// PacketBuffer handles..." as a stable global fact. completed_items is the
// complement of open_loops and carries the same defect by construction.
//
// The summary's remaining fields are kept, because they assert something about
// the learner rather than about the session: confirmed_decisions and
// user_corrections are durable by definition, and key_errors records a mistake
// in the learner's understanding, which is exactly what topic memory is for.
type durableSummaryDocument struct {
	ConfirmedDecisions []string `json:"confirmed_decisions,omitempty"`
	UserCorrections    []string `json:"user_corrections,omitempty"`
	KeyErrors          []string `json:"key_errors,omitempty"`
	References         []string `json:"references,omitempty"`
}

// durableSummaryEvidence strictly decodes the producer contract and rebuilds
// an allowlisted document. Returning the raw payload on any decode/encode
// failure would let session-scoped state bypass this write-authority boundary.
func durableSummaryEvidence(payload string) (string, error) {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return "", ErrNoDurableMemory
	}
	if !strings.HasPrefix(trimmed, "{") {
		return "", fmt.Errorf("%w: expected JSON object", ErrInvalidSummaryEvidence)
	}
	document, err := model.DecodeSessionSummaryDocument(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: decode: %v", ErrInvalidSummaryEvidence, err)
	}
	if len([]rune(strings.TrimSpace(document.CurrentGoal))) > 1000 {
		return "", fmt.Errorf("%w: current_goal is too long", ErrInvalidSummaryEvidence)
	}
	groups := []*[]string{&document.CompletedItems, &document.KeyErrors, &document.References}
	for _, group := range groups {
		cleaned, err := cleanSummaryEvidenceItems(*group)
		if err != nil {
			return "", err
		}
		*group = cleaned
	}
	confirmed := summaryEvidenceTexts(document.ConfirmedDecisions)
	corrections := summaryEvidenceTexts(document.UserCorrections)
	if len(confirmed)+len(corrections)+len(document.KeyErrors) == 0 {
		return "", ErrNoDurableMemory
	}
	durable := durableSummaryDocument{
		ConfirmedDecisions: confirmed,
		UserCorrections:    corrections,
		KeyErrors:          document.KeyErrors,
		References:         document.References,
	}
	encoded, err := json.Marshal(durable)
	if err != nil {
		return "", fmt.Errorf("%w: encode: %v", ErrInvalidSummaryEvidence, err)
	}
	return string(encoded), nil
}

func summaryEvidenceTexts(items model.SessionSummaryEvidenceList) []string {
	texts := make([]string, 0, len(items))
	for _, item := range items {
		if text := strings.TrimSpace(item.Text); text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

func cleanSummaryEvidenceItems(items []string) ([]string, error) {
	cleaned := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if len([]rune(item)) > 1000 {
			return nil, fmt.Errorf("%w: summary item is too long", ErrInvalidSummaryEvidence)
		}
		cleaned = append(cleaned, item)
		if len(cleaned) > 20 {
			return nil, fmt.Errorf("%w: too many summary items", ErrInvalidSummaryEvidence)
		}
	}
	return cleaned, nil
}

func (l *DatabaseEvidenceLoader) agentEvidence(ctx context.Context, job *model.MemoryExtractionJob) (Evidence, error) {
	run, err := l.agent.GetRun(ctx, job.UserID, job.SourceSessionID, job.SourceRunID)
	if err != nil {
		return Evidence{}, err
	}
	steps, err := l.agent.ListSteps(ctx, job.UserID, job.SourceSessionID, job.SourceRunID)
	if err != nil {
		return Evidence{}, err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "run_status=%s\ntermination=%s\nuser_input=%s\nfinal_answer=%s\n", run.Status, run.TerminationReason, run.InputSummary, run.AnswerSummary)
	for _, step := range steps {
		if step.Type == model.AgentStepTool || step.Type == model.AgentStepAnswer {
			fmt.Fprintf(&out, "step=%s tool=%s result=%s error=%s\n", step.Type, step.ToolName, step.ResultSummary, step.ErrorMessage)
		}
	}
	authority := EvidenceAuthorityUserConfirmed
	if job.EventType != model.MemoryEventExplicitRemember {
		authority = agentRunAuthority(steps)
	}
	return Evidence{
		EventType: job.EventType, Authority: authority, UserID: job.UserID, SourceID: job.SourceID,
		SourceSessionID: job.SourceSessionID, SourceRunID: job.SourceRunID,
		SourceRefs: []string{"agent_run:" + run.ID}, Content: boundedEvidence(out.String(), 12000),
	}, nil
}

func (l *DatabaseEvidenceLoader) learningEvidence(ctx context.Context, job *model.MemoryExtractionJob) (Evidence, error) {
	var session model.LearningSession
	err := l.db.WithContext(ctx).Where("id = ? AND user_id = ? AND session_id = ?", job.SourceID, job.UserID, job.SourceSessionID).First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Evidence{}, fmt.Errorf("learning session not found")
	}
	if err != nil {
		return Evidence{}, err
	}
	var attempts []model.LearningAttempt
	if err := l.db.WithContext(ctx).Where("learning_session_id = ? AND user_id = ? AND status = ?", session.ID, job.UserID, model.LearningAttemptCompleted).Order("completed_at ASC").Find(&attempts).Error; err != nil {
		return Evidence{}, err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "topic=%s\ntopic_key=%s\nstatus=%s\n", session.Topic, session.TopicKey, session.Status)
	refs := []string{"learning_session:" + session.ID}
	for _, attempt := range attempts {
		fmt.Fprintf(&out, "attempt=%s score=%d feedback=%s missing=%s\n", attempt.ID, attempt.EffectiveTotal, attempt.Feedback, strings.Join(attempt.MissingPoints, ","))
		refs = append(refs, "learning_attempt:"+attempt.ID)
	}
	if len(attempts) == 0 {
		return Evidence{}, ErrNoDurableMemory
	}
	return Evidence{EventType: job.EventType, Authority: EvidenceAuthorityLearningAssessment, UserID: job.UserID, SourceID: job.SourceID, SourceSessionID: job.SourceSessionID, SourceRefs: refs, Content: boundedEvidence(out.String(), 12000)}, nil
}

func agentRunAuthority(steps []model.AgentStep) EvidenceAuthority {
	toolCount := 0
	knowledgeCount := 0
	for _, step := range steps {
		if step.Type != model.AgentStepTool {
			continue
		}
		toolCount++
		if step.ToolName == "knowledge_search" || step.ToolName == "document_read" {
			knowledgeCount++
		}
	}
	if toolCount > 0 && toolCount == knowledgeCount {
		return EvidenceAuthorityKnowledgeOnly
	}
	return EvidenceAuthorityAssistantInference
}

func boundedEvidence(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return value
}
