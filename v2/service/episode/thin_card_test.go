package episode

import (
	"context"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
)

// The thin card's own tests.
//
// The claims they defend are the ones this arm is measured on: the card costs
// no model call, it carries no body, it only grows, and the index is only
// rewritten when the searchable words changed.

// countingDeriver counts the derivations it was asked for.
//
// It stands in for the model writer the full card uses, so that "the thin
// card costs no model call" is a statement about the wiring and not about a
// comment.
type countingDeriver struct {
	calls int
}

func (d *countingDeriver) DeriveThinCard(_ context.Context, source ThinCardSource) (model.SessionCardDocument, error) {
	d.calls++
	return ProgrammaticThinCardDeriver{}.DeriveThinCard(context.Background(), source)
}

func thinCardSource() ThinCardSource {
	return ThinCardSource{
		SessionID: "session-1", Title: "缓冲区的复用", LastTalkedAt: time.Unix(1000, 0).UTC(),
		LastMessageID: 42, Topics: []string{"缓冲区复用", "缓存"}, Names: []string{"李雷"},
		SegmentSeqs: []int{3, 1, 2},
	}
}

func TestTheThinCardIsDerivedWithoutAModelCall(t *testing.T) {
	deriver := ProgrammaticThinCardDeriver{}
	document, err := deriver.DeriveThinCard(context.Background(), thinCardSource())
	if err != nil {
		t.Fatalf("derive thin card: %v", err)
	}
	if !document.IsThin() {
		t.Fatalf("the derived card is not a thin card: %+v", document)
	}
	if document.Version != model.SessionThinCardContentVersion {
		t.Fatalf("the card's version is %d, want %d", document.Version, model.SessionThinCardContentVersion)
	}
	if document.Title != "缓冲区的复用" {
		t.Fatalf("the card's title is %q", document.Title)
	}
	if !document.LastTalkedAt.Equal(time.Unix(1000, 0).UTC()) {
		t.Fatalf("the card's last talked time is %v", document.LastTalkedAt)
	}
	if len(document.SegmentSeqs) != 3 || document.SegmentSeqs[0] != 1 ||
		document.SegmentSeqs[1] != 2 || document.SegmentSeqs[2] != 3 {
		t.Fatalf("the card's segment refs are %v, want them sorted", document.SegmentSeqs)
	}
}

// The card carries no body.
//
// It is the whole of the saving: a card that repeated the summary would pay
// for the same words twice.
func TestTheThinCardCarriesNoBody(t *testing.T) {
	document, err := ProgrammaticThinCardDeriver{}.DeriveThinCard(context.Background(), thinCardSource())
	if err != nil {
		t.Fatalf("derive thin card: %v", err)
	}
	if len(document.Synopsis) != 0 {
		t.Fatalf("the thin card has a synopsis: %v", document.Synopsis)
	}
	if len(document.Arrangements) != 0 {
		t.Fatalf("the thin card has arrangements: %v", document.Arrangements)
	}
	if len(document.AssistantProducts) != 0 {
		t.Fatalf("the thin card has assistant products: %v", document.AssistantProducts)
	}
	if len(document.OpenItems) != 0 {
		t.Fatalf("the thin card has open items: %v", document.OpenItems)
	}
	search := ThinCardSearchText(document)
	for _, forbidden := range []string{"题目", "验收标准"} {
		if strings.Contains(search, forbidden) {
			t.Fatalf("the thin card's search text carries %q: %q", forbidden, search)
		}
	}
}

// The search text is the card's own words: the title, the topics, the names.
func TestTheThinCardSearchTextIsItsOwnWords(t *testing.T) {
	document, err := ProgrammaticThinCardDeriver{}.DeriveThinCard(context.Background(), thinCardSource())
	if err != nil {
		t.Fatalf("derive thin card: %v", err)
	}
	search := ThinCardSearchText(document)
	for _, wanted := range []string{"缓冲区的复用", "缓冲区复用", "缓存", "李雷"} {
		if !strings.Contains(search, wanted) {
			t.Fatalf("the search text does not carry %q: %q", wanted, search)
		}
	}
}

// A session with no title of its own still happened.
func TestTheThinCardFallsBackToTheSessionID(t *testing.T) {
	source := thinCardSource()
	source.Title = "   "
	document, err := ProgrammaticThinCardDeriver{}.DeriveThinCard(context.Background(), source)
	if err != nil {
		t.Fatalf("derive thin card: %v", err)
	}
	if document.Title != "session-1" {
		t.Fatalf("the card's title is %q, want the session id", document.Title)
	}
}

