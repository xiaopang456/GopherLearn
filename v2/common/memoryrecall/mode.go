package memoryrecall

import "fmt"

// ReadMode selects the whole long-term memory read path, not one component of
// it.
//
// STABILIZE-031 makes the unit of choice a complete mode because STABILIZE-030
// introduced three coupled things at once — the multi-axis ReadPlan and Episode
// recall, the TopicResolver's early resolution whose keys feed the Episode
// exact channel, and the semantic layer. A switch on only the last of them
// would leave the first two on by default and would leave the middle one
// unmeasured, which is exactly the state the ablation exposed.
//
// The first four modes are strictly nested: each adds exactly one thing to the
// one above it. That nesting is what makes a four-arm comparison attributable.
//
// STABILIZE-032 adds a fifth that is not part of that nesting. model_planned_v1
// is a complete alternative read path, not another layer on the v3 stack: it
// runs no Intent router, no TopicResolver and no v3 Episode plan, because
// stacking it on top of them would leave the old trigger rules deciding what it
// ever gets to see, and would make its results unattributable.
type ReadMode string

const (
	// ModeLegacyV2 is the complete pre-STABILIZE-030 read path: a single
	// mutually exclusive Intent router, the old Episode recall, no
	// TopicResolver and no request-scoped vector sharing.
	//
	// It is the production default, and stays the default until a brand-new
	// blind set says some other mode should replace it. A development gate that
	// did not pass is not a reason to ship anything.
	ModeLegacyV2 ReadMode = "legacy_v2"
	// ModeEpisodeV3 adds the multi-axis ReadPlan and the Episode recall
	// changes. The context builder attaches no TopicKeyResolver at all, so
	// topic memory still takes the v2 "match by name after recall" path.
	ModeEpisodeV3 ReadMode = "episode_v3"
	// ModeEpisodeV3Lexical adds the resolver's authoritative and lexical layers,
	// resolved before Episode recall so their keys can reach the exact channel.
	// The semantic layer is not constructed.
	ModeEpisodeV3Lexical ReadMode = "episode_v3_lexical"
	// ModeEpisodeV3Semantic adds the semantic layer, including its safety
	// revisions.
	ModeEpisodeV3Semantic ReadMode = "episode_v3_semantic"
	// ModeModelPlannedV1 is the STABILIZE-032 read path: a small model plans
	// which of the learner's own topics and episodes the question needs, the
	// server rebuilds and re-checks those candidates from MySQL, and a second
	// model call decides which of them actually support the question.
	//
	// It exists because STABILIZE-031 ended with owner, status, version and
	// source closure all correct and the safety result still worse than the
	// baseline: every added false-premise injection was a record of the
	// learner's own, in force, on the subject, and unsupporting. That is a
	// relation between two texts, and no threshold over word overlap or vector
	// distance decided it correctly in both directions.
	//
	// It is not the default and does not become the default by passing. This
	// task can award it candidacy and nothing more.
	ModeModelPlannedV1 ReadMode = "model_planned_v1"
	// ModeAgentMemoryToolV1 is the STABILIZE-033 read path: nothing but global
	// semantic memory is injected, and topic semantic memory and Episodes are
	// reachable only through a memory_search tool the answering Agent may call
	// inside its existing ReAct loop.
	//
	// It exists because STABILIZE-032 measured both halves of the same
	// sentence. A model deciding what the question needs raised complete recall
	// from 13/39 to 27/39, so the semantic decision is worth making; putting
	// two extra model stages in front of the answer cost a P50 of 5.07s and a
	// no-retry P95 of 19.9s, so it cannot be made there. The Agent is already
	// reading the question and already choosing tools, so this mode asks it to
	// decide one more thing rather than adding a stage that decides nothing
	// else.
	//
	// Like every mode before it, passing does not make it the default. This
	// task can award it candidacy and nothing more.
	ModeAgentMemoryToolV1 ReadMode = "agent_memory_tool_v1"
	// ModeAgentMemoryToolV2 is the STABILIZE-039 read path: everything
	// agent_memory_tool_v1 does, plus one thing the server already knew and was
	// throwing away.
	//
	// When the learner has an active LearningSession in this chat session, the
	// server holds an authoritative TopicKey before the first model call. v1
	// still made the Agent re-decide that topic, spend an embedding and a
	// hybrid retrieval on it, and let those records compete with Episodes for
	// the per-kind and final window slots — STABILIZE-035/036 measured three of
	// the ten missing evidence groups dying in exactly that final merge.
	//
	// So this mode reads topic semantic memory by exact key straight from
	// MySQL, injects it before the first model call, and issues its S#
	// references there. Episodes are untouched: they are still reachable only
	// through memory_search, because "what happened last time" is the one
	// question a topic note can never answer.
	//
	// It is not the default and passing does not make it one. Like every mode
	// before it, this task can award it candidacy and nothing more.
	ModeAgentMemoryToolV2 ReadMode = "agent_memory_tool_v2"
	// ModeAgentMemoryToolV3 is the STABILIZE-040 read path: v1's window, plus a
	// second tool the Agent may call when the question is about how this
	// learner understands the subject the server has them working on now.
	//
	// It exists because STABILIZE-039C measured both halves of v2's sentence.
	// Reading topic memory by exact key is worth doing - on the eighteen
	// ordinary topic cases it took embeddings from 18 to 1, Redis queries from
	// 48 to 2 and recall from 17/18 to 18/18. Doing it whenever a learning
	// session happens to be open is not: the other sixty-two cases were asking
	// about episodes, mixed subjects, a neighbouring topic or a premise that was
	// never true, and every one of them got the topic block anyway and then went
	// and searched. The saving stayed local; the injection did not.
	//
	// So the exact read stops being a trigger and becomes a choice. The window
	// carries bounded global memory and nothing else - no S# before the first
	// model call under any circumstances - and topic_memory_read reads the
	// authoritative TopicKey the server is already holding, or says it has none.
	// Episodes and any topic the server cannot name authoritatively stay with
	// memory_search, because "what happened last time" and "what do they think
	// about some other subject" are not questions an exact key can answer.
	//
	// What it does not promise is a shorter turn. It buys back a tool round
	// trip on exactly the cases v2 was saving one on, and its claim is about
	// retrieval infrastructure and unrequested context, not first-token latency.
	//
	// It is not the default and passing does not make it one.
	ModeAgentMemoryToolV3 ReadMode = "agent_memory_tool_v3"
	// ModeAgentMemoryToolV4 is the STABILIZE-041 read path. It keeps v3's two
	// tools and budgets, but the narrow read exposes two facts v3 hid: which
	// server-held topic it read and whether its bounded window omitted records.
	// The Agent can therefore continue with memory_search after observing a
	// topic mismatch or an incomplete window instead of treating any successful
	// narrow read as a complete answer.
	//
	// It is an experimental identity, never a production default.
	ModeAgentMemoryToolV4 ReadMode = "agent_memory_tool_v4"
	// ModeAgentMemoryUserConfirmedV1 is the STABILIZE-050 read path: the user,
	// not a model, authorises which topic's personal state this answer may use.
	// A live quiz is not itself a read grant: it proves a TopicKey exists, not
	// that an arbitrary chat question is about that topic. Until a dedicated
	// learning-turn identity exists, this mode requires the same confirmation
	// for every Topic learning-state and Topic-memory read.
	//
	// It exists because STABILIZE-049 measured the ceiling of the alternative.
	// The candidate pool covered the gold Primary 40/40, so recall was never
	// the problem; the small model's final authorisation still landed at 36,
	// 36 and 35 of 40, injecting the wrong topic's personal state on three or
	// four questions every round, with stable failures across unrelated
	// subjects. What it could not do is separate "technical background this
	// answer touches" from "the subject this learner is actually working on",
	// and that is a question about the learner's intent rather than about the
	// text.
	//
	// So the server stops guessing. An active LearningSession still names its
	// topic authoritatively and is read exactly as before. In ordinary chat the
	// server may only *propose* candidates, and nothing of that topic's
	// mastery, weak points, history or notes is read until the user says which
	// candidates this answer may use. Without a confirmation the turn answers
	// with no topic personalisation at all - it never falls back to the nearest
	// topic, the most recent topic or any similar memory.
	//
	// It is the read-side twin of the write-side confirmation the learner
	// already has: they decide what becomes a long-term memory, and now they
	// decide which topic's memory this answer may read.
	//
	// It is not the default and implementing it does not make it one.
	ModeAgentMemoryUserConfirmedV1 ReadMode = "agent_memory_user_confirmed_v1"
	// ModeEpisodeAssertionGovernedV11 is the STABILIZE-058 v1.1 read path.
	//
	// It is everything agent_memory_user_confirmed_v1 does, plus one thing that
	// mode could not express: which of an Episode's assertions still represents
	// the present. memory_search still returns Episodes only, and the topic
	// user-confirmation chain is untouched; what changes is that each returned
	// card is expanded into its slots, each slot carries current / replaced /
	// unknown, and a reference names the slot rather than the card.
	//
	// It is a separate identity rather than a change to the mode above because
	// that mode has published measurements. A predicate that started returning
	// something new for it would silently change what those numbers describe.
	//
	// It is not the default and implementing it does not make it one.
	ModeEpisodeAssertionGovernedV11 ReadMode = "episode_assertion_governed_v1_1"
	// ModeAgentMemoryExplicitActionsV1 is the STABILIZE-061 read path.
	//
	// It changes one thing: what the two memory tools are called and what their
	// descriptions say they can do. The retrieval chain, the embedding model,
	// the candidate ordering, the MySQL re-verification, the topic
	// user-confirmation chain and the draft confirmation chain are the
	// assertion-governed mode's, unchanged and uncopied.
	//
	// It exists because STABILIZE-060 measured a failure with one shape. Twenty
	// negative questions, seventeen correctly not searched, and all three
	// failures in the same layer: every way of saying "remember this" first
	// called memory_search. Nothing about permissions, parameters, indexes or
	// lifecycle was involved - the seven safety counters were zero and no
	// question sent a kinds field. What the model was given was a tool table
	// holding propose_memory and memory_search, two names from the same family,
	// neither of which says that one writes the current message and the other
	// reads earlier ones.
	//
	// So this mode names the actions instead: save_memory_draft may only quote
	// the message in front of it and searches nothing, search_past_sessions may
	// only read earlier sessions and saves nothing. The server still does no
	// natural-language classification of what the learner wants; it supplies
	// two capabilities whose boundaries are legible, constrains their sources
	// and side effects, and fixes their order when both are available. Deciding
	// which are needed stays the Agent's job.
	//
	// If the explicit names do not change the routing, the answer is not more
	// prompt examples. It is that the decision does not belong to a language
	// model at this point in the turn.
	//
	// It is not the default and implementing it does not make it one.
	ModeAgentMemoryExplicitActionsV1 ReadMode = "agent_memory_explicit_actions_v1"
	// ModeAgentMemoryExplicitSourcesV1 is the STABILIZE-062 read path. It keeps
	// the explicit write and Episode-read actions from STABILIZE-061 and gives
	// uploaded-document retrieval an equally source-specific name. No
	// retriever, permission rule, budget or ordering rule changes.
	ModeAgentMemoryExplicitSourcesV1 ReadMode = "agent_memory_explicit_sources_v1"
	// ModeAgentMemorySourcePriorityV1 is the STABILIZE-063 read path. v1.4.3's
	// valid run showed four questions about the learner's own past answered
	// from uploaded documents only: the base prompt's technical-first boundary,
	// the tool description's "technical or note content", and the server's
	// knowledge-first forcing each pushed the same direction, and none of the
	// three is removable alone. This mode keeps the explicit-sources tool table
	// and amends all three at once: the prompt states a source-priority rule,
	// the descriptions name their own sources' priority, and the server hands
	// the non-save first tool choice to the model instead of binding the
	// document tool. Attribution is to the whole contract, never to a name.
	ModeAgentMemorySourcePriorityV1 ReadMode = "agent_memory_source_priority_v1"
	// ModeAgentMemorySourcePriorityV2 is the source-priority contract plus one
	// read-back authority: the Session Summary's own evidence ids.
	//
	// It is a separate identity rather than an edit to the mode above because
	// STABILIZE-063 published numbers describe v1's prompt bytes, tool schema and
	// budgets, and a digest of that prompt is recorded in a run seal. Widening
	// read_history_message there would change what those measurements are about
	// without changing anything measurable about the comparison.
	//
	// What it adds is narrow and it is all server-side. An injected summary is a
	// compression, and its evidence_message_ids are the only ids in it that point
	// at rows that still exist; under this mode those ids become per-message,
	// per-session grants, exactly as an episode's do, and the learner's words
	// behind a summary sentence can be read back instead of paraphrased. Every
	// other mode keeps its existing grant source and nothing else changes: same
	// tool names, same schema, same budgets.
	//
	// It is not the default and implementing it does not make it one.
	ModeAgentMemorySourcePriorityV2 ReadMode = "agent_memory_source_priority_v2"
	// ModeAgentMemorySourcePriorityV3 is the STABILIZE-064 read path: the same
	// read_history_message tool reaching BOTH compressed memories at once.
	//
	// v2 measures one source. The summary's evidence ids become grants, and that
	// is the whole widening - but an Episode card, which cites ids the same way,
	// never reaches this mode's window at all: source-priority identities register
	// memory_search, and AutoInjectsTopicAndEpisodeMemory is !UsesMemorySearchTool,
	// so the builder takes the "global only" branch and fetches no episodes. Under
	// v1 and v2 the episode half of HistoryMessageGrant is therefore dead code, and
	// any claim that the experiment measured "compressed memory read-back" measured
	// one compression, not two.
	//
	// v3 is a separate identity rather than an edit to v2 because v2 has published
	// numbers, a sealed prompt digest and a sealed tool table, and because the
	// change here is not additive to v2's behaviour: it moves the mode out of the
	// memory_search family, which changes what the ContextBuilder retrieves. A
	// predicate that started returning something new for v2 would silently change
	// what those measurements describe.
	//
	// What it holds, precisely: the source-priority tool naming and prompt bytes;
	// the Session Summary injected with its visible evidence ids as summary grants;
	// Episode cards injected by the builder's own auto-injection path (which still
	// requires EpisodeRecallMode=inject_enabled and a ready session-history probe)
	// with each card's evidence ids as episode grants, bounded by the card's own
	// source watermark and by per-id visibility in the rendered block; and
	// read_history_message as the single raw-message reader for both kinds, with the
	// production default call ceiling.
	//
	// What it does not hold: memory_search, and therefore not source_quotes. An
	// episode's bounded quotations stay a separate path with separate authority and
	// are recorded as out of scope for this measurement.
	//
	// It is not the default and implementing it does not make it one.
	ModeAgentMemorySourcePriorityV3 ReadMode = "agent_memory_source_priority_v3"
)

