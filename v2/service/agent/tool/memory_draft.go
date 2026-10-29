package agenttool

import (
	"GopherAI/common/topicmatch"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	memoryservice "GopherAI/service/memory"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

const ProposeMemoryName = "propose_memory"

type MemoryDraftProposer interface {
	ProposeDraft(context.Context, memoryservice.DraftRequest) (*model.MemoryDraft, bool, error)
}

type MemoryDraftRegistry struct {
	proposer MemoryDraftProposer
	topics   *memoryservice.TopicCatalogue
	now      func() time.Time
	// explicitNames exposes the same proposer under the STABILIZE-061 name and
	// description. The permission gate, the identity handling, the idempotency
	// key and the confirmation flag are untouched by it.
	explicitNames bool
}

// UseExplicitActionNames switches this registry to the STABILIZE-061 surface.
func (r *MemoryDraftRegistry) UseExplicitActionNames(enabled bool) { r.explicitNames = enabled }

func NewMemoryDraftRegistry(proposer MemoryDraftProposer) (*MemoryDraftRegistry, error) {
	if proposer == nil {
		return nil, fmt.Errorf("memory draft proposer is required")
	}
	return &MemoryDraftRegistry{proposer: proposer, now: time.Now}, nil
}

// UseTopicCatalogue attaches the same topic namespace used by learning and
// UserMemory retrieval. Without it, a model can create a legal but isolated
// key such as "tcp-sticky-packet" while mastery is stored under "tcp粘包".
func (r *MemoryDraftRegistry) UseTopicCatalogue(catalogue *memoryservice.TopicCatalogue) {
	r.topics = catalogue
}

// UseClock keeps the time shown to the model aligned with the service clock.
// It is primarily useful for deterministic evaluation; production uses
// time.Now. Validation still happens inside GovernanceService, so the model
// never becomes the authority for whether an expiry is acceptable.
func (r *MemoryDraftRegistry) UseClock(now func() time.Time) {
	if now != nil {
		r.now = now
	}
}

type ProposeMemoryInput struct {
	Scope      string   `json:"scope" jsonschema:"required,enum=global,enum=topic,description=global applies across topics; topic is the user's own understanding or difficulty for one technical topic."`
	Subject    string   `json:"subject" jsonschema:"required,description=Concise subject no longer than 200 characters."`
	Content    string   `json:"content" jsonschema:"required,description=Self-contained fact the user explicitly asked to remember; no longer than 400 characters."`
	Tags       []string `json:"tags,omitempty" jsonschema:"description=Zero to five lowercase retrieval tags."`
	TopicKey   string   `json:"topic_key,omitempty" jsonschema:"description=Required for topic scope and forbidden for global scope."`
	Durability string   `json:"durability" jsonschema:"required,enum=stable,enum=time_bound"`
	ValidUntil string   `json:"valid_until,omitempty" jsonschema:"description=Future RFC3339 timestamp required only for time_bound."`
}

type ProposeMemoryOutput struct {
	DraftID                  string                 `json:"draft_id"`
	Scope                    model.MemoryScope      `json:"scope"`
	Subject                  string                 `json:"subject"`
	Content                  string                 `json:"content"`
	Tags                     model.StringList       `json:"tags"`
	TopicKey                 string                 `json:"topic_key,omitempty"`
	Durability               model.MemoryDurability `json:"durability"`
	ValidUntil               *time.Time             `json:"valid_until,omitempty"`
	ConflictIDs              model.StringList       `json:"conflict_ids"`
	RequiresUserConfirmation bool                   `json:"requires_user_confirmation"`
	Replay                   bool                   `json:"replay"`
}

func (r *MemoryDraftRegistry) Tools(ctx context.Context, run serviceagent.RunContext) ([]tool.BaseTool, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if run.UserID <= 0 || strings.TrimSpace(run.SessionID) == "" || strings.TrimSpace(run.RunID) == "" {
		return nil, fmt.Errorf("trusted memory draft identity is required")
	}
	// Tool visibility is an authorization boundary, not a semantic prediction.
	// The model may propose a draft only when the raw user question contains a
	// deterministic write-intent signal. Ordinary questions and memory-recall
	// questions therefore cannot expose this mutating workflow to the model.
	if !explicitMemoryIntent(run.Question) {
		return nil, nil
	}
	var topics []memoryservice.Topic
	if r.topics != nil {
		var err error
		topics, err = r.topics.List(ctx, run.UserID)
		if err != nil {
			return nil, fmt.Errorf("load memory topic catalogue: %w", err)
		}
	}
	now := r.now().In(time.Local)
	name := ProposeMemoryName
	description := "Use only when the user explicitly asks to remember/save a personal preference, goal, correction, understanding, confusion, analogy, or pitfall. Global is for background, teaching preferences, study goals/plans and prohibitions even when they mention a technical subject. Topic is only for the learner's own understanding, confusion, analogy or pitfall about one technical topic. This creates a visible draft only; it never writes active memory. Do not use for ordinary chat, assistant inferences, retrieved note passages, or quiz scores."
	if r.explicitNames {
		name = serviceagent.SaveMemoryDraftToolName
		description = saveMemoryDraftDescription
	}
	description += fmt.Sprintf("\nCurrent server time is %s. For time_bound memory, valid_until must be a future RFC3339 timestamp with timezone. If the user omits the year, choose the next future occurrence of that month and day; for a stated 'before/by' date, use the end of that local day.", now.Format(time.RFC3339))
	if catalogue := memoryservice.CatalogueForPrompt(topics, 100); catalogue != "" {
		description += "\n" + catalogue
	}
	propose, err := toolutils.InferTool(name,
		description,
		func(ctx context.Context, input *ProposeMemoryInput) (*ProposeMemoryOutput, error) {
			if input == nil {
				return nil, fmt.Errorf("memory proposal input is required")
			}
			payload, _ := json.Marshal(input)
			digest := sha256.Sum256(append([]byte(run.RunID+":"), payload...))
			var validUntil *time.Time
			if value := strings.TrimSpace(input.ValidUntil); value != "" {
				parsed, err := time.Parse(time.RFC3339, value)
				if err != nil {
					return nil, err
				}
				validUntil = &parsed
			}
			// Model-generated labels are suggestions, not authority. Normalize
			// them to the server vocabulary while keeping direct user input strict.
			normalizedTags := memoryservice.NormalizeMemoryTags(input.Tags)
			topicKey := input.TopicKey
			if input.Scope == string(model.MemoryScopeTopic) {
				topicKey = resolveDraftTopicKey(topics, run.Question, input.TopicKey)
			}
			draft, created, err := r.proposer.ProposeDraft(ctx, memoryservice.DraftRequest{
				UserID: run.UserID, SessionID: run.SessionID, RunID: run.RunID,
				Input: memoryservice.ExplicitMemoryInput{
					Scope: model.MemoryScope(input.Scope), Subject: input.Subject, Content: input.Content, Tags: normalizedTags,
					TopicKey: topicKey, Durability: model.MemoryDurability(input.Durability), ValidUntil: validUntil,
					IdempotencyKey: "agent:" + hex.EncodeToString(digest[:]),
				},
			})
			if err != nil {
				return nil, err
			}
			return &ProposeMemoryOutput{
				DraftID: draft.ID, Scope: draft.Scope, Subject: draft.Subject, Content: draft.Content, Tags: draft.Tags,
				TopicKey: draft.TopicKey, Durability: draft.Durability, ValidUntil: draft.ValidUntil, ConflictIDs: draft.ConflictIDs,
				RequiresUserConfirmation: true, Replay: !created,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{propose}, nil
}

func resolveDraftTopicKey(topics []memoryservice.Topic, question, proposed string) string {
	key, matched := memoryservice.Resolve(topics, proposed)
	if matched || len(topics) == 0 {
		return key
	}
	prepared := topicmatch.NewQuestion(question)
	bestKey, bestScore, tied := "", 0.0, false
	for _, topic := range topics {
		score := prepared.Coverage(topic.Key)
		if named := prepared.Coverage(topic.Name); named > score {
			score = named
		}
		if score < topicmatch.DefaultMinCoverage {
			continue
		}
		switch {
		case score > bestScore:
			bestKey, bestScore, tied = topic.Key, score, false
		case score == bestScore && topic.Key != bestKey:
			tied = true
		}
	}
	if bestKey != "" && !tied {
		return bestKey
	}
	return key
}

func explicitMemoryIntent(question string) bool {
	value := strings.ToLower(strings.TrimSpace(question))
	if value == "" {
		return false
	}

	// A recall question reads existing memory; it does not grant permission to
	// create a new draft. Check it before English phrases such as "remember my".
	if containsAny(value, []string{
		"你还记得", "还记得我", "还记得这", "是否记得", "记不记得",
		"do you remember", "did you remember", "what do you remember", "can you remember",
	}) {
		return false
	}

	if containsAny(value, []string{
		"请记住", "记住我", "记住这", "记住：", "记住:", "帮我记", "给我记", "替我记",
		"记一下", "记下来", "保存这", "保存为我的",
		"remember this", "remember that", "remember my", "please remember", "save this", "save that", "keep this in mind",
	}) {
		return true
	}

	// Cross-session defaults are also explicit memory instructions even when
	// the user does not literally say "remember".
	futureDirective := containsAny(value, []string{
		"以后", "今后", "从现在起", "接下来", "from now on", "going forward", "in the future",
	})
	persistentPolicy := containsAny(value, []string{
		"请默认", "默认我", "都按", "请一直", "一直按", "请始终", "始终先", "请把我当",
		"default to", "assume that i", "always treat", "always explain", "always use",
	})
	return futureDirective && persistentPolicy
}

func containsAny(value string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}
