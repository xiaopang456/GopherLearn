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
//
// It carries the routing rule, because the routing is the model's to make:
// the server does not read the question and decide that the turn is about
// another conversation, because the server cannot see the whole conversation
// and the model can. A rule the server applied to the question's words would
// refuse exactly the questions that need it - "what did we talk about last
// week in the other chat" is a bare-looking question that is not about this
// conversation at all.
//
// So the rule is stated here, where the model reads it, and the server keeps
// the boundaries: whose history it may read, which conversation a read may
// be taken in, which ids a read may cite, and how much of it may come back.
const searchSessionCardsDescription = "Search your earlier conversations for the one a question is about. Prefer answering from THIS conversation: its current state, its summary segments and its recent words are already in front of you, and they are the answer to a question about this conversation. Call this tool only when the learner explicitly asks about another conversation, or when the previous turn settled which conversation is meant. Make ONE search and read what it returns: the thin cards locate the conversations, they do not carry their bodies. When the search returns ONE conversation that clearly matches, go straight into it - open it with browse_summary_segments or read_session_whiteboard and answer the question there. When it returns several that cannot be told apart, or none that is credible, ask the learner with ask_which_conversation straight away - do NOT open the candidates one by one to read their summaries first, because that spends a call per candidate on conversations the learner may not have meant. It returns candidates: the conversation id, its title, what it was about, when it was last talked in and the summary segments it points at. A date the learner named filters the candidates to that period."

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
	// The query is the question's own reading: the words, the topic keys,
	// how it bounded itself in time, and the period it named. The period
	// FILTERS - it says which conversations are eligible, and the decision
	// to leave this conversation at all is made by the cross-session
	// intent, not by the period.
	Recall(ctx context.Context, userID int64, currentSession string,
		query episode.ThinCardQuery) ([]episode.ThinCardCandidate, error)
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
	// The search runs when the model asks for it.
	//
	// Whether the turn is about another conversation is the MODEL's decision,
	// made with the whole conversation in front of it - and the tool's
	// description says how to make it. The server does not read the
	// question and refuse, because a rule applied to the question's words
	// refuses the questions that need it: "what did we talk about last week
	// in the other chat" looks like a bare pointer and is not about this
	// conversation at all.
	//
	// What the server keeps is the boundaries: whose history this reaches,
	// which conversations a read may be taken in, which ids it may cite, and
	// how much of it comes back.
	query := thinCardQuery(run.Question, strings.TrimSpace(input.Query))
	candidates, err := r.retriever.Recall(ctx, run.UserID, run.SessionID, query)
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
	// A NAMED conversation is looked up BY IDENTITY.
	//
	// It is the case the clarification makes: the learner picked one of the
	// conversations the model offered, and the model has to be able to open
	// THAT one. Searching for it again with the question's words would
	// find it only if its words happen to match, and the learner picked it
	// precisely because the words did not tell them apart.
	//
	// The lookup is the card index's own, so it is owner-scoped: a
	// conversation the learner does not own is not found, and neither is
	// one that has no card.
	if wanted != "" {
		candidate, found, lookupErr := r.retriever.CardOfSession(ctx, run.UserID, wanted)
		if lookupErr != nil {
			if ctxErr := contextError(ctx); ctxErr != nil {
				return nil, ctxErr
			}
			out.Status = cardsStatusUnavailable
			out.Reason = cardsReasonStoreFailure
			out.Retryable = true
			return out, nil
		}
		if found {
			// The conversation being answered is not a candidate: it is
			// already in the window, and offering it as a candidate would
			// send the model looking for something it has.
			if strings.TrimSpace(candidate.SessionID) != strings.TrimSpace(run.SessionID) {
				view := thinCardCandidateView(candidate)
				out.Candidates = append(out.Candidates, view)
				tokens += thinCardCandidateTokens(view)
				r.openCandidate(run, candidate)
			}
		}
		out.ObservationTokens = tokens
		if len(out.Candidates) == 0 {
			out.Status = cardsStatusNoEvidence
			out.Reason = cardsReasonNoMatch
		}
		return out, nil
	}
	for _, candidate := range candidates {
		if len(out.Candidates) >= limit {
			out.Truncated = true
			break
		}
		session := strings.TrimSpace(candidate.SessionID)
		if session == "" {
			continue
		}
		view := thinCardCandidateView(candidate)
		out.Candidates = append(out.Candidates, view)
		tokens += thinCardCandidateTokens(view)
		r.openCandidate(run, candidate)
	}
	out.ObservationTokens = tokens
	if len(out.Candidates) == 0 {
		out.Status = cardsStatusNoEvidence
		out.Reason = cardsReasonNoMatch
	}
	return out, nil
}