// sourcePriorityFamily reports whether a mode runs the STABILIZE-063 source
// priority contract.
//
// The family is stated once because the whole claim about v2 is that it changes
// exactly one thing. Written out per predicate, the two identities would drift
// the first time someone extended one list and forgot the other, and the drift
// would be attributed to the summary readback the round exists to measure.
func (m ReadMode) sourcePriorityFamily() bool {
	return m == ModeAgentMemorySourcePriorityV1 || m == ModeAgentMemorySourcePriorityV2
}

// The limits of the STABILIZE-050 read-side confirmation live here rather than
// beside its implementation because configuration validation has to enforce
// them, and config is imported by packages the implementation itself imports.
// Putting them in the leaf package the mode already lives in keeps one
// definition instead of two that drift.
const (
	// TopicConsentMinSecretLength refuses a signing key short enough to brute
	// force. The mode fails startup without one rather than falling back to an
	// unsigned ticket.
	TopicConsentMinSecretLength = 32
	// TopicConsentMaxSelectable is the ceiling on how much personal state one
	// answer may draw on. Two, because a genuine two-subject comparison exists
	// and a third has no observed case behind it.
	TopicConsentMaxSelectable = 2
	// TopicConsentMaxCandidates bounds the suggestion list. Beyond a handful
	// the user is being asked to do the classification the server just admitted
	// it cannot do.
	TopicConsentMaxCandidates = 5
)

