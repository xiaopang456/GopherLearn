package memory

import (
	"GopherAI/model"
	"context"
	"errors"
	"strings"
	"testing"
)

type stubProgressLister struct {
	rows []model.TopicProgress
	err  error
}

func (s stubProgressLister) ListTopicProgress(_ context.Context, userID int64, limit int) ([]model.TopicProgress, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([]model.TopicProgress, 0, len(s.rows))
	for _, row := range s.rows {
		if row.UserID == userID {
			out = append(out, row)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type stubTopicKeyLister struct {
	rows []TopicKeyCount
	err  error
}

func (s stubTopicKeyLister) ListTopicKeys(context.Context, int64, int) ([]TopicKeyCount, error) {
	return s.rows, s.err
}

func graded(userID int64, key, name string) model.TopicProgress {
	return model.TopicProgress{
		UserID: userID, TopicKey: key, Topic: name, Level: model.TopicProgressLevel("developing"),
	}
}

// The bug this whole file exists for: the quiz side and the memory side each
// minted keys, both legal, neither the same, and the join silently returned
// nothing. The catalogue must present one namespace.
func TestCatalogueMergesBothSidesAndPrefersGradedTopics(t *testing.T) {
	learning := stubProgressLister{rows: []model.TopicProgress{
		graded(7, "rpc-粘包", "RPC 粘包"),
		graded(8, "foreign-topic", "另一个用户的主题"),
	}}
	memory := stubTopicKeyLister{rows: []TopicKeyCount{
		{TopicKey: "rpc-粘包", Topic: "RPC 粘包", Notes: 3},
		{TopicKey: "gin-路由分组", Topic: "Gin 路由分组", Notes: 5},
	}}
	catalogue, err := NewTopicCatalogue(learning, memory, 50)
	if err != nil {
		t.Fatal(err)
	}
	topics, err := catalogue.List(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 2 {
		t.Fatalf("the shared key must appear once, not twice: %+v", topics)
	}
	// Graded first, and the note count from the memory side folded onto it.
	if topics[0].Key != "rpc-粘包" || !topics[0].Graded || topics[0].Notes != 3 {
		t.Fatalf("graded topic did not absorb its notes: %+v", topics[0])
	}
	if topics[1].Key != "gin-路由分组" || topics[1].Graded {
		t.Fatalf("note-only topic must still be offered, ungraded: %+v", topics[1])
	}
	for _, topic := range topics {
		if topic.Key == "foreign-topic" {
			t.Fatal("another user's topic leaked into the catalogue")
		}
	}
}

func TestResolveReusesAnExistingKeyAndKeepsDistinctTopicsApart(t *testing.T) {
	topics := []Topic{{Key: "redis"}, {Key: "redis-ttl"}}
	cases := []struct {
		proposed string
		want     string
		matched  bool
	}{
		// Different wording, same key after normalisation: this is the drift the
		// catalogue is meant to stop.
		{"Redis TTL", "redis-ttl", true},
		{"redis  ttl", "redis-ttl", true},
		// Containment must not merge. Mastery is tracked per topic, and folding
		// `redis-ttl` into `redis` would destroy the distinction it is tracked at.
		{"Redis", "redis", true},
		// Nothing matches: a new topic is minted rather than forced into one.
		{"Gin 路由分组", "gin-路由分组", false},
		{"   ", "", false},
	}
	for _, c := range cases {
		key, matched := Resolve(topics, c.proposed)
		if key != c.want || matched != c.matched {
			t.Errorf("Resolve(%q) = (%q, %v), want (%q, %v)", c.proposed, key, matched, c.want, c.matched)
		}
	}
}

// The prompt is what lets the model reuse a topic whose wording shares nothing
// with the proposal. It must carry identity and nothing else.
func TestCataloguePromptCarriesIdentityWithoutAssessment(t *testing.T) {
	topics := []Topic{
		{Key: "rpc-粘包", Name: "RPC 粘包", Graded: true, Level: model.TopicProgressLevel("weak"), Notes: 3},
		{Key: "gin-路由分组", Name: "Gin 路由分组", Notes: 5},
	}
	rendered := CatalogueForPrompt(topics, 50)
	for _, want := range []string{"rpc-粘包", "RPC 粘包", "[quizzed]", "gin-路由分组"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("prompt is missing %q:\n%s", want, rendered)
		}
	}
	// Scores must not reach a prompt: the design forbids assessment numbers
	// leaving the mastery channel, and the model is being asked which topic this
	// is, not how well the learner knows it.
	for _, forbidden := range []string{"weak", "3", "5"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("prompt leaked assessment detail %q:\n%s", forbidden, rendered)
		}
	}
	if CatalogueForPrompt(nil, 50) != "" {
		t.Error("an empty catalogue must render nothing rather than an empty instruction")
	}
}

func TestCatalogueRejectsUntrustedIdentityAndBadLimits(t *testing.T) {
	catalogue, err := NewTopicCatalogue(stubProgressLister{}, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalogue.List(context.Background(), 0); err == nil {
		t.Fatal("listing must refuse an untrusted user id")
	}
	if _, err := NewTopicCatalogue(nil, nil, 50); err == nil {
		t.Fatal("a catalogue with no source cannot answer anything")
	}
	if _, err := NewTopicCatalogue(stubProgressLister{}, nil, 0); err == nil {
		t.Fatal("an unbounded listing must be rejected")
	}
}

func TestCatalogueSurfacesReadFailures(t *testing.T) {
	catalogue, _ := NewTopicCatalogue(stubProgressLister{err: errors.New("mysql down")}, nil, 50)
	if _, err := catalogue.List(context.Background(), 7); err == nil {
		t.Fatal("a failed read must not be reported as an empty catalogue")
	}
}
