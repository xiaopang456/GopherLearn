package agenttool

import (
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
)

const (
	historyRunSession     = "session-current"
	historyEpisodeSession = "session-episode"
)

// fakeHistoryReader stands in for the message repository. It enforces the two
// boundaries the real query carries in SQL - the owner, and the session each id
// was granted from - so a tool that built its request wrongly cannot pass by
// reading a stub that was too forgiving to notice.
type fakeHistoryReader struct {
	rows    map[uint]model.Message
	userIDs []int64
	scopes  []map[string][]uint
	err     error
	// leak stands in for a reader that ignores the session scoping it was handed:
	// whatever it returns, the tool must still refuse the row.
	leak []model.Message
}

func (r *fakeHistoryReader) ReadOwnedMessages(context.Context, int64, []uint) ([]model.Message, error) {
	return nil, nil
}

func (r *fakeHistoryReader) ReadOwnedMessagesBySession(_ context.Context, userID int64,
	messageIDsBySession map[string][]uint) ([]model.Message, error) {
	r.userIDs = append(r.userIDs, userID)
	cloned := make(map[string][]uint, len(messageIDsBySession))
	for sessionID, ids := range messageIDsBySession {
		cloned[sessionID] = append([]uint(nil), ids...)
	}
	r.scopes = append(r.scopes, cloned)
	if r.err != nil {
		return nil, r.err
	}
	if userID != 7 {
		return nil, nil
	}
	var out []model.Message
	out = append(out, r.leak...)
	for sessionID, ids := range cloned {
		for _, id := range ids {
			row, ok := r.rows[id]
			if !ok || row.SessionID != sessionID {
				continue
			}
			out = append(out, row)
		}
	}
	slices.SortFunc(out, func(a, b model.Message) int { return int(a.ID) - int(b.ID) })
	return out, nil
}

func (r *fakeHistoryReader) requestCount() int { return len(r.scopes) }

func userMessage(id uint, sessionID, content string) model.Message {
	return model.Message{ID: id, SessionID: sessionID, IsUser: true, Content: content,
		CreatedAt: time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)}
}

func summaryGrant(id uint, sessionID string) serviceagent.HistoryMessageGrant {
	return serviceagent.HistoryMessageGrant{
		MessageID: id, SourceKind: serviceagent.HistorySourceSummary, SourceSessionID: sessionID,
		SummaryVersion: 3, SummaryWatermark: 40,
	}
}

func episodeGrant(id uint, sessionID string) serviceagent.HistoryMessageGrant {
	return serviceagent.HistoryMessageGrant{
		MessageID: id, SourceKind: serviceagent.HistorySourceEpisode, SourceSessionID: sessionID,
		EpisodeID: "episode-1", EpisodeVersion: historyEpisodeVersion,
		EpisodeWatermark: historyEpisodeWatermark,
	}
}

func historyRun(grants ...serviceagent.HistoryMessageGrant) serviceagent.RunContext {
	return serviceagent.RunContext{
		UserID: 7, SessionID: historyRunSession, RunID: "run", Question: "我当时是怎么说的？",
		HistoryGrants: grants,
	}
}

