package episode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"GopherAI/common/observability"
	"GopherAI/model"
	"github.com/cloudwego/eino/schema"
)

// The card writer's limits are the server's limits, and the model is told
// them. These tests are about the three rules that follow from that:
//
//   - a card that is over a count limit keeps its entries whole and leaves the
//     rest out, and says how many were left out;
//   - the entries that record a later change survive the cut, because a card
//     that keeps the value from before the change and drops the value after it
//     states something that is no longer true;
//   - an entry the server cannot store is dropped as a whole - never shortened,
//     because a cut sentence can lose the negation that made it true;
//   - a card that cites something it was never shown is still refused as a
//     whole, because that is a different failure and it must stay different -
//     and it stays different even when the entry carrying the forged citation
//     is unstorable for another reason.

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

// TestAForgedCitationIsNotLaunderedByAnUnstorableEntry is the regression that
// the check has to stay a whole-card refusal.
//
// The forged citation sits on an entry that is too long to store. If the
// storability check ran first, that entry would be dropped as a whole and the
// forged reference would go with it, and the card would be accepted for a
// reason that has nothing to do with the forgery.
func TestAForgedCitationIsNotLaunderedByAnUnstorableEntry(t *testing.T) {
	long := strings.Repeat("很", model.SessionCardEntryTextMaxRunes+10)
	arguments := fmt.Sprintf(`{
		"version":1,
		"title":"题量",
		"synopsis":"讨论了每轮题量。",
		"topics":[],
		"arrangements":[
			{"text":"每轮9道题","status":"stated","message_ids":[1]},
			{"text":%q,"status":"stated","message_ids":[99]}],
		"assistant_products":[],
		"open_items":[]}`, long)
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(arguments)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatalf("build the generator: %v", err)
	}
	if _, err := generator.Generate(context.Background(), cardEvidenceForLimits()); err == nil {
		t.Fatal("a card with a forged citation on an unstorable entry was accepted")
	} else if !strings.Contains(err.Error(), "source boundary") {
		t.Fatalf("the refusal is %v, want the source boundary", err)
	}
}

// TestTheEntriesThatRecordALaterChangeSurviveTheCut is the rule that keeps a
// card from stating something that is no longer true.
//
// The model writes the entries in the order the session happened in, so the
// change comes last, and a list that is cut at its end takes the change with
// it. The card would then keep the value from before the change and drop the
// value the learner ended with.
func TestTheEntriesThatRecordALaterChangeSurviveTheCut(t *testing.T) {
	entries := make([]string, 0, model.SessionCardEntryLimit+4)
	for i := 0; i < model.SessionCardEntryLimit+4; i++ {
		entries = append(entries, fmt.Sprintf(`{"text":"条目%d","status":"stated","message_ids":[1]}`, i))
	}
	// The change is written last, the way the model writes it.
	entries = append(entries,
		`{"text":"原来每轮做14道题","status":"later_changed","message_ids":[1]}`,
		`{"text":"现在改成每轮9道题","status":"later_changed","message_ids":[1]}`)
	arguments := fmt.Sprintf(`{
		"version":1,
		"title":"题量",
		"synopsis":"讨论了每轮题量。",
		"topics":[{"key":"ti","label":"题量"}],
		"arrangements":[%s],
		"assistant_products":[],
		"open_items":[]}`, strings.Join(entries, ","))
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(arguments)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatalf("build the generator: %v", err)
	}
	document, err := generator.Generate(context.Background(), cardEvidenceForLimits())
	if err != nil {
		t.Fatalf("the card was refused: %v", err)
	}
	if len(document.Arrangements) != model.SessionCardEntryLimit {
		t.Fatalf("the card holds %d arrangements, want %d", len(document.Arrangements), model.SessionCardEntryLimit)
	}
	changed := 0
	for _, entry := range document.Arrangements {
		if entry.Status.Normalized() == model.SessionCardStatusLaterChanged {
			changed++
		}
	}
	if changed != 2 {
		t.Fatalf("the card holds %d entries that record a later change, want both", changed)
	}
	// The entries keep the order the model wrote them in.
	for index, entry := range document.Arrangements {
		if index < model.SessionCardEntryLimit-4 {
			if want := fmt.Sprintf("条目%d", index); entry.Text != want {
				t.Fatalf("arrangement %d is %q, want %q", index, entry.Text, want)
			}
			continue
		}
	}
}

