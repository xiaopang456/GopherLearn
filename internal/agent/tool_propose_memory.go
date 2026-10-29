package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// ProposeMemoryName is the third tool this server can register, and the only one that
// writes. It stores a candidate; it does not decide anything.
const ProposeMemoryName = "propose_memory"

// MemoryProposal is everything a model is allowed to say about the memory it wants
// recorded: which layer, which subject, and the sentence itself.
//
// There is no owner, no session, no message list, no status and no confidence here, and
// that is the whole design. Those fields are what make a memory checkable and what decide
// whether it counts, so they are read from the turn's principal and from the server's own
// rules - never from text produced in the same conversation.
type MemoryProposal struct {
	Scope    domain.MemoryScope
	TopicKey string
	Content  string
}

// MemoryProposalOutcome reports what the write did. Created=false means the same claim was
// already waiting for its decision, which the tool says out loud instead of letting the
// model believe it just stored a second copy.
//
// Draft is the row that was stored, and it is only populated when Created is true: the
// writer returns the pre-existing record on a duplicate, and echoing that id onto this
// turn's card would invite a decision on a draft another conversation created.
type MemoryProposalOutcome struct {
	Created bool
	Draft   *domain.Memory
}

// MemoryProposer is the seam to the write side of long-term memory, and its signature is
// the security property: the principal is the only way a user, a session or a source
// message reaches the draft.
//
// It can store, and nothing else. There is no confirm, no reject, no archive and no list
// here, because a capability that could promote its own proposal would be a memory system
// where the model decides what the user believes - the exact thing the confirmation gate
// exists to prevent.
type MemoryProposer interface {
	ProposeMemory(ctx context.Context, principal Principal, proposal MemoryProposal) (MemoryProposalOutcome, error)
}

var (
	// ErrNoMemoryProposer reports a tool assembled without a writer: caught at
	// construction, so a running server cannot have an agent that offers a capability
	// nobody wired up.
	ErrNoMemoryProposer = errors.New("agent: propose_memory has no memory writer")
	// ErrProposalProvenanceMissing reports a turn with no stored question to cite. It
	// refuses rather than storing a sourceless memory, because an unconfirmable claim
	// cannot be corrected later: there is nothing left to look at.
	ErrProposalProvenanceMissing = errors.New("agent: this turn has no stored message for the draft to cite")
	// ErrProposalWrite reports a draft the memory service would not store. It is a
	// failure, not an empty result: "I could not record that" and "there was nothing to
	// record" are different answers, and only one of them is honest here.
	ErrProposalWrite = errors.New("agent: the memory draft could not be stored")
	// The three refusals below each also wrap ErrInvalidArguments, so a caller asking
	// "was this a bad-argument turn?" keeps working while a test can still name which
	// rule fired.
	ErrProposalScopeInvalid   = errors.New("agent: the proposed memory scope is not one this server offers")
	ErrProposalTopicKeyUse    = errors.New("agent: the proposed memory's topic key does not fit its scope")
	ErrProposalContentInvalid = errors.New("agent: the proposed memory content is blank or too long")
)

// ProposeMemoryOptions bounds what one proposal may carry. A configured value may loosen a
// bound down to the domain's own ceiling, never past it.
type ProposeMemoryOptions struct {
	// ContentRunes caps the proposed sentence. The domain allows a longer record; an
	// agent-observed one should be a sentence, which is what this bound is for.
	ContentRunes int
	// TopicKeyRunes caps a topic key, up to the domain's own limit.
	TopicKeyRunes int
}

// Normalize fills in working defaults with ceilings.
func (o ProposeMemoryOptions) Normalize() ProposeMemoryOptions {
	if o.ContentRunes <= 0 || o.ContentRunes > domain.MaxMemoryContentRunes {
		o.ContentRunes = 400
	}
	if o.TopicKeyRunes <= 0 || o.TopicKeyRunes > domain.MaxMemoryTopicKeyRunes {
		o.TopicKeyRunes = domain.MaxMemoryTopicKeyRunes
	}
	return o
}

// ProposeMemory is the propose_memory tool.
type ProposeMemory struct {
	proposer MemoryProposer
	options  ProposeMemoryOptions
}

// NewProposeMemory installs the tool. A missing writer is refused here rather than at
// first use.
func NewProposeMemory(proposer MemoryProposer, options ProposeMemoryOptions) (*ProposeMemory, error) {
	if proposer == nil {
		return nil, ErrNoMemoryProposer
	}
	return &ProposeMemory{proposer: proposer, options: options.Normalize()}, nil
}