func invokeRead(t *testing.T, tools []tool.BaseTool, ids ...int) ReadHistoryMessageOutput {
	t.Helper()
	payload, err := json.Marshal(ReadHistoryMessageInput{MessageIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tools[0].(tool.InvokableTool).InvokableRun(context.Background(), string(payload))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var out ReadHistoryMessageOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out
}

func newHistoryTools(t *testing.T, reader HistoryMessageReader, run serviceagent.RunContext) []tool.BaseTool {
	t.Helper()
	// The default verifier agrees with the window: it reports exactly the facts the
	// grants in this run claim. These tests are about session scoping and batching, so
	// a refusal here would hide the thing under test. The refusals themselves have
	// their own tests below, against a verifier that disagrees on one field at a time.
	return newHistoryToolsWithEvidence(t, reader, agreeingEpisodeEvidence(run), run)
}

func newHistoryToolsWithEvidence(t *testing.T, reader HistoryMessageReader, evidence EpisodeEvidenceReader,
	run serviceagent.RunContext) []tool.BaseTool {
	t.Helper()
	registry, err := NewHistoryMessageRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseEpisodeEvidenceReader(evidence)
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

// agreeingEpisodeEvidence reads a card back the way the store would when nothing has
// changed since the render.
func agreeingEpisodeEvidence(run serviceagent.RunContext) EpisodeEvidenceReader {
	return EpisodeEvidenceReaderFunc(func(_ context.Context, userID int64, episodeID string) (EpisodeEvidenceFacts, error) {
		facts := EpisodeEvidenceFacts{EpisodeID: episodeID, UserID: userID,
			Version: historyEpisodeVersion, Status: string(model.EpisodeStatusActive),
			IndexStatus: string(model.EpisodeIndexIndexed), Watermark: historyEpisodeWatermark}
		for _, grant := range run.HistoryGrants {
			if grant.SourceKind != serviceagent.HistorySourceEpisode || grant.EpisodeID != episodeID {
				continue
			}
			facts.SessionID = grant.SourceSessionID
			facts.Version = grant.EpisodeVersion
			facts.EvidenceMessageIDs = append(facts.EvidenceMessageIDs, grant.MessageID)
		}
		return facts, nil
	})
}

const (
	historyEpisodeVersion   = uint64(2)
	historyEpisodeWatermark = uint(1000)
)

// Message ids are sequential across every account and every session, so "the id
// the model asked for" is not a safe input: counting from a known id walks the
// learner's whole history. Only what an injected card cited is readable, and an
// id outside that set never reaches the database.
func TestHistoryReadRefusesIdsNoCardCited(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyRunSession, "那就用 Protobuf"),
		12: userMessage(12, historyRunSession, "被摘要压缩掉了、但摘要没有引用的一句话"),
	}}
	tools := newHistoryTools(t, reader, historyRun(summaryGrant(11, historyRunSession)))

	out := invokeRead(t, tools, 11, 12)

	if len(out.Messages) != 1 || out.Messages[0].MessageID != 11 {
		t.Fatalf("messages=%+v want only the cited id", out.Messages)
	}
	if len(out.Refused) != 1 || out.Refused[0] != 12 {
		t.Fatalf("refused=%v want the uncited id reported", out.Refused)
	}
	if reader.requestCount() != 1 {
		t.Fatalf("reader calls=%d want exactly one", reader.requestCount())
	}
	for _, ids := range reader.scopes[0] {
		if slices.Contains(ids, uint(12)) {
			t.Fatal("an uncited id was sent to the database")
		}
	}
	if len(reader.userIDs) != 1 || reader.userIDs[0] != 7 {
		t.Fatalf("userIDs=%v want the run's trusted owner", reader.userIDs)
	}
}

// Below the watermark is not the same as cited by the summary. Most of what a
// summary folded away was dropped precisely because it was not worth recording,
// and opening the whole numeric prefix would hand the model the session.
func TestHistoryReadRefusesUncitedIDBelowWatermark(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		39: userMessage(39, historyRunSession, "摘要里没有提到的旧消息"),
	}}
	tools := newHistoryTools(t, reader, historyRun(summaryGrant(11, historyRunSession)))

	out := invokeRead(t, tools, 39)

	if len(out.Messages) != 0 || len(out.Refused) != 1 || out.Refused[0] != 39 {
		t.Fatalf("out=%+v want the uncited id below the watermark refused", out)
	}
	if reader.requestCount() != 0 {
		t.Fatalf("the database was queried for an uncited id: %v", reader.scopes)
	}
}

// Nothing was recalled, so nothing is citable. Offering the tool would only
// invite calls that can be refused.
func TestHistoryToolAbsentWithoutGrants(t *testing.T) {
	if tools := newHistoryTools(t, &fakeHistoryReader{}, historyRun()); len(tools) != 0 {
		t.Fatalf("tools=%d want none when no card was injected", len(tools))
	}
}

