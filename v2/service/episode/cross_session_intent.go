package episode

// The cross-session intent.
//
// It is one question, asked before the tools are: is this turn about a
// conversation other than the one being answered?
//
// The answer decides whether the model may go looking at an earlier
// conversation at all. The rule is the one the learner would expect:
//
//   - A question that names something is about that something, wherever it
//     was said. "the three exercises you set me last time" names the
//     exercises, so the earlier conversation is the answer's home.
//   - A question that names a period is about that period. "last week" is a
//     different week from this one, whatever this conversation holds.
//   - A question that names NOTHING but a pointer - "last time", "last
//     week" - is about the conversation in front of the learner, if that
//     conversation has a history to point back to. The current conversation
//     is the first place to look, and looking elsewhere before looking here
//     is the wrong order.
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
	// CrossSessionNamedSubject means the question named something, so the
	// earlier conversation is where the answer is.
	CrossSessionNamedSubject = "named_subject"
	// CrossSessionTimeRange means the question named a period, which is
	// another conversation by definition.
	CrossSessionTimeRange = "time_range"
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

// CrossSessionIntentOf reads a question and the run's own state.
//
// It reuses the read plan rather than inventing a second reading of the
// question: the plan already knows which words describe HOW the question was
// asked ("last time") and which describe its subject, and a second reader
// would eventually disagree with the first one about a question both of them
// read.
func CrossSessionIntentOf(question string, currentSessionHasHistory bool) CrossSessionIntent {
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
	if len(plan.TopicTerms) > 0 || len(plan.ExactIdentifiers) > 0 {
		return CrossSessionIntent{Cross: true, Reason: CrossSessionNamedSubject}
	}
	// A period is another conversation by definition.
	if plan.TemporalMode == TemporalRange {
		return CrossSessionIntent{Cross: true, Reason: CrossSessionTimeRange}
	}
	// A bare pointer. This conversation answers it if it has a history.
	if currentSessionHasHistory {
		return CrossSessionIntent{Reason: CrossSessionCurrentFirst}
	}
	return CrossSessionIntent{Cross: true, Reason: CrossSessionEmptyPointer}
}