// TestATitleAndASynopsisThatAreTooLongAreCutAndRecorded covers the two fields
// that are the card's own description of the session.
//
// A model that writes a long description loses the tail of that description
// and keeps the facts; a card refused for a long description loses the whole
// session, and a session that happened is worth a card.
func TestATitleAndASynopsisThatAreTooLongAreCutAndRecorded(t *testing.T) {
	title := strings.Repeat("题", model.SessionCardTitleMaxRunes+10)
	synopsis := "第一句。" + strings.Repeat("很", model.SessionCardSynopsisMaxRunes)
	arguments := fmt.Sprintf(`{
		"version":1,
		"title":%q,
		"synopsis":%q,
		"topics":[{"key":"ti","label":"题量"}],
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[1]}],
		"assistant_products":[],
		"open_items":[]}`, title, synopsis)
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(arguments)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatalf("build the generator: %v", err)
	}
	document, err := generator.Generate(context.Background(), cardEvidenceForLimits())
	if err != nil {
		t.Fatalf("a card with a long description was refused: %v", err)
	}
	if got := utf8.RuneCountInString(document.Title); got > model.SessionCardTitleMaxRunes {
		t.Fatalf("the title holds %d runes, want at most %d", got, model.SessionCardTitleMaxRunes)
	}
	if got := utf8.RuneCountInString(document.Synopsis); got > model.SessionCardSynopsisMaxRunes {
		t.Fatalf("the synopsis holds %d runes, want at most %d", got, model.SessionCardSynopsisMaxRunes)
	}
	// The cut lands at the end of a sentence, so what is kept is a whole
	// sentence and not half of one.
	if !strings.HasSuffix(document.Synopsis, "。") {
		t.Fatalf("the synopsis was cut mid-sentence: %q", document.Synopsis)
	}
	// The facts are kept whole: the entries are not touched by the cut.
	if len(document.Arrangements) != 1 || document.Arrangements[0].Text != "每轮9道题" {
		t.Fatalf("the entries changed: %+v", document.Arrangements)
	}
	// The cut is recorded, so a reader who sees a short description can tell
	// that it was cut.
	snapshot := observability.Memory.Snapshot()
	if snapshot.Counters["session_card_truncated_total{field=title}"] < 1 ||
		snapshot.Counters["session_card_truncated_total{field=synopsis}"] < 1 {
		t.Fatalf("the cut was not recorded: %+v", snapshot.Counters)
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
		fmt.Sprintf("a topic key or label at most %d characters", model.EpisodeTitleMaxRunes),
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
	// Every list of entries carries the count limit and the text limit.
	for _, name := range []string{"arrangements", "assistant_products", "open_items"} {
		property, ok := converted.Properties.Get(name)
		if !ok || property == nil {
			t.Fatalf("the schema has no %s", name)
		}
		if property.MaxItems == nil || *property.MaxItems != uint64(model.SessionCardEntryLimit) {
			t.Fatalf("the schema's %s carry maxItems %v, want %d", name, property.MaxItems, model.SessionCardEntryLimit)
		}
		if property.Items == nil || property.Items.Properties == nil {
			t.Fatalf("the schema's %s have no entries", name)
		}
		text, ok := property.Items.Properties.Get("text")
		if !ok || text == nil || text.MaxLength == nil || *text.MaxLength != uint64(model.SessionCardEntryTextMaxRunes) {
			t.Fatalf("the schema's %s entry text carries maxLength %v", name, text)
		}
	}
	// The topics carry the count limit and the length limit of their own
	// fields. A limit the model cannot see is a limit it will break.
	topics, ok := converted.Properties.Get("topics")
	if !ok || topics == nil {
		t.Fatal("the schema has no topics")
	}
	if topics.MaxItems == nil || *topics.MaxItems != uint64(model.SessionCardTopicLimit) {
		t.Fatalf("the schema's topics carry maxItems %v, want %d", topics.MaxItems, model.SessionCardTopicLimit)
	}
	if topics.Items == nil || topics.Items.Properties == nil {
		t.Fatal("the schema's topics have no fields")
	}
	for _, name := range []string{"key", "label"} {
		field, ok := topics.Items.Properties.Get(name)
		if !ok || field == nil || field.MaxLength == nil || *field.MaxLength != uint64(model.EpisodeTitleMaxRunes) {
			t.Fatalf("the schema's topic %s carries maxLength %v, want %d", name, field, model.EpisodeTitleMaxRunes)
		}
	}
	// The two free texts carry their limits too.
	for name, limit := range map[string]int{
		"synopsis": model.SessionCardSynopsisMaxRunes,
		"title":    model.SessionCardTitleMaxRunes,
	} {
		property, ok := converted.Properties.Get(name)
		if !ok || property == nil || property.MaxLength == nil || *property.MaxLength != uint64(limit) {
			t.Fatalf("the schema's %s carries maxLength %v, want %d", name, property, limit)
		}
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
	// The kept count is a number of its own, so a reader can tell what the
	// omitted count is counted against.
	kept := after.Totals["session_card_kept_entries_total"] - before.Totals["session_card_kept_entries_total"]
	if kept != int64(model.SessionCardEntryLimit) {
		t.Fatalf("the kept entries were counted %d times, want %d", kept, model.SessionCardEntryLimit)
	}
}

// TestADroppedEntryIsCountedApartFromAnOmittedOne keeps the two apart.
//
// An omitted entry is one that fitted but did not fit together with the
// others; a dropped one is one the server could not store at all. They are
// two different facts about the card, and a reader who cannot tell them apart
// cannot tell a card that was trimmed from one that was cleaned.
func TestADroppedEntryIsCountedApartFromAnOmittedOne(t *testing.T) {
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
	before := observability.Memory.Snapshot()
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(arguments)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatalf("build the generator: %v", err)
	}
	if _, err := generator.Generate(context.Background(), cardEvidenceForLimits()); err != nil {
		t.Fatalf("generate the card: %v", err)
	}
	after := observability.Memory.Snapshot()
	dropped := after.Totals["session_card_dropped_items_total"] - before.Totals["session_card_dropped_items_total"]
	if dropped != 1 {
		t.Fatalf("the dropped items were counted %d times, want 1", dropped)
	}
	omitted := after.Totals["session_card_omitted_items_total"] - before.Totals["session_card_omitted_items_total"]
	if omitted != 0 {
		t.Fatalf("a dropped entry was counted as an omitted one: %d", omitted)
	}
	if after.Counters["session_card_limited_total{kind=dropped_entries}"]-
		before.Counters["session_card_limited_total{kind=dropped_entries}"] != 1 {
		t.Fatalf("the dropped entries were not counted: %+v", after.Counters)
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
	// Expect says what the server does with the card now: "accepted" (the
	// default) or "refused".
	Expect string `json:"expect"`
	// MustSurvive are the entry texts that have to be in the card the server
	// keeps. It is what makes a fixture about WHICH entries survive, and not
	// only about how many were left out.
	MustSurvive []string `json:"must_survive"`
}

// TestTheRecordedFailuresAreHandled replays the cards the real model wrote.
//
// The fixtures are the real answers, recorded by the evaluation's replay of the
// card writer (the run directory of the phase-2 acceptance). The test runs
// against the fixtures that are in this repository, so the replay is part of
// every build; PHASE2_CARD_REPLAY_DIR points it at the recorded answers
// instead.
//
// The fixtures in this repository are written in the shape of the recorded
// answers but do not carry the learner's words: the repository is public, and
// a recorded answer is a real conversation. The recorded ones are read from
// the run directory, which is not published.
//
// What it has to show:
//
//   - a card that was over a count limit is accepted now, with the entries
//     that fit kept whole and the rest left out;
//   - the entries that record a later change are among the ones that fit,
//     because a card that keeps the old value and drops the new one states
//     something that is no longer true;
//   - a card that was cut off by the output budget is still a truncation, and
//     is no longer mistaken for a protocol violation;
//   - a card that cites something it was never shown is still refused.
func TestTheRecordedFailuresAreHandled(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("PHASE2_CARD_REPLAY_DIR"))
	if dir == "" {
		dir = filepath.Join("testdata", "card-replay")
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
		// The counts are taken before the server touches the document: the
		// check below is about what the model wrote and what the card kept.
		writtenChanges := map[string]int{}
		for index, group := range document.EntryGroups() {
			count := 0
			for _, entry := range group {
				if entry.Status.Normalized() == model.SessionCardStatusLaterChanged {
					count++
				}
			}
			writtenChanges[strconv.Itoa(index)] = count
		}
		report, err := validateGeneratedCard(&document, evidence)
		if recorded.Expect == "refused" {
			// The refusal has to stay a refusal. A card that cites a message
			// it was never shown is making things up about where its content
			// came from, and that is a whole-card failure.
			if err == nil {
				t.Fatalf("%s: a card that cites a message it was never shown was accepted", recorded.Scenario)
			}
			if !strings.Contains(err.Error(), "source boundary") {
				t.Fatalf("%s: the refusal is %v, want the source boundary", recorded.Scenario, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: the recorded failure is still refused: %v", recorded.Scenario, err)
		}
		if len(document.Arrangements) > model.SessionCardEntryLimit ||
			len(document.AssistantProducts) > model.SessionCardEntryLimit ||
			len(document.OpenItems) > model.SessionCardEntryLimit {
			t.Fatalf("%s: the card still holds more than the limit", recorded.Scenario)
		}
		// The entries that have to be there are checked by their text, not by
		// their count: a fixture that keeps twelve entries and drops the one
		// that carried the change is a different failure from one that keeps
		// twelve of the wrong ones.
		for _, want := range recorded.MustSurvive {
			if !cardHasText(document, want) {
				t.Fatalf("%s: the card does not carry %q", recorded.Scenario, want)
			}
		}
		// The entries that record a later change are never the ones left out.
		// The model writes them last, so a list that is cut at its end takes
		// the change with it, and the card then states the value from before
		// the change as if it still held.
		for index, group := range document.EntryGroups() {
			written := writtenChanges[strconv.Itoa(index)]
			kept := 0
			for _, entry := range group {
				if entry.Status.Normalized() == model.SessionCardStatusLaterChanged {
					kept++
				}
			}
			want := written
			if want > model.SessionCardEntryLimit {
				want = model.SessionCardEntryLimit
			}
			if kept != want {
				t.Fatalf("%s: the card keeps %d of the %d entries that record a later change, want %d",
					recorded.Scenario, kept, written, want)
			}
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

// cardHasText says whether one of the card's entries carries the text.
func cardHasText(document model.SessionCardDocument, text string) bool {
	for _, group := range document.EntryGroups() {
		for _, entry := range group {
			if entry.Text == text {
				return true
			}
		}
	}
	return false
}