// DefaultReadMode is the shipped default. It is a function rather than a bare
// constant reference so that "what does production run" has exactly one answer
// in the codebase.
func DefaultReadMode() ReadMode { return ModeLegacyV2 }

func (m ReadMode) Valid() bool {
	switch m {
	case ModeLegacyV2, ModeEpisodeV3, ModeEpisodeV3Lexical, ModeEpisodeV3Semantic, ModeModelPlannedV1,
		ModeAgentMemoryToolV1, ModeAgentMemoryToolV2, ModeAgentMemoryToolV3, ModeAgentMemoryToolV4,
		ModeAgentMemoryUserConfirmedV1, ModeEpisodeAssertionGovernedV11,
		ModeAgentMemoryExplicitActionsV1, ModeAgentMemoryExplicitSourcesV1,
		ModeAgentMemorySourcePriorityV1, ModeAgentMemorySourcePriorityV2,
		ModeAgentMemorySourcePriorityV3:
		return true
	default:
		return false
	}
}

// UsesModelPlanner reports whether this mode constructs the small-model planner
// chain at all.
//
// Under every other mode the answer is no, and the design requires that the
// chain not be constructed rather than constructed and left unused. A client
// that exists gets called eventually, and a client that exists needs a key,
// which is how a mode nobody selected starts reading credentials.
func (m ReadMode) UsesModelPlanner() bool { return m == ModeModelPlannedV1 }