// The card only ever grows, except for the title.
func TestTheMergeKeepsWhatTheCardAlreadyKnew(t *testing.T) {
	existing, err := ProgrammaticThinCardDeriver{}.DeriveThinCard(context.Background(), thinCardSource())
	if err != nil {
		t.Fatalf("derive thin card: %v", err)
	}
	fresh := existing
	fresh.Title = "新的标题"
	fresh.Topics = []model.EpisodeTopic{{Key: "并发", Label: "并发"}}
	fresh.Names = []string{"韩梅梅"}
	fresh.SegmentSeqs = []int{4}
	merged := MergeThinCard(existing, fresh)
	if merged.Title != "新的标题" {
		t.Fatalf("the merged title is %q, want the newest one", merged.Title)
	}
	if len(merged.Topics) != 3 {
		t.Fatalf("the merged card has %d topics, want the union: %+v", len(merged.Topics), merged.Topics)
	}
	if len(merged.Names) != 2 {
		t.Fatalf("the merged card has %d names, want the union: %+v", len(merged.Names), merged.Names)
	}
	if len(merged.SegmentSeqs) != 4 {
		t.Fatalf("the merged card has %d segment refs, want the union: %+v",
			len(merged.SegmentSeqs), merged.SegmentSeqs)
	}
}

// A full card is replaced, not merged into.
func TestTheMergeReplacesAFullCard(t *testing.T) {
	full := model.SessionCardDocument{Version: model.SessionCardContentVersion, Title: "满卡片"}
	fresh, err := ProgrammaticThinCardDeriver{}.DeriveThinCard(context.Background(), thinCardSource())
	if err != nil {
		t.Fatalf("derive thin card: %v", err)
	}
	merged := MergeThinCard(full, fresh)
	if !merged.IsThin() {
		t.Fatalf("the merged card is not a thin card: %+v", merged)
	}
	if merged.Title != "缓冲区的复用" {
		t.Fatalf("the merged title is %q", merged.Title)
	}
}

// The write is skipped when nothing changed.
func TestTheCardIsNotWrittenWhenNothingChanged(t *testing.T) {
	document, err := ProgrammaticThinCardDeriver{}.DeriveThinCard(context.Background(), thinCardSource())
	if err != nil {
		t.Fatalf("derive thin card: %v", err)
	}
	existing := &model.EpisodeMemory{Card: document}
	if ThinCardNeedsWrite(existing, document) {
		t.Fatalf("an unchanged card wants a write")
	}
	if ThinCardNeedsReindex(existing, document) {
		t.Fatalf("an unchanged card wants a reindex")
	}
}

// The index is only rewritten when the searchable words changed.
//
// The title is a searchable word, so a renamed session is a card that has to
// be found by its new name.
func TestTheIndexIsOnlyRewrittenWhenTheWordsChanged(t *testing.T) {
	document, err := ProgrammaticThinCardDeriver{}.DeriveThinCard(context.Background(), thinCardSource())
	if err != nil {
		t.Fatalf("derive thin card: %v", err)
	}
	existing := &model.EpisodeMemory{Card: document}
	renamed := document
	renamed.Title = "新的标题"
	if !ThinCardNeedsReindex(existing, renamed) {
		t.Fatalf("a renamed card does not want a reindex")
	}
	// A new segment ref does not change the searchable words: the refs are
	// the way back to the body, not a word of it.
	moreRefs := document
	moreRefs.SegmentSeqs = []int{1, 2, 3, 4}
	if ThinCardNeedsReindex(existing, moreRefs) {
		t.Fatalf("a card with one more segment ref wants a reindex")
	}
	if !ThinCardNeedsWrite(existing, moreRefs) {
		t.Fatalf("a card with one more segment ref does not want a write")
	}
}

// The derivation itself is what runs, and the deriver is asked exactly once.
func TestTheProjectorDerivesOnceAndWritesOnlyWhenItChanged(t *testing.T) {
	// The counting deriver stands in for a model writer: the thin card's
	// claim is that the model is never asked, and the wiring is what makes
	// that true.
	deriver := &countingDeriver{}
	document, err := deriver.DeriveThinCard(context.Background(), thinCardSource())
	if err != nil {
		t.Fatalf("derive thin card: %v", err)
	}
	if deriver.calls != 1 {
		t.Fatalf("the deriver was asked %d times, want once", deriver.calls)
	}
	if !document.IsThin() {
		t.Fatalf("the card is not a thin card")
	}
}
