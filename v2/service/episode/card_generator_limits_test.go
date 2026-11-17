package episode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"GopherAI/common/observability"
	"GopherAI/model"
	"github.com/cloudwego/eino/schema"
)

// The card writer's limits are the server's limits, and the model is told
// them. These tests are about the three rules that follow from that:
//
//   - a card that is over a count limit keeps its first entries whole and
//     leaves the rest out, and says how many were left out;
//   - an entry the server cannot store is dropped as a whole - never shortened,
//     because a cut sentence can lose the negation that made it true;
//   - a card that cites something it was never shown is still refused as a
//     whole, because that is a different failure and it must stay different.

func cardEvidenceForLimits() CardEvidence {
	now := time.Now()
	return cardEvidenceFor(
		model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "每轮9道题吧", CreatedAt: now},
		model.Message{ID: 2, SessionID: "session-card", IsUser: false, Content: "我给你三道取消机制练习", CreatedAt: now},
	)
}

// cardWithEntries builds a card with the given number of arrangements.
func cardWithEntries(count int) string {
	entries := make([]string, 0, count)
	for i := 0; i < count; i++ {
		entries = append(entries, fmt.Sprintf(`{"text":"条目%d","status":"stated","message_ids":[1]}`, i))
	}
	return fmt.Sprintf(`{
		"version":1,
		"title":"题量",
		"synopsis":"讨论了每轮题量。",
		"topics":[{"key":"ti","label":"题量"}],
		"arrangements":[%s],
		"assistant_products":[],
		"open_items":[]}`, strings.Join(entries, ","))
}

func TestCardOverTheEntryLimitKeepsWholeEntries(t *testing.T) {
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(cardWithEntries(model.SessionCardEntryLimit + 1))}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatalf("build the generator: %v", err)
	}
	document, err := generator.Generate(context.Background(), cardEvidenceForLimits())
	if err != nil {
		t.Fatalf("a card over the limit was refused: %v", err)
	}
	if len(document.Arrangements) != model.SessionCardEntryLimit {
		t.Fatalf("the card holds %d arrangements, want %d", len(document.Arrangements), model.SessionCardEntryLimit)
	}
	// The entries that are kept are the first ones, whole.
	for index, entry := range document.Arrangements {
		if want := fmt.Sprintf("条目%d", index); entry.Text != want {
			t.Fatalf("arrangement %d is %q, want %q", index, entry.Text, want)
		}
	}
}

func TestAnEntryThatCannotBeStoredIsDroppedAndNotShortened(t *testing.T) {
	long := strings.Repeat("很", model.SessionCardEntryTextMaxRunes+10)
	arguments := fmt.Sprintf(`{
		"version":1,
		"title":"题量",
		"synopsis":"讨论了每轮题量。",
		"topics":[{"key":"ti","label":"题量"}],
		"arrangements":[
			{"text":"每轮9道题","status":"stated","message_ids":[1]},
			{"text":%q,"status":"stated","message_ids":[1]}],
		"assistant_products":[],
		"open_items":[]}`, long)
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(arguments)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatalf("build the generator: %v", err)
	}
	document, err := generator.Generate(context.Background(), cardEvidenceForLimits())
	if err != nil {
		t.Fatalf("one unstorable entry refused the whole card: %v", err)
	}
	if len(document.Arrangements) != 1 {
		t.Fatalf("the card holds %d arrangements, want the one that fits", len(document.Arrangements))
	}
	if document.Arrangements[0].Text != "每轮9道题" {
		t.Fatalf("the kept arrangement is %q", document.Arrangements[0].Text)
	}
	// The long entry is gone as a whole. A shortened version of it would be a
	// sentence the model did not write.
	for _, entry := range document.Arrangements {
		if strings.Contains(entry.Text, "很") {
			t.Fatalf("the unstorable entry was shortened into the card: %q", entry.Text)
		}
	}
}