// A grant that does not describe itself honestly authorises nothing, and the
// tool is where that is decided rather than where it is discovered.
func TestHistoryToolAbsentForUnusableGrants(t *testing.T) {
	beyondWatermark := summaryGrant(41, historyRunSession)
	beyondWatermark.SummaryWatermark = 40
	wrongSession := summaryGrant(11, historyEpisodeSession)
	episodeWithoutID := episodeGrant(21, historyEpisodeSession)
	episodeWithoutID.EpisodeID = ""
	unknownKind := serviceagent.HistoryMessageGrant{MessageID: 11, SourceKind: "whatever",
		SourceSessionID: historyRunSession}
	for name, grant := range map[string]serviceagent.HistoryMessageGrant{
		"summary id above its watermark":  beyondWatermark,
		"summary id from another session": wrongSession,
		"episode id without provenance":   episodeWithoutID,
		"unknown source kind":             unknownKind,
	} {
		if tools := newHistoryTools(t, &fakeHistoryReader{}, historyRun(grant)); len(tools) != 0 {
			t.Fatalf("%s still registered the read tool", name)
		}
	}
}

// One call may mix the two evidence sources, and each id must be asked of the
// session that granted it: summary evidence lives in the session being answered,
// episode evidence in the session the episode describes.
func TestHistoryReadMixesSummaryAndEpisodeSessions(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyRunSession, "我在本会话里说过的原话"),
		21: userMessage(21, historyEpisodeSession, "我在上一个会话里说过的原话"),
	}}
	tools := newHistoryTools(t, reader, historyRun(
		summaryGrant(11, historyRunSession), episodeGrant(21, historyEpisodeSession)))

	out := invokeRead(t, tools, 21, 11)

	if len(out.Messages) != 2 || out.Messages[0].MessageID != 11 || out.Messages[1].MessageID != 21 {
		t.Fatalf("messages=%+v want both sources, in ascending id order", out.Messages)
	}
	if reader.requestCount() != 1 {
		t.Fatalf("reader calls=%d want one batched read", reader.requestCount())
	}
	scopes := reader.scopes[0]
	if got := scopes[historyRunSession]; !slices.Equal(got, []uint{11}) {
		t.Fatalf("current-session scope=%v want only the summary id", got)
	}
	if got := scopes[historyEpisodeSession]; !slices.Equal(got, []uint{21}) {
		t.Fatalf("episode-session scope=%v want only the episode id", got)
	}
}

// An implementation that scoped episode reads to the run's own session - the
// mistake this design exists to avoid - returns nothing here, and a row that came
// back from the wrong session must be dropped rather than trusted.
func TestHistoryReadRejectsEpisodeRowFromRunSession(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		21: userMessage(21, historyRunSession, "同一条 id 落在本会话里的另一行"),
	}}
	tools := newHistoryTools(t, reader, historyRun(episodeGrant(21, historyEpisodeSession)))

	out := invokeRead(t, tools, 21)

	if len(out.Messages) != 0 {
		t.Fatalf("messages=%+v want a row outside the granting session dropped", out.Messages)
	}
	if !slices.Equal(reader.scopes[0][historyEpisodeSession], []uint{21}) {
		t.Fatalf("scopes=%v want the episode's own session, not the run's", reader.scopes[0])
	}
}

// Symmetrically: a summary id may not be satisfied by a same-id row from some
// other session the learner also owns.
func TestHistoryReadRejectsSummaryRowFromAnotherSession(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyEpisodeSession, "别的会话里同号的一行"),
	}}
	tools := newHistoryTools(t, reader, historyRun(summaryGrant(11, historyRunSession)))

	out := invokeRead(t, tools, 11)

	if len(out.Messages) != 0 {
		t.Fatalf("messages=%+v want a cross-session row dropped", out.Messages)
	}
	if _, asked := reader.scopes[0][historyEpisodeSession]; asked {
		t.Fatalf("scopes=%v want the summary id asked of the current session only", reader.scopes[0])
	}
}

