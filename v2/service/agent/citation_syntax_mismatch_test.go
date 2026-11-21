package agent

import (
	"strings"
	"testing"
)

// The prompt and the citation validator disagree about citation syntax.
//
// The conversation prompt tells the model to "write no bracketed numbers" and
// to point at the learner's words "with the message_ids=... form". The final
// citation check parses only [number] brackets. So a citation the prompt asks
// for is invisible to the check, and a number the check parses is a form the
// prompt forbids for conversation answers.
//
// This test pins the mismatch so an evaluation probe cannot mistake one for
// the other again: an answer written in the taught form produces no citation
// ids, while [405] does.
func TestTheCitationValidatorOnlyParsesNumericBrackets(t *testing.T) {
	taught := finalCitationPattern.FindAllStringSubmatch("答案是动作、提问、验收 [message_ids=405]", -1)
	if len(taught) != 0 {
		t.Fatalf("the taught message_ids form produced citation ids: %v", taught)
	}
	bare := finalCitationPattern.FindAllStringSubmatch("答案是动作、提问、验收（message_ids=405）", -1)
	if len(bare) != 0 {
		t.Fatalf("the bare message_ids form produced citation ids: %v", bare)
	}
	numeric := finalCitationPattern.FindAllStringSubmatch("答案是动作、提问、验收 [405]", -1)
	if len(numeric) != 1 || numeric[0][1] != "405" {
		t.Fatalf("the numeric form did not produce a citation id: %v", numeric)
	}
	if !strings.Contains(conversationCitationRule, "write no bracketed numbers") ||
		!strings.Contains(conversationCitationRule, "message_ids=...") {
		t.Fatalf("the prompt no longer teaches the message_ids form: %s", conversationCitationRule)
	}
}