func TestACompliantCardIsUnchanged(t *testing.T) {
	arguments := `{
		"version":1,
		"title":"题量",
		"synopsis":"讨论了每轮题量，助手给了三道取消机制练习。",
		"topics":[{"key":"ti","label":"题量"}],
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[1]}],
		"assistant_products":[{"text":"三道取消机制练习","status":"assistant_proposed","message_ids":[2]}],
		"open_items":[]}`
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(arguments)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatalf("build the generator: %v", err)
	}
	document, err := generator.Generate(context.Background(), cardEvidenceForLimits())
	if err != nil {
		t.Fatalf("a compliant card was refused: %v", err)
	}
	if document.Title != "题量" || document.Synopsis != "讨论了每轮题量，助手给了三道取消机制练习。" {
		t.Fatalf("the card changed: %+v", document)
	}
	if len(document.Arrangements) != 1 || document.Arrangements[0].Text != "每轮9道题" {
		t.Fatalf("the arrangements changed: %+v", document.Arrangements)
	}
	if len(document.AssistantProducts) != 1 || document.AssistantProducts[0].Status != model.SessionCardStatusAssistantProposed {
		t.Fatalf("the assistant products changed: %+v", document.AssistantProducts)
	}
	if len(document.Topics) != 1 || document.Topics[0].Label != "题量" {
		t.Fatalf("the topics changed: %+v", document.Topics)
	}
}

func TestACardThatCitesAMessageItWasNeverShownIsStillRefused(t *testing.T) {
	arguments := `{
		"version":1,
		"title":"题量",
		"synopsis":"讨论了每轮题量。",
		"topics":[],
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[99]}],
		"assistant_products":[],
		"open_items":[]}`
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(arguments)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatalf("build the generator: %v", err)
	}
	if _, err := generator.Generate(context.Background(), cardEvidenceForLimits()); err == nil {
		t.Fatal("a card that cites a message it was never shown was accepted")
	} else if !strings.Contains(err.Error(), "source boundary") {
		t.Fatalf("the refusal is %v, want the source boundary", err)
	}
}

func TestThePromptAndTheSchemaCarryTheServerLimits(t *testing.T) {
	// The prompt says the numbers in words. They are written into the prompt
	// as literals (the evaluation harness reads the product's prompts out of
	// the source that way), so this test is what keeps them equal to the
	// server's limits.
	for _, want := range []string{
		fmt.Sprintf("at most %d arrangements", model.SessionCardEntryLimit),
		fmt.Sprintf("at most %d assistant_products", model.SessionCardEntryLimit),
		fmt.Sprintf("at most %d open_items", model.SessionCardEntryLimit),
		fmt.Sprintf("at most %d topics", model.SessionCardTopicLimit),
		fmt.Sprintf("an entry's text is at most %d characters", model.SessionCardEntryTextMaxRunes),
		fmt.Sprintf("the synopsis at most %d", model.SessionCardSynopsisMaxRunes),
		fmt.Sprintf("the title at most %d", model.SessionCardTitleMaxRunes),
	} {
		if !strings.Contains(cardSystemPrompt, want) {
			t.Fatalf("the prompt does not say %q", want)
		}
	}
	// The schema says them in the form the model reads.
	converted, err := sessionCardTool().ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatalf("read the tool schema: %v", err)
	}
	property, ok := converted.Properties.Get("arrangements")
	if !ok || property == nil {
		t.Fatal("the schema has no arrangements")
	}
	if property.MaxItems == nil || *property.MaxItems != uint64(model.SessionCardEntryLimit) {
		t.Fatalf("the schema's arrangements carry maxItems %v, want %d", property.MaxItems, model.SessionCardEntryLimit)
	}
	if property.Items == nil || property.Items.Properties == nil {
		t.Fatal("the schema's arrangements have no entries")
	}
	text, ok := property.Items.Properties.Get("text")
	if !ok || text == nil || text.MaxLength == nil || *text.MaxLength != uint64(model.SessionCardEntryTextMaxRunes) {
		t.Fatalf("the schema's entry text carries maxLength %v", text)
	}
}

