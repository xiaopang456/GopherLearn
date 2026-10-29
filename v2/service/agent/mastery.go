package agent

import (
	"GopherAI/common/topicmatch"
	learningdao "GopherAI/dao/learning"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// AGENT-017: mastery is L1b cross-session structured state, not long-term
// memory. It is recomputed from graded attempts by TopicProgress, so this
// provider reads it live instead of storing a natural-language snapshot that
// would silently go stale after the next quiz.
//
// Two rules are enforced here rather than trusted to the prompt:
//
//   - No score ever reaches the model. Deterministic Go code turns the score
//     into a teaching instruction. A model that never sees "67/100" cannot
//     leak it, and cannot treat it as a fact about the user.
//   - Nothing in this file writes. The only path that may change mastery is
//     grading a LearningAttempt.
type MasteryProvider interface {
	CurrentMastery(context.Context, RunContext) (string, error)
}

// MasteryTopicKeyProvider exposes the exact topics selected by the same
// deterministic match that rendered the instruction. ContextBuilder uses this
// as strong evidence for Topic UserMemory without asking a second matcher (or
// a model) to guess the topic again.
type MasteryTopicKeyProvider interface {
	CurrentMasteryWithTopics(context.Context, RunContext) (string, []string, error)
}

// MasteryTopics separates the two things the returned key slice used to mix.
//
// STABILIZE-039 needs the distinction because it spends the keys differently.
// Matched keys pick which assessment shapes the explanation: a wrong one makes
// the answer badly pitched. Authoritative keys decide which of the learner own
// private notes are put in front of the model before it has read anything, with
// a citable S# attached - a wrong one there is the server asserting, in the
// learner own voice, what they think about a subject they never mentioned. The
// two cannot share an entry gate, so they no longer share a slice.
type MasteryTopics struct {
	// Authoritative are TopicKeys named by server state rather than derived
	// from the question. In this version there is exactly one source: the
	// active LearningSession of this user in this chat session. Nothing the
	// model, the client or a matcher produced may enter here.
	Authoritative []string
	// Matched is the full set the deterministic matcher selected, unchanged
	// from what CurrentMasteryWithTopics has always returned. It is a superset
	// of Authoritative only by accident, never by construction: a terminal or
	// expired learning session still matches with score 2 and still lands here,
	// and is deliberately not authoritative.
	Matched []string
}

// MasterySourcedTopicProvider is the source-aware form of the reader above.
//
// It is a third interface rather than a change to the second because the second
// has callers whose behaviour must not move. legacy_v2 and every v3 mode keep
// consuming Matched through CurrentMasteryWithTopics and see nothing new.
type MasterySourcedTopicProvider interface {
	CurrentMasteryWithTopicSources(context.Context, RunContext) (string, MasteryTopics, error)
}

type MasteryProviderFunc func(context.Context, RunContext) (string, error)

func (f MasteryProviderFunc) CurrentMastery(ctx context.Context, input RunContext) (string, error) {
	return f(ctx, input)
}

type EmptyMasteryProvider struct{}

func (EmptyMasteryProvider) CurrentMastery(context.Context, RunContext) (string, error) {
	return "", nil
}

// TopicProgressReader is the read-only slice of the learning repository that
// mastery injection is allowed to use.
type TopicProgressReader interface {
	ListTopicProgress(context.Context, int64, int) ([]model.TopicProgress, error)
}

type MasteryConfig struct {
	// CandidateLimit bounds the database read, not the injected result.
	CandidateLimit int
	// MaxTopics bounds how many matched topics reach the prompt.
	MaxTopics int
	// MinConfidentAttempts mirrors the learning service definition of
	// "enough evidence". Below it, depth is not adjusted.
	MinConfidentAttempts int
	// StaleAfter marks an assessment as merely indicative once it is old.
	StaleAfter time.Duration
	// AttemptTextLimit bounds the attempt rows read to build the term index.
	// It spans every topic, so it is larger than a per-topic limit would be.
	AttemptTextLimit int
	// MinCoverage and MinBigrams were constants until the claim that the
	// remaining failures are inherent needed testing. A claim that no setting
	// fixes them can only be checked by sweeping the settings.
	MinCoverage float64
	MinBigrams  int
}

func DefaultMasteryConfig() MasteryConfig {
	return MasteryConfig{
		CandidateLimit:       50,
		MaxTopics:            2,
		MinConfidentAttempts: 3,
		StaleAfter:           60 * 24 * time.Hour,
		AttemptTextLimit:     120,
		MinCoverage:          minTopicCoverage,
		MinBigrams:           minDistinctiveBigrams,
	}
}

func (c MasteryConfig) Validate() error {
	if c.CandidateLimit < 1 || c.CandidateLimit > 200 {
		return fmt.Errorf("mastery candidate limit must be between 1 and 200")
	}
	if c.MaxTopics < 1 || c.MaxTopics > 5 {
		return fmt.Errorf("mastery topic count must be between 1 and 5")
	}
	if c.MinConfidentAttempts < 1 {
		return fmt.Errorf("mastery minimum confident attempts must be positive")
	}
	if c.StaleAfter <= 0 {
		return fmt.Errorf("mastery stale window must be positive")
	}
	return nil
}

type TopicProgressMasteryProvider struct {
	detail   TopicBundleReader
	terms    TopicTextReader
	progress TopicProgressReader
	learning ActiveLearningReader
	config   MasteryConfig
	now      func() time.Time
}

func NewMasteryProvider(progress TopicProgressReader, learning ActiveLearningReader, config MasteryConfig) (*TopicProgressMasteryProvider, error) {
	if progress == nil {
		return nil, fmt.Errorf("topic progress reader is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &TopicProgressMasteryProvider{progress: progress, learning: learning, config: config, now: time.Now}, nil
}

// UseTopicDetail attaches the full record for a matched topic: the questions
// the learner was asked, the points the grader found missing, and the notes
// filed under it.
//
// Without it the prompt gets a band and a weak-point list — enough to pick a
// depth, not enough to pick a subject. The quiz module already stores the rest
// (LearningQuestion.Question, LearningAttempt.MissingPoints) and the screen
// shows it to the learner; it simply never reached the Agent. Twelve attempts
// of question plus missing points is roughly 850 runes, so the reason it was
// left out was never budget.
//
// Optional on purpose: a deployment without the join still gets the band, and
// a failure to load detail degrades to the band rather than dropping mastery.
// UseClock fixes the assessment clock. Production never calls it; a frozen
// fixture corpus does, so that staleness and learning-session expiry are
// properties of the fixture rather than of the day the evaluation ran.
func (p *TopicProgressMasteryProvider) UseClock(now func() time.Time) {
	if now != nil {
		p.now = now
	}
}

func (p *TopicProgressMasteryProvider) UseTopicDetail(detail TopicBundleReader) {
	p.detail = detail
}

// UseTopicTerms lets a question reach a topic through the vocabulary of the
// questions the learner was asked, not only through the topic's name.
//
// Optional, and a failure to load leaves name matching in place: widening how a
// topic can be found must never be able to stop it being found at all.
func (p *TopicProgressMasteryProvider) UseTopicTerms(terms TopicTextReader) {
	p.terms = terms
}

func (p *TopicProgressMasteryProvider) CurrentMastery(ctx context.Context, input RunContext) (string, error) {
	text, _, err := p.CurrentMasteryWithTopics(ctx, input)
	return text, err
}

func (p *TopicProgressMasteryProvider) CurrentMasteryWithTopics(ctx context.Context, input RunContext) (string, []string, error) {
	text, topics, err := p.CurrentMasteryWithTopicSources(ctx, input)
	return text, topics.Matched, err
}

// CurrentMasteryWithTopicSources does the one pass both callers need.
//
// There is a single active-learning read here, and both results come out of it.
// Splitting the interface must not split the query: a second GetActive would be
// a second chance for the two answers to disagree about which topic this turn
// is on, on top of costing a round trip on the request path.
func (p *TopicProgressMasteryProvider) CurrentMasteryWithTopicSources(ctx context.Context, input RunContext) (string, MasteryTopics, error) {
	if input.UserID <= 0 || strings.TrimSpace(input.Question) == "" {
		return "", MasteryTopics{}, fmt.Errorf("mastery lookup requires a trusted user id and a question")
	}

	// An active quiz names its topic authoritatively; question matching is
	// only the fallback for ordinary chat.
	activeKey := ""
	var topics MasteryTopics
	if p.learning != nil {
		active, err := p.learning.GetActive(ctx, input.UserID, input.SessionID)
		if err != nil && !errors.Is(err, learningdao.ErrLearningSessionNotFound) {
			return "", MasteryTopics{}, fmt.Errorf("load active learning topic: %w", err)
		}
		if err == nil && active != nil {
			// Unchanged for matching: whatever GetActive returned still wins
			// the lexical contest with score 2, exactly as before.
			activeKey = active.TopicKey
			// Stricter for bootstrap. GetActive resolves an active_key row and
			// checks neither status nor expiry, and the pre-039 matcher did not
			// either. A finished, cancelled or expired quiz is a fine reason to
			// keep pitching an explanation at that topic and a bad reason to
			// put the learner private notes in front of the model as their
			// current subject.
			if key, ok := authoritativeTopicKey(active, input, p.now()); ok {
				topics.Authoritative = []string{key}
			}
		}
	}

	candidates, err := p.progress.ListTopicProgress(ctx, input.UserID, p.config.CandidateLimit)
	if err != nil {
		return "", MasteryTopics{}, fmt.Errorf("list topic progress: %w", err)
	}
	if len(candidates) == 0 {
		// The authoritative key survives an empty progress list. It came from
		// the learning session, not from TopicProgress, so a learner on their
		// very first quiz still gets their topic notes.
		return "", topics, nil
	}

	var terms *topicTerms
	if p.terms != nil {
		records, err := p.terms.ListRecentAttemptTexts(ctx, input.UserID, p.config.AttemptTextLimit)
		if err != nil {
			log.Printf("[agent-mastery] topic terms degraded: error_type=%T", err)
		} else {
			terms = buildTopicTerms(records)
		}
	}

	matched := matchTopics(candidates, input.Question, activeKey, input.UserID, p.config, terms)
	if len(matched) == 0 {
		return "", topics, nil
	}
	rendered := renderMastery(matched, p.now().UTC(), p.config)
	topics.Matched = make([]string, 0, len(matched))
	for _, item := range matched {
		topics.Matched = append(topics.Matched, item.progress.TopicKey)
	}
	if p.detail == nil {
		return rendered, topics, nil
	}
	return rendered + p.renderDetail(ctx, input.UserID, matched), topics, nil
}

// authoritativeTopicKey is the whole trust boundary of STABILIZE-039, in one
// place so there is exactly one thing to read when asking what the server is
// willing to vouch for.
//
// Every clause is a rejection, and the two ownership clauses are defence in
// depth rather than logic: the DAO already filters on both, and a leak across
// accounts is a hard gate failure, so they are re-checked where the decision is
// actually made.
func authoritativeTopicKey(active *model.LearningSession, input RunContext, now time.Time) (string, bool) {
	if active == nil {
		return "", false
	}
	if active.UserID != input.UserID || active.SessionID != strings.TrimSpace(input.SessionID) {
		return "", false
	}
	if active.Status.Terminal() || !active.ExpiresAt.After(now) {
		return "", false
	}
	key := model.NormalizeLearningTopicKey(active.TopicKey)
	if key == "" {
		return "", false
	}
	return key, true
}

// The mastery block has a shape the budget has to respect, so the two lines
// that mark it are named rather than written twice. Everything between the
// header and the instruction is one quiz record per line, and those records are
// the only part that may be dropped when the block does not fit.
const (
	masteryDetailHeader       = "What they have already been asked on"
	masteryRecordsInstruction = "These records are facts, not judgements:"
)

// renderDetail appends what the learner was actually asked and what they left
// out. It never fails the turn: detail is an enrichment of an instruction that
// is already complete without it.
func (p *TopicProgressMasteryProvider) renderDetail(ctx context.Context, userID int64, matched []matchedTopic) string {
	var out strings.Builder
	for _, item := range matched {
		bundle, err := p.detail.LoadTopic(ctx, userID, item.progress.TopicKey)
		if err != nil {
			log.Printf("[agent-mastery] topic detail degraded: topic=%s error_type=%T", item.progress.TopicKey, err)
			continue
		}
		if len(bundle.RecentAttempts) == 0 && len(bundle.Notes) == 0 {
			continue
		}
		fmt.Fprintf(&out, "\n%s \"%s\":\n", masteryDetailHeader, strings.TrimSpace(item.progress.Topic))
		for _, attempt := range bundle.RecentAttempts {
			fmt.Fprintf(&out, "- %s —— %s", strings.TrimSpace(attempt.Question), attempt.Verdict())
			// The missing points are the teachable residue of a wrong answer:
			// each names something this learner has already been shown once and
			// still did not produce.
			if missing := strings.Join(attempt.Missing, "、"); missing != "" {
				fmt.Fprintf(&out, "；未答出：%s", missing)
			}
			out.WriteString("\n")
		}
		for _, note := range bundle.Notes {
			fmt.Fprintf(&out, "- 他自己记下的：%s\n", strings.TrimSpace(note))
		}
	}
	if out.Len() == 0 {
		return ""
	}
	// Two kinds of information are in this block and they need opposite rules.
	//
	// The band above is a judgement the system derived; it stays internal. The
	// lines below are facts the grader recorded — which question was asked, and
	// which points the answer left out. Those are already shown to the learner
	// on the quiz screen under 需要补充, so hiding them here buys nothing.
	//
	// The first attempt forbade both, and the result was worse than saying
	// nothing: asked "我哪里没答好", the model had a legitimate question, a
	// record that answered it, and an instruction not to use the record. It
	// resolved the conflict by inventing — announcing three points the learner
	// had scored zero on as things they had 已准确答对. Removing the conflict
	// removes the reason to invent.
	return out.String() + masteryRecordsInstruction + " you may tell the learner " +
		"which question was asked and which points their answer left out, and you should do so " +
		"plainly if they ask how they did. Never state a score, a percentage, or a mastery level. " +
		"Never claim they answered something correctly unless a line above says so — if the record " +
		"says a point was missed, it was missed. Prefer spending the explanation on those points."
}

type matchedTopic struct {
	progress model.TopicProgress
	score    float64
	isActive bool
}

// minTopicCoverage is the share of one part of a topic name that the question
// must reproduce
// before that topic's assessment is applied.
//
// Half is a judgement, not a measurement: loading the wrong topic is worse than
// loading none, because it tells the model the learner is weak at something
// they did not ask about. When in doubt the system says nothing and the answer
// is merely un-personalised.
const minTopicCoverage = 0.5

// matchTopics is deliberately lexical and deterministic. TopicKey is derived
// from whatever topic name the learner typed when starting a quiz, so it is
// not a controlled vocabulary; semantic matching is deferred to the EXP-002B
// recall selection rather than guessed at here.
func matchTopics(candidates []model.TopicProgress, question, activeKey string, userID int64, config MasteryConfig, terms *topicTerms) []matchedTopic {
	prepared := topicmatch.NewQuestion(question)

	matches := make([]matchedTopic, 0, len(candidates))
	for _, candidate := range candidates {
		// Defence in depth: the DAO already filters by user, but a mastery
		// leak across users would be a hard gate failure, so re-check.
		if candidate.UserID != userID {
			continue
		}
		isActive := activeKey != "" && candidate.TopicKey == activeKey
		score := 0.0
		if isActive {
			// An active quiz names its topic from server state, so it does not
			// compete on wording at all. The value is above any coverage a
			// lexical match can reach, so a perfectly worded question about a
			// different topic still cannot displace it.
			score = 2
		} else {
			score = prepared.Coverage(candidate.TopicKey)
			if score < config.MinCoverage {
				// The name did not carry it. A term that belongs to this topic's
				// quiz history and to no other is independent evidence, so one
				// is enough; requiring two would discard the single rare
				// identifier that is often the whole question ("autoAck").
				// Below the name threshold, never above it: a topic found only
				// through its quiz vocabulary is weaker evidence than one the
				// learner named, and must not displace it. The first version
				// scored term hits above the threshold and watched a correct
				// name match lose to an incidental word elsewhere.
				if hits := terms.hits(candidate.TopicKey, question, config.MinBigrams); hits > 0 {
					score = config.MinCoverage - 0.1 + float64(hits)/1000
				} else {
					continue
				}
			}
		}
		matches = append(matches, matchedTopic{progress: candidate, score: score, isActive: isActive})
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		left, right := matches[i].progress, matches[j].progress
		if !sameOptionalTime(left.LastAttemptAt, right.LastAttemptAt) {
			return laterOptionalTime(left.LastAttemptAt, right.LastAttemptAt)
		}
		return left.TopicKey < right.TopicKey
	})
	if len(matches) > config.MaxTopics {
		matches = matches[:config.MaxTopics]
	}
	return matches
}

var asciiSegmentPattern = regexp.MustCompile(`[a-z0-9]+`)

// renderMastery converts structured progress into teaching instructions.
//
// It must never emit MasteryScore, Confidence or ValidAttempts. The bands
// below are the only thing the model gets to see.
func renderMastery(matched []matchedTopic, now time.Time, config MasteryConfig) string {
	var out strings.Builder
	out.WriteString("Learner mastery (server-computed from graded quiz attempts; " +
		"use it to choose depth and emphasis only):\n")

	suggestQuiz := false
	for _, item := range matched {
		progress := item.progress
		out.WriteString("- topic \"")
		out.WriteString(strings.TrimSpace(progress.Topic))
		out.WriteString("\": ")

		if progress.ValidAttempts < config.MinConfidentAttempts || progress.Level == model.TopicProgressInsufficient {
			// Not enough evidence: explicitly do not let the model act on a
			// score that the learning service itself calls unreliable.
			out.WriteString("not enough assessment evidence yet. Explain at your default depth and do not assume prior knowledge.")
			suggestQuiz = true
			out.WriteString("\n")
			continue
		}

		out.WriteString(masteryBand(progress))
		if points := weakPointPhrase(progress.WeakPoints); points != "" {
			out.WriteString(" Focus on: ")
			out.WriteString(points)
			out.WriteString(".")
		}
		if isStaleAssessment(progress.LastAttemptAt, now, config.StaleAfter) {
			out.WriteString(" This assessment is old, so treat it as indicative rather than current.")
		}
		out.WriteString("\n")
	}

	if suggestQuiz {
		out.WriteString("After answering, you may offer a short quiz on the untested topic.\n")
	}
	// Deliberately does not name what is being withheld. Saying "never mention
	// scores" would itself tell the model that a numeric assessment exists,
	// which is exactly the thing this provider is built to keep out of reach.
	out.WriteString("This rating is internal: never quote it, name it, or mention that it exists. " +
		"It decides depth and emphasis only.")
	return out.String()
}

func masteryBand(progress model.TopicProgress) string {
	switch {
	case progress.Level == model.TopicProgressMastered:
		return "the learner handles this well. Skip elementary repetition and go straight to the harder edges, while still answering the current question completely."
	case progress.MasteryScore >= 70:
		return "mostly solid with gaps remaining. Skip the basics and expand on the gaps."
	default:
		return "still shaky. Explain in plain language and build up from the underlying mechanism."
	}
}

func weakPointPhrase(points []string) string {
	cleaned := make([]string, 0, len(points))
	seen := map[string]struct{}{}
	for _, point := range points {
		point = strings.TrimSpace(point)
		if point == "" {
			continue
		}
		if utf8.RuneCountInString(point) > 60 {
			point = strings.TrimSpace(string([]rune(point)[:60]))
		}
		if _, exists := seen[point]; exists {
			continue
		}
		seen[point] = struct{}{}
		cleaned = append(cleaned, point)
		if len(cleaned) == 3 {
			break
		}
	}
	return strings.Join(cleaned, "; ")
}

func isStaleAssessment(lastAttemptAt *time.Time, now time.Time, staleAfter time.Duration) bool {
	if lastAttemptAt == nil {
		return false
	}
	return now.Sub(*lastAttemptAt) > staleAfter
}

func sameOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func laterOptionalTime(left, right *time.Time) bool {
	if left == nil {
		return false
	}
	if right == nil {
		return true
	}
	return left.After(*right)
}