// An id the summary and an episode both cite is one message: it is returned once
// even though it was granted twice.
func TestHistoryReadSameIDFromTwoSourcesReturnsOnce(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyRunSession, "两处以同一句话为证据"),
	}}
	tools := newHistoryTools(t, reader, historyRun(
		summaryGrant(11, historyRunSession), episodeGrant(11, historyRunSession)))

	out := invokeRead(t, tools, 11, 11)

	if len(out.Messages) != 1 {
		t.Fatalf("messages=%+v want one row for a doubly granted id", out.Messages)
	}
	if got := len(reader.scopes[0][historyRunSession]); got != 1 {
		t.Fatalf("ids asked=%d want the duplicate grant collapsed before the query", got)
	}
}

func TestHistoryReadBoundsBatchAndLength(t *testing.T) {
	long := strings.Repeat("字", maxHistoryMessageRunes+50)
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyRunSession, long),
	}}
	tools := newHistoryTools(t, reader, historyRun(
		summaryGrant(11, historyRunSession), summaryGrant(12, historyRunSession),
		summaryGrant(13, historyRunSession), summaryGrant(14, historyRunSession),
		summaryGrant(15, historyRunSession), summaryGrant(16, historyRunSession)))

	out := invokeRead(t, tools, 11)
	if len(out.Messages) != 1 || !out.Messages[0].Truncated {
		t.Fatalf("messages=%+v want one truncated message", out.Messages)
	}
	if got := len([]rune(out.Messages[0].Content)); got != maxHistoryMessageRunes {
		t.Fatalf("content runes=%d want %d", got, maxHistoryMessageRunes)
	}

	// A single call must not be able to pull the whole granted set at once.
	payload, _ := json.Marshal(ReadHistoryMessageInput{MessageIDs: []int{11, 12, 13, 14, 15, 16}})
	if _, err := tools[0].(tool.InvokableTool).InvokableRun(context.Background(), string(payload)); err == nil {
		t.Fatal("six ids in one call were accepted")
	}
}

func TestHistoryReadReturnsOnlyRequestedLearnerMessages(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: {ID: 11, SessionID: historyRunSession, IsUser: false, Content: "assistant reconstruction",
			CreatedAt: time.Now()},
		12: userMessage(12, historyRunSession, "learner words"),
	}}
	// Row 13 was never asked for. A reader that hands one back has ignored its
	// scoping, and the tool must still not surface it.
	reader.leak = []model.Message{userMessage(13, historyRunSession, "未请求的行")}
	tools := newHistoryTools(t, reader, historyRun(summaryGrant(11, historyRunSession), summaryGrant(12, historyRunSession)))

	out := invokeRead(t, tools, 11, 12)

	if len(out.Messages) != 1 || out.Messages[0].MessageID != 12 || out.Messages[0].Role != "user" {
		t.Fatalf("messages=%+v want only the requested learner row", out.Messages)
	}
}

func TestHistoryReadEmptyWhenEveryIDIsRefusedOrMissing(t *testing.T) {
	tools := newHistoryTools(t, &fakeHistoryReader{rows: map[uint]model.Message{}},
		historyRun(summaryGrant(11, historyRunSession)))

	missing := invokeRead(t, tools, 11)
	if len(missing.Messages) != 0 {
		t.Fatalf("missing messages=%+v want no evidence", missing.Messages)
	}
	refused := invokeRead(t, tools, 12)
	if len(refused.Messages) != 0 || len(refused.Refused) != 1 || refused.Refused[0] != 12 {
		t.Fatalf("refused=%+v want one refused id and no evidence", refused)
	}
}

