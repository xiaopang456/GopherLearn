package agent

// The attribute of a claim whose value is a measurement.
//
// The run of 2026-11-14 refused three updates with attribute_not_in_quote:
// the learner's own words carried the value, the value was the entry's
// value, the subject was right, and the attribute was a summary of the
// aspect of the subject the value belongs to - "每次读多少页" - that the
// learner never said in those words.
//
// These tests rebuild that shape with the repository's own words: a page
// count per session, a word count per session and a question count per
// round. The counterexamples are the ones that must not come back - a value
// the learner ruled out, a value the learner only asked about, another item
// that carries the same number, and the same item counted in another unit.
//
// Nothing here calls a model. Every case is the product's own path over
// words that really were said.

import (
	"strings"
	"testing"

	"GopherAI/model"
)

// boardEntry returns the entry a board holds under a key.
//
// The tests below ask whether a value is on the board, and the board holds
// its entries in three groups.
func boardEntry(board model.SessionWhiteboardDocument, key string) (model.SessionWhiteboardEntry, bool) {
	for _, group := range [][]model.SessionWhiteboardEntry{
		board.Constraints, board.Plan, board.OpenItems,
	} {
		for _, entry := range group {
			if entry.Key == key {
				return entry, true
			}
		}
	}
	return model.SessionWhiteboardEntry{}, false
}

// The learner's own words.
//
// The messages are the ones the repository already uses, and the question
// count per round is a shape the repository does not have yet.
const (
	// pagesPerSessionMessage is a page count the learner settled.
	pagesPerSessionMessage = "阅读计划以后每次读41页"
	// wordsPerSessionMessage is a word count the learner settled.
	wordsPerSessionMessage = "背单词改成每次45个吧"
	// questionsPerRoundMessage is a question count the learner settled.
	questionsPerRoundMessage = "作业改成每轮 9 道题吧"
	// ruledOutMessage is a value the learner ruled out.
	ruledOutMessage = "背单词还是每次看30个吧，别改成45个"
	// askedAboutMessage is a value the learner only asked about.
	askedAboutMessage = "阅读计划如果改成41页会怎样"
	// askedAboutWithScopeMessage is a value the learner only asked about,
	// in a sentence that does say how often.
	askedAboutWithScopeMessage = "阅读计划要是改成每次读41页会怎么样？先别改。"
	// anotherBookMessage is a number the learner said about another book.
	anotherBookMessage = "另一本书41页，这本仍然26页"
	// anotherUnitMessage is the same number in another unit.
	anotherUnitMessage = "阅读计划每次读41分钟"
)

// quantityClaim is the claim of a measurement whose attribute is a summary.
//
// The attribute names the aspect of the subject the value belongs to, and it
// is not a sentence the learner said: "每次读多少页" is a name for the
// aspect, and the learner said "以后每次读41页".
func quantityClaim(key string, messageID uint, quote, subject, attribute, value string, stance string) WhiteboardLearnerClaim {
	return learnerClaim(key, messageID, quote, subject, attribute, value, stance)
}

// The summary attribute is not in the learner's words.
//
// The old check refused a claim whose attribute was not in the quote, and
// these attributes are not in their quotes: they are the summaries that made
// the run drop three updates the learner had stated plainly. The assertion
// keeps the cases honest - if a wording of a claim ever makes the attribute
// a sentence the learner said, the case below no longer reproduces the
// refusal and has to be rewritten.
func TestTheSummaryOfAQuantityIsNotASentenceTheLearnerSaid(t *testing.T) {
	cases := []struct {
		name    string
		quote   string
		subject string
		summary string
	}{
		{"pages per session", pagesPerSessionMessage, "阅读计划", "每次读多少页"},
		{"words per session", wordsPerSessionMessage, "背单词", "每次背多少个"},
		{"questions per round", questionsPerRoundMessage, "作业", "每轮题量"},
	}
	for _, one := range cases {
		if strings.Contains(one.quote, one.summary) {
			t.Fatalf("%s: the attribute %q is in the quote: this is not the case the run refused",
				one.name, one.summary)
		}
		if !strings.Contains(one.quote, one.subject) {
			t.Fatalf("%s: the subject %q is not in the quote", one.name, one.subject)
		}
	}
}

