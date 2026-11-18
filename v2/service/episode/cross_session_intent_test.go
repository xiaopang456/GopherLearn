package episode

import "testing"

// The cross-session intent.
//
// It is the rule the learner would expect, and it is stated in one place so
// that the two search tools cannot disagree about it.

// TestTheBarePointerStaysInThisConversation covers the rule that matters most:
// a question that names nothing but a pointer is about the conversation in
// front of the learner, if that conversation has a history.
func TestTheBarePointerStaysInThisConversation(t *testing.T) {
	for _, question := range []string{"上次呢", "上次讲了什么", "上次那个决定"} {
		intent := CrossSessionIntentOf(question, true)
		if intent.Cross {
			t.Fatalf("%q wants another conversation: %+v", question, intent)
		}
		if intent.Reason != CrossSessionCurrentFirst {
			t.Fatalf("%q was read as %q, want %q", question, intent.Reason, CrossSessionCurrentFirst)
		}
	}
}

// A bare pointer in a conversation with no history has to mean an earlier
// one: there is nothing here to point back to.
func TestTheBarePointerLeavesAnEmptyConversation(t *testing.T) {
	intent := CrossSessionIntentOf("上次讲了什么", false)
	if !intent.Cross {
		t.Fatalf("the pointer stayed in an empty conversation: %+v", intent)
	}
	if intent.Reason != CrossSessionEmptyPointer {
		t.Fatalf("the reason is %q, want %q", intent.Reason, CrossSessionEmptyPointer)
	}
}

// A question that names a subject is about that subject, wherever it was said.
//
// This is the case the whole card exists for: "the three exercises you set
// me last time" names the exercises, and the answer is in the earlier
// conversation that holds them.
func TestANamedSubjectLeavesThisConversation(t *testing.T) {
	for _, question := range []string{
		"上次你给我排的那三道题里，第二道练的是什么？",
		"上次关于缓冲区复用的决定是什么",
	} {
		intent := CrossSessionIntentOf(question, true)
		if !intent.Cross {
			t.Fatalf("%q stayed in this conversation: %+v", question, intent)
		}
		if intent.Reason != CrossSessionNamedSubject {
			t.Fatalf("%q was read as %q, want %q", question, intent.Reason, CrossSessionNamedSubject)
		}
	}
}

// A named period is another conversation by definition.
func TestANamedPeriodLeavesThisConversation(t *testing.T) {
	intent := CrossSessionIntentOf("上周讲的内容是什么", true)
	if !intent.Cross {
		t.Fatalf("a named period stayed in this conversation: %+v", intent)
	}
}

// A question that is not about the learner's past does not go anywhere.
func TestAnOrdinaryQuestionDoesNotLeaveThisConversation(t *testing.T) {
	intent := CrossSessionIntentOf("什么是缓冲区", true)
	if intent.Cross {
		t.Fatalf("an ordinary question wants another conversation: %+v", intent)
	}
}

// The resumption is not a lookup, and this conversation is the continuation.
func TestTheResumptionStaysInThisConversation(t *testing.T) {
	intent := CrossSessionIntentOf("继续上次", true)
	if intent.Cross {
		t.Fatalf("the resumption wants another conversation: %+v", intent)
	}
}