// The model's arguments are ids and nothing else: it cannot name a user, a
// session, a watermark or a summary version, so the only owner and session a
// read can bind to are the run's.
func TestHistoryInputCarriesNoIdentityFields(t *testing.T) {
	if got := reflect.TypeOf(ReadHistoryMessageInput{}).NumField(); got != 1 {
		t.Fatalf("read_history_message takes %d argument fields, want message_ids alone", got)
	}
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		11: userMessage(11, historyRunSession, "原话"),
		12: userMessage(12, historyEpisodeSession, "同一用户的另一个会话里的原话"),
	}}
	tools := newHistoryTools(t, reader, historyRun(summaryGrant(11, historyRunSession)))

	out := invokeRead(t, tools, 11, 12)

	if len(out.Messages) != 1 || out.Messages[0].MessageID != 11 {
		t.Fatalf("out=%+v want only the granted id", out)
	}
	if _, asked := reader.scopes[0][historyEpisodeSession]; asked {
		t.Fatalf("scopes=%v want the run's session alone, never a session from the payload", reader.scopes[0])
	}
	if reader.userIDs[0] != 7 {
		t.Fatalf("userIDs=%v want the run's owner", reader.userIDs)
	}
}

// fixedEpisodeEvidence builds one card's store answer, so a test can move exactly one
// field away from what the window claimed and check that this is what the tool does
// with it.
func fixedEpisodeEvidence(mutate func(*EpisodeEvidenceFacts)) EpisodeEvidenceReader {
	return EpisodeEvidenceReaderFunc(func(_ context.Context, userID int64, episodeID string) (EpisodeEvidenceFacts, error) {
		facts := EpisodeEvidenceFacts{EpisodeID: episodeID, UserID: userID,
			SessionID: historyEpisodeSession, Version: historyEpisodeVersion,
			Status: string(model.EpisodeStatusActive), IndexStatus: string(model.EpisodeIndexIndexed),
			Watermark: historyEpisodeWatermark, EvidenceMessageIDs: []uint{21, 22}}
		if mutate != nil {
			mutate(&facts)
		}
		return facts, nil
	})
}

func TestEpisodeGrantRefusals(t *testing.T) {
	cases := []struct {
		name     string
		evidence EpisodeEvidenceReader
		wire     bool
		reason   string
	}{
		{"owner mismatch", fixedEpisodeEvidence(func(f *EpisodeEvidenceFacts) { f.UserID = 8 }), true,
			refusalEpisodeOwner},
		{"source session mismatch", fixedEpisodeEvidence(func(f *EpisodeEvidenceFacts) {
			f.SessionID = "session-someone-elses"
		}), true, refusalEpisodeSession},
		{"id outside the card's closure", fixedEpisodeEvidence(func(f *EpisodeEvidenceFacts) {
			f.EvidenceMessageIDs = []uint{22}
		}), true, refusalEpisodeClosure},
		{"card identity mismatch", fixedEpisodeEvidence(func(f *EpisodeEvidenceFacts) { f.EpisodeID = "episode-2" }),
			true, refusalEpisodeMissing},
		{"version moved", fixedEpisodeEvidence(func(f *EpisodeEvidenceFacts) { f.Version = 3 }), true,
			refusalEpisodeVersion},
		{"episode archived", fixedEpisodeEvidence(func(f *EpisodeEvidenceFacts) {
			f.Status = string(model.EpisodeStatusArchived)
		}), true, refusalEpisodeInactive},
		{"index not verified", fixedEpisodeEvidence(func(f *EpisodeEvidenceFacts) {
			f.IndexStatus = string(model.EpisodeIndexPending)
		}), true, refusalEpisodeIndex},
		{"citation past the watermark", fixedEpisodeEvidence(func(f *EpisodeEvidenceFacts) { f.Watermark = 20 }),
			true, refusalEpisodeWatermark},
		{"no such card", EpisodeEvidenceReaderFunc(func(_ context.Context, _ int64, _ string) (EpisodeEvidenceFacts, error) {
			return EpisodeEvidenceFacts{}, ErrEpisodeEvidenceMissing
		}), true, refusalEpisodeMissing},
		{"store unreadable", EpisodeEvidenceReaderFunc(func(_ context.Context, _ int64, _ string) (EpisodeEvidenceFacts, error) {
			return EpisodeEvidenceFacts{}, errors.New("connection reset")
		}), true, refusalUnreadable},
		{"no verifier wired", nil, false, refusalVerifierAbsent},
	}
	for _, testCase := range cases {
		reader := &fakeHistoryReader{rows: map[uint]model.Message{
			21: userMessage(21, historyEpisodeSession, "我当时的原话"),
		}}
		registry, err := NewHistoryMessageRegistry(reader)
		if err != nil {
			t.Fatal(err)
		}
		if testCase.wire {
			registry.UseEpisodeEvidenceReader(testCase.evidence)
		}
		tools, err := registry.Tools(context.Background(), historyRun(episodeGrant(21, historyEpisodeSession)))
		if err != nil {
			t.Fatal(err)
		}
		out := invokeRead(t, tools, 21)
		if len(out.Messages) != 0 {
			t.Errorf("%s: a refused citation still returned %v", testCase.name, out.Messages)
		}
		if !slices.Contains(out.Refused, 21) {
			t.Errorf("%s: refused list is %v, want it to name 21", testCase.name, out.Refused)
		}
		if len(out.RefusalReasons) != 1 || out.RefusalReasons[0].Reason != testCase.reason {
			t.Errorf("%s: reasons=%+v want one %q", testCase.name, out.RefusalReasons, testCase.reason)
		}
		// The refusal is not a filter applied to a result. An id that authorises
		// nothing must never become a query, or every refusal would still be a read.
		if reader.requestCount() != 0 {
			t.Errorf("%s: the message store was queried %d time(s) for a refused citation",
				testCase.name, reader.requestCount())
		}
	}
}

