package agent

import "strings"

// Whether the learner asked to have something remembered.
//
// This predicate used to live in the tool package, because the tool package
// was the only thing that consulted it: the draft tool was hidden from the
// model unless the question matched the phrase list. The draft tool is now on
// the table every turn, and what the phrase list decides has moved as well - it
// decides whether the prompt tells the model to propose saving, not whether the
// model may call the tool. The prompt is built here, so the predicate lives
// here, and the tool package asks.
//
// It is still one predicate and one phrase list. Two copies would drift, and a
// drift here is invisible: the tool would be available under one rule and the
// prompt would describe another.

// ExplicitMemoryIntent reports whether the learner asked to save something.
//
// A recall question reads existing memory; it does not grant permission to
// create a new draft. That check comes first, because English phrases such as
// "remember my" are ambiguous: "do you remember my name" is a question about
// the past, and "remember my name is X" is an instruction.
func ExplicitMemoryIntent(question string) bool {
	value := strings.ToLower(strings.TrimSpace(question))
	if value == "" {
		return false
	}

	// A recall question reads existing memory; it does not grant permission to
	// create a new draft. Check it before English phrases such as "remember my".
	if containsAnyPhrase(value, []string{
		"你还记得", "还记得我", "还记得这", "是否记得", "记不记得",
		"do you remember", "did you remember", "what do you remember", "can you remember",
	}) {
		return false
	}

	if containsAnyPhrase(value, []string{
		"请记住", "记住我", "记住这", "记住：", "记住:", "帮我记", "给我记", "替我记",
		"记一下", "记下来", "保存这", "保存为我的",
		"remember this", "remember that", "remember my", "please remember", "save this", "save that", "keep this in mind",
	}) {
		return true
	}

	// Cross-session defaults are also explicit memory instructions even when
	// the user does not literally say "remember".
	futureDirective := containsAnyPhrase(value, []string{
		"以后", "今后", "从现在起", "接下来", "from now on", "going forward", "in the future",
	})
	persistentPolicy := containsAnyPhrase(value, []string{
		"请默认", "默认我", "都按", "请一直", "一直按", "请始终", "始终先", "请把我当",
		"default to", "assume that i", "always treat", "always explain", "always use",
	})
	return futureDirective && persistentPolicy
}

// containsAnyPhrase reports whether the value contains one of the markers.
func containsAnyPhrase(value string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}
