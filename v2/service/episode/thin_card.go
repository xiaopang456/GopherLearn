package episode

import (
	"GopherAI/model"
	"context"
	"sort"
	"strings"
	"time"
)

// The thin card.
//
// A thin card is a FINDING AID, and that is the whole of it: which session
// this was, what it was called, when it was last talked in, what it was about,
// and where its summary segments are.
//
// It carries none of the session's body. The body - the题目, the arrangements,
// the open items, the words themselves - is read where it already lives: the
// source session's summary segments, its whiteboard and its own messages. A
// card that repeated the body would pay for the same words twice, and the two
// copies would drift apart.
//
// The derivation costs NO model call. Every field comes from data the program
// already has: the session's title, the time of its last message, the topic
// and the names its summary segments already recorded. The thin card is what
// is left when the model is taken out of the card writer.

// ThinCardSource is everything a thin card is derived from.
//
// It is a plain value and not a database handle so that the derivation can be
// exercised without a product, a database and a model - and so that what the
// derivation reads is visible in one place.
type ThinCardSource struct {
	// SessionID is the session the card belongs to. It is not in the document
	// itself: the row that holds the card already says which session it is,
	// and a copy inside the document is a second place for the two to drift.
	SessionID string
	// Title is the session's own title.
	Title string
	// LastTalkedAt is when the session was last talked in.
	LastTalkedAt time.Time
	// LastMessageID is the id of that last message.
	//
	// It is the card's own reach: the thin card describes the session as far
	// as its last message, and a read-back through the card may not go past
	// that line.
	LastMessageID uint
	// Topics are the subjects the session's summary segments recorded.
	//
	// They are the clues that tell two sessions apart when their titles do
	// not. They come from the segments, never from a model.
	Topics []string
	// Names are the proper names the session's summary segments recorded.
	Names []string
	// SegmentSeqs are the summary segments of the session.
	//
	// They are the way back to the compressed body, and they are what makes
	// a thin card a pointer rather than a summary.
	SegmentSeqs []int
}

// ThinCardDeriver derives a thin card from what the program already has.
//
// It is an interface so that the write path can be exercised without one,
// and so that the switch between the model-written card and the thin card is
// a wiring decision and not a change of behaviour deep inside the worker.
type ThinCardDeriver interface {
	DeriveThinCard(context.Context, ThinCardSource) (model.SessionCardDocument, error)
}

// ProgrammaticThinCardDeriver derives a thin card with no model call at all.
//
// The zero is the point. A card arm that spends one model call per session
// writes a card that repeats the summary; a card arm that spends none writes
// a card that points at the summary.
type ProgrammaticThinCardDeriver struct{}

// DeriveThinCard builds the card.
//
// The title falls back to the session id when the session has no title of its
// own: a card with no title cannot be found by its name, and an untitled
// session still happened.
func (d ProgrammaticThinCardDeriver) DeriveThinCard(_ context.Context, source ThinCardSource) (model.SessionCardDocument, error) {
	document := model.SessionCardDocument{
		Version:      model.SessionThinCardContentVersion,
		Title:        thinCardTitle(source),
		LastTalkedAt: source.LastTalkedAt,
	}
	document.Topics = thinCardTopics(source.Topics)
	document.Names = thinCardNames(source.Names)
	document.SegmentSeqs = thinCardSegmentSeqs(source.SegmentSeqs)
	if err := document.Validate(); err != nil {
		return model.SessionCardDocument{}, err
	}
	return document, nil
}

// thinCardTitle is the card's title.
//
// The session's own title comes first. A session whose title changed keeps
// the new one, because a card that names the old title points at a name the
// learner no longer uses.
func thinCardTitle(source ThinCardSource) string {
	title := strings.TrimSpace(source.Title)
	if title == "" {
		title = strings.TrimSpace(source.SessionID)
	}
	return truncateRunes(title, model.SessionCardTitleMaxRunes)
}

// thinCardTopics are the card's topics, deduplicated and bounded.
//
// The key is derived from the label with the same rule the full card uses, so
// that a thin card and a full card about the same session are filed under the
// same key and a search that found one finds the other.
func thinCardTopics(labels []string) []model.EpisodeTopic {
	out := make([]model.EpisodeTopic, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		key := model.NormalizeLearningTopicKey(label)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model.EpisodeTopic{Key: key, Label: truncateRunes(label, model.EpisodeTitleMaxRunes)})
		if len(out) >= model.SessionCardTopicLimit {
			break
		}
	}
	return out
}

// thinCardNames are the card's names, deduplicated and bounded.
func thinCardNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, truncateRunes(name, model.EpisodeTitleMaxRunes))
		if len(out) >= model.SessionThinCardNameLimit {
			break
		}
	}
	return out
}

// thinCardSegmentSeqs are the card's segment refs, sorted and deduplicated.
//
// Sorted because a card's refs are read by a person and by a program, and
// two runs that derived the same card have to produce the same card.
func thinCardSegmentSeqs(seqs []int) []int {
	cleaned := make([]int, 0, len(seqs))
	seen := make(map[int]struct{}, len(seqs))
	for _, seq := range seqs {
		if seq <= 0 {
			continue
		}
		if _, exists := seen[seq]; exists {
			continue
		}
		seen[seq] = struct{}{}
		cleaned = append(cleaned, seq)
	}
	sort.Ints(cleaned)
	if len(cleaned) > model.SessionThinCardSegmentLimit {
		cleaned = cleaned[:model.SessionThinCardSegmentLimit]
	}
	return cleaned
}