// Cross-source independence: one id cited by both a summary and an episode is still
// readable when the episode's card has moved on, because the summary's own grant stands
// on its own. Dropping it here would make a verification failure on one source erase
// the other source's authority.
func TestEpisodeRefusalDoesNotEraseTheSummaryGrant(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		21: userMessage(21, historyRunSession, "我在本会话里的原话"),
	}}
	tools := newHistoryToolsWithEvidence(t, reader,
		fixedEpisodeEvidence(func(f *EpisodeEvidenceFacts) { f.Version = 99 }),
		historyRun(summaryGrant(21, historyRunSession), episodeGrant(21, historyEpisodeSession)))
	out := invokeRead(t, tools, 21)
	if len(out.Messages) != 1 || out.Messages[0].MessageID != 21 {
		t.Fatalf("messages=%+v want the summary grant to stand on its own", out.Messages)
	}
	if len(out.RefusalReasons) != 1 || out.RefusalReasons[0].Reason != refusalEpisodeVersion {
		t.Errorf("reasons=%+v want the moved card recorded once", out.RefusalReasons)
	}
	// The id is readable, so it is not a refusal. Listing it would tell the model the
	// citation failed when the answer it got came from the summary's own grant.
	if slices.Contains(out.Refused, 21) {
		t.Errorf("refused=%v: a still-readable id must not be listed as refused", out.Refused)
	}
}

// A card's citation is only worth what the card still says it is worth: an id the
// stored card never cites cannot be authorised by the render alone, even when the same
// run has other grants.
func TestEpisodeGrantFromStaleRenderIsRefusedBeforeQuery(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		33: userMessage(33, historyEpisodeSession, "被新版本删掉的引用"),
	}}
	tools := newHistoryToolsWithEvidence(t, reader,
		fixedEpisodeEvidence(func(f *EpisodeEvidenceFacts) { f.EvidenceMessageIDs = []uint{21} }),
		historyRun(episodeGrant(33, historyEpisodeSession)))
	out := invokeRead(t, tools, 33)
	if len(out.Messages) != 0 || !slices.Contains(out.Refused, 33) || reader.requestCount() != 0 {
		t.Fatalf("out=%+v queries=%d want a pre-query refusal", out, reader.requestCount())
	}
}
