package agent

import (
	learningdao "GopherAI/dao/learning"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TopicBundleProvider is the join that AGENT-021 exists to make possible.
//
// Before it, three stores held pieces of the same subject and nothing put them
// together: TopicProgress knew the learner was weak at RPC framing, the attempt
// log knew which question they had failed, and long-term memory held the note
// they wrote about it — each keyed differently, none reachable from the others.
// A single `topic_key` now addresses all three.
//
// The provider owns the join and nothing else. It does not decide relevance:
// the model has already chosen the key from the catalogue by the time this
// runs.
type TopicBundleProvider struct {
	progress TopicProgressGetter
	attempts TopicAttemptLister
	notes    TopicNoteLister
	config   TopicBundleConfig
	now      func() time.Time
}

// TopicBundleReader returns everything filed under one topic.
//
// A load_topic tool once let the model ask for a topic by name. It was removed
// with AGENT-022: the tool required the model to produce a correct topic_key,
// and that it can pick the right topic is precisely what three experiments
// refuted. Keeping a tool whose premise had been disproved would have preserved
// the disproved assumption in code. The interface stays because the mastery
// path reads through it.
type TopicBundleReader interface {
	LoadTopic(ctx context.Context, userID int64, topicKey string) (TopicBundle, error)
}

type TopicBundle struct {
	TopicKey string
	Topic    string
	// Found reports whether the learner has anything under this key at all. A
	// model that invents a key must be told so plainly rather than handed an
	// empty bundle it might read as "nothing known about a real topic".
	Found bool
	// MasteryInstruction is rendered by the provider, not here.
	//
	// Turning a level into teaching guidance is one rule, and AGENT-017 already
	// owns it in service/agent/mastery.go. A second copy in this tool would be
	// two sources for one decision, which is the failure mode this whole
	// redesign exists to remove — it is how the quiz side and the memory side
	// ended up with two topic namespaces in the first place.
	//
	// It is a band in words, never a score: a number in the prompt invites the
	// model to quote it back, and a learner told "your mastery is 42" has been
	// graded at rather than taught.
	MasteryInstruction string
	WeakPoints         []string
	Notes              []string
	// RecentAttempts describes how the learner actually performed, most recent
	// first. It is what makes "we did three questions on this" answerable.
	RecentAttempts []TopicAttempt
}

// TopicAttempt is one graded attempt, rendered for teaching.
//
// An earlier version carried only the ordinal and one missing point, on the
// reasoning that the wording of a question the learner has already seen teaches
// nothing. That was wrong twice over. The question text is what makes an
// attempt identifiable at all — "第 2 题答错了" is unusable, "粘包和半包的区别
// 那题答错了" is not — and the graded rubric records several missing points,
// not one, each of which is a specific thing still to be taught.
//
// The cost objection does not survive arithmetic either. A question runs about
// forty runes and the missing points about thirty; a topic with twelve attempts
// is roughly 850 runes, well inside the payload budget.
// Verdict describes an attempt in three bands rather than two.
//
// A binary pass mark produced a contradiction visible in the first real run: an
// answer scoring 72 was announced as 答对 and then followed by three things the
// grader said were missing. Raising the threshold does not fix that — it only
// moves it, since an answer at 76 with the same three gaps would read the same
// way. What the grader recorded is not one fact but two: whether the answer
// passed, and what it left out.
//
// The three bands map to three different instructions. Fully correct means skip
// it; passed with gaps means touch on them; failed means teach it.
func (a TopicAttempt) Verdict() string {
	switch {
	case a.Correct && len(a.Missing) == 0:
		return "答对"
	case a.Correct:
		return "答对但有遗漏"
	default:
		return "答错"
	}
}

type TopicAttempt struct {
	Question string
	Correct  bool
	// Missing lists everything the grader found absent from the answer. These
	// are the compressed form of the grader's prose: the paragraph explains why
	// marks were lost, these name what to cover.
	Missing []string
}

type TopicProgressGetter interface {
	GetTopicProgress(ctx context.Context, userID int64, topicKey string) (*model.TopicProgress, error)
}

type TopicAttemptLister interface {
	ListTopicAttemptsWithQuestions(ctx context.Context, userID int64, topicKey string, limit int) ([]learningdao.TopicAttemptRecord, error)
}

type TopicNoteLister interface {
	ListByTopicKey(ctx context.Context, userID int64, memoryType model.MemoryScope, topicKey string, minConfidence float64, limit int) ([]model.LongTermMemory, error)
}

type TopicBundleConfig struct {
	MaxNotes      int
	MaxAttempts   int
	MinConfidence float64
	// PassMark is the graded total at or above which an attempt counts as
	// correct. Attempts are scored out of ten by the grader, and the model is
	// shown a verdict rather than the number for the same reason mastery is
	// shown as a band: a score in the prompt gets quoted back at the learner.
	PassMark int
}

func DefaultTopicBundleConfig() TopicBundleConfig {
	// Twelve attempts is four quiz rounds of three, which is already a lot of
	// work on one subject. Capping lower than the budget allows would discard
	// history for no reason; the payload trimmer, not this number, is what
	// protects the context.
	// Seventy-five, not sixty. The grader marks correctness out of sixty,
	// completeness out of twenty-five and clarity out of fifteen, so an answer
	// can reach sixty while missing most of what completeness asks for.
	return TopicBundleConfig{MaxNotes: 8, MaxAttempts: 12, MinConfidence: 0.6, PassMark: 75}
}

func (c TopicBundleConfig) Validate() error {
	if c.MaxNotes < 1 || c.MaxNotes > 20 || c.MaxAttempts < 1 || c.MaxAttempts > 30 {
		return fmt.Errorf("topic bundle limits must be within range")
	}
	if c.MinConfidence < 0 || c.MinConfidence > 1 {
		return fmt.Errorf("topic bundle confidence floor must be between 0 and 1")
	}
	// The grader scores out of one hundred, not ten: the UI shows 82/100 built
	// from correctness 50/60, completeness 20/25 and clarity 12/15.
	if c.PassMark < 1 || c.PassMark > 100 {
		return fmt.Errorf("topic bundle pass mark must be between 1 and 100")
	}
	return nil
}

func NewTopicBundleProvider(progress TopicProgressGetter, attempts TopicAttemptLister, notes TopicNoteLister, config TopicBundleConfig) (*TopicBundleProvider, error) {
	if progress == nil || attempts == nil || notes == nil {
		return nil, fmt.Errorf("topic bundle provider requires progress, attempt and note readers")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &TopicBundleProvider{progress: progress, attempts: attempts, notes: notes, config: config, now: time.Now}, nil
}

// LoadTopic assembles one topic. A topic exists if any of the three stores has
// something for it: a learner may have written notes without ever being quizzed,
// or been quizzed on something they never wrote about.
func (p *TopicBundleProvider) LoadTopic(ctx context.Context, userID int64, topicKey string) (TopicBundle, error) {
	topicKey = strings.TrimSpace(topicKey)
	if userID <= 0 || topicKey == "" {
		return TopicBundle{}, fmt.Errorf("topic bundle identity is required")
	}

	progress, err := p.progress.GetTopicProgress(ctx, userID, topicKey)
	if err != nil && !errors.Is(err, learningdao.ErrAttemptNotFound) {
		return TopicBundle{}, fmt.Errorf("load topic progress: %w", err)
	}

	notes, err := p.notes.ListByTopicKey(ctx, userID, model.MemoryScopeTopic, topicKey, p.config.MinConfidence, p.config.MaxNotes)
	if err != nil {
		return TopicBundle{}, fmt.Errorf("load topic notes: %w", err)
	}

	bundle := TopicBundle{TopicKey: topicKey}
	for _, note := range notes {
		// Second gate. The query filtered on owner already; this repeats the
		// check on the rows themselves, so a mistake in one place cannot put
		// another learner's note in front of this one.
		if note.UserID != userID || note.Status != model.LongTermMemoryActive {
			continue
		}
		bundle.Notes = append(bundle.Notes, renderTopicNote(note))
	}

	if progress != nil && progress.UserID == userID {
		bundle.Topic = progress.Topic
		bundle.MasteryInstruction = renderTopicMastery(*progress, p.now().UTC(), p.config)
		bundle.WeakPoints = progress.WeakPoints
		attempts, err := p.attempts.ListTopicAttemptsWithQuestions(ctx, userID, topicKey, p.config.MaxAttempts)
		if err != nil {
			return TopicBundle{}, fmt.Errorf("load topic attempts: %w", err)
		}
		for _, record := range attempts {
			if record.UserID != userID {
				continue
			}
			question := strings.TrimSpace(record.QuestionText)
			if question == "" {
				question = fmt.Sprintf("question %d", record.QuestionOrdinal)
			}
			bundle.RecentAttempts = append(bundle.RecentAttempts, TopicAttempt{
				Question: question,
				Correct:  record.EffectiveTotal >= p.config.PassMark,
				Missing:  cleanMissingPoints(record.MissingPoints),
			})
		}
	}

	bundle.Found = len(bundle.Notes) > 0 || progress != nil
	if bundle.Topic == "" {
		bundle.Topic = topicKey
	}
	return bundle, nil
}

// renderTopicMastery reuses the AGENT-017 bands rather than restating them.
//
// The rule that an unreliable assessment must not change how something is
// explained lives in one place, and this is a second caller of it, not a second
// copy.
func renderTopicMastery(progress model.TopicProgress, now time.Time, config TopicBundleConfig) string {
	if progress.ValidAttempts < DefaultMasteryConfig().MinConfidentAttempts || progress.Level == model.TopicProgressInsufficient {
		return "not enough assessment evidence yet. Explain at your default depth and do not assume prior knowledge."
	}
	instruction := masteryBand(progress)
	if points := weakPointPhrase(progress.WeakPoints); points != "" {
		instruction += " Focus on: " + points + "."
	}
	if isStaleAssessment(progress.LastAttemptAt, now, DefaultMasteryConfig().StaleAfter) {
		instruction += " This assessment is old, so treat it as indicative rather than current."
	}
	return instruction
}

// renderTopicNote drops the type and tag prefix the similarity path used. With
// a topic already fixed, naming it again on every line spends budget to repeat
// what the model just asked for.
func renderTopicNote(note model.LongTermMemory) string {
	subject := strings.TrimSpace(note.Subject)
	content := strings.TrimSpace(note.Content)
	if subject == "" {
		return content
	}
	return subject + "：" + content
}

// cleanMissingPoints keeps every point the grader recorded, dropping only
// blanks.
//
// The grader also writes a paragraph of prose explaining the marks. That is not
// carried: at roughly two hundred runes per attempt it would triple the payload
// to restate, in sentences, what these few words already say.
func cleanMissingPoints(points []string) []string {
	out := make([]string, 0, len(points))
	for _, point := range points {
		if trimmed := strings.TrimSpace(point); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
