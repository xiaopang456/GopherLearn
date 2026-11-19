package episode

import "strings"

// The cross-session intent.
//
// It is one question, asked before the tools are: is this turn about a
// conversation other than the one being answered?
//
// The answer decides whether the model may go looking at an earlier
// conversation at all. The rule is the one the learner would expect:
//
//   - A question that NAMES another conversation goes there. "the other
//     chat", "another session" - the learner said which one.
//   - A question that names a SUBJECT and a pointer to the past goes there
//     too: the subject says WHICH earlier conversation, so the answer is in
//     it and not here. "the three exercises you set me last time" names the
//     exercises, and the answer is in the earlier conversation that holds
//     them.
//   - A question that names a subject and NOTHING else is an ordinary
//     question. Whether it is answered here is decided by whether this
//     conversation has a history to answer it from.
//   - A question that names nothing but a POINTER - "last time", "last
//     week" - is about the conversation in front of the learner, if that
//     conversation has a history. The current conversation is the first
//     place to look, and looking elsewhere before looking here is the wrong
//     order.
//
// A PERIOD IS A FILTER, NOT A REASON.
//
// "last week" says which conversations are eligible, not that another
// conversation has to be opened. The old rule read a named period as
// "another conversation by definition", and that put a question like "what
// did we talk about last week" into the cross-session search while naming
// nothing at all - the one case where this conversation is the better
// answer, because it is the conversation in front of the learner.
//
// The last case is the one that used to be decided by the retriever's
// ranking, which is to say it was decided by whatever the index happened to
// score highest. The rule is now stated where the tools can read it.

// CrossSessionIntent is the answer to that question.
type CrossSessionIntent struct {
	// Cross says the question has to leave this conversation.
	Cross bool
	// Reason says why, in the vocabulary the read plan already uses.
	Reason string
}

// The reasons a cross-session intent reports.
const (
	// CrossSessionNamedSubject means the question named a subject and a
	// pointer to the past, so the subject says which earlier conversation
	// holds the answer.
	CrossSessionNamedSubject = "named_subject"
	// CrossSessionPointer means the question named another conversation, or
	// the learner confirmed that another one should be looked at.
	CrossSessionPointer = "cross_session_pointer"
	// CrossSessionEmptyPointer means the question was a bare pointer and
	// this conversation has no history to point back to, so the pointer has
	// to mean an earlier one.
	CrossSessionEmptyPointer = "empty_session_pointer"
	// CrossSessionCurrentFirst means the question was a bare pointer and
	// THIS conversation has the history. The answer is here first.
	CrossSessionCurrentFirst = "current_session_has_history"
	// CrossSessionNoHistory means the question was not about the learner's
	// past at all.
	CrossSessionNoHistory = "no_personal_history_intent"
)

// The words that name another conversation of this learner.
//
// They are the clearest possible "go and look", and nothing overrides them.
var crossSessionPointers = []string{
	"另一个聊天", "别的聊天", "其他聊天", "另一次聊天",
	"另一个会话", "别的会话", "其他会话", "另一次会话",
	"另一个对话", "别的对话", "其他对话",
	"前面对话", "之前那个聊天", "之前那次聊天", "上次那个聊天",
	"别的记录", "其他记录", "别的聊天记录", "其他聊天记录",
}

// The words that confirm the learner wants another conversation looked at.
//
// A confirmation is not a question, and the read plan reads it as one about
// the present - so it is recognised before the plan is consulted at all.
var crossSessionConfirmations = []string{
	"就查那个", "就查那次", "就查另一个", "查另一个", "查别的", "查其他",
	"就那个聊天", "就那次会话", "就那个会话", "就另一个",
	"是那个聊天", "是那次会话", "是那个会话", "是上次那个",
}

// The deictics a confirmation may use.
var confirmationDeictics = []string{"那个", "那次", "这条", "这边", "另一个"}

// The words that confirm something.
var confirmationWords = []string{"对", "嗯", "没错", "行", "是", "就", "可以", "那就", "就要"}