// UsesReadPlan reports whether the mode runs the multi-axis ReadPlan and the v3
// Episode recall.
func (m ReadMode) UsesReadPlan() bool {
	return m != ModeLegacyV2 && m != ModeModelPlannedV1 && !m.UsesMemorySearchTool()
}

// UsesMemorySearchTool reports whether this turn registers memory_search.
//
// Only the two Agent memory-tool modes do. Registering the tool anywhere else
// would give the model a second, differently governed way of reaching long-term
// memory in a mode whose results were measured without it.
func (m ReadMode) UsesMemorySearchTool() bool {
	return m == ModeAgentMemoryToolV1 || m == ModeAgentMemoryToolV2 || m == ModeAgentMemoryToolV3 ||
		m == ModeAgentMemoryToolV4 || m == ModeAgentMemoryUserConfirmedV1 ||
		m == ModeEpisodeAssertionGovernedV11 || m == ModeAgentMemoryExplicitActionsV1 ||
		m == ModeAgentMemoryExplicitSourcesV1 || m.sourcePriorityFamily()
}

// UsesUserConfirmedTopicContext reports whether this turn may read topic
// personal state that a user confirmation authorised.
//
// It is a separate predicate from UsesAuthoritativeTopicBootstrap rather than a
// second case inside it, for the reason every mode predicate here is separate:
// v2's published measurements describe a mode that injects the learning
// session's topic and nothing else, and a predicate that started returning true
// for a new mode would change what those measurements are about.
func (m ReadMode) UsesUserConfirmedTopicContext() bool {
	return m == ModeAgentMemoryUserConfirmedV1 || m == ModeEpisodeAssertionGovernedV11 ||
		m == ModeAgentMemoryExplicitActionsV1 || m == ModeAgentMemoryExplicitSourcesV1 ||
		m.sourcePriorityFamily()
}

