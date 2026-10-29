package memory

import (
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"fmt"
	"sort"
	"strings"
)

// AGENT-021 makes `topic_key` the shared primary key of long-term memory and
// learning state.
//
// Before this, the two sides invented keys independently. The quiz module keyed
// on whatever the learner typed as a topic, normalised; the memory extractor
// keyed on whatever the model wrote as `topic`, normalised the same way. Both
// produced legal keys and neither produced the same one, so a learner who was
// graded on "RPC 粘包" could accumulate memories under `packetbuffer-拆包` and
// nothing would ever join them. The failure is silent: both records exist, both
// look right, and the lookup simply returns nothing.
//
// The catalogue is the single place that answers "what topics does this user
// have". Read and write both go through it, which is what keeps the two sides
// from drifting apart again.

// TopicProgressLister is the learning side's view. Topics the learner has been
// graded on are the authoritative ones: they were named by the learner, and
// they carry mastery.
type TopicProgressLister interface {
	ListTopicProgress(ctx context.Context, userID int64, limit int) ([]model.TopicProgress, error)
}

// MemoryTopicLister is the memory side's view: topics that carry notes but were
// never quizzed.
type MemoryTopicLister interface {
	ListTopicKeys(ctx context.Context, userID int64, limit int) ([]TopicKeyCount, error)
}

type TopicKeyCount struct {
	TopicKey string
	Topic    string
	Notes    int
}

// MemoryTopicAdapter lets the memory repository satisfy MemoryTopicLister
// without the DAO importing this package. The DAO row type is deliberately its
// own: a service-level type crossing into the data layer is how the two ended
// up coupled the last time.
type MemoryTopicAdapter struct {
	Repository interface {
		ListTopicKeys(ctx context.Context, userID int64, limit int) ([]memorydao.TopicKeyRow, error)
	}
}

func (a MemoryTopicAdapter) ListTopicKeys(ctx context.Context, userID int64, limit int) ([]TopicKeyCount, error) {
	rows, err := a.Repository.ListTopicKeys(ctx, userID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]TopicKeyCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, TopicKeyCount{TopicKey: row.TopicKey, Topic: row.Topic, Notes: row.Notes})
	}
	return out, nil
}

// Topic is one entry of the catalogue. It carries enough for a model to choose
// between entries without loading any content.
type Topic struct {
	Key string
	// Name is the learner's own wording. The key is derived from it, so the key
	// alone would be a lossy thing to show a model.
	Name string
	// Graded reports whether the learner has been quizzed on this topic. A
	// graded topic is the stronger identity: it was named deliberately, and
	// merging a note into it links the note to real mastery data.
	Graded bool
	Level  model.TopicProgressLevel
	Notes  int
}

type TopicCatalogue struct {
	learning TopicProgressLister
	memory   MemoryTopicLister
	limit    int
}

func NewTopicCatalogue(learning TopicProgressLister, memory MemoryTopicLister, limit int) (*TopicCatalogue, error) {
	if learning == nil && memory == nil {
		return nil, fmt.Errorf("topic catalogue requires at least one source")
	}
	if limit < 1 || limit > 200 {
		return nil, fmt.Errorf("topic catalogue limit must be between 1 and 200")
	}
	return &TopicCatalogue{learning: learning, memory: memory, limit: limit}, nil
}

// List merges both sides. Graded topics win on collision: when a key exists on
// both sides it is the same topic, and the learning row is the one that carries
// mastery.
func (c *TopicCatalogue) List(ctx context.Context, userID int64) ([]Topic, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("topic catalogue requires a trusted user id")
	}
	byKey := map[string]*Topic{}

	if c.learning != nil {
		progress, err := c.learning.ListTopicProgress(ctx, userID, c.limit)
		if err != nil {
			return nil, fmt.Errorf("list graded topics: %w", err)
		}
		for _, row := range progress {
			if row.UserID != userID || strings.TrimSpace(row.TopicKey) == "" {
				continue
			}
			byKey[row.TopicKey] = &Topic{
				Key: row.TopicKey, Name: row.Topic, Graded: true, Level: row.Level,
			}
		}
	}

	if c.memory != nil {
		keys, err := c.memory.ListTopicKeys(ctx, userID, c.limit)
		if err != nil {
			return nil, fmt.Errorf("list note topics: %w", err)
		}
		for _, row := range keys {
			if strings.TrimSpace(row.TopicKey) == "" {
				continue
			}
			if existing, ok := byKey[row.TopicKey]; ok {
				existing.Notes = row.Notes
				continue
			}
			byKey[row.TopicKey] = &Topic{Key: row.TopicKey, Name: row.Topic, Notes: row.Notes}
		}
	}

	topics := make([]Topic, 0, len(byKey))
	for _, topic := range byKey {
		topics = append(topics, *topic)
	}
	// Graded topics first, then by note count, then by key. The order is what a
	// model sees, and a stable one keeps its choices reproducible.
	sort.SliceStable(topics, func(i, j int) bool {
		if topics[i].Graded != topics[j].Graded {
			return topics[i].Graded
		}
		if topics[i].Notes != topics[j].Notes {
			return topics[i].Notes > topics[j].Notes
		}
		return topics[i].Key < topics[j].Key
	})
	return topics, nil
}

// Resolve maps a proposed topic onto the catalogue.
//
// Only exact identity after normalisation is merged here. Containment is
// deliberately not merged: `redis` and `redis-ttl` normalise to keys where one
// contains the other, yet they are different topics, and folding them would
// destroy the distinction mastery is tracked at. Synonyms that share no
// characters — "RPC 粘包" against "PacketBuffer 拆包" — are beyond any rule a
// string comparison can express, which is why the catalogue is also handed to
// the model at extraction time (see CatalogueForPrompt).
func Resolve(topics []Topic, proposed string) (key string, matched bool) {
	normalized := model.NormalizeLearningTopicKey(proposed)
	if normalized == "" {
		return "", false
	}
	for _, topic := range topics {
		if topic.Key == normalized {
			return topic.Key, true
		}
	}
	return normalized, false
}

// CatalogueForPrompt renders the catalogue for a model to choose from.
//
// It carries no memory content and no scores: the model is being asked which
// topic this belongs to, not what the learner knows. Numbers would also leak
// assessment into a place the design forbids it (see the design document,
// section 2.5).
func CatalogueForPrompt(topics []Topic, limit int) string {
	if len(topics) == 0 {
		return ""
	}
	if limit > 0 && len(topics) > limit {
		topics = topics[:limit]
	}
	var out strings.Builder
	out.WriteString("Existing topics for this learner. Reuse one of these keys when the memory belongs to it; only invent a new topic when none applies.\n")
	for _, topic := range topics {
		fmt.Fprintf(&out, "- %s", topic.Key)
		if name := strings.TrimSpace(topic.Name); name != "" && name != topic.Key {
			fmt.Fprintf(&out, " (%s)", name)
		}
		if topic.Graded {
			out.WriteString(" [quizzed]")
		}
		out.WriteString("\n")
	}
	return out.String()
}