// CrossSessionIntentOf reads a question and the run's own state.
//
// It reuses the read plan rather than inventing a second reading of the
// question: the plan already knows which words describe HOW the question was
// asked ("last time") and which describe its subject, and a second reader
// would eventually disagree with the first one about a question both of them
// read.
func CrossSessionIntentOf(question string, currentSessionHasHistory bool) CrossSessionIntent {
	// The question names another conversation, or the learner confirmed that
	// one should be looked at.
	//
	// This is read FIRST and from the raw question. The read plan reads a
	// confirmation ("yes, that one") as a question about the present, and a
	// question about the present must not be turned into a search - and a
	// confirmation must not be turned into an ordinary question either.
	if namesAnotherConversation(question) {
		return CrossSessionIntent{Cross: true, Reason: CrossSessionPointer}
	}
	plan := BuildReadPlan(RouteRequest{
		Question: question, CurrentSessionHasHistory: currentSessionHasHistory,
	})
	// The plan's own verdict comes first: a plan that does not need a
	// recall does not need an earlier conversation. That covers the
	// resumption ("carry on from last time" with history here) and the
	// ordinary question that is not about the past at all.
	if !plan.NeedEpisode {
		reason := plan.Reason()
		if reason == "" {
			reason = CrossSessionNoHistory
		}
		return CrossSessionIntent{Reason: reason}
	}
	// A subject, or a name the learner spelled out.
	//
	// A subject on its own is not enough: it says what the question is about,
	// not which conversation holds the answer. It needs a pointer to the
	// past as well - "the three exercises you set me last time" names the
	// exercises AND points at the earlier conversation, and the two together
	// are what makes the answer live there.
	if len(plan.TopicTerms) > 0 || len(plan.ExactIdentifiers) > 0 {
		if plan.TemporalMode != TemporalNone {
			return CrossSessionIntent{Cross: true, Reason: CrossSessionNamedSubject}
		}
	}
	// What is left is a question about the learner's past that names nothing
	// in particular: a bare pointer ("last time", "last week"), or a bare
	// subject. This conversation answers it when it has a history.
	if currentSessionHasHistory {
		return CrossSessionIntent{Reason: CrossSessionCurrentFirst}
	}
	// Nothing here to answer from, so the answer is in an earlier one.
	return CrossSessionIntent{Cross: true, Reason: CrossSessionEmptyPointer}
}

// namesAnotherConversation says whether the question names another
// conversation of this learner, or confirms that one should be looked at.
func namesAnotherConversation(question string) bool {
	question = strings.TrimSpace(question)
	if question == "" {
		return false
	}
	if containsAny(question, crossSessionPointers...) {
		return true
	}
	return crossSessionConfirmation(question)
}

// crossSessionConfirmation says the learner confirmed that another
// conversation should be looked at.
//
// It is a SHORT question that carries a confirmation and points at something
// that is not this conversation. The length and the shape are the point:
// "yes, that one" confirms a choice, and "yes, that plan" is a statement
// about a plan - a statement about a plan is not a request to go and look
// for it, and reading it as one would send the model off through the
// learner's whole history to answer a sentence about a plan.
func crossSessionConfirmation(question string) bool {
	runes := []rune(strings.TrimSpace(question))
	if len(runes) == 0 || len(runes) > 14 {
		return false
	}
	if containsAny(question, crossSessionConfirmations...) {
		return true
	}
	if !containsAny(question, confirmationWords...) {
		return false
	}
	for _, deictic := range confirmationDeictics {
		index := strings.Index(question, deictic)
		if index < 0 {
			continue
		}
		// A bare deictic confirms only when nothing follows it.
		rest := strings.TrimSpace(question[index+len(deictic):])
		if rest == "" || strings.HasPrefix(rest, "，") || strings.HasPrefix(rest, ",") ||
			strings.HasPrefix(rest, "。") || strings.HasPrefix(rest, "？") ||
			strings.HasPrefix(rest, "?") || strings.HasPrefix(rest, "、") {
			return true
		}
	}
	return false
}