// DefersFirstToolChoiceToModel reports whether the first tool of the turn is
// the model's decision rather than the server's.
//
// It exists because "this mode registers memory_search" and "this mode studies
// how a model picks its first tool" were the same condition until
// STABILIZE-056, and they are not the same claim. The four agent_memory_tool
// identities exist to measure exactly that choice: forcing knowledge_search
// ahead of it would answer the question they were built to ask, and would spend
// one of their six slots on a knowledge base a personal question has no use
// for. Their published numbers describe runs where nothing was forced, so this
// stays true for all four, frozen.
//
// agent_memory_user_confirmed_v1 is not one of those studies. The topic a
// question may personalise from is already settled - by the user, before the
// first model call - and its memory_search is restricted to Episodes, so the
// model's first tool choice is not a measurement here, it is just a choice. Let
// it suppress knowledge retrieval and an ordinary technical question can be
// answered with no document evidence at all, which the grounding gate then
// refuses. That is STABILIZE-055 scenarios 1-3.
func (m ReadMode) DefersFirstToolChoiceToModel() bool {
	return m == ModeAgentMemoryToolV1 || m == ModeAgentMemoryToolV2 || m == ModeAgentMemoryToolV3 ||
		m == ModeAgentMemoryToolV4
}

// RestrictsMemorySearchToEpisodic reports whether memory_search may return
// semantic records at all in this mode.
//
// Only the user-confirmed mode says no. Its whole claim is that topic personal
// state arrives only through an authoritative learning session or an explicit
// user confirmation, and a tool that could still fetch topic notes under
// kinds=["semantic"] would be a second, unconfirmed door into the same data -
// which is exactly the claim being made false. Episodes are untouched: "what
// happened last time" is history, not a topic's personal state.
func (m ReadMode) RestrictsMemorySearchToEpisodic() bool {
	return m == ModeAgentMemoryUserConfirmedV1 || m == ModeEpisodeAssertionGovernedV11 ||
		m == ModeAgentMemoryExplicitActionsV1 || m == ModeAgentMemoryExplicitSourcesV1 ||
		m.sourcePriorityFamily()
}

