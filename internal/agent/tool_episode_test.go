package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xiaopang456/GopherLearn/internal/contextwindow"
	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// fakeEpisodeSearcher is the search seam the tool tests drive.
type fakeEpisodeSearcher struct {
	userID, sessionID, turnID, query string
	calls                            int
	reading                          EpisodeReading
	err                              error
}

func (f *fakeEpisodeSearcher) RecallEpisodes(_ context.Context, userID, sessionID, turnID, query string) (EpisodeReading, error) {
	f.calls++
	f.userID, f.sessionID, f.turnID, f.query = userID, sessionID, turnID, query
	return f.reading, f.err
}

// capturingSearcher records the authorization callback's outcome so tests can
// assert exactly which ids a rendering granted.
type capturingSearcher struct {
	fakeEpisodeSearcher
	granted   []EpisodeAuthorization
	authError error
	calls     int
}

func (f *capturingSearcher) RecallEpisodes(ctx context.Context, userID, sessionID, turnID, query string) (EpisodeReading, error) {
	reading, err := f.fakeEpisodeSearcher.RecallEpisodes(ctx, userID, sessionID, turnID, query)
	if err != nil {
		return reading, err
	}
	f.calls++
	reading.AuthorizeDisplayed = func(displayed []EpisodeAuthorization) error {
		if f.authError != nil {
			return f.authError
		}
		f.granted = append(f.granted, displayed...)
		return nil
	}
	return reading, nil
}

func episodeSearchCard(title, synopsis, decisionText string, ids ...string) EpisodeEntry {
	return EpisodeEntry{
		Title:      title,
		Synopsis:   synopsis,
		Topics:     []string{"JSON 编解码器"},
		Assertions: []EpisodeAssertionLine{{Label: "决定", Text: decisionText, MessageIDs: ids}},
		Citation:   domain.Source{SourceID: "src-episode-test", Title: title, Section: "情景经历", Snippet: synopsis},
	}
}

func TestEpisodeSearchRefusesMissingSearcherAndBadQueries(t *testing.T) {
	if _, err := NewEpisodeSearch(nil, EpisodeSearchOptions{}); !errors.Is(err, ErrNoEpisodeSearcher) {
		t.Fatalf("missing searcher = %v, want ErrNoEpisodeSearcher", err)
	}
	tool, err := NewEpisodeSearch(&fakeEpisodeSearcher{}, EpisodeSearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), Principal{UserID: "u", SessionID: "s", UserMessageID: "t"}, []byte(`{"query":"   "}`)); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("blank query = %v, want ErrInvalidArguments", err)
	}
	long := strings.Repeat("策", 600)
	if _, err := tool.Execute(context.Background(), Principal{UserID: "u", SessionID: "s", UserMessageID: "t"}, []byte(`{"query":"`+long+`"}`)); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("over-long query = %v, want ErrInvalidArguments", err)
	}
	if _, err := tool.Execute(context.Background(), Principal{UserID: "u", SessionID: "s", UserMessageID: "t"}, []byte(`{"query":"x","session":"other"}`)); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("undeclared field = %v, want ErrInvalidArguments", err)
	}
	// The identity comes from the principal, never from the arguments.
	searcher := &fakeEpisodeSearcher{}
	tool, _ = NewEpisodeSearch(searcher, EpisodeSearchOptions{})
	if _, err := tool.Execute(context.Background(), Principal{}, []byte(`{"query":"x"}`)); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("missing principal = %v, want ErrNoPrincipal", err)
	}
}