func TestTheCardLimitsAreRecorded(t *testing.T) {
	before := observability.Memory.Snapshot()
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(cardWithEntries(model.SessionCardEntryLimit + 3))}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatalf("build the generator: %v", err)
	}
	if _, err := generator.Generate(context.Background(), cardEvidenceForLimits()); err != nil {
		t.Fatalf("generate the card: %v", err)
	}
	after := observability.Memory.Snapshot()
	counters := after.Counters["session_card_limited_total{kind=omitted_entries}"] - before.Counters["session_card_limited_total{kind=omitted_entries}"]
	if counters != 1 {
		t.Fatalf("the omitted entries were counted %d times, want 1", counters)
	}
	omitted := after.Totals["session_card_omitted_items_total"] - before.Totals["session_card_omitted_items_total"]
	if omitted != 3 {
		t.Fatalf("the omitted items were counted %d times, want 3", omitted)
	}
}

// --- the recorded failures --------------------------------------------------------

// recordedCardFailure is one real answer the real model wrote and the real
// server refused.
type recordedCardFailure struct {
	Scenario   string   `json:"scenario"`
	Arguments  string   `json:"arguments"`
	MessageIDs []uint   `json:"message_ids"`
	Problems   []string `json:"problems"`
}

// TestTheRecordedFailuresAreHandled replays the cards the real model wrote.
//
// The fixtures are the real answers, recorded by the evaluation's replay of the
// card writer (the run directory of the phase-2 acceptance). The test is
// skipped when they are not there, so an ordinary test run does not depend on
// a file outside the repository; when they are there, every recorded failure is
// put through the writer's own path.
//
// What it has to show:
//
//   - a card that was over a count limit is accepted now, with the entries
//     that fit kept whole and the rest left out;
//   - a card that was cut off by the output budget is still a truncation, and
//     is no longer mistaken for a protocol violation;
//   - a card that cites something it was never shown is still refused.
func TestTheRecordedFailuresAreHandled(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("PHASE2_CARD_REPLAY_DIR"))
	if dir == "" {
		t.Skip("there are no recorded failures to replay")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the recorded failures: %v", err)
	}
	played := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		var recorded recordedCardFailure
		if err := json.Unmarshal(raw, &recorded); err != nil {
			t.Fatalf("decode %s: %v", entry.Name(), err)
		}
		played++
		messages := make([]model.Message, 0, len(recorded.MessageIDs))
		for _, id := range recorded.MessageIDs {
			messages = append(messages, model.Message{ID: id, SessionID: "session-card"})
		}
		evidence := cardEvidenceFor(messages...)
		document, err := decodeCardToolResponse(cardToolResponse(recorded.Arguments))
		if err != nil {
			// A cut-off answer cannot be repaired: the JSON never closed. It
			// has to stay a truncation and not become a protocol violation.
			if !strings.Contains(err.Error(), "truncat") {
				t.Fatalf("%s: the refusal is %v, want the truncation", recorded.Scenario, err)
			}
			continue
		}
		report, err := validateGeneratedCard(&document, evidence)
		if err != nil {
			t.Fatalf("%s: the recorded failure is still refused: %v", recorded.Scenario, err)
		}
		if len(document.Arrangements) > model.SessionCardEntryLimit ||
			len(document.AssistantProducts) > model.SessionCardEntryLimit ||
			len(document.OpenItems) > model.SessionCardEntryLimit {
			t.Fatalf("%s: the card still holds more than the limit", recorded.Scenario)
		}
		leftOut := report.OmittedEntries + report.DroppedEntries + report.OmittedTopics + report.DroppedTopics
		if len(recorded.Problems) == 0 {
			// A card the server accepted before must not change now.
			if leftOut != 0 {
				t.Fatalf("%s: a compliant card was changed: %+v", recorded.Scenario, report)
			}
			continue
		}
		if leftOut == 0 {
			t.Fatalf("%s: nothing was left out, so the recording was not exercised", recorded.Scenario)
		}
		t.Logf("%s: %d entries kept, %d omitted, %d dropped",
			recorded.Scenario, report.KeptEntries, report.OmittedEntries, report.DroppedEntries)
	}
	if played == 0 {
		t.Fatalf("there were no recorded failures in %s", dir)
	}
	t.Logf("replayed %d recorded card failures", played)
}
