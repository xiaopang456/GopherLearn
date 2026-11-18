package agenttool

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"GopherAI/common/rag"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/episode"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

// The tool that finds the earlier conversations a question could be about.
//
// It is the first step of the on-demand retrieval chain, and it is built on
// the thin card: a card that carries no body, so keeping it current costs no
// model call, and a question that names nothing in particular can still be
// answered with "these are the conversations you had".
//
// What it returns is a CANDIDATE, not an answer. The body of a conversation is
// read afterwards, from the conversation the model picks, and only from that
// one: a question about an earlier session brings one session's summary into
// the window instead of every session's card.
//
// The candidates it returns are the user's own earlier conversations, and the
// tool opens them for the rest of the turn. It cannot open a conversation the
// card index did not find, and it never opens the conversation being answered:
// the present is not the past.

const SearchSessionCardsName = "search_session_cards"

// The bounds of one search.
const (
	// maxSearchSessionCards is how many candidates one call may return.
	maxSearchSessionCards = 8
	// defaultSearchSessionCards is how many it returns when the model does
	// not say.
	defaultSearchSessionCards = 5
	// maxThinCardTitleRunes bounds one candidate's title. A title is a name,
	// not a paragraph.
	maxThinCardTitleRunes = 120
)

// searchSessionCardsDescription is the contract the model reads.
const searchSessionCardsDescription = "Search your earlier conversations for the one a question is about. Use it when you do not yet know which conversation holds the answer. It returns candidates: the conversation id, its title, what it was about, when it was last talked in and the summary segments it points at. It reads no transcript. When the question says \"last time\", the candidates are ordered newest first, so the first one is the last conversation the learner had: check its summary against the question, and if the evidence does not match, go on to the next candidate. Pick one candidate and read that conversation with browse_summary_segments or read_session_whiteboard."

// The states a search can report.
const (
	cardsStatusOK          = "ok"
	cardsStatusNoEvidence  = "no_evidence"
	cardsStatusUnavailable = "unavailable"
)

// The reasons a search gives.
const (
	cardsReasonNone         = "none"
	cardsReasonNoMatch      = "no_match"
	cardsReasonInvalidInput = "invalid_request"
	cardsReasonStoreFailure = "store_unavailable"
	// cardsReasonCurrentFirst says the question was a bare pointer and THIS
	// conversation has the history, so the answer is here before it is
	// anywhere else.
	cardsReasonCurrentFirst = "current_session_has_history"
)

