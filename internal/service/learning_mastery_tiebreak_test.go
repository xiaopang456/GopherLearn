package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// The mastery note picks the topic whose name the question reproduces most completely.
// When two topics score exactly the same, the only thing left to decide with is the order
// the store handed the rows over in, and that is worth pinning down rather than leaving to
// whatever the map happened to yield: the note is presented to a model as a server-written
// fact, and a fact that changes between runs depending on Go's map seed is not a fact.
//
// This file drives that decision with a store whose row order is stated by the test. The
// alternative - two real quizzes on two real topics - cannot produce equal scores on
// purpose without leaning on the wall clock to separate two attempts by a measurable
// amount, and a clock-dependent test either passes by luck or asserts nothing at all.
//
// What the store guarantees about its own ordering (most recent attempt first, regardless
// of creation order) is asserted where that promise is actually made: in the shared
// repository contract suite, not here.

// orderedProgressStore answers the one read a mastery note makes, in the order the test
// says.
//
// It embeds the whole repository interface and implements one method, so any other call
// panics on a nil receiver. That is the point: this proves how narrow the note path is. A
// future change that reached for a session, an attempt or a question here would fail this
// file loudly instead of quietly widening what one prompt read costs.
type orderedProgressStore struct {
	repository.Repository
	rows  []domain.TopicProgress
	calls int
}

func (s *orderedProgressStore) ListTopicProgress(context.Context, string, int) ([]domain.TopicProgress, error) {
	s.calls++
	return append([]domain.TopicProgress(nil), s.rows...), nil
}

// tiedProgress builds one reliable projection on a given topic, active at a stated moment.
// The levels differ so the prose identifies which topic won, which keeps the assertion
// from passing on a note that merely contains both names.
func tiedProgress(id, topicKey, topic string, level domain.TopicProgressLevel, active time.Time) domain.TopicProgress {
	stamped := active
	return domain.TopicProgress{
		ID: id, UserID: "user-1", TopicKey: topicKey, Topic: topic,
		Mastery: 70, Confidence: 0.5, ValidCount: domain.LearningReliableAttemptCount,
		Level: level, WeakPoints: []string{"说不清" + topic},
		LastAttempt: &stamped, CreatedAt: stamped, UpdatedAt: stamped,
	}
}

const (
	tieQuestion    = "Redis 的机制到底是怎么运作的？"
	tieOlderTopic  = "Redis 持久化"
	tieNewerTopic  = "Redis 主从复制"
	tieOlderPhrase = "稳定成立"    // the mastered topic's band
	tieNewerPhrase = "方向正确"    // the developing topic's band
	tieOlderID     = "tpr-a-1" // the older row owns the lexically smaller id, on purpose
	tieNewerID     = "tpr-z-9" // so id order and recency order point at different topics
)

// tiedRows returns the two topics with equal match scores, newest first as the real store
// would hand them over.
//
// Both stamps are deliberately in the past and well inside the staleness window: a future
// timestamp would make "not stale" true by accident, and the two rows must differ from
// each other in recency for the ordering claim below to mean anything.
func tiedRows() (older, newer domain.TopicProgress) {
	base := time.Now().UTC().Add(-72 * time.Hour)
	return tiedProgress(tieOlderID, domain.NormalizeLearningTopicKey(tieOlderTopic), tieOlderTopic,
			domain.TopicProgressMastered, base),
		tiedProgress(tieNewerID, domain.NormalizeLearningTopicKey(tieNewerTopic), tieNewerTopic,
			domain.TopicProgressDeveloping, base.Add(24*time.Hour))
}

