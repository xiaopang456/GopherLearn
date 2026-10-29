package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"GopherAI/common/memoryrecall"
	"GopherAI/model"
	"GopherAI/service/agent/topicconsent"
)

const confirmedTestSecret = "stabilize050-context-test-secret-0123456789"

func userConfirmedContextConfig() ContextConfig {
	config := DefaultContextConfig()
	config.MemoryReadMode = memoryrecall.ModeAgentMemoryUserConfirmedV1
	config.EpisodeRecallMode = EpisodeRecallInject
	return config
}

// recordingLearningState records the keys it was asked for, so a test can prove
// mastery was read for the confirmed topic and for nothing else.
type recordingLearningState struct {
	mu       sync.Mutex
	calls    int
	lastKeys []string
	states   []ConfirmedTopicLearningState
	err      error
}

func (r *recordingLearningState) LearningStateByKeys(_ context.Context, _ RunContext,
	keys []string) ([]ConfirmedTopicLearningState, error) {
	r.mu.Lock()
	r.calls++
	r.lastKeys = append([]string(nil), keys...)
	r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	return r.states, nil
}

func (r *recordingLearningState) keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lastKeys...)
}

func (r *recordingLearningState) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type confirmedFixture struct {
	builder   *DatabaseContextBuilder
	bootstrap *recordingTopicBootstrap
	learning  *recordingLearningState
	provider  *recordingMemoryProvider
	signer    *topicconsent.Signer
}