// GovernsAssertionTime reports whether this turn expands Episodes into slots
// and attaches current / replaced / unknown to each.
//
// Only the v1.1 identity does. Every other mode - including the user-confirmed
// mode it extends - keeps returning whole cards with no assertion state, which
// is what their measurements describe.
func (m ReadMode) GovernsAssertionTime() bool {
	return m == ModeEpisodeAssertionGovernedV11 || m == ModeAgentMemoryExplicitActionsV1 ||
		m == ModeAgentMemoryExplicitSourcesV1 || m.sourcePriorityFamily()
}

// UsesExplicitMemoryActionNames reports whether the two memory tools are
// exposed under names that state their capability and exclude each other's.
//
// Only agent_memory_explicit_actions_v1 does, and it is its own predicate for
// the reason every mode predicate here is: the modes above have published
// measurements taken against propose_memory and memory_search, and a predicate
// that renamed their tools the moment a new mode appeared would make those
// numbers describe a tool surface that never existed.
func (m ReadMode) UsesExplicitMemoryActionNames() bool {
	return m == ModeAgentMemoryExplicitActionsV1 || m == ModeAgentMemoryExplicitSourcesV1 ||
		m.sourcePriorityFamily() || m == ModeAgentMemorySourcePriorityV3
}

// UsesExplicitKnowledgeSourceName reports whether uploaded-document retrieval
// is named by its source instead of by the overloaded word "knowledge".
func (m ReadMode) UsesExplicitKnowledgeSourceName() bool {
	return m == ModeAgentMemoryExplicitSourcesV1 || m.sourcePriorityFamily() ||
		m == ModeAgentMemorySourcePriorityV3
}

// UsesSourcePrioritySurface reports whether the mode names its tools under the
// STABILIZE-063 source-priority surface.
//
// It is exported because anything that wires a mode's tools - production, or an
// offline evaluator that must not invent its own contract - has to answer this
// question, and the answer covers both source-priority identities. Naming the
// modes one by one at each call site is how a new identity ends up with v1's tool
// table and v2's prompt.
func (m ReadMode) UsesSourcePrioritySurface() bool {
	return m.sourcePriorityFamily() || m == ModeAgentMemorySourcePriorityV3
}

