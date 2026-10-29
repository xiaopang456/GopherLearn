package responder

import (
	"context"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// Turn is one prior conversation message as the responder sees it. It carries
// role and text only: stored citations belong to the history endpoint, not to a
// model prompt, where they would be noise the model might imitate.
type Turn struct {
	Role    string
	Content string
}

// Request identifies the conversation input supplied to a responder. Session
// fields are resolved server-side from the stored session plus this turn's
// knowledge options; a responder must not re-derive them from client input.
type Request struct {
	UserID                 string
	SessionID              string
	Content                string
	SessionMode            domain.SessionMode
	AnswerStrategy         domain.AnswerStrategy
	KnowledgeEnabled       bool
	KnowledgeCollectionIDs []string
	// UserMessageID is the stored id of the question this turn is answering. The service
	// knows it only after the user message has been written, so it is filled in after
	// openTurn rather than by prepareTurn - and it is the only message id a tool is
	// allowed to cite as provenance.
	//
	// It exists because a proposal about "what just happened" must point at something that
	// actually happened. The assistant answer is not stored yet at this point, so naming it
	// would mean inventing an id for a record that does not exist; naming an older message
	// would mean citing evidence outside this turn. Empty means the turn has no stored
	// question, and a capability that needs provenance must refuse rather than proceed.
	UserMessageID string
	// TopicMastery is a server-written note about how well the learner has recently
	// performed on the topic they last practised. It is empty for every strategy that
	// does not use it, and it carries no numbers: an explanation of "you are at 77" is
	// not something a learner should receive from a model that was only asked to explain
	// a subject. The agent that plans the next explanation reads it, the answer text does
	// not.
	TopicMastery string
	// Mastery is kept separate from the policy prompt so the context window can
	// apply the v2 mastery budget and trim it without changing the non-trimmable
	// system rules.
	Mastery string
	// TaskState is the server's structured statement of the learner's active quiz:
	// its topic, status, progress, and - when one is open - the stem of the
	// question now being asked. It is resolved from the store like every other
	// field here, it is carried only by an agent turn, and it is required rather
	// than decorative context: a turn whose quiz the model cannot see cannot
	// answer "which question was that" from anything but invention.
	TaskState string
	// History is the already-stored conversation, oldest first, trimmed to the
	// service's context budget. It excludes Content, so the question being
	// answered appears exactly once in what a model is sent.
	History []Turn
	// Summary is the session's sealed rolling summary: the compressed record of
	// exchanges that have already left History. It is resolved by the service from
	// stored state, like every other field here, and it is one value shared by all
	// three answer strategies - a strategy that summarized the conversation its own
	// way would give the same user three different recollections of the past.
	//
	// Empty means "there is no summary yet", never "there is nothing to recall":
	// the recent window is still sent in full.
	Summary string
	// These three fields are server-resolved learning-agent context layers. They are
	// intentionally separate from SystemPrompt so contextwindow can trim them by
	// priority without granting them policy authority. Chat and knowledge requests
	// leave them empty.
	GlobalMemory  string
	TopicMemory   string
	EpisodeMemory string
}

// Answer is one assistant reply plus the citations that support it. Sources is
// empty for a direct answer, so an ungrounded turn stays shaped exactly as it
// was before retrieval existed.
type Answer struct {
	Content string
	Sources []domain.Source
	// FinalBlockID names the streamed text block whose fragments are this answer's text, set
	// by a strategy that published its answer through blocks. It is the address
	// message.completed's final_block_id carries, and it exists so the receipt a learner reads
	// can be checked against the characters they already watched arrive - rather than being a
	// new copy of the answer that happens to look similar.
	//
	// Empty means no block claimed this answer, which is what a non-streaming turn, a
	// re-chunked turn, and every chat or knowledge answer reports.
	FinalBlockID string
}

// Responder is the model boundary used by application services. Implementations
// may be local test doubles or later provider adapters; HTTP code never knows
// which one is installed.
type Responder interface {
	Respond(context.Context, Request) (Answer, error)
	Stream(context.Context, Request, func(string) error) (Answer, error)
}

// EchoResponder is the deterministic direct answer used for development and
// tests. It performs no network or provider calls and returns the caller's text
// unchanged, adding no citations: an answer through this responder was not
// retrieved and is not grounded.
type EchoResponder struct{}

func (EchoResponder) Respond(ctx context.Context, request Request) (Answer, error) {
	select {
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	default:
	}
	return Answer{Content: request.Content}, nil
}

func (EchoResponder) Stream(ctx context.Context, request Request, emit func(string) error) (Answer, error) {
	content := request.Content
	if content == "" {
		return Answer{}, nil
	}
	runes := []rune(content)
	for offset := 0; offset < len(runes); {
		select {
		case <-ctx.Done():
			return Answer{}, ctx.Err()
		default:
		}
		end := offset + 16
		if end > len(runes) {
			end = len(runes)
		}
		part := string(runes[offset:end])
		if err := emit(part); err != nil {
			return Answer{}, err
		}
		offset = end
	}
	return Answer{Content: strings.Clone(content)}, nil
}