// MergeThinCard folds a freshly derived card into the card a session already
// has.
//
// The merge is what makes the card cheap to keep current. The card is written
// more than once - a title changes, a new summary segment lands - and each
// time it has to say everything it already knew plus the new thing, without
// losing what it knew.
//
// The rules are short on purpose:
//
//   - the title is the newest one;
//   - the topics and the names are the union of what was there and what is
//     new, deduplicated;
//   - the segment refs are the union, because a segment that was already
//     cited is still cited.
//
// The result is a card that only ever grows, except for the title, which is
// replaced. A card that shrinks would lose the way back to a session the
// learner is looking for.
func MergeThinCard(existing, fresh model.SessionCardDocument) model.SessionCardDocument {
	if existing.Version != model.SessionThinCardContentVersion {
		// The existing card is a full card, or is of a shape this merge does
		// not understand. It is replaced rather than merged into: mixing the
		// two shapes would produce a card that claims to be both.
		return fresh
	}
	merged := fresh
	if merged.Title == "" {
		merged.Title = existing.Title
	}
	merged.Topics = mergeTopics(existing.Topics, fresh.Topics)
	merged.Names = mergeNames(existing.Names, fresh.Names)
	merged.SegmentSeqs = mergeSegmentSeqs(existing.SegmentSeqs, fresh.SegmentSeqs)
	if merged.LastTalkedAt.IsZero() {
		merged.LastTalkedAt = existing.LastTalkedAt
	}
	return merged
}

// mergeTopics joins two topic lists, keeping the order and dropping repeats.
func mergeTopics(existing, fresh []model.EpisodeTopic) []model.EpisodeTopic {
	out := make([]model.EpisodeTopic, 0, len(existing)+len(fresh))
	seen := make(map[string]struct{}, len(existing)+len(fresh))
	for _, list := range [][]model.EpisodeTopic{existing, fresh} {
		for _, topic := range list {
			key := strings.TrimSpace(topic.Key)
			if key == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, topic)
			if len(out) >= model.SessionCardTopicLimit {
				return out
			}
		}
	}
	return out
}

// mergeNames joins two name lists, keeping the order and dropping repeats.
func mergeNames(existing, fresh []string) []string {
	out := make([]string, 0, len(existing)+len(fresh))
	seen := make(map[string]struct{}, len(existing)+len(fresh))
	for _, list := range [][]string{existing, fresh} {
		for _, name := range list {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
			if len(out) >= model.SessionThinCardNameLimit {
				return out
			}
		}
	}
	return out
}

// mergeSegmentSeqs joins two segment ref lists, sorted and deduplicated.
func mergeSegmentSeqs(existing, fresh []int) []int {
	return thinCardSegmentSeqs(append(append([]int{}, existing...), fresh...))
}

// ThinCardSearchText is the text a thin card is searched by.
//
// It is the card's own words and nothing else: the title, the topics and the
// names. It does NOT include the synopsis, because a thin card has none, and
// it does NOT include the entries, because a thin card has none either.
//
// The text is short on purpose. It is the whole of what a thin card costs to
// index, and the size of it is one of the numbers this arm is measured on.
func ThinCardSearchText(document model.SessionCardDocument) string {
	parts := []string{strings.TrimSpace(document.Title)}
	for _, topic := range document.Topics {
		parts = append(parts, strings.TrimSpace(topic.Label), strings.TrimSpace(topic.Key))
	}
	for _, name := range document.Names {
		parts = append(parts, strings.TrimSpace(name))
	}
	return truncateRunes(strings.Join(parts, "\n"), model.EpisodeSearchTextMaxRunes)
}

// ThinCardNeedsReindex says whether a card's searchable text changed.
//
// It is the whole of the "only reindex when the text changed" rule: a card
// that was rewritten with the same words does not need to be indexed again,
// and an index write is a write to a shared store that every other attempt
// is also writing to.
func ThinCardNeedsReindex(existing *model.EpisodeMemory, document model.SessionCardDocument) bool {
	if existing == nil {
		return true
	}
	return ThinCardSearchText(existing.Card) != ThinCardSearchText(document)
}

// ThinCardNeedsWrite says whether a card has to be written at all.
//
// A card that already says the same thing is not written again. The write is
// cheap, but the version bump is not: it makes every other reader of the
// card check the new version, and a card that did not change has not earned
// that.
func ThinCardNeedsWrite(existing *model.EpisodeMemory, document model.SessionCardDocument) bool {
	if existing == nil {
		return true
	}
	if existing.Card.Version != document.Version {
		return true
	}
	if strings.TrimSpace(existing.Card.Title) != strings.TrimSpace(document.Title) {
		return true
	}
	if !existing.Card.LastTalkedAt.Equal(document.LastTalkedAt) {
		return true
	}
	return !sameStringSets(existing.Card.Names, document.Names) ||
		!sameIntSets(existing.Card.SegmentSeqs, document.SegmentSeqs) ||
		!sameTopicKeys(existing.Card.Topics, document.Topics)
}

// sameStringSets reports whether two lists hold the same strings.
func sameStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, value := range a {
		seen[value] = struct{}{}
	}
	for _, value := range b {
		if _, exists := seen[value]; !exists {
			return false
		}
	}
	return true
}

// sameIntSets reports whether two lists hold the same numbers.
func sameIntSets(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[int]struct{}, len(a))
	for _, value := range a {
		seen[value] = struct{}{}
	}
	for _, value := range b {
		if _, exists := seen[value]; !exists {
			return false
		}
	}
	return true
}

// sameTopicKeys reports whether two topic lists hold the same keys.
func sameTopicKeys(a, b []model.EpisodeTopic) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, topic := range a {
		seen[strings.TrimSpace(topic.Key)] = struct{}{}
	}
	for _, topic := range b {
		if _, exists := seen[strings.TrimSpace(topic.Key)]; !exists {
			return false
		}
	}
	return true
}