// OrdersMemoryDraftBeforeMemoryRead reports whether the draft tool, when its
// own permission gate has exposed it, must be the turn's first tool.
//
// It is separate from the predicate above because renaming and ordering are two
// claims, and a round that changed both without being able to report them apart
// could not say which one moved the result.
func (m ReadMode) OrdersMemoryDraftBeforeMemoryRead() bool {
	return m == ModeAgentMemoryExplicitActionsV1 || m == ModeAgentMemoryExplicitSourcesV1 ||
		m.sourcePriorityFamily() || m == ModeAgentMemorySourcePriorityV3
}

// UsesSummaryHistoryRead reports whether the Session Summary's own evidence ids
// may become read-back authorisations for read_history_message.
//
// Only the v2 source-priority identity does, and it is a predicate rather than a
// change inside the shared tool because every mode below it has measurements
// taken when an episode was the only card that could cite anything. The tool is
// one implementation for all of them; what widens is the grant set this mode
// hands it, so an older mode cannot read a summary's ids merely because a newer
// mode can.
func (m ReadMode) UsesSummaryHistoryRead() bool { return m == ModeAgentMemorySourcePriorityV2 }

// ReadsEvidenceBack reports whether this mode's window may turn a compressed
// card's evidence ids into read_history_message authorisations.
//
// It is exactly v2 union v3, and it exists as its own predicate rather than an
// edit to UsesSummaryHistoryRead because that one names what v2's published
// measurements were taken under: a summary as the only widening source. v3 adds
// the second compression, and anything that asks "may this mode read evidence
// back at all" - the grant assembly and the per-capability call ceiling - has to
// mean both identities while neither predicate changes answer for the other.
func (m ReadMode) ReadsEvidenceBack() bool {
	return m.UsesSummaryHistoryRead() || m == ModeAgentMemorySourcePriorityV3
}

// UsesEpisodeEvidenceRead reports whether this mode narrows an Episode card's
// citations to the ids the rendered block actually shows, and bounds them by the
// card's own source watermark.
//
// v3 alone. The older injecting modes keep the grant set their published artefacts
// were taken under - every id stored on the card, unbounded by the watermark - and
// that is stated here as a predicate rather than discovered by reading two call
// sites. The narrowing is not a cosmetic tightening: it removes ids from a card
// whose user_corrections group renderEpisode never prints, which changes what a
// read of the learner's raw words may be authorised by. A mode whose numbers are
// already published does not get that change retroactively.
func (m ReadMode) UsesEpisodeEvidenceRead() bool {
	return m == ModeAgentMemorySourcePriorityV3
}

// UsesTopicMemoryReadTool reports whether this turn registers
// topic_memory_read.
//
// Only agent_memory_tool_v3 does, and it is a third predicate rather than a
// clause inside either of the two above for the reason the second one was
// separated from the first: v1 and v2 have published measurements, and a
// predicate that changed what either of them registers the moment a new mode
// was added would make those measurements describe a tool surface that no
// longer exists.
func (m ReadMode) UsesTopicMemoryReadTool() bool {
	return m == ModeAgentMemoryToolV3 || m == ModeAgentMemoryToolV4
}

// UsesTopicMemoryReadObservationV4 distinguishes the new observation contract
// from v3's frozen schema while both continue to expose the same tool name.
func (m ReadMode) UsesTopicMemoryReadObservationV4() bool { return m == ModeAgentMemoryToolV4 }

// SharesIssuedRecordsAcrossMemoryTools reports whether the two memory tools of
// this mode must refuse to hand the same record over twice.
//
// It is true exactly when both tools are registered. With one tool there is
// nothing to share, and turning it on for v1 or v2 would change what a second
// memory_search may return in a mode whose results were measured without it.
func (m ReadMode) SharesIssuedRecordsAcrossMemoryTools() bool {
	return m.UsesMemorySearchTool() && m.UsesTopicMemoryReadTool()
}

