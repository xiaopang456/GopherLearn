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

// A question that names a subject AND a pointer to the past is about that
// subject, wherever it was said.
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

// THE PAIR: a period on its own stays in this conversation, and a period
// with an explicit pointer to another conversation goes there.
//
// The period is a FILTER, not a reason. "上周讲了什么" names no
// conversation at all, and the conversation in front of the learner is the
// better answer to it; sending the model through the learner's whole
// history to answer it is the wrong order.
//
// "上周另一个聊天里讲了什么" names another conversation, and that is the
// clearest possible "go and look": nothing overrides it.
func TestAPeriodAloneStaysAndAPeriodWithAPointerGoes(t *testing.T) {
	stay := CrossSessionIntentOf("上周讲了什么", true)
	if stay.Cross {
		t.Fatalf("a bare period wanted another conversation: %+v", stay)
	}
	if stay.Reason != CrossSessionCurrentFirst {
		t.Fatalf("a bare period was read as %q, want %q", stay.Reason, CrossSessionCurrentFirst)
	}
	goes := CrossSessionIntentOf("上周另一个聊天里讲了什么", true)
	if !goes.Cross {
		t.Fatalf("a named other conversation stayed in this conversation: %+v", goes)
	}
	if goes.Reason != CrossSessionPointer {
		t.Fatalf("a named other conversation was read as %q, want %q", goes.Reason, CrossSessionPointer)
	}
}

// A period with a subject goes there as well: the subject says WHICH
// earlier conversation, and the period says when.
func TestAPeriodWithASubjectGoes(t *testing.T) {
	intent := CrossSessionIntentOf("上周那份练习清单，单项里最后定的是写什么？", true)
	if !intent.Cross {
		t.Fatalf("a period with a subject stayed in this conversation: %+v", intent)
	}
	if intent.Reason != CrossSessionNamedSubject {
		t.Fatalf("it was read as %q, want %q", intent.Reason, CrossSessionNamedSubject)
	}
}

// A bare period in a conversation with no history still has to mean an
// earlier one: there is nothing here to answer from.
func TestABarePeriodLeavesAnEmptyConversation(t *testing.T) {
	intent := CrossSessionIntentOf("上周讲了什么", false)
	if !intent.Cross {
		t.Fatalf("a bare period stayed in an empty conversation: %+v", intent)
	}
	if intent.Reason != CrossSessionEmptyPointer {
		t.Fatalf("the reason is %q, want %q", intent.Reason, CrossSessionEmptyPointer)
	}
}

// The learner confirms that another conversation should be looked at.
//
// The confirmation is not a question, and the read plan reads it as one
// about the present - so it has to be recognised before the plan is read.
func TestAConfirmationGoes(t *testing.T) {
	for _, question := range []string{"对，是那个", "就查那个", "就上周那个", "是那个聊天"} {
		intent := CrossSessionIntentOf(question, true)
		if !intent.Cross {
			t.Fatalf("%q did not go looking: %+v", question, intent)
		}
		if intent.Reason != CrossSessionPointer {
			t.Fatalf("%q was read as %q, want %q", question, intent.Reason, CrossSessionPointer)
		}
	}
}

// A statement about a plan is not a confirmation, and a plan is not another
// conversation.
//
// Reading "yes, that plan" as a request to go and look would send the model
// through the learner's whole history to answer a sentence about a plan.
func TestAStatementAboutAPlanDoesNotGo(t *testing.T) {
	for _, question := range []string{"是那个方案", "行，就按这个来", "对，那个安排我们改成了按周复习"} {
		intent := CrossSessionIntentOf(question, true)
		if intent.Cross {
			t.Fatalf("%q wanted another conversation: %+v", question, intent)
		}
	}
}

// A question that is not about the learner's past does not go anywhere.
func TestAnOrdinaryQuestionDoesNotLeaveThisConversation(t *testing.T) {
	intent := CrossSessionIntentOf("什么是缓冲区", true)
	if intent.Cross {
		t.Fatalf("an ordinary question wants another conversation: %+v", intent)
	}
}

// A subject with no pointer at all is an ordinary question: it says what the
// question is about, not which conversation holds the answer.
//
// The read plan does not even read it as a question about the learner's
// past, and that is the better reading: there is nothing here that says the
// answer is somewhere else.
func TestASubjectWithNoPointerStaysInThisConversation(t *testing.T) {
	intent := CrossSessionIntentOf("缓冲区复用是怎么做的", true)
	if intent.Cross {
		t.Fatalf("a subject with no pointer wanted another conversation: %+v", intent)
	}
	if intent.Reason != CrossSessionNoHistory {
		t.Fatalf("it was read as %q, want %q", intent.Reason, CrossSessionNoHistory)
	}
}

// The resumption is not a lookup, and this conversation is the continuation.
func TestTheResumptionStaysInThisConversation(t *testing.T) {
	intent := CrossSessionIntentOf("继续上次", true)
	if intent.Cross {
		t.Fatalf("the resumption wants another conversation: %+v", intent)
	}
}