// SearchSessionCardsInput is what the model asks for.
type SearchSessionCardsInput struct {
	Session    string `json:"session,omitempty" jsonschema:"description=Limit the search to one conversation id. Leave empty to search all of them."`
	Query      string `json:"query" jsonschema:"required,description=The clue to look for: the subject, a name, or a phrase from the conversation you are trying to find."`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"description=How many candidates to return, 1-8."`
}

// SearchSessionCardsOutput is what the call reports.
type SearchSessionCardsOutput struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
	// Candidates are the conversations that could be the one, best first.
	Candidates []ThinCardCandidateView `json:"candidates"`
	// ObservationTokens is what the answer has to spend on this observation.
	ObservationTokens int `json:"observation_tokens"`
	// Notes says what the call did not do, and why.
	Notes     string `json:"notes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

// ThinCardCandidateView is one candidate as the model sees it.
//
// It is the card's own words and where its body is. It is not the body: the
// segments are named, not quoted.
type ThinCardCandidateView struct {
	// SessionID is the conversation. The model names it back when it wants
	// the conversation opened.
	SessionID string `json:"session_id"`
	// CardID and CardVersion identify the card this candidate came from.
	CardID      string `json:"card_id"`
	CardVersion uint64 `json:"card_version"`
	// Title is the conversation's own title.
	Title string `json:"title"`
	// Topics are the subjects the conversation was filed under.
	Topics []string `json:"topics"`
	// Names are the proper names the conversation recorded.
	Names []string `json:"names"`
	// SegmentSeqs are the summary segments of that conversation. They are
	// where the body is.
	SegmentSeqs []int `json:"segment_seqs"`
	// LastTalkedAt is when the conversation was last talked in.
	LastTalkedAt string `json:"last_talked_at,omitempty"`
	// Score is the search's own score.
	Score float64 `json:"score"`
}

// ThinCardRetriever finds the earlier conversations a question could be about.
//
// It is the retriever's own narrow interface rather than the concrete type,
// so that the tool can be exercised without an index and without a database.
type ThinCardRetriever interface {
	// Recall returns the conversations that could be the one. It reads the
	// cards and never their bodies.
	//
	// The temporal mode is the question's own reading of "last time", and
	// it decides whether the time channel is the whole answer or one of
	// three. It is not a hint: a question that named no subject is answered
	// by the newest conversations alone.
	Recall(ctx context.Context, userID int64, currentSession string, terms, topics []string,
		temporal episode.TemporalMode) ([]episode.ThinCardCandidate, error)
	// CardOfSession returns the card of one conversation, by identity.
	//
	// It is the lookup a segment hit is checked against: a segment search
	// can find a stretch of turns in a conversation whose card the WORD
	// search did not find, and checking by words would make that hit
	// unreachable in exactly the case the segment search exists for.
	CardOfSession(ctx context.Context, userID int64, sessionID string) (episode.ThinCardCandidate, bool, error)
}

// SearchSessionCardsRegistry builds the search tool.
type SearchSessionCardsRegistry struct {
	retriever ThinCardRetriever
	surface   serviceagent.ToolSurface
}

// NewSearchSessionCardsRegistry builds the registry.
func NewSearchSessionCardsRegistry(retriever ThinCardRetriever) (*SearchSessionCardsRegistry, error) {
	if retriever == nil {
		return nil, fmt.Errorf("%w: a thin card retriever is required", ErrInvalidKnowledgeSearchConfig)
	}
	return &SearchSessionCardsRegistry{retriever: retriever}, nil
}

// UseToolSurface changes only the name and description shown to the model.
func (r *SearchSessionCardsRegistry) UseToolSurface(surface serviceagent.ToolSurface) {
	r.surface = surface
}

// Tools returns the search tool for one run.
func (r *SearchSessionCardsRegistry) Tools(ctx context.Context, run serviceagent.RunContext) ([]tool.BaseTool, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if run.UserID <= 0 || strings.TrimSpace(run.SessionID) == "" {
		return nil, fmt.Errorf("%w: a trusted user id and session are required", ErrInvalidKnowledgeSearchRequest)
	}
	name := serviceagent.MustToolNameFor(serviceagent.CapabilitySearchSessionCards, r.resolvedSurface())
	search, err := toolutils.InferTool(name, searchSessionCardsDescription,
		func(ctx context.Context, input *SearchSessionCardsInput) (*SearchSessionCardsOutput, error) {
			return r.search(ctx, run, input)
		})
	if err != nil {
		return nil, fmt.Errorf("build session card search schema: %w", err)
	}
	return []tool.BaseTool{search}, nil
}

func (r *SearchSessionCardsRegistry) resolvedSurface() serviceagent.ToolSurface {
	if r.surface == "" {
		return serviceagent.SurfaceLegacy
	}
	return r.surface
}

// search runs one call.
func (r *SearchSessionCardsRegistry) search(ctx context.Context, run serviceagent.RunContext,
	input *SearchSessionCardsInput) (*SearchSessionCardsOutput, error) {
	out := &SearchSessionCardsOutput{Status: cardsStatusOK, Reason: cardsReasonNone,
		Candidates: []ThinCardCandidateView{}}
	if input == nil || strings.TrimSpace(input.Query) == "" {
		out.Status = cardsStatusUnavailable
		out.Reason = cardsReasonInvalidInput
		return out, nil
	}
	limit := input.MaxResults
	if limit <= 0 {
		limit = defaultSearchSessionCards
	}
	if limit > maxSearchSessionCards {
		limit = maxSearchSessionCards
	}
	// The order of the search is decided before the search runs.
	//
	// A question that only points - "last time", "last week" - and a
	// conversation that already has a history of its own is answered HERE
	// first. Going looking elsewhere before looking here is the wrong order,
	// and the learner pays for the wrong order in a card search that returns
	// an earlier conversation's answer to a question about this one.
	//
	// The rule reads the LEARNER's question rather than the tool's query:
	// the model may have put "last time" in the query while the learner
	// wrote "the three exercises you set me last time", and the second is a
	// question about an earlier conversation.
	intent := episode.CrossSessionIntentOf(run.Question, run.CurrentSessionHasHistory)
	if !intent.Cross {
		out.Status = cardsStatusNoEvidence
		out.Reason = cardsReasonCurrentFirst
		out.Notes = "This question points at a conversation without naming a subject, and this conversation already has a history. Answer from it. Search again only if you can name a subject, a name or a phrase from the conversation you are looking for."
		return out, nil
	}
	terms, topics, temporal := thinCardQueryTerms(strings.TrimSpace(input.Query))
	candidates, err := r.retriever.Recall(ctx, run.UserID, run.SessionID, terms, topics, temporal)
	if err != nil {
		if ctxErr := contextError(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		out.Status = cardsStatusUnavailable
		out.Reason = cardsReasonStoreFailure
		out.Retryable = true
		return out, nil
	}
	wanted := strings.TrimSpace(input.Session)
	tokens := 0
	for _, candidate := range candidates {
		if len(out.Candidates) >= limit {
			out.Truncated = true
			break
		}
		session := strings.TrimSpace(candidate.SessionID)
		if session == "" {
			continue
		}
		if wanted != "" && session != wanted {
			continue
		}
		view := thinCardCandidateView(candidate)
		out.Candidates = append(out.Candidates, view)
		tokens += thinCardCandidateTokens(view)
		// The candidate becomes reachable for the rest of the turn.
		//
		// The card is the authority: it is the user's own card about the
		// user's own earlier conversation, and it is what every read taken
		// in that conversation is checked against. The run's own
		// conversation is never added - a card about the conversation
		// being answered is not a door to the past, and the grant set
		// refuses it.
		if run.GrantSet != nil {
			run.GrantSet.ExtendCardScope(run, serviceagent.CardScope{
				SessionID: session, CardID: candidate.CardID, CardVersion: candidate.CardVersion,
				// The watermark is the ceiling of every read taken in that
				// conversation through this card. A scope without one puts
				// the ceiling at zero, and a ceiling of zero hides every
				// segment and every message of the conversation the candidate
				// just pointed at.
				CardWatermark: candidate.Watermark,
			})
		}
	}
	out.ObservationTokens = tokens
	if len(out.Candidates) == 0 {
		out.Status = cardsStatusNoEvidence
		out.Reason = cardsReasonNoMatch
	}
	return out, nil
}

// thinCardCandidateView renders one candidate.
func thinCardCandidateView(candidate episode.ThinCardCandidate) ThinCardCandidateView {
	view := ThinCardCandidateView{
		SessionID:   strings.TrimSpace(candidate.SessionID),
		CardID:      candidate.CardID,
		CardVersion: candidate.CardVersion,
		Title:       candidate.Title,
		Topics:      append([]string{}, candidate.Topics...),
		Names:       append([]string{}, candidate.Names...),
		SegmentSeqs: append([]int{}, candidate.SegmentSeqs...),
		Score:       candidate.Score,
	}
	title, _ := boundRunes(view.Title, maxThinCardTitleRunes)
	view.Title = title
	if !candidate.LastTalkedAt.IsZero() {
		view.LastTalkedAt = candidate.LastTalkedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return view
}

// thinCardCandidateTokens is what one candidate costs the answer.
func thinCardCandidateTokens(view ThinCardCandidateView) int {
	tokens := rag.EstimateTokens(view.Title) + 8
	for _, topic := range view.Topics {
		tokens += rag.EstimateTokens(topic) + 1
	}
	for _, name := range view.Names {
		tokens += rag.EstimateTokens(name) + 1
	}
	return tokens
}

// thinCardQueryTerms reads the question into the terms, the topic keys and
// the temporal mode the card index is searched by.
//
// The read plan does the reading: it removes the history shell and the
// discourse frame, so what is left is the subject of the question. The topic
// keys are the plan's exact ones, and they are empty when the question named
// no subject - a card that is filed under no topic cannot be found by one.
//
// The temporal mode is the plan's too, and it is what decides whether the
// newest conversations are the whole answer or one of the answers.
func thinCardQueryTerms(query string) ([]string, []string, episode.TemporalMode) {
	plan := episode.BuildReadPlan(episode.RouteRequest{Question: query})
	terms := plan.TopicTerms
	if len(terms) == 0 {
		terms = thinCardFallbackTerms(query)
	}
	return terms, plan.ExactTopicKeys, plan.TemporalMode
}

// thinCardFallbackTerms splits a question the read plan read as having no
// subject at all.
//
// It is the fallback, not the first choice: the plan's terms are better
// because the plan knows which words describe HOW the question was asked. This
// one only knows how to cut a question into pieces.
//
// The Chinese bigrams go to the untokenized tag field beside the text, because
// the Chinese tokenizer cannot match a term its dictionary does not hold.
func thinCardFallbackTerms(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	})
	out := make([]string, 0, len(fields)+8)
	seen := make(map[string]struct{}, len(fields)+8)
	add := func(term string) {
		term = strings.TrimSpace(term)
		if len([]rune(term)) < 2 {
			return
		}
		key := strings.ToLower(term)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		out = append(out, term)
	}
	for _, field := range fields {
		add(field)
	}
	for _, bigram := range rag.HanBigramQueryTerms(rag.ExtractLexicalTerms(query)) {
		add(bigram)
	}
	return out
}