// Name implements Tool.
func (t *ProposeMemory) Name() string { return ProposeMemoryName }

// Description tells the model what this capability is and, in the same breath, what it is
// not: a proposal nobody has confirmed is not a fact about the user, and cannot be treated
// as one. Where the decision happens is stated too, because a model that sends the user to
// another page is describing a UI this tool no longer drives.
func (t *ProposeMemory) Description() string {
	return "提议一条值得长期记住的候选记忆（全局事实与偏好、某个主题的理解、一次经历）。" +
		"服务端只会把它存成待确认草稿，归属用户、来源会话与来源消息都由服务端填写，" +
		"模型无法指定，也无法确认。草稿会出现在本轮回复下方的确认卡片里，" +
		"由用户在那里确认或拒绝；在用户确认之前它不是事实，不得当作已确认的记忆引用，" +
		"也不要引导用户去其它页面确认。每回合最多提议一条。"
}

// Parameters is the tool's whole public contract: three keys, an enumerated scope, and
// unknown keys refused. Anything that would decide the memory's owner, source or state is
// absent, so a model cannot ask for it and a provider cannot advertise it.
func (t *ProposeMemory) Parameters() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"object","additionalProperties":false,"required":["scope","content"],"properties":{`+
		`"scope":{"type":"string","enum":["global","topic","episodic"],`+
		`"description":"记忆层次：global 稳定事实与偏好，topic 某一主题的理解（需 topic_key），episodic 一次具体经历"},`+
		`"topic_key":{"type":"string","maxLength":%d,`+
		`"description":"主题标识；scope 为 topic 时必填，global 时必须留空"},`+
		`"content":{"type":"string","maxLength":%d,`+
		`"description":"要记住的那一句话，简体中文，不超过 %d 个字符"}}}`,
		t.options.TopicKeyRunes, t.options.ContentRunes, t.options.ContentRunes))
}

// proposeMemoryArguments mirrors Parameters(). The declared set is derived from this struct
// by the parser, so the schema and the accepted fields cannot drift apart.
type proposeMemoryArguments struct {
	Scope    string `json:"scope"`
	TopicKey string `json:"topic_key"`
	Content  string `json:"content"`
}

var _ Tool = (*ProposeMemory)(nil)

// Execute stores one pending draft as the turn's principal.
//
// Order matters: identity is checked before anything is parsed, so a turn that could not
// have produced a valid draft never reaches the point where model text is looked at; the
// arguments are then decoded strictly; and the source is read out of the principal, which
// the model never writes.
func (t *ProposeMemory) Execute(ctx context.Context, principal Principal, arguments json.RawMessage) (Observation, error) {
	if ctx == nil {
		return Observation{}, errors.New("agent: propose_memory needs a context")
	}
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	if strings.TrimSpace(principal.UserID) == "" {
		return Observation{}, ErrNoPrincipal
	}
	// The two fields a draft cites are also server-supplied. If either is missing, this
	// turn has no verifiable evidence, and the honest outcome is a refusal: a memory with
	// no source cannot be checked later, and an id invented to fill the gap would be a
	// citation pointing at nothing.
	if strings.TrimSpace(principal.SessionID) == "" || strings.TrimSpace(principal.UserMessageID) == "" {
		return Observation{}, fmt.Errorf("%w: %w", ErrProposalWrite, ErrProposalProvenanceMissing)
	}

	var parsed proposeMemoryArguments
	if err := decodeArguments(arguments, &parsed); err != nil {
		return Observation{}, err
	}
	proposal, err := t.validate(parsed)
	if err != nil {
		return Observation{}, err
	}

	outcome, err := t.proposer.ProposeMemory(ctx, principal, proposal)
	if err != nil {
		return Observation{}, fmt.Errorf("%w: %w", ErrProposalWrite, err)
	}
	// The sentence a watcher sees is chosen from the write's own outcome: a duplicate
	// claim created no record, and a step saying "已提交" then would send the learner off
	// to confirm something that was never written.
	summary := "同样的候选草稿已在等待确认，本轮没有新增"
	if outcome.Created {
		summary = "新增 1 条待确认记忆草稿"
	}
	return Observation{Text: t.acknowledged(outcome), Found: false,
		Summary: summary, Draft: draftCard(outcome)}, nil
}

// draftCard projects the row this turn stored, and refuses to project anything else.
//
// A created flag without a usable row produces no card rather than a card with a blank or
// borrowed id: a button that addresses nothing would report a decision the server never
// received. The scope has to be one this server offers for the same reason - an unlisted
// value would render as a label nobody defined.
func draftCard(outcome MemoryProposalOutcome) *responder.MemoryDraft {
	if !outcome.Created || outcome.Draft == nil {
		return nil
	}
	stored := outcome.Draft
	if strings.TrimSpace(stored.MemoryID) == "" || strings.TrimSpace(stored.Content) == "" ||
		!stored.Scope.IsValid() {
		return nil
	}
	// The topic key travels with the scope that owns it and with nothing else, so a row
	// that somehow carries both a global scope and a subject cannot put that pair in
	// front of a learner as one label.
	topicKey := stored.TopicKey
	if stored.Scope != domain.MemoryScopeTopic {
		topicKey = ""
	}
	return &responder.MemoryDraft{
		DraftID:  stored.MemoryID,
		Scope:    stored.Scope,
		TopicKey: topicKey,
		Content:  stored.Content,
	}
}

// validate applies the rules the schema advertises, because a tool must not assume the
// provider enforced anything. It mirrors domain.MemoryDraft's field rules so an illegal
// proposal is refused as a bad argument rather than surfacing later as a write failure.
func (t *ProposeMemory) validate(parsed proposeMemoryArguments) (MemoryProposal, error) {
	scope := domain.MemoryScope(strings.TrimSpace(parsed.Scope))
	if !scope.IsValid() {
		// The offending value is model text, so it is described rather than quoted: an
		// error that echoes it back can carry conversation content into a log or a
		// provider reply.
		return MemoryProposal{}, fmt.Errorf("%w: scope is not one of global, topic, episodic: %w",
			ErrInvalidArguments, ErrProposalScopeInvalid)
	}
	topicKey := strings.TrimSpace(parsed.TopicKey)
	switch scope {
	case domain.MemoryScopeGlobal:
		// A global fact carrying a key would stop being global: the "one active version"
		// key is (user, scope, topic_key), so a stray key splits the namespace in two.
		if topicKey != "" {
			return MemoryProposal{}, fmt.Errorf("%w: a global memory must not carry a topic key: %w",
				ErrInvalidArguments, ErrProposalTopicKeyUse)
		}
	case domain.MemoryScopeTopic:
		if topicKey == "" {
			return MemoryProposal{}, fmt.Errorf("%w: a topic memory needs a topic key: %w",
				ErrInvalidArguments, ErrProposalTopicKeyUse)
		}
	}
	if len([]rune(topicKey)) > t.options.TopicKeyRunes {
		return MemoryProposal{}, fmt.Errorf("%w: topic key longer than %d characters: %w",
			ErrInvalidArguments, t.options.TopicKeyRunes, ErrProposalTopicKeyUse)
	}
	if strings.IndexFunc(topicKey, unicode.IsControl) >= 0 {
		return MemoryProposal{}, fmt.Errorf("%w: topic key must not contain control characters: %w",
			ErrInvalidArguments, ErrProposalTopicKeyUse)
	}
	content := strings.TrimSpace(parsed.Content)
	if content == "" {
		return MemoryProposal{}, fmt.Errorf("%w: content must not be blank: %w",
			ErrInvalidArguments, ErrProposalContentInvalid)
	}
	if len([]rune(content)) > t.options.ContentRunes {
		return MemoryProposal{}, fmt.Errorf("%w: content longer than %d characters: %w",
			ErrInvalidArguments, t.options.ContentRunes, ErrProposalContentInvalid)
	}
	return MemoryProposal{Scope: scope, TopicKey: topicKey, Content: content}, nil
}

// acknowledged is the whole of what the model learns about its own proposal: that a draft
// is waiting, that confirming it is somebody else's job, and no identifiers it could reuse.
// No memory id, no session id, no message id - a value the model never sees is a value it
// cannot cite, forge, or ask for by name.
func (t *ProposeMemory) acknowledged(outcome MemoryProposalOutcome) string {
	prefix := ObservationLabel + " " + ProposeMemoryName + "："
	if !outcome.Created {
		return prefix + "同样的候选草稿已经在等待用户确认，本轮没有重复生成。" +
			"它仍然只是待确认草稿，不是用户已经确认的事实。" +
			"请不要再次提议，也不要在用户确认前把它当作记忆引用。"
	}
	return prefix + "已生成一条待确认记忆草稿，用户会在本轮回复下方的确认卡片里选择确认或拒绝。" +
		"在用户确认之前，它不是事实：不要把它当作已确认的记忆引用，也不要声称已经记住。"
}
