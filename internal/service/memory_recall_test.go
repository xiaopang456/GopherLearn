package service

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// fixedGrant is the test stand-in for the server-side authorization decision. It exists
// because the production default deliberately grants no topics: a test that wants to see
// topic memory has to say who authorized it, which is the whole point of the seam.
type fixedGrant struct {
	grant MemoryGrant
	err   error
}

func (f fixedGrant) AuthorizeMemoryScope(context.Context, string, string) (MemoryGrant, error) {
	if f.err != nil {
		return MemoryGrant{}, f.err
	}
	return f.grant, nil
}

// recallFixture owns one store with two accounts, each with a session and a message, so a
// test can name real provenance instead of inventing ids.
type recallFixture struct {
	service      *MemoryService
	store        repository.Repository
	userID       string
	sessionID    string
	messageID    string
	otherUser    string
	otherSess    string
	otherMessage string
	secondSess   string
	secondMsg    string
}

func newRecallFixture(t *testing.T) recallFixture {
	t.Helper()
	ctx := t.Context()
	store := repository.NewInMemoryRepository()
	owner, err := store.CreateUser(ctx, &domain.User{Email: "owner@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := recallFixture{service: NewMemoryService(store), store: store, userID: owner.ID}
	for _, slot := range []*struct{ session, message *string }{
		{&fixture.sessionID, &fixture.messageID},
		{&fixture.secondSess, &fixture.secondMsg},
	} {
		session, err := store.CreateSession(ctx, &domain.Session{UserID: owner.ID, Title: "会话", Mode: domain.SessionModeChat})
		if err != nil {
			t.Fatal(err)
		}
		message, err := store.AppendMessage(ctx, owner.ID, session.ID, &domain.Message{Role: "user", Content: "AOF 的 fsync 策略"})
		if err != nil {
			t.Fatal(err)
		}
		*slot.session, *slot.message = session.ID, message.ID
	}
	intruder, err := store.CreateUser(ctx, &domain.User{Email: "intruder@example.com", DisplayName: "Intruder"})
	if err != nil {
		t.Fatal(err)
	}
	intruderSession, err := store.CreateSession(ctx, &domain.Session{UserID: intruder.ID, Title: "别人的会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	intruderMessage, err := store.AppendMessage(ctx, intruder.ID, intruderSession.ID, &domain.Message{Role: "user", Content: "别人的 fsync 话"})
	if err != nil {
		t.Fatal(err)
	}
	fixture.otherUser, fixture.otherSess, fixture.otherMessage = intruder.ID, intruderSession.ID, intruderMessage.ID
	return fixture
}

// file stores one memory of the owner's with provenance that really exists, and confirms
// it when asked. Provenance comes from the arguments rather than the shape so no test can
// file a memory citing a session it did not intend.
func (f recallFixture) file(t *testing.T, sessionID, messageID string, shape domain.MemoryDraft, confirm bool) *domain.Memory {
	t.Helper()
	draft := shape
	draft.SourceSessionID = sessionID
	draft.SourceMessageIDs = []string{messageID}
	stored, err := f.service.CreateMemoryDraft(t.Context(), f.userID, draft)
	if err != nil {
		t.Fatalf("file memory: %v", err)
	}
	if !confirm {
		return stored
	}
	confirmed, err := f.service.ConfirmMemoryDraft(t.Context(), f.userID, stored.MemoryID)
	if err != nil {
		t.Fatalf("confirm memory: %v", err)
	}
	return confirmed
}

// fileForeign is the same for another account, used only to prove recall never reaches it.
func (f recallFixture) fileForeign(t *testing.T, shape domain.MemoryDraft) *domain.Memory {
	t.Helper()
	draft := shape
	draft.SourceSessionID = f.otherSess
	draft.SourceMessageIDs = []string{f.otherMessage}
	stored, err := f.service.CreateMemoryDraft(t.Context(), f.otherUser, draft)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := f.service.ConfirmMemoryDraft(t.Context(), f.otherUser, stored.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	return confirmed
}

func globalDraft(content string, confidence float64) domain.MemoryDraft {
	return domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: content, Confidence: confidence}
}

// topicDraft names its own slot, because two active memories in one topic key would stop
// being two facts and become a version history.
func topicDraft(topic, content string, confidence float64) domain.MemoryDraft {
	return domain.MemoryDraft{
		Scope: domain.MemoryScopeTopic, TopicKey: topic, Content: content, Confidence: confidence,
	}
}

func episodeDraft(content string, confidence float64) domain.MemoryDraft {
	return domain.MemoryDraft{Scope: domain.MemoryScopeEpisodic, Content: content, Confidence: confidence}
}

func recallFor(t *testing.T, fixture recallFixture, grant MemoryGrant, options MemorySearchOptions) *MemoryAccess {
	t.Helper()
	access, err := NewMemoryAccess(fixture.store, fixedGrant{grant: grant}, options)
	if err != nil {
		t.Fatal(err)
	}
	return access
}

func (f recallFixture) query(text string) MemoryQuery {
	return MemoryQuery{UserID: f.userID, SessionID: f.sessionID, Query: text}
}

func TestRecallReturnsOnlyTheCallersOwnActiveMemories(t *testing.T) {
	ctx := t.Context()
	fixture := newRecallFixture(t)
	access := recallFor(t, fixture, MemoryGrant{AllowGlobal: true}, MemorySearchOptions{})

	mine := fixture.file(t, fixture.sessionID, fixture.messageID,
		globalDraft("用户主要用 fsync 相关的 Redis 配置工作", 1), true)
	fixture.fileForeign(t, globalDraft("别人的 fsync 记忆，不该被读到", 1))
	// A pending candidate and a rejected one hold the same words and must stay invisible.
	fixture.file(t, fixture.sessionID, fixture.messageID, globalDraft("还没确认的 fsync 候选", 1), false)
	rejected := fixture.file(t, fixture.sessionID, fixture.messageID, globalDraft("被拒绝的 fsync 候选", 1), false)
	if _, err := fixture.service.RejectMemoryDraft(ctx, fixture.userID, rejected.MemoryID); err != nil {
		t.Fatal(err)
	}

	hits, note, err := access.Recall(ctx, fixture.query("fsync"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Memory.MemoryID != mine.MemoryID {
		t.Fatalf("hits = %+v, want exactly the caller's one active memory", hits)
	}
	if strings.Contains(hits[0].Content, "不该被读到") {
		t.Fatal("a foreign account's memory was recalled")
	}
	if !strings.Contains(note, "全局") {
		t.Fatalf("the scope note should say what was searched: %q", note)
	}
}

func TestRecallScopeRulesPerLayer(t *testing.T) {
	ctx := t.Context()
	fixture := newRecallFixture(t)
	globalMemory := fixture.file(t, fixture.sessionID, fixture.messageID, globalDraft("全局：fsync 偏好 everysec", 1), true)
	topicRedis := fixture.file(t, fixture.sessionID, fixture.messageID,
		topicDraft("redis", "redis 主题：AOF fsync 还没吃透", 1), true)
	topicMySQL := fixture.file(t, fixture.sessionID, fixture.messageID,
		topicDraft("mysql", "mysql 主题：binlog 的 fsync 也相关", 1), true)
	episodeHere := fixture.file(t, fixture.sessionID, fixture.messageID,
		episodeDraft("这轮决定先用 fsync everysec", 1), true)
	episodeThere := fixture.file(t, fixture.secondSess, fixture.secondMsg,
		episodeDraft("另一会话的 fsync 决定", 1), true)

	cases := []struct {
		name  string
		grant MemoryGrant
		want  []string
	}{
		{"nothing authorized", MemoryGrant{}, nil},
		{"global only", MemoryGrant{AllowGlobal: true}, []string{globalMemory.MemoryID}},
		{"one authorized topic", MemoryGrant{TopicKeys: []string{"redis"}}, []string{topicRedis.MemoryID}},
		// Tied scores keep creation order, which is a stored fact rather than the order the
		// engine happened to return.
		{"both topics", MemoryGrant{TopicKeys: []string{"redis", "mysql"}}, []string{topicRedis.MemoryID, topicMySQL.MemoryID}},
		{"current session episodes", MemoryGrant{SessionIDs: []string{fixture.sessionID}}, []string{episodeHere.MemoryID}},
		{"another session's episodes", MemoryGrant{SessionIDs: []string{fixture.secondSess}}, []string{episodeThere.MemoryID}},
		{"only the named session", MemoryGrant{SessionIDs: []string{fixture.sessionID, fixture.sessionID}},
			[]string{episodeHere.MemoryID}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			access := recallFor(t, fixture, testCase.grant, MemorySearchOptions{})
			hits, note, err := access.Recall(ctx, fixture.query("fsync"))
			if err != nil {
				t.Fatal(err)
			}
			got := orderOf(hits)
			want := strings.Join(testCase.want, ",")
			if got != want {
				t.Fatalf("hits = %q, want %q (note %q)", got, want, note)
			}
			if testCase.grant.Empty() && !strings.Contains(note, "授权") {
				t.Fatalf("an unauthorized read must say so: %q", note)
			}
		})
	}
}

// TestRecallDefaultConfidenceFloorIsSixTenths pins the production default as a literal,
// not as a constant compared against itself: the number is a measured decision (the M2
// filter from the memory pairing experiment) and a test that said
// "default == default" could not notice it drifting to zero.
func TestRecallDefaultConfidenceFloorIsSixTenths(t *testing.T) {
	ctx := t.Context()

	t.Run("the configuration default", func(t *testing.T) {
		if got := (MemorySearchOptions{}).Normalize().MinConfidence; got != 0.6 {
			t.Fatalf("normalized default floor = %v, want 0.6", got)
		}
		// A configured floor may raise the bar and never lower it, and an unusable value
		// falls back to 0.6 instead of becoming a filter that never fires.
		for name, pair := range map[string][2]float64{
			"zero means unset":    {0, 0.6},
			"below the default":   {0.3, 0.6},
			"exactly the default": {0.6, 0.6},
			"above it":            {0.8, 0.8},
			"negative":            {-1, 0.6},
			"above one":           {1.5, 0.6},
			"not a number":        {math.NaN(), 0.6},
			"positive infinity":   {math.Inf(1), 0.6},
			"negative infinity":   {math.Inf(-1), 0.6},
		} {
			if got := (MemorySearchOptions{MinConfidence: pair[0]}).Normalize().MinConfidence; got != pair[1] {
				t.Fatalf("%s: configured %v normalized to %v, want %v", name, pair[0], got, pair[1])
			}
		}
	})

	fixture := newRecallFixture(t)
	low := fixture.file(t, fixture.sessionID, fixture.messageID, topicDraft("c59", "0.59 的 fsync 记忆", 0.59), true)
	at := fixture.file(t, fixture.sessionID, fixture.messageID, topicDraft("c60", "0.60 的 fsync 记忆", 0.6), true)
	full := fixture.file(t, fixture.sessionID, fixture.messageID, topicDraft("c100", "1.00 的 fsync 记忆", 1), true)
	grant := MemoryGrant{TopicKeys: []string{"c59", "c60", "c100"}}
	access := recallFor(t, fixture, grant, MemorySearchOptions{})

	cases := []struct {
		name   string
		floor  float64
		want   []string
		reject bool
	}{
		{"unset floor keeps 0.59 hidden", 0, []string{at.MemoryID, full.MemoryID}, false},
		{"floor below the default cannot lower it", 0.3, []string{at.MemoryID, full.MemoryID}, false},
		{"the default itself", 0.6, []string{at.MemoryID, full.MemoryID}, false},
		{"a tighter request floor", 0.8, []string{full.MemoryID}, false},
		{"highest possible floor", 1, []string{full.MemoryID}, false},
		{"negative floor is refused", -0.1, nil, true},
		{"floor above one is refused", 1.5, nil, true},
		{"nan floor is refused", math.NaN(), nil, true},
		{"infinite floor is refused", math.Inf(1), nil, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			hits, _, err := access.Recall(ctx, MemoryQuery{
				UserID: fixture.userID, SessionID: fixture.sessionID, Query: "fsync", MinConfidence: testCase.floor,
			})
			if testCase.reject {
				if !errors.Is(err, ErrMemoryConfidenceInvalid) {
					t.Fatalf("floor %v = %v, want an explicit refusal", testCase.floor, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(hits))
			for _, hit := range hits {
				got = append(got, hit.Memory.MemoryID)
			}
			if strings.Join(got, ",") != strings.Join(testCase.want, ",") {
				t.Fatalf("floor %v recalled %v, want %v", testCase.floor, got, testCase.want)
			}
			if strings.Contains(orderOf(hits), low.MemoryID) {
				t.Fatalf("a 0.59 memory was recalled at floor %v", testCase.floor)
			}
		})
	}

	// Confidence never rescues a row that is not active: every one of these is 1.00.
	pending := fixture.file(t, fixture.sessionID, fixture.messageID, topicDraft("p1", "待确认的 fsync", 1), false)
	archived := fixture.file(t, fixture.sessionID, fixture.messageID, topicDraft("p2", "已拒绝的 fsync", 1), false)
	if _, err := fixture.service.RejectMemoryDraft(ctx, fixture.userID, archived.MemoryID); err != nil {
		t.Fatal(err)
	}
	superseded := fixture.file(t, fixture.sessionID, fixture.messageID, topicDraft("p3", "被替换的 fsync 旧版", 1), true)
	fixture.file(t, fixture.sessionID, fixture.messageID, topicDraft("p3", "替换它的 fsync 新版", 1), true)
	visible, _, err := recallFor(t, fixture, MemoryGrant{
		TopicKeys: []string{"c59", "c60", "c100", "p1", "p2", "p3"},
	}, MemorySearchOptions{}).Recall(ctx, fixture.query("fsync"))
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range visible {
		if hit.Memory.MemoryID == pending.MemoryID || hit.Memory.MemoryID == archived.MemoryID ||
			hit.Memory.MemoryID == superseded.MemoryID {
			t.Fatalf("a non-active memory with confidence 1.00 was recalled: %+v", hit)
		}
	}
}

// TestRecallHidesSupersededAndLowConfidence covers the filters that are not status-column
// lookups: the store already drops superseded rows, and confidence is a read-side rule.
func TestRecallHidesSupersededAndLowConfidence(t *testing.T) {
	ctx := t.Context()
	fixture := newRecallFixture(t)
	older := fixture.file(t, fixture.sessionID, fixture.messageID, topicDraft("redis", "旧的 fsync 结论", 1), true)
	replacement := fixture.file(t, fixture.sessionID, fixture.messageID, topicDraft("redis", "新的 fsync 结论", 1), true)
	faint := fixture.file(t, fixture.sessionID, fixture.messageID, topicDraft("kafka", "把握不大的 fsync 猜测", 0.59), true)
	if older.MemoryID == replacement.MemoryID {
		t.Fatal("the supersede case needs two rows")
	}

	access := recallFor(t, fixture, MemoryGrant{TopicKeys: []string{"redis", "kafka"}}, MemorySearchOptions{})
	hits, _, err := access.Recall(ctx, fixture.query("fsync"))
	if err != nil {
		t.Fatal(err)
	}
	// 0.59 is active, authorized by topic, a lexical match - and still invisible, because
	// the default floor is 0.6. The superseded row is dropped by the store.
	if orderOf(hits) != replacement.MemoryID {
		t.Fatalf("hits = %s, want only the replacement: %s is below the default floor and %s is superseded",
			orderOf(hits), faint.MemoryID, older.MemoryID)
	}

	raised, _, err := access.Recall(ctx, MemoryQuery{
		UserID: fixture.userID, SessionID: fixture.sessionID, Query: "fsync", MinConfidence: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if orderOf(raised) != replacement.MemoryID {
		t.Fatalf("above a 0.5 request floor = %s, want the default 0.6 to still apply", orderOf(raised))
	}

	configured := recallFor(t, fixture, MemoryGrant{TopicKeys: []string{"redis", "kafka"}},
		MemorySearchOptions{MinConfidence: 0.9})
	filtered, _, err := configured.Recall(ctx, fixture.query("fsync"))
	if err != nil {
		t.Fatal(err)
	}
	if orderOf(filtered) != replacement.MemoryID {
		t.Fatalf("configured floor = %s, want only the confident memory", orderOf(filtered))
	}
	// A per-request floor may tighten the configuration, never loosen it below a value the
	// deployment considered safe.
	loosened, _, err := configured.Recall(ctx, MemoryQuery{
		UserID: fixture.userID, SessionID: fixture.sessionID, Query: "fsync", MinConfidence: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if orderOf(loosened) != replacement.MemoryID {
		t.Fatalf("a zero per-request floor widened the configured one: %s", orderOf(loosened))
	}
}

// TestRecallBoundsResultsAndText checks each bound in isolation. A configuration that sets
// all three tightly would pass with two of them ignored, because the tightest alone decides
// the outcome - so every cap gets a case where it is the only one that could bind.
func TestRecallBoundsResultsAndText(t *testing.T) {
	ctx := t.Context()
	fixture := newRecallFixture(t)
	keys := []string{"主题0", "主题1", "主题2", "主题3"}
	for index := range 4 {
		fixture.file(t, fixture.sessionID, fixture.messageID,
			topicDraft("主题"+strconv.Itoa(index),
				"fsync 结论编号 "+strconv.Itoa(index)+"，内容较长用来测试单条与总长度上限是否生效。", 1), true)
	}
	grant := MemoryGrant{TopicKeys: keys}
	query := fixture.query("fsync")

	t.Run("result count", func(t *testing.T) {
		access := recallFor(t, fixture, grant, MemorySearchOptions{MaxResults: 2, ItemRunes: 200, TotalRunes: 4000})
		hits, _, err := access.Recall(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 {
			t.Fatalf("hits = %d, want exactly the configured 2", len(hits))
		}
	})
	t.Run("total text", func(t *testing.T) {
		access := recallFor(t, fixture, grant, MemorySearchOptions{MaxResults: 4, ItemRunes: 200, TotalRunes: 60})
		hits, _, err := access.Recall(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, hit := range hits {
			total += len([]rune(hit.Content))
		}
		if len(hits) >= 4 || total > 60 {
			t.Fatalf("hits = %d totalling %d runes, want fewer than four inside 60 runes", len(hits), total)
		}
	})
	t.Run("one memory", func(t *testing.T) {
		access := recallFor(t, fixture, grant, MemorySearchOptions{MaxResults: 1, ItemRunes: 12, TotalRunes: 4000})
		hits, _, err := access.Recall(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 {
			t.Fatalf("hits = %d, want one", len(hits))
		}
		// 12 runes plus the ellipsis the clip appends.
		if length := len([]rune(hits[0].Content)); length > 13 {
			t.Fatalf("one memory carried %d runes, over the 12 rune cap: %q", length, hits[0].Content)
		}
	})

	// A cap that drops a memory must not leave its citation behind, so every returned
	// citation belongs to a memory that was returned.
	access := recallFor(t, fixture, grant, MemorySearchOptions{MaxResults: 2, ItemRunes: 200, TotalRunes: 4000})
	hits, _, err := access.Recall(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.Reference.Citation.SourceID != "src-"+hit.Memory.MemoryID {
			t.Fatalf("citation %q does not belong to memory %q", hit.Reference.Citation.SourceID, hit.Memory.MemoryID)
		}
	}
}

// TestRecallOrderIsDeterministicAndNotRowOrder is why the sort is explicit: a better match
// stored later must come first, and two identical calls must agree on the whole sequence.
func TestRecallOrderIsDeterministicAndNotRowOrder(t *testing.T) {
	ctx := t.Context()
	fixture := newRecallFixture(t)
	weaker := fixture.file(t, fixture.sessionID, fixture.messageID,
		topicDraft("a1", "只提到 fsync", 1), true)
	stronger := fixture.file(t, fixture.sessionID, fixture.messageID,
		topicDraft("a2", "同时提到 fsync 和 everysec 的完整结论", 1), true)
	tied := fixture.file(t, fixture.sessionID, fixture.messageID,
		topicDraft("a3", "另一条 fsync everysec 的结论", 1), true)
	// Authorized, active, confident - and about something else entirely. Admission is the
	// lexical floor: sharing no term at all must keep a memory out, however relevant it
	// might be to a human reading the whole account.
	unrelated := fixture.file(t, fixture.sessionID, fixture.messageID,
		topicDraft("a4", "番茄炒蛋要先放糖还是盐", 1), true)

	access := recallFor(t, fixture, MemoryGrant{TopicKeys: []string{"a1", "a2", "a3", "a4"}},
		MemorySearchOptions{MaxResults: 5})
	query := fixture.query("fsync everysec")
	first, _, err := access.Recall(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := access.Recall(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("hits = %d, want the three memories that share a term with the query", len(first))
	}
	if orderOf(first) == "" || strings.Contains(orderOf(first), unrelated.MemoryID) {
		t.Fatalf("an unrelated memory was recalled: %s", orderOf(first))
	}
	if orderOf(first) != orderOf(second) {
		t.Fatalf("two identical calls disagreed: %s vs %s", orderOf(first), orderOf(second))
	}
	if first[len(first)-1].Memory.MemoryID != weaker.MemoryID {
		t.Fatalf("the partial match should rank last, got %s (score %v)",
			first[len(first)-1].Memory.MemoryID, first[len(first)-1].Score)
	}
	// The two full matches tie on score, so their order is the memory's own creation
	// order - which is a stored fact, not whatever row order the engine returned.
	if first[0].Memory.MemoryID != stronger.MemoryID || first[1].Memory.MemoryID != tied.MemoryID {
		t.Fatalf("tied scores should keep creation order: %s", orderOf(first))
	}
	if first[0].Score <= first[len(first)-1].Score {
		t.Fatalf("scores are not ordered: %+v", first)
	}
}

func TestRecallRejectsInvalidRequestsAndMissingWiring(t *testing.T) {
	ctx := t.Context()
	fixture := newRecallFixture(t)
	access := recallFor(t, fixture, MemoryGrant{AllowGlobal: true}, MemorySearchOptions{})
	if _, _, err := access.Recall(ctx, MemoryQuery{SessionID: fixture.sessionID, Query: "fsync"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("no user = %v, want unauthorized", err)
	}
	if _, _, err := access.Recall(ctx, MemoryQuery{UserID: fixture.userID, Query: "  "}); !errors.Is(err, ErrMemoryQueryInvalid) {
		t.Fatalf("blank query = %v, want invalid", err)
	}
	if _, _, err := access.Recall(ctx, MemoryQuery{
		UserID: fixture.userID, Query: strings.Repeat("字", DefaultMemoryQueryRunes+1),
	}); !errors.Is(err, ErrMemoryQueryInvalid) {
		t.Fatalf("over-long query = %v, want invalid", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := access.Recall(cancelled, fixture.query("fsync")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled = %v, want the cancellation", err)
	}
	if _, err := NewMemoryAccess(nil, fixedGrant{}, MemorySearchOptions{}); !errors.Is(err, ErrNoMemoryStore) {
		t.Fatalf("no store = %v", err)
	}
	if _, err := NewMemoryAccess(fixture.store, nil, MemorySearchOptions{}); !errors.Is(err, ErrNoMemoryAuthorizer) {
		t.Fatalf("no authorizer = %v", err)
	}
	// An authorizer that fails ends the read. "Could not decide the scope" is not "there is
	// nothing to find", and the tool must not present the first as the second.
	broken, err := NewMemoryAccess(fixture.store, fixedGrant{err: repository.ErrNotFound}, MemorySearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := broken.Recall(ctx, fixture.query("fsync")); err == nil {
		t.Fatal("a failing authorizer was treated as an empty result")
	}
}

func TestRecallCarriesAnInternalReferenceAndNoModelVisibleIdentifier(t *testing.T) {
	ctx := t.Context()
	fixture := newRecallFixture(t)
	stored := fixture.file(t, fixture.sessionID, fixture.messageID, globalDraft("用户偏好中文回答", 0.9), true)
	access := recallFor(t, fixture, MemoryGrant{AllowGlobal: true}, MemorySearchOptions{})
	hits, _, err := access.Recall(ctx, fixture.query("中文"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
	hit := hits[0]
	if hit.Reference.MemoryID != stored.MemoryID || hit.Reference.Scope != domain.MemoryScopeGlobal ||
		hit.Reference.SourceSessionID != fixture.sessionID ||
		len(hit.Reference.SourceMessageIDs) != 1 || hit.Reference.SourceMessageIDs[0] != fixture.messageID {
		t.Fatalf("internal reference is incomplete: %+v", hit.Reference)
	}
	if hit.Reference.Citation.SourceID != "src-"+stored.MemoryID || hit.Reference.Citation.Title != "全局记忆" {
		t.Fatalf("citation built from the wrong row: %+v", hit.Reference.Citation)
	}
	if hit.Reference.Citation.DocumentID != "" || hit.Reference.Citation.CollectionID != "" ||
		hit.Reference.Citation.ChunkID != "" {
		t.Fatalf("a memory citation must not pretend to be a document chunk: %+v", hit.Reference.Citation)
	}
	if strings.Contains(hit.Content, stored.MemoryID) || strings.Contains(hit.Content, fixture.sessionID) {
		t.Fatalf("recalled text carried identifiers a model could echo back: %q", hit.Content)
	}
	// The reference is a copy: editing it must not reach the stored row.
	hit.Reference.SourceMessageIDs[0] = "clobbered"
	reread, _, err := access.Recall(ctx, fixture.query("中文"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reread) != 1 || reread[0].Reference.SourceMessageIDs[0] != fixture.messageID {
		t.Fatalf("a caller edited through the reference: %+v", reread)
	}
}

// TestProductionGrantCoversGlobalAndTheQueryingSession pins the authorizer the binary
// actually runs with, rather than a test copy of it. The session a recall is running for
// decides the episode scope, and no topic is ever granted - so the same user asking from a
// different session sees a different, smaller set.
func TestProductionGrantCoversGlobalAndTheQueryingSession(t *testing.T) {
	ctx := t.Context()
	fixture := newRecallFixture(t)
	global := fixture.file(t, fixture.sessionID, fixture.messageID, globalDraft("全局：fsync 偏好", 1), true)
	here := fixture.file(t, fixture.sessionID, fixture.messageID, episodeDraft("本会话的 fsync 决定", 1), true)
	there := fixture.file(t, fixture.secondSess, fixture.secondMsg, episodeDraft("另一会话的 fsync 决定", 1), true)
	topic := fixture.file(t, fixture.sessionID, fixture.messageID, topicDraft("redis", "redis 主题的 fsync 掌握度", 1), true)

	access, err := NewMemoryAccess(fixture.store, SessionMemoryGrant{}, MemorySearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	inThisSession, _, err := access.Recall(ctx, MemoryQuery{
		UserID: fixture.userID, SessionID: fixture.sessionID, Query: "fsync",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := orderOf(inThisSession); got != global.MemoryID+","+here.MemoryID {
		t.Fatalf("recall from session A = %s, want the global memory and this session's episode", got)
	}
	inOtherSession, _, err := access.Recall(ctx, MemoryQuery{
		UserID: fixture.userID, SessionID: fixture.secondSess, Query: "fsync",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := orderOf(inOtherSession); got != global.MemoryID+","+there.MemoryID {
		t.Fatalf("recall from session B = %s, want the global memory and that session's episode", got)
	}
	for _, forbidden := range []string{topic.MemoryID, topic.TopicKey} {
		for _, hit := range append(append([]MemoryHit{}, inThisSession...), inOtherSession...) {
			if hit.Memory.MemoryID == forbidden || hit.Memory.TopicKey == forbidden {
				t.Fatalf("topic memory reached a recall that was never granted one: %+v", hit)
			}
		}
	}
	// A recall with no session at all keeps global memory only: the episode layer needs a
	// session to be about, and inventing one would be a scope the server never granted.
	anonymous, _, err := access.Recall(ctx, MemoryQuery{UserID: fixture.userID, Query: "fsync"})
	if err != nil {
		t.Fatal(err)
	}
	if orderOf(anonymous) != global.MemoryID {
		t.Fatalf("sessionless recall = %s, want only the global memory", orderOf(anonymous))
	}
}

func orderOf(hits []MemoryHit) string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.Memory.MemoryID)
	}
	return strings.Join(ids, ",")
}