func TestEpisodeSearchReportsEmptyAndRegistersCitations(t *testing.T) {
	searcher := &fakeEpisodeSearcher{}
	tool, _ := NewEpisodeSearch(searcher, EpisodeSearchOptions{})
	observation, err := tool.Execute(context.Background(),
		Principal{UserID: "u", SessionID: "s", UserMessageID: "turn-1"},
		[]byte(`{"query":"之前关于编码器的决定"}`))
	if err != nil {
		t.Fatal(err)
	}
	if observation.Found {
		t.Fatal("an empty search reported found")
	}
	if !strings.Contains(observation.Text, "没有读到") || !strings.Contains(observation.Text, "不要编造") {
		t.Fatalf("the empty result is not honest: %q", observation.Text)
	}

	card := episodeSearchCard("编解码器选型", "用户确定了选型。", "用户决定采用 JSON", "msg-1", "msg-2")
	searcher.reading = EpisodeReading{Cards: []EpisodeEntry{card}, ScopeNote: "检索范围说明。"}
	observation, err = tool.Execute(context.Background(),
		Principal{UserID: "u", SessionID: "s", UserMessageID: "turn-2"},
		[]byte(`{"query":"之前关于编码器的决定"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Found || len(observation.Sources) != 1 {
		t.Fatalf("observation = found %v with %d sources", observation.Found, len(observation.Sources))
	}
	if observation.Sources[0].Section != "情景经历" {
		t.Fatalf("citation = %+v", observation.Sources[0])
	}
	// The message ids reach the observation: they are what the read tool is
	// asked for. Everything else the card carries stays on the server.
	if !strings.Contains(observation.Text, "message_ids") {
		t.Fatal("the observation does not show the read-back ids")
	}
	if searcher.userID != "u" || searcher.sessionID != "s" || searcher.turnID != "turn-2" {
		t.Fatalf("searcher identity = %s/%s/%s", searcher.userID, searcher.sessionID, searcher.turnID)
	}
}

func TestEpisodeSearchBoundsCardsToTheBudget(t *testing.T) {
	cards := make([]EpisodeEntry, 5)
	for index := range cards {
		cards[index] = episodeSearchCard("卡", "概要", "决定", "msg")
	}
	tool, _ := NewEpisodeSearch(&fakeEpisodeSearcher{reading: EpisodeReading{Cards: cards}}, EpisodeSearchOptions{MaxCards: 3})
	observation, err := tool.Execute(context.Background(), Principal{UserID: "u", SessionID: "s", UserMessageID: "t"}, []byte(`{"query":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Three cards reach the model, so three citations exist: a citation can
	// never claim grounding the model was never shown.
	if len(observation.Sources) != 3 {
		t.Fatalf("citations = %d, want the shown three", len(observation.Sources))
	}
	if !strings.Contains(observation.Text, "2 条因长度上限") {
		t.Fatalf("the omission is not reported: %q", observation.Text)
	}
}

// TestEpisodeSearchGrantsExactlyTheRenderedIDs pins the rendering-to-grant
// boundary: the prefix trap (msg-1 vs msg-10), the cut id, the clipped card and
// the shown id all resolve exactly as the final text reads, not as a substring
// search over it would.
func TestEpisodeSearchGrantsExactlyTheRenderedIDs(t *testing.T) {
	searcher := &capturingSearcher{fakeEpisodeSearcher: fakeEpisodeSearcher{
		reading: EpisodeReading{Cards: []EpisodeEntry{
			episodeSearchCard("编码器选型", "确定了选型。", "用户决定采用流式接口", "msg-10"),
		}},
	}}
	tool, _ := NewEpisodeSearch(searcher, EpisodeSearchOptions{})
	if _, err := tool.Execute(context.Background(),
		Principal{UserID: "u", SessionID: "s", UserMessageID: "t"},
		[]byte(`{"query":"之前的决定"}`)); err != nil {
		t.Fatal(err)
	}
	if len(searcher.granted) != 1 || len(searcher.granted[0].MessageIDs) != 1 ||
		searcher.granted[0].MessageIDs[0] != "msg-10" {
		t.Fatalf("grants = %+v, want exactly msg-10", searcher.granted)
	}
}

func TestEpisodeSearchDoesNotCrossGrantPrefixIDs(t *testing.T) {
	// One assertion citing msg-1 only: a block that legitimately contains
	// msg-1 must not read as containing msg-10, and one citing msg-10 only
	// must not read as containing msg-1. The old substring check failed both.
	cases := []struct {
		name string
		id   string
	}{
		{name: "short id shown, long id absent", id: "msg-1"},
		{name: "long id shown, short id absent", id: "msg-10"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			searcher := &capturingSearcher{fakeEpisodeSearcher: fakeEpisodeSearcher{
				reading: EpisodeReading{Cards: []EpisodeEntry{
					episodeSearchCard("编码器选型", "确定了选型。", "用户决定采用流式接口", testCase.id),
				}},
			}}
			tool, _ := NewEpisodeSearch(searcher, EpisodeSearchOptions{})
			if _, err := tool.Execute(context.Background(),
				Principal{UserID: "u", SessionID: "s", UserMessageID: "t"},
				[]byte(`{"query":"之前的决定"}`)); err != nil {
				t.Fatal(err)
			}
			if len(searcher.granted) != 1 {
				t.Fatalf("grants = %+v, want one card's slice", searcher.granted)
			}
			got := searcher.granted[0].MessageIDs
			if len(got) != 1 || got[0] != testCase.id {
				t.Fatalf("granted ids = %v, want exactly [%s]", got, testCase.id)
			}
		})
	}
}

func TestEpisodeSearchGrantsNothingForTruncatedOrClippedCards(t *testing.T) {
	t.Run("the id list is cut mid-way", func(t *testing.T) {
		// The cut lands exactly where the first id ends: whatever comes after
		// is not shown, so only that id stays granted.
		card := episodeSearchCard("编码器选型", "确定了选型。", "用户决定采用流动接口", "first-shown", "cut-off-mid")
		probe, _ := NewEpisodeSearch(&fakeEpisodeSearcher{reading: EpisodeReading{Cards: []EpisodeEntry{card}}}, EpisodeSearchOptions{})
		rendered := probe.renderCard(1, card)
		cut := strings.Index(rendered.block, "first-shown") + len("first-shown")
		cardRunes := utf8.RuneCountInString(rendered.block[:cut])
		searcher := &capturingSearcher{fakeEpisodeSearcher: fakeEpisodeSearcher{
			reading: EpisodeReading{Cards: []EpisodeEntry{card}},
		}}
		tool, _ := NewEpisodeSearch(searcher, EpisodeSearchOptions{CardRunes: cardRunes})
		observation, err := tool.Execute(context.Background(),
			Principal{UserID: "u", SessionID: "s", UserMessageID: "t"},
			[]byte(`{"query":"之前的决定"}`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(observation.Text, "first-shown") {
			t.Fatalf("the first id should survive the cut: %q", observation.Text)
		}
		if strings.Contains(observation.Text, "cut-off-mid") {
			t.Fatalf("the test cut does not actually fall on the second id: %q", observation.Text)
		}
		if len(searcher.granted) != 1 || len(searcher.granted[0].MessageIDs) != 1 ||
			searcher.granted[0].MessageIDs[0] != "first-shown" {
			t.Fatalf("grants = %+v, want exactly first-shown", searcher.granted)
		}
	})
	t.Run("CardRunes cuts inside an id", func(t *testing.T) {
		// The cut lands in the middle of the one id the card cites: a partial
		// id is on the wire, the whole id is not, and a partial id grants
		// nothing at all.
		const target = "inside-cut-target"
		card := episodeSearchCard("编码器选型", "确定了选型。", "用户决定采用流动接口", target)
		probe, _ := NewEpisodeSearch(&fakeEpisodeSearcher{reading: EpisodeReading{Cards: []EpisodeEntry{card}}}, EpisodeSearchOptions{})
		rendered := probe.renderCard(1, card)
		idStart := utf8.RuneCountInString(rendered.block[:strings.Index(rendered.block, target)])
		searcher := &capturingSearcher{fakeEpisodeSearcher: fakeEpisodeSearcher{
			reading: EpisodeReading{Cards: []EpisodeEntry{card}},
		}}
		tool, _ := NewEpisodeSearch(searcher, EpisodeSearchOptions{CardRunes: idStart + 6})
		observation, err := tool.Execute(context.Background(),
			Principal{UserID: "u", SessionID: "s", UserMessageID: "t"},
			[]byte(`{"query":"之前的决定"}`))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(observation.Text, target) {
			t.Fatalf("the cut did not land inside the id: %q", observation.Text)
		}
		if len(searcher.granted) != 0 {
			t.Fatalf("a partially rendered id was granted: %+v", searcher.granted)
		}
	})
	t.Run("ObservationRunes cuts inside an id", func(t *testing.T) {
		// Same cut, different boundary: the per-card budget leaves the card
		// whole and the whole-observation budget does the cutting - the id
		// must stay ungranted here too.
		const target = "inside-cut-target"
		card := episodeSearchCard("编码器选型", "确定了选型。", "用户决定采用流动接口", target)
		probe, _ := NewEpisodeSearch(&fakeEpisodeSearcher{reading: EpisodeReading{Cards: []EpisodeEntry{card}}}, EpisodeSearchOptions{})
		rendered := probe.renderCard(1, card)
		idStart := utf8.RuneCountInString(rendered.block[:strings.Index(rendered.block, target)])
		searcher := &capturingSearcher{fakeEpisodeSearcher: fakeEpisodeSearcher{
			reading: EpisodeReading{Cards: []EpisodeEntry{card}},
		}}
		tool, _ := NewEpisodeSearch(searcher, EpisodeSearchOptions{ObservationRunes: idStart + 6})
		observation, err := tool.Execute(context.Background(),
			Principal{UserID: "u", SessionID: "s", UserMessageID: "t"},
			[]byte(`{"query":"之前的决定"}`))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(observation.Text, target) {
			t.Fatalf("the cut did not land inside the id: %q", observation.Text)
		}
		if len(searcher.granted) != 0 {
			t.Fatalf("a partially rendered id was granted: %+v", searcher.granted)
		}
	})
	t.Run("the whole card is clipped away", func(t *testing.T) {
		shown := episodeSearchCard(" shown", "概要", "决定", "msg-shown")
		shown.Title = "放得下的卡"
		cut := episodeSearchCard("放不下的卡", "概要", "决定", "msg-cut")
		searcher := &capturingSearcher{fakeEpisodeSearcher: fakeEpisodeSearcher{
			reading: EpisodeReading{Cards: []EpisodeEntry{shown, cut}},
		}}
		tool, _ := NewEpisodeSearch(searcher, EpisodeSearchOptions{MaxCards: 1})
		if _, err := tool.Execute(context.Background(),
			Principal{UserID: "u", SessionID: "s", UserMessageID: "t"},
			[]byte(`{"query":"之前的决定"}`)); err != nil {
			t.Fatal(err)
		}
		if len(searcher.granted) != 1 || searcher.granted[0].EpisodeID != "" ||
			len(searcher.granted[0].MessageIDs) != 1 || searcher.granted[0].MessageIDs[0] != "msg-shown" {
			t.Fatalf("grants = %+v, want only the shown card's id", searcher.granted)
		}
	})
}

func TestEpisodeSearchFailsWhenAuthorizationFails(t *testing.T) {
	boom := errors.New("store unavailable")
	searcher := &capturingSearcher{
		fakeEpisodeSearcher: fakeEpisodeSearcher{reading: EpisodeReading{
			Cards: []EpisodeEntry{episodeSearchCard("编码器选型", "概要", "决定", "msg-1")},
		}},
		authError: boom,
	}
	tool, _ := NewEpisodeSearch(searcher, EpisodeSearchOptions{})
	observation, err := tool.Execute(context.Background(),
		Principal{UserID: "u", SessionID: "s", UserMessageID: "t"},
		[]byte(`{"query":"之前的决定"}`))
	if !errors.Is(err, ErrEpisodeAuthorization) {
		t.Fatalf("authorization failure = %v, want ErrEpisodeAuthorization", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("the underlying cause is not wrapped: %v", err)
	}
	if observation.Text != "" || len(observation.Sources) != 0 {
		t.Fatalf("a failed authorization still returned an observation: %+v", observation)
	}
}

// fakeHistoryReader is the read-back seam the tool tests drive.
type fakeHistoryReader struct {
	ids    []string
	turnID string
	result HistoryReading
	err    error
}

func (f *fakeHistoryReader) ReadEpisodeHistory(_ context.Context, _, _, turnID string, messageIDs []string) (HistoryReading, error) {
	f.turnID = turnID
	f.ids = append([]string(nil), messageIDs...)
	return f.result, f.err
}

func TestReadHistoryMessageRefusesMissingReaderAndBadIds(t *testing.T) {
	if _, err := NewReadHistoryMessage(nil, HistoryReadOptions{}); !errors.Is(err, ErrNoHistoryReader) {
		t.Fatalf("missing reader = %v, want ErrNoHistoryReader", err)
	}
	tool, err := NewReadHistoryMessage(&fakeHistoryReader{}, HistoryReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{UserID: "u", SessionID: "s", UserMessageID: "t"}
	if _, err := tool.Execute(context.Background(), principal, []byte(`{}`)); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("no ids = %v, want ErrInvalidArguments", err)
	}
	if _, err := tool.Execute(context.Background(), principal, []byte(`{"message_ids":["a","b","c","d","e","f"]}`)); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("too many ids = %v, want ErrInvalidArguments", err)
	}
	if _, err := tool.Execute(context.Background(), principal, []byte(`{"message_ids":[" "]}`)); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("blank id = %v, want ErrInvalidArguments", err)
	}
}

func TestReadHistoryMessageRendersMessagesAndRefusals(t *testing.T) {
	reader := &fakeHistoryReader{result: HistoryReading{
		Messages: []HistoryMessage{{
			MessageID: "msg-1", Role: "user", SentAt: "2026-01-02T15:04:05Z",
			Content: "我决定用 JSON 编解码器。", EpisodeTitle: "编解码器选型",
			Citation: domain.Source{SourceID: "src-history-msg-1"},
		}},
		Refusals: []HistoryRefusal{{MessageID: "msg-9", Reason: "message_not_cited_this_turn"}},
	}}
	tool, _ := NewReadHistoryMessage(reader, HistoryReadOptions{})
	observation, err := tool.Execute(context.Background(),
		Principal{UserID: "u", SessionID: "s", UserMessageID: "turn-7"},
		[]byte(`{"message_ids":["msg-1","msg-9"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Found || len(observation.Sources) != 1 {
		t.Fatalf("observation = found %v with %d sources", observation.Found, len(observation.Sources))
	}
	if !strings.Contains(observation.Text, "我决定用 JSON 编解码器") {
		t.Fatalf("the read-back lost its content: %q", observation.Text)
	}
	if !strings.Contains(observation.Text, "msg-9（message_not_cited_this_turn）") {
		t.Fatalf("the refusal does not name its reason: %q", observation.Text)
	}
	if reader.turnID != "turn-7" {
		t.Fatalf("the read carried turn %q", reader.turnID)
	}
}

func TestReadHistoryMessageReportsNothingReadable(t *testing.T) {
	tool, _ := NewReadHistoryMessage(&fakeHistoryReader{result: HistoryReading{
		Refusals: []HistoryRefusal{{MessageID: "msg-1", Reason: "episode_not_found"}},
	}}, HistoryReadOptions{})
	observation, err := tool.Execute(context.Background(),
		Principal{UserID: "u", SessionID: "s", UserMessageID: "t"}, []byte(`{"message_ids":["msg-1"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if observation.Found || len(observation.Sources) != 0 {
		t.Fatalf("a refused read produced citations: %+v", observation)
	}
	if !strings.Contains(observation.Text, "没有读到") {
		t.Fatalf("the refusal is not stated: %q", observation.Text)
	}
}

// The two tools' declared schemas stay JSON objects with the exact keys the
// argument structs decode, so the advertised contract and the parser cannot
// drift apart.
func TestEpisodeToolSchemasAreDeclaredObjects(t *testing.T) {
	search, _ := NewEpisodeSearch(&fakeEpisodeSearcher{}, EpisodeSearchOptions{})
	history, _ := NewReadHistoryMessage(&fakeHistoryReader{}, HistoryReadOptions{})
	for _, tool := range []Tool{search, history} {
		var schema map[string]any
		if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
			t.Fatalf("%s schema is not JSON: %v", tool.Name(), err)
		}
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Fatalf("%s schema shape = %v", tool.Name(), schema)
		}
	}
}

func TestEpisodeSearchUsesFinalDisplayRangeForDedup(t *testing.T) {
	search := &fakeEpisodeSearcher{reading: EpisodeReading{Cards: []EpisodeEntry{{EpisodeID: "ep", EpisodeVersion: 1, Title: "same", Synopsis: "short"}}}}
	tool, err := NewEpisodeSearch(search, EpisodeSearchOptions{MaxCards: 1, CardRunes: 200, ObservationRunes: 200})
	if err != nil {
		t.Fatal(err)
	}
	e := contextwindow.NewEvidence(1000)
	ctx := contextwindow.WithEvidence(context.Background(), e)
	principal := Principal{UserID: "u", SessionID: "s", UserMessageID: "m"}
	if _, err := tool.Execute(ctx, principal, json.RawMessage(`{"query":"one"}`)); err != nil {
		t.Fatal(err)
	}
	used := e.Used()
	if _, err := tool.Execute(ctx, principal, json.RawMessage(`{"query":"two"}`)); err != nil {
		t.Fatal(err)
	}
	if e.Used() != used {
		t.Fatalf("same final display range charged twice: %d -> %d", used, e.Used())
	}
}