// newConfirmedFixture wires the mode the way production does, with no active
// learning session unless one is passed in.
func newConfirmedFixture(t *testing.T, active *model.LearningSession) confirmedFixture {
	t.Helper()
	learningDAO := &scopedActiveLearning{session: active}
	mastery := newTestMasteryProvider(t, &stubProgressReader{}, learningDAO)
	bootstrap := &recordingTopicBootstrap{records: []TopicMemoryRecord{
		topicRecord("m-1", "jwt-鉴权", "签名", "签名只防篡改，不隐藏 Payload", 1730000000, 11),
	}}
	state := &recordingLearningState{states: []ConfirmedTopicLearningState{{
		TopicKey: "jwt-鉴权", DisplayName: "JWT 鉴权",
		MasteryInstruction: "已入门，可以直接讲进阶细节",
		WeakPoints:         []string{"刷新令牌轮换"},
	}}}
	provider := &recordingMemoryProvider{
		globalMemory:  []string{"讲解偏好：先说结论"},
		topicMemory:   []string{"这条不该出现"},
		episodeMemory: []model.EpisodeMemory{{ID: "e-1", Title: "上次聊过登录", LastMessageAt: masteryTestClock}},
	}
	builder, err := NewContextBuilder(userConfirmedContextConfig(), stubHistory{}, nil, mastery, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseEpisodeProvider(provider)
	builder.UseTopicKeyResolver(provider)
	builder.UsePlannedMemoryProvider(provider)
	builder.UseGlobalMemoryProvider(provider)
	builder.UseAuthoritativeTopicMemoryProvider(bootstrap)
	builder.UseConfirmedTopicLearningStateProvider(state)

	signer, err := topicconsent.NewSigner(confirmedTestSecret, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	signer.UseClock(func() time.Time { return masteryTestClock })
	builder.UseTopicConsentVerifier(signer)
	return confirmedFixture{builder: builder, bootstrap: bootstrap, learning: state, provider: provider, signer: signer}
}

func confirmedCandidates() []topicconsent.Candidate {
	return []topicconsent.Candidate{
		{TopicKey: "jwt-鉴权", DisplayName: "JWT 鉴权", Source: topicconsent.SourceGradedTopic},
		{TopicKey: "session-状态机", DisplayName: "Session 状态机", Source: topicconsent.SourceRecentNotes},
	}
}

func buildConfirmed(t *testing.T, fixture confirmedFixture, consent TopicConsentSubmission) ContextWindow {
	t.Helper()
	window, err := fixture.builder.Build(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", RequestID: "req-1", Question: "登录态怎么校验？",
		EpisodeRecallReady: true, MemoryEvidence: NewMemoryEvidenceRegistry(),
		TopicConsent: consent,
	})
	if err != nil {
		t.Fatal(err)
	}
	return window
}

// A live learning session proves which quiz exists, not that an arbitrary chat
// question is about it. It must not become a blanket personal-state grant.
func TestActiveLearningSessionDoesNotReadWithoutATicket(t *testing.T) {
	active := learningSession(11, "s-1", "JWT 鉴权", model.LearningSessionWaitingAnswer,
		masteryTestClock.Add(time.Hour))
	fixture := newConfirmedFixture(t, active)
	window := buildConfirmed(t, fixture, TopicConsentSubmission{})

	assembled := renderWindow(window)
	if strings.Contains(assembled, "不隐藏 Payload") || strings.Contains(assembled, "已入门") {
		t.Fatalf("an active quiz granted topic personal state to ordinary chat: %s", assembled)
	}
	if fixture.bootstrap.calls.Load() != 0 || fixture.learning.callCount() != 0 {
		t.Fatal("an active quiz reached a personal reader without confirmation")
	}
	if len(window.UsedTopicKeys) != 0 {
		t.Fatalf("used topics = %v", window.UsedTopicKeys)
	}
}

type forbiddenMatchedMastery struct {
	mu    sync.Mutex
	calls int
}

func (m *forbiddenMatchedMastery) CurrentMastery(context.Context, RunContext) (string, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return "MATCHED MASTERY MUST NOT APPEAR", nil
}

func (m *forbiddenMatchedMastery) CurrentMasteryWithTopicSources(context.Context, RunContext) (string, MasteryTopics, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return "MATCHED MASTERY MUST NOT APPEAR", MasteryTopics{
		Matched: []string{"jwt-鉴权"}, Authoritative: []string{"rpc-粘包"},
	}, nil
}

func (m *forbiddenMatchedMastery) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// The older mastery matcher used to run before consent verification. That was
// a second, unconfirmed door into the same learning state and could also render
// one state twice after confirmation.
func TestConfirmedModeDoesNotRunTheLegacyMasteryMatcher(t *testing.T) {
	mastery := &forbiddenMatchedMastery{}
	provider := &recordingMemoryProvider{globalMemory: []string{"讲解偏好：先说结论"}}
	builder, err := NewContextBuilder(userConfirmedContextConfig(), stubHistory{}, nil, mastery, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(provider)
	builder.UseAuthoritativeTopicMemoryProvider(&recordingTopicBootstrap{})
	builder.UseConfirmedTopicLearningStateProvider(&recordingLearningState{})

	window, err := builder.Build(context.Background(), RunContext{
		UserID: 11, SessionID: "s-1", RequestID: "req-1", Question: "JWT 怎么校验？",
		EpisodeRecallReady: true, MemoryEvidence: NewMemoryEvidenceRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if mastery.callCount() != 0 {
		t.Fatalf("legacy mastery matcher calls = %d", mastery.callCount())
	}
	if strings.Contains(renderWindow(window), "MATCHED MASTERY MUST NOT APPEAR") {
		t.Fatal("unconfirmed mastery reached the prompt")
	}
}

func TestConfirmedSelectionOverridesRatherThanCombinesWithAnActiveQuiz(t *testing.T) {
	active := learningSession(11, "s-1", "RPC 粘包", model.LearningSessionWaitingAnswer,
		masteryTestClock.Add(time.Hour))
	fixture := newConfirmedFixture(t, active)
	ticket, _, err := fixture.signer.Issue(11, "s-1", "req-1", "登录态怎么校验？", confirmedCandidates())
	if err != nil {
		t.Fatal(err)
	}
	window := buildConfirmed(t, fixture, TopicConsentSubmission{
		Ticket: ticket, SelectedTopicKeys: []string{"jwt-鉴权"},
	})
	if got := fixture.bootstrap.keys(); len(got) != 1 || got[0] != "jwt-鉴权" {
		t.Fatalf("topic memory read %v, want only confirmed jwt-鉴权", got)
	}
	if got := fixture.learning.keys(); len(got) != 1 || got[0] != "jwt-鉴权" {
		t.Fatalf("learning state read %v, want only confirmed jwt-鉴权", got)
	}
	if len(window.UsedTopicKeys) != 1 || window.UsedTopicKeys[0] != "jwt-鉴权" {
		t.Fatalf("used topics = %v", window.UsedTopicKeys)
	}
}

// Path 2 without a confirmation: nothing personal about any topic is read, and
// no substitute topic is chosen.
func TestOrdinaryChatWithoutATicketReadsNoTopicState(t *testing.T) {
	fixture := newConfirmedFixture(t, nil)
	window := buildConfirmed(t, fixture, TopicConsentSubmission{})

	assembled := renderWindow(window)
	if strings.Contains(assembled, "不隐藏 Payload") || strings.Contains(assembled, "已入门") {
		t.Fatalf("topic personal state reached an unconfirmed turn: %s", assembled)
	}
	if strings.Contains(assembled, "这条不该出现") {
		t.Fatalf("an older topic reader ran: %s", assembled)
	}
	if fixture.bootstrap.calls.Load() != 0 {
		t.Fatal("topic memory was read without a confirmation")
	}
	if fixture.learning.callCount() != 0 {
		t.Fatal("mastery was read without a confirmation")
	}
	if len(window.UsedTopicKeys) != 0 {
		t.Fatalf("used topics = %v", window.UsedTopicKeys)
	}
	// Global personalisation is unchanged: it was never topic-scoped.
	if !strings.Contains(assembled, "先说结论") {
		t.Fatalf("global memory must still be injected: %s", assembled)
	}
}

// Path 2 with a confirmation: exactly the chosen topic is read, both halves,
// merged into one block with their provenances kept apart.
func TestConfirmedTopicReadsBothHalvesAndOnlyTheChosenKey(t *testing.T) {
	fixture := newConfirmedFixture(t, nil)
	ticket, _, err := fixture.signer.Issue(11, "s-1", "req-1", "登录态怎么校验？", confirmedCandidates())
	if err != nil {
		t.Fatal(err)
	}
	window := buildConfirmed(t, fixture, TopicConsentSubmission{
		Ticket: ticket, SelectedTopicKeys: []string{"jwt-鉴权"}})

	assembled := renderWindow(window)
	if !strings.Contains(assembled, "不隐藏 Payload") {
		t.Fatalf("the confirmed notes were not injected: %s", assembled)
	}
	if !strings.Contains(assembled, "已入门") || !strings.Contains(assembled, "刷新令牌轮换") {
		t.Fatalf("the confirmed learning state was not injected: %s", assembled)
	}
	// One block, two labelled halves. Only the notes are citable.
	if !strings.Contains(assembled, confirmedTopicHeader) ||
		!strings.Contains(assembled, learningStateHeader) || !strings.Contains(assembled, confirmedNotesHeader) {
		t.Fatalf("the merged block lost a provenance label: %s", assembled)
	}
	if got := fixture.bootstrap.keys(); len(got) != 1 || got[0] != "jwt-鉴权" {
		t.Fatalf("topic memory read %v", got)
	}
	if got := fixture.learning.keys(); len(got) != 1 || got[0] != "jwt-鉴权" {
		t.Fatalf("learning state read %v", got)
	}
	if len(window.UsedTopicKeys) != 1 || window.UsedTopicKeys[0] != "jwt-鉴权" {
		t.Fatalf("used topics = %v", window.UsedTopicKeys)
	}
	// The reference binds to the real record, and the mastery half never gets
	// one: an answer must not be able to cite the server's assessment as though
	// the learner had said it.
	if len(window.PreinjectedMemory) != 1 || window.PreinjectedMemory[0].ID != "m-1" {
		t.Fatalf("preinjected = %+v", window.PreinjectedMemory)
	}
}

func TestConfirmingNothingAnswersWithoutTopicPersonalisation(t *testing.T) {
	fixture := newConfirmedFixture(t, nil)
	ticket, _, err := fixture.signer.Issue(11, "s-1", "req-1", "登录态怎么校验？", confirmedCandidates())
	if err != nil {
		t.Fatal(err)
	}
	window := buildConfirmed(t, fixture, TopicConsentSubmission{Ticket: ticket})

	if fixture.bootstrap.calls.Load() != 0 || fixture.learning.callCount() != 0 {
		t.Fatal("selecting no topic must read nothing")
	}
	if len(window.UsedTopicKeys) != 0 {
		t.Fatalf("used topics = %v", window.UsedTopicKeys)
	}
	if !strings.Contains(renderWindow(window), "先说结论") {
		t.Fatal("the turn must still answer normally")
	}
}

// Every rejection class must be refused before any personal read happens, and
// must degrade to "no topic personalisation" rather than to a guess.
func TestRejectedConsentNeverReachesAPersonalRead(t *testing.T) {
	valid, _, err := newConfirmedFixture(t, nil).signer.Issue(11, "s-1", "req-1", "登录态怎么校验？", confirmedCandidates())
	if err != nil {
		t.Fatal(err)
	}
	other, err := topicconsent.NewSigner("an-entirely-different-secret-0123456789012", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	other.UseClock(func() time.Time { return masteryTestClock })
	forged, _, err := other.Issue(11, "s-1", "req-1", "登录态怎么校验？", confirmedCandidates())
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]TopicConsentSubmission{
		"forged signature": {Ticket: forged, SelectedTopicKeys: []string{"jwt-鉴权"}},
		"tampered ticket":  {Ticket: valid + "x", SelectedTopicKeys: []string{"jwt-鉴权"}},
		"not a candidate":  {Ticket: valid, SelectedTopicKeys: []string{"redis-分布式锁"}},
		"three topics":     {Ticket: valid, SelectedTopicKeys: []string{"jwt-鉴权", "session-状态机", "go-并发"}},
		"no ticket at all": {SelectedTopicKeys: []string{"jwt-鉴权"}},
	}
	for name, consent := range cases {
		fixture := newConfirmedFixture(t, nil)
		window := buildConfirmed(t, fixture, consent)
		if fixture.bootstrap.calls.Load() != 0 || fixture.learning.callCount() != 0 {
			t.Fatalf("%s: a personal read happened", name)
		}
		if len(window.UsedTopicKeys) != 0 {
			t.Fatalf("%s: used topics = %v", name, window.UsedTopicKeys)
		}
		if strings.Contains(renderWindow(window), "不隐藏 Payload") {
			t.Fatalf("%s: topic memory reached the window", name)
		}
	}
}

func TestCrossUserSessionRequestAndQuestionTicketsAreRefused(t *testing.T) {
	fixture := newConfirmedFixture(t, nil)
	ticket, _, err := fixture.signer.Issue(11, "s-1", "req-1", "登录态怎么校验？", confirmedCandidates())
	if err != nil {
		t.Fatal(err)
	}
	consent := TopicConsentSubmission{Ticket: ticket, SelectedTopicKeys: []string{"jwt-鉴权"}}
	for name, run := range map[string]RunContext{
		"cross user":     {UserID: 12, SessionID: "s-1", RequestID: "req-1", Question: "登录态怎么校验？"},
		"cross session":  {UserID: 11, SessionID: "s-2", RequestID: "req-1", Question: "登录态怎么校验？"},
		"cross request":  {UserID: 11, SessionID: "s-1", RequestID: "req-2", Question: "登录态怎么校验？"},
		"cross question": {UserID: 11, SessionID: "s-1", RequestID: "req-1", Question: "粘包怎么处理？"},
	} {
		local := newConfirmedFixture(t, nil)
		run.TopicConsent = consent
		run.EpisodeRecallReady = true
		run.MemoryEvidence = NewMemoryEvidenceRegistry()
		if _, err := local.builder.Build(context.Background(), run); err != nil {
			t.Fatalf("%s: build failed: %v", name, err)
		}
		if local.bootstrap.calls.Load() != 0 || local.learning.callCount() != 0 {
			t.Fatalf("%s: a personal read happened", name)
		}
	}
}

// The same request may be retried - the run path replays on request id - so the
// consent must survive a second identical build.
func TestSameRequestIDCanReuseTheSameConfirmation(t *testing.T) {
	fixture := newConfirmedFixture(t, nil)
	ticket, _, err := fixture.signer.Issue(11, "s-1", "req-1", "登录态怎么校验？", confirmedCandidates())
	if err != nil {
		t.Fatal(err)
	}
	consent := TopicConsentSubmission{Ticket: ticket, SelectedTopicKeys: []string{"jwt-鉴权"}}
	for attempt := 0; attempt < 2; attempt++ {
		window := buildConfirmed(t, fixture, consent)
		if len(window.UsedTopicKeys) != 1 {
			t.Fatalf("attempt %d: used topics = %v", attempt, window.UsedTopicKeys)
		}
	}
}

// A failing topic read costs personalisation, never the answer, and never
// substitutes a different topic.
func TestTopicReadFailureKeepsTheAnswerAndSubstitutesNothing(t *testing.T) {
	fixture := newConfirmedFixture(t, nil)
	fixture.bootstrap.err = errors.New("mysql unavailable")
	fixture.learning.err = errors.New("progress unavailable")
	ticket, _, err := fixture.signer.Issue(11, "s-1", "req-1", "登录态怎么校验？", confirmedCandidates())
	if err != nil {
		t.Fatal(err)
	}
	window := buildConfirmed(t, fixture, TopicConsentSubmission{
		Ticket: ticket, SelectedTopicKeys: []string{"jwt-鉴权"}})

	assembled := renderWindow(window)
	if strings.Contains(assembled, "这条不该出现") {
		t.Fatalf("a similar-topic fallback ran: %s", assembled)
	}
	if len(window.UsedTopicKeys) != 0 {
		t.Fatalf("a failed read must not be reported as used: %v", window.UsedTopicKeys)
	}
	if !strings.Contains(assembled, "先说结论") {
		t.Fatalf("the turn lost its answer context: %s", assembled)
	}
}

func TestConfirmedModeUsesItsOwnSystemPrompt(t *testing.T) {
	prompt := SystemPromptFor(memoryrecall.ModeAgentMemoryUserConfirmedV1)
	if prompt == SystemPromptFor(memoryrecall.ModeAgentMemoryToolV2) {
		t.Fatal("the confirmed mode must not inherit v2's prompt")
	}
	if !strings.Contains(prompt, "confirmed topic block") {
		t.Fatalf("the prompt must describe the confirmed block: %s", prompt)
	}
	if !strings.Contains(prompt, "past sessions only") {
		t.Fatal("the prompt must say memory_search is episodic here")
	}
}

// Modes that are not the confirmed one must not gain a consent path, and the
// default must stay legacy_v2.
func TestOtherModesAreUnchanged(t *testing.T) {
	if memoryrecall.DefaultReadMode() != memoryrecall.ModeLegacyV2 {
		t.Fatalf("default = %s", memoryrecall.DefaultReadMode())
	}
	for _, mode := range memoryrecall.ModeNames() {
		// The v1.1 assertion identity is the confirmed mode plus assertion
		// governance, so it carries the same two consent properties by
		// construction. Every mode measured before it must not.
		// STABILIZE-061's identity is v1.1 with the two memory tools renamed,
		// so it carries them for the same reason. Renaming a tool must not be
		// able to loosen the consent chain, which is what this list checks by
		// requiring the property to be declared rather than inferred.
		confirmed := mode == memoryrecall.ModeAgentMemoryUserConfirmedV1 ||
			mode == memoryrecall.ModeEpisodeAssertionGovernedV11 ||
			mode == memoryrecall.ModeAgentMemoryExplicitActionsV1 ||
			mode == memoryrecall.ModeAgentMemoryExplicitSourcesV1
		if mode.UsesUserConfirmedTopicContext() != confirmed {
			t.Fatalf("%s: UsesUserConfirmedTopicContext = %v", mode, !confirmed)
		}
		if mode.RestrictsMemorySearchToEpisodic() != confirmed {
			t.Fatalf("%s: RestrictsMemorySearchToEpisodic = %v", mode, !confirmed)
		}
		// v2's published measurements describe a mode that bootstraps the
		// learning session's topic; that must not have moved.
		if mode.UsesAuthoritativeTopicBootstrap() != (mode == memoryrecall.ModeAgentMemoryToolV2) {
			t.Fatalf("%s: UsesAuthoritativeTopicBootstrap moved", mode)
		}
	}
	if !memoryrecall.ModeAgentMemoryUserConfirmedV1.UsesMemorySearchTool() {
		t.Fatal("episodes must stay reachable in the confirmed mode")
	}
	if memoryrecall.ModeAgentMemoryUserConfirmedV1.UsesTopicMemoryReadTool() {
		t.Fatal("the confirmed mode must not register topic_memory_read")
	}
}