// UsesAuthoritativeTopicBootstrap reports whether the ContextBuilder reads topic
// semantic memory by exact key before the first model call.
//
// Only agent_memory_tool_v2 does. It is a separate predicate from
// UsesMemorySearchTool because the two are independent claims: v1 registers the
// tool and injects nothing but global memory, and that is the arrangement its
// measurements were taken under. A single predicate covering both would change
// v1 the moment v2 was added, which is the one thing a new experimental mode
// must never do.
// v3 answers no. It reads exactly the same records by exactly the same key, and
// it does so only when the Agent asks - which is the entire difference between
// the two modes and the only thing STABILIZE-040 is testing.
func (m ReadMode) UsesAuthoritativeTopicBootstrap() bool { return m == ModeAgentMemoryToolV2 }

// AutoInjectsTopicAndEpisodeMemory reports whether the ContextBuilder puts
// topic semantic memory and Episodes into the window on its own.
//
// Every mode but one does, and the one that does not is the whole point of
// STABILIZE-033: those two blocks arrive only if the Agent asks for them, so
// that "was this evidence needed" is a decision with a record rather than a
// side effect of a trigger rule.
// v2 answers no as well, and for the same reason plus one more. It does inject
// topic semantic memory, but not on its own: it injects exactly the topic the
// server was already told the learner is working on, and Episodes still arrive
// only if the Agent asks. "The builder decided this was relevant" is precisely
// what neither mode does.
// v3 answers no in the strongest sense of the three: it injects no topic memory
// at all. The exact read is still available, but only through a tool, so the
// window it hands the first model call is v1's window exactly.
func (m ReadMode) AutoInjectsTopicAndEpisodeMemory() bool { return !m.UsesMemorySearchTool() }

// UsesTopicResolver reports whether a TopicResolver is attached at all. Under
// the two lower modes none is constructed, so none can be called.
func (m ReadMode) UsesTopicResolver() bool {
	return m == ModeEpisodeV3Lexical || m == ModeEpisodeV3Semantic
}

// UsesSemanticTopicLayer reports whether the resolver may propose keys from the
// vector and BM25 channels.
func (m ReadMode) UsesSemanticTopicLayer() bool { return m == ModeEpisodeV3Semantic }

// UsesRecallCycle reports whether a request-scoped Cycle has any consumer. The
// legacy mode has none, and the design requires that it not exist there rather
// than exist unused.
// UsesRecallCycle reports whether a request-scoped Cycle has any consumer.
//
// legacy_v2 has none, and model_planned_v1 has none either: its embedding, when
// it makes one, is for the planner's own query rather than for the question, so
// a cycle keyed on the question would either be bypassed or, worse, hand it a
// vector for text it did not ask about.
func (m ReadMode) UsesRecallCycle() bool { return m.UsesReadPlan() }

// ModeNames lists the modes in nesting order, for reports and error messages.
func ModeNames() []ReadMode {
	return []ReadMode{ModeLegacyV2, ModeEpisodeV3, ModeEpisodeV3Lexical, ModeEpisodeV3Semantic,
		ModeModelPlannedV1, ModeAgentMemoryToolV1, ModeAgentMemoryToolV2, ModeAgentMemoryToolV3,
		ModeAgentMemoryToolV4, ModeAgentMemoryUserConfirmedV1, ModeEpisodeAssertionGovernedV11,
		ModeAgentMemoryExplicitActionsV1, ModeAgentMemoryExplicitSourcesV1}
}

// ParseReadMode fails closed. An unknown value must stop startup rather than
// fall back to something: a silent fallback is how a mode nobody chose ends up
// running in production.
func ParseReadMode(value string) (ReadMode, error) {
	mode := ReadMode(value)
	if !mode.Valid() {
		return "", fmt.Errorf("memory read mode %q must be one of legacy_v2, episode_v3, episode_v3_lexical, episode_v3_semantic, model_planned_v1, agent_memory_tool_v1, agent_memory_tool_v2, agent_memory_tool_v3, agent_memory_tool_v4, agent_memory_user_confirmed_v1, episode_assertion_governed_v1_1, agent_memory_explicit_actions_v1, agent_memory_explicit_sources_v1, agent_memory_source_priority_v1, agent_memory_source_priority_v2, agent_memory_source_priority_v3", value)
	}
	return mode, nil
}