// TestTiedTopicsAreDecidedByTheStoresOrder is the whole tie rule in four parts: the two
// candidates really do score the same, the first row the store returns wins, reversing the
// rows reverses the answer, and neither the ids nor ten repeats change anything.
func TestTiedTopicsAreDecidedByTheStoresOrder(t *testing.T) {
	older, newer := tiedRows()

	// The premise: identical scores. If this stops being true the test below is no longer
	// about ties at all, so it is asserted rather than assumed.
	matcher := newLearningTopicMatcher(tieQuestion)
	olderScore, olderOK := matcher.match(older.TopicKey)
	newerScore, newerOK := matcher.match(newer.TopicKey)
	if !olderOK || !newerOK || olderScore != newerScore {
		t.Fatalf("the fixture is not a tie: %q=%v(%v) vs %q=%v(%v)",
			older.TopicKey, olderScore, olderOK, newer.TopicKey, newerScore, newerOK)
	}
	// And the ids must disagree with recency, or "ids do not decide" proves nothing: an
	// implementation that sorted by id ascending would pick the older topic here, which is
	// not the first row and not the newer one.
	if !(older.ID < newer.ID) {
		t.Fatalf("the ids sort the same way as recency (%q, %q): the swap below would be tautological",
			older.ID, newer.ID)
	}

	noteFor := func(t *testing.T, rows []domain.TopicProgress) (string, int) {
		t.Helper()
		store := &orderedProgressStore{rows: rows}
		service := NewLearningService(store, nil, nil, nil, nil)
		note, err := service.MasteryNote(context.Background(), "user-1", tieQuestion)
		if err != nil {
			t.Fatalf("note: %v", err)
		}
		return note, store.calls
	}

	t.Run("newest first means the newer topic is described", func(t *testing.T) {
		note, calls := noteFor(t, []domain.TopicProgress{newer, older})
		if !strings.Contains(note, tieNewerTopic) || !strings.Contains(note, tieNewerPhrase) {
			t.Fatalf("note = %q, want the first row's topic and band", note)
		}
		if strings.Contains(note, tieOlderTopic) {
			t.Fatalf("the displaced topic leaked into the note:\n%s", note)
		}
		// One read, nothing else: a tie must not be resolved by going back to the store.
		if calls != 1 {
			t.Fatalf("%d projection reads, want exactly 1", calls)
		}
	})

	t.Run("the same rows in the opposite order change the answer", func(t *testing.T) {
		// This is the half that separates "first row wins" from "a fixed topic always
		// wins". If the outcome were driven by the topic text, the ids, or a map, both
		// orderings would produce the same note and this would pass by accident.
		note, calls := noteFor(t, []domain.TopicProgress{older, newer})
		if !strings.Contains(note, tieOlderTopic) || !strings.Contains(note, tieOlderPhrase) {
			t.Fatalf("note = %q, want the older topic once it is the first row", note)
		}
		if strings.Contains(note, tieNewerTopic) {
			t.Fatalf("the newer topic survived reordering:\n%s", note)
		}
		if calls != 1 {
			t.Fatalf("%d projection reads, want exactly 1", calls)
		}
	})

	t.Run("identifiers do not decide", func(t *testing.T) {
		// Same rows, same order, ids swapped: the answer must not move.
		//
		// The two halves of this claim live in different subtests. An implementation that
		// sorted by id would already have failed the case above, because there the lexically
		// smaller id belongs to the row that is NOT first. Here the ids are permuted so
		// that id order agrees with the row order, which is the configuration where such an
		// implementation would suddenly look right - so the only thing this case can prove,
		// and what it proves, is that an id permutation is not an input to the choice.
		left, right := newer, older
		left.ID, right.ID = right.ID, left.ID
		note, _ := noteFor(t, []domain.TopicProgress{left, right})
		if !strings.Contains(note, tieNewerTopic) {
			t.Fatalf("swapping ids moved the note off the first row:\n%s", note)
		}
	})

	t.Run("ten runs are byte-identical", func(t *testing.T) {
		// Map iteration is randomised per process, and the store double above reads from a
		// slice - so the only way this case has teeth is by running the real selection
		// repeatedly and comparing whole strings, rather than checking one property.
		rows := []domain.TopicProgress{newer, older}
		first, _ := noteFor(t, rows)
		if first == "" {
			t.Fatal("a tie produced no note at all")
		}
		for repeat := range 10 {
			again, _ := noteFor(t, rows)
			if again != first {
				t.Fatalf("run %d differs from the first:\n%s\n---\n%s", repeat, first, again)
			}
		}
	})
}

// TestTiedTopicsKeepTheirOrderUnderMoreCandidates scales the same rule up: eight
// equally-scoring rows, and the first one still wins. This is what a map-backed store
// would break, because insertion order there is not iteration order.
func TestTiedTopicsKeepTheirOrderUnderMoreCandidates(t *testing.T) {
	older, newer := tiedRows()
	var rows []domain.TopicProgress
	for index := range 8 {
		row := newer
		if index%2 == 1 {
			row = older
		}
		row.Topic = row.Topic + "（第 " + string(rune('A'+index)) + " 组）"
		row.TopicKey = domain.NormalizeLearningTopicKey(row.Topic)
		rows = append(rows, row)
	}
	// All eight must still tie, or the scan below is ranking again rather than tie-breaking.
	matcher := newLearningTopicMatcher(tieQuestion)
	var score float64
	for index, row := range rows {
		value, ok := matcher.match(row.TopicKey)
		if !ok {
			t.Fatalf("candidate %d (%s) stopped matching after the suffix: %v", index, row.TopicKey, value)
		}
		if index == 0 {
			score = value
			continue
		}
		if value != score {
			t.Fatalf("candidate %d scores %v, want the same %v as the first", index, value, score)
		}
	}

	store := &orderedProgressStore{rows: rows}
	service := NewLearningService(store, nil, nil, nil, nil)
	note, err := service.MasteryNote(context.Background(), "user-1", tieQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, tieNewerTopic+"（第 A 组）") {
		t.Fatalf("the first of eight tied candidates did not win:\n%s", note)
	}
	if store.calls != 1 {
		t.Fatalf("%d reads, want 1", store.calls)
	}
	// The same eight rows in reverse must hand the note to the new first row.
	reversed := make([]domain.TopicProgress, 0, len(rows))
	for index := len(rows) - 1; index >= 0; index-- {
		reversed = append(reversed, rows[index])
	}
	backward := &orderedProgressStore{rows: reversed}
	other := NewLearningService(backward, nil, nil, nil, nil)
	second, err := other.MasteryNote(context.Background(), "user-1", tieQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, tieOlderTopic+"（第 H 组）") {
		t.Fatalf("reversing eight tied candidates did not move the note:\n%s", second)
	}
	if backward.calls != 1 {
		t.Fatalf("%d reads, want 1", backward.calls)
	}
}