// The learner settled a page count per session. The value is the entry's
// value, the subject is right, and the attribute is a summary - so the entry
// is the learner's own current value.
func TestThePageCountPerSessionIsTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, pagesPerSessionMessage, 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
	}, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			quantityClaim("pages", 95, pagesPerSessionMessage, "阅读计划", "每次读多少页", "41 页",
				whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	entry, found := boardEntry(result.Board, "pages")
	if !found {
		t.Fatalf("the board does not carry the reading plan: %+v", result.Board.Constraints)
	}
	if entry.Value != "41 页" {
		t.Fatalf("the board carries %q, want the learner's new value", entry.Value)
	}
	if entry.EntryOrigin() != model.SessionWhiteboardOriginUser {
		t.Fatalf("the entry's origin = %q, want the learner's", entry.EntryOrigin())
	}
	if result.Diagnostics.VerifiedClaims != 1 {
		t.Fatalf("verified claims = %d, want 1", result.Diagnostics.VerifiedClaims)
	}
}

// The learner settled a word count per session.
func TestTheWordCountPerSessionIsTheLearnersValue(t *testing.T) {
	batch := assistantBatch(93, wordsPerSessionMessage, 94, "往后每次 45 个。")
	result, err := ApplyWhiteboardProposal(model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
	}, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_session", "背单词每次背 45 个", "45 个", "", 93),
		},
		Claims: []WhiteboardLearnerClaim{
			quantityClaim("words_per_session", 93, wordsPerSessionMessage, "背单词", "每次背多少个", "45 个",
				whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	entry, found := boardEntry(result.Board, "words_per_session")
	if !found {
		t.Fatalf("the board does not carry the word count: %+v", result.Board.Constraints)
	}
	if entry.Value != "45 个" {
		t.Fatalf("the board carries %q, want the learner's new value", entry.Value)
	}
	if result.Diagnostics.VerifiedClaims != 1 {
		t.Fatalf("verified claims = %d, want 1", result.Diagnostics.VerifiedClaims)
	}
}

// The learner settled a question count per round.
func TestTheQuestionCountPerRoundIsTheLearnersValue(t *testing.T) {
	batch := assistantBatch(93, questionsPerRoundMessage, 94, "往后每轮 9 道题。")
	result, err := ApplyWhiteboardProposal(model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
	}, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("questions_per_round", "作业每轮做 9 道题", "9 道题", "", 93),
		},
		Claims: []WhiteboardLearnerClaim{
			quantityClaim("questions_per_round", 93, questionsPerRoundMessage, "作业", "每轮题量", "9 道题",
				whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	entry, found := boardEntry(result.Board, "questions_per_round")
	if !found {
		t.Fatalf("the board does not carry the question count: %+v", result.Board.Constraints)
	}
	if entry.Value != "9 道题" {
		t.Fatalf("the board carries %q, want the learner's new value", entry.Value)
	}
	if result.Diagnostics.VerifiedClaims != 1 {
		t.Fatalf("verified claims = %d, want 1", result.Diagnostics.VerifiedClaims)
	}
}

// The learner ruled the value out. The words are the learner's own and the
// value is in them, but the learner said the opposite of what the value
// says, so the value is not a current fact.
func TestTheRuledOutQuantityIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(93, ruledOutMessage, 94, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_session", "背单词每次背 45 个", "45 个", "", 93),
		},
		Claims: []WhiteboardLearnerClaim{
			quantityClaim("words_per_session", 93, ruledOutMessage, "背单词", "每次背多少个", "45 个",
				whiteboardClaimStanceNegated),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := boardEntry(result.Board, "words_per_session"); found {
		t.Fatalf("the board carries the ruled out value: %+v", result.Board.Constraints)
	}
	if result.Diagnostics.UnsettledClaims != 1 {
		t.Fatalf("unsettled claims = %d, want 1", result.Diagnostics.UnsettledClaims)
	}
}

// The ruled out value stays out even when the model calls the words a
// decision.
//
// This is the case the scope of the value is for. The learner's sentence is
// two clauses: "还是每次看30个吧" and "别改成45个". The 每次 belongs to the
// thirty words, and the forty-five stand in a clause that says no scope at
// all - so the value is not a value of a clause that says how often, whatever
// the model says about the words.
func TestTheRuledOutQuantityStaysOutEvenWhenTheModelIsWrong(t *testing.T) {
	batch := assistantBatch(93, ruledOutMessage, 94, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_session", "背单词每次背 45 个", "45 个", "", 93),
		},
		Claims: []WhiteboardLearnerClaim{
			quantityClaim("words_per_session", 93, ruledOutMessage, "背单词", "每次背多少个", "45 个",
				whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := boardEntry(result.Board, "words_per_session"); found {
		t.Fatalf("the board carries the ruled out value: %+v", result.Board.Constraints)
	}
	if result.Diagnostics.VerifiedClaims != 0 {
		t.Fatalf("verified claims = %d, want 0", result.Diagnostics.VerifiedClaims)
	}
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
}

// The learner asked what would happen. Nothing was decided, so the board
// keeps what it has.
func TestTheQuantityTheLearnerOnlyAskedAboutIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, askedAboutMessage, 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			quantityClaim("pages", 95, askedAboutMessage, "阅读计划", "每次读多少页", "41 页",
				whiteboardClaimStanceHypothetical),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := boardEntry(result.Board, "pages"); value.Value != "26 页/次" {
		t.Fatalf("the board carries %q, want the value that holds now", value.Value)
	}
	if result.Diagnostics.UnsettledClaims != 1 {
		t.Fatalf("unsettled claims = %d, want 1", result.Diagnostics.UnsettledClaims)
	}
}

// The question the learner only asked about, in a sentence that does say how
// often.
//
// This is the boundary of the scope check, and it is written down here
// rather than left to be found: the clause does say 每次, so the scope of
// the value is in order and the value is refused by the model's own answer
// about the words - the learner asked, they did not decide. A model that
// called these words a decision would have the value settled, and the
// program cannot prove such a model wrong. The scope check does not reach
// this sentence, and the report of the change says so.
func TestTheHypothesisThatNamesTheScopeIsRefusedByTheModelsOwnAnswer(t *testing.T) {
	batch := assistantBatch(95, askedAboutWithScopeMessage, 96, "不改的话还是 26 页。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			quantityClaim("pages", 95, askedAboutWithScopeMessage, "阅读计划", "每次读多少页", "41 页",
				whiteboardClaimStanceHypothetical),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := boardEntry(result.Board, "pages"); value.Value != "26 页/次" {
		t.Fatalf("the board carries %q, want the value that holds now", value.Value)
	}
	if result.Diagnostics.UnsettledClaims != 1 {
		t.Fatalf("unsettled claims = %d, want 1", result.Diagnostics.UnsettledClaims)
	}
}

// Another item carries the same number. The words the learner counted per
// session are not the words they count per day, and a per-day entry cannot
// take a value out of a per-session sentence.
func TestAnotherItemWithTheSameNumberIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(93, wordsPerSessionMessage, 94, "往后每次 45 个。")
	result, err := ApplyWhiteboardProposal(model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
	}, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_day", "背单词每天背 45 个", "45 个", "", 93),
		},
		Claims: []WhiteboardLearnerClaim{
			quantityClaim("words_per_day", 93, wordsPerSessionMessage, "背单词", "每天背多少个", "45 个",
				whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := boardEntry(result.Board, "words_per_day"); found {
		t.Fatalf("the board carries the per day value: %+v", result.Board.Constraints)
	}
	if result.Diagnostics.VerifiedClaims != 0 {
		t.Fatalf("verified claims = %d, want 0", result.Diagnostics.VerifiedClaims)
	}
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
}

// The same item counted in another unit. Forty-one pages are not forty-one
// minutes, however close the numbers are.
func TestTheSameItemInAnotherUnitIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, anotherUnitMessage, 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			quantityClaim("pages", 95, anotherUnitMessage, "阅读计划", "每次读多少页", "41 分钟",
				whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := boardEntry(result.Board, "pages"); value.Value != "26 页/次" {
		t.Fatalf("the board carries %q, want the value that holds now", value.Value)
	}
	if result.Diagnostics.VerifiedClaims != 0 {
		t.Fatalf("verified claims = %d, want 0", result.Diagnostics.VerifiedClaims)
	}
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
}

// The number of another book is not the number of this one.
func TestTheNumberOfAnotherBookIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, anotherBookMessage, 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			quantityClaim("pages", 95, anotherBookMessage, "另一本书", "每次读多少页", "41 页",
				whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := boardEntry(result.Board, "pages"); value.Value != "26 页/次" {
		t.Fatalf("the board carries %q, want the value that holds now", value.Value)
	}
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
}

// A name is checked the way it always was.
//
// The attribute of a name is a fact about the learner's words, and the run
// of 2026-11-14 refused a study-format entry for exactly this reason: the
// attribute was not a sentence the learner said.
func TestTheAttributeOfANameIsStillLookedForInTheWords(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("study_format", "阅读计划改成听录播", "听录播", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			quantityClaim("study_format", 95, "阅读计划以后每次读41页", "阅读计划", "学习形式", "听录播",
				whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := boardEntry(result.Board, "study_format"); found {
		t.Fatalf("the board carries the entry: %+v", result.Board.Constraints)
	}
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
}