// openCandidate makes a candidate reachable for the rest of the turn.
//
// The card is the authority: it is the user's own card about the user's own
// earlier conversation, and it is what every read taken in that conversation
// is checked against. The run's own conversation is never added - a card
// about the conversation being answered is not a door to the past, and the
// grant set refuses it.
func (r *SearchSessionCardsRegistry) openCandidate(run serviceagent.RunContext, candidate episode.ThinCardCandidate) {
	if run.GrantSet == nil {
		return
	}
	session := strings.TrimSpace(candidate.SessionID)
	if session == "" {
		return
	}
	run.GrantSet.ExtendCardScope(run, serviceagent.CardScope{
		SessionID: session, CardID: candidate.CardID, CardVersion: candidate.CardVersion,
		// The watermark is the ceiling of every read taken in that
		// conversation through this card. A scope without one puts the
		// ceiling at zero, and a ceiling of zero hides every segment and
		// every message of the conversation the candidate just pointed at.
		CardWatermark: candidate.Watermark,
	})
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

// thinCardQuery reads the question into the query the card index is asked.
//
// THE PERIOD AND THE TEMPORAL MODE COME FROM THE LEARNER'S QUESTION, and the
// words come from the query the model passed.
//
// The split is deliberate, and it is the same split the cross-session intent
// makes. The model compresses a question into a query, and a compression
// drops the time word: "上周那份练习清单，单项里最后定的是写什么" becomes
// "练习清单". A period read from the query would therefore be lost exactly
// when it matters, and a period is a FILTER - it says which conversations
// are eligible.
//
// The words come from the model's query because the model knows what it is
// looking for, and the learner's own words are the fallback for a model that
// passed a query with no subject in it.
func thinCardQuery(learnerQuestion, modelQuery string) episode.ThinCardQuery {
	learner := episode.BuildReadPlan(episode.RouteRequest{Question: learnerQuestion})
	model := episode.BuildReadPlan(episode.RouteRequest{Question: modelQuery})
	terms := model.TopicTerms
	if len(terms) == 0 {
		terms = learner.TopicTerms
	}
	if len(terms) == 0 {
		terms = thinCardFallbackTerms(modelQuery)
	}
	if len(terms) == 0 {
		terms = thinCardFallbackTerms(learnerQuestion)
	}
	topics := model.ExactTopicKeys
	if len(topics) == 0 {
		topics = learner.ExactTopicKeys
	}
	return episode.ThinCardQuery{
		Terms:    terms,
		Topics:   topics,
		Temporal: learner.TemporalMode,
		From:     learner.From,
		To:       learner.To,
	}
}

// ThinCardQueryFor reads a clue into the query the card index is asked.
//
// It is the tool's own reading, exposed so that the server's cross-session
// lookup asks the index exactly what the tool would have asked. One copy of
// the rule: two readers of a question would otherwise drift, and the drift
// would show up as a candidate list the tool and the server disagree about.
func ThinCardQueryFor(learnerQuestion, modelQuery string) episode.ThinCardQuery {
	return thinCardQuery(learnerQuestion, modelQuery)
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
