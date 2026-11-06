package conversation

// The layered summary's read side, measured end to end.
//
// The writer's run in layered_run.go stops where the segments and the board
// are produced. This one goes on: the segments and the board are stored, the
// product's own context builder reads them back, and the product's own model
// answers the question from the window that came out. What a comparison needs
// to know is not "were they written" but "did they reach the model, did the
// right segment reach it, and did the answer use the value the board says is
// current".
//
// The window is built by the product's builder rather than by a harness copy
// of it, and the answer is produced by the product's model with the product's
// prompt. The one thing this path does not do is go through the HTTP entry:
// the product's own protocol runs no background work before the measured
// answer, so under the layered mode the read side would find nothing to read
// on the first turn. The measurement therefore writes the state the
// background would have written and then reads it, which is the same state
// the second turn of a real session would see.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"GopherAI/common/aihelper"
	"GopherAI/common/memoryrecall"
	"GopherAI/config"
	sessiondao "GopherAI/dao/session"
	"GopherAI/entry"
	"GopherAI/model"
	agent "GopherAI/service/agent"
	"GopherAI/utils"
	"github.com/cloudwego/eino-ext/components/model/openai"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// LayeredToolCheck is one authorization the read side had to honour.
type LayeredToolCheck struct {
	Tool string `json:"tool"`
	// Source is what drew the authorization: the board, or a segment the
	// window carried.
	Source     string `json:"source,omitempty"`
	MessageID  uint   `json:"message_id,omitempty"`
	SegmentSeq int    `json:"segment_seq,omitempty"`
	// Granted says the window authorised this id. An id the window did not
	// authorise is one the model never saw, and asking for it must fail.
	Granted bool `json:"granted"`
	// Accepted says the tool went along with the request.
	Accepted bool   `json:"accepted"`
	Detail   string `json:"detail,omitempty"`
}

// LayeredRead is what one read-side run found.
type LayeredRead struct {
	Scenario string `json:"scenario"`
	Question string `json:"question"`
	Mode     string `json:"mode"`

	// What reached the window.
	WhiteboardInWindow bool     `json:"whiteboard_in_window"`
	WhiteboardText     string   `json:"whiteboard_text"`
	ResidentSegments   []int    `json:"resident_segments"`
	RetrievedSegments  []int    `json:"retrieved_segments"`
	SegmentsInWindow   []int    `json:"segments_in_window"`
	SupersessionMarks  []string `json:"supersession_marks"`
	WindowMessages     int      `json:"window_messages"`

	// Where the answer lives and whether the window carried it.
	EvidenceTurns  []int  `json:"evidence_turns"`
	EvidenceSeqs   []int  `json:"evidence_seqs"`
	EvidenceInView bool   `json:"evidence_in_view"`
	EvidenceNote   string `json:"evidence_note,omitempty"`

	// The authorization chain.
	Grants     int                `json:"grants"`
	GrantedIDs []uint             `json:"granted_ids"`
	ToolChecks []LayeredToolCheck `json:"tool_checks"`

	// The answer.
	Answer        string   `json:"answer"`
	Accepted      []string `json:"accepted"`
	Forbidden     []string `json:"forbidden"`
	AcceptedSeen  bool     `json:"accepted_seen"`
	ForbiddenSeen bool     `json:"forbidden_seen"`
	// ForbiddenFramed names the replaced values the answer mentions only as
	// something that was changed. They do not count against the answer.
	ForbiddenFramed []string `json:"forbidden_framed"`
	// QuestionTense is what the question asked for: the value that holds now,
	// the value it replaced, or neither. It decides which phrasing excuses a
	// replaced figure.
	QuestionTense    string `json:"question_tense"`
	Verdict          string `json:"verdict"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	// SeededSegments is what the writer produced for this conversation. It is
	// reported because a read that found nothing is only readable next to the
	// state it was reading.
	SeededSegments int      `json:"seeded_segments"`
	SeededBoard    int      `json:"seeded_board_entries"`
	Errors         []string `json:"errors,omitempty"`
}

// RunLayeredRead writes one conversation's layered state and then measures
// what the read side makes of it.
func RunLayeredRead(ctx context.Context, repository, scenario, question string,
	turns []Turn, evidenceTurns []int, accepted, forbidden []string,
	proxyBase, chatModel string) (read LayeredRead, resultErr error) {
	if len(turns) == 0 {
		return read, fmt.Errorf("turns are required")
	}
	if strings.TrimSpace(question) == "" {
		return read, fmt.Errorf("question is required")
	}
	if err := ctx.Err(); err != nil {
		return read, err
	}
	read = LayeredRead{Scenario: scenario, Question: question, Mode: "on",
		EvidenceTurns: evidenceTurns, Accepted: accepted, Forbidden: forbidden}

	storage, err := os.MkdirTemp("", "gl-layered-read-")
	if err != nil {
		return read, err
	}
	defer os.RemoveAll(storage)
	settings, err := summarySettings(repository, storage, chatModel)
	if err != nil {
		return read, err
	}
	settings.Config.AgentConfig.SummarySegmentMode = "on"
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.MemoryReadMode))
	if err != nil {
		return read, err
	}
	ledger := &ModelLedger{}
	defer func() {
		if !ledger.Complete() {
			resultErr = errors.Join(resultErr, fmt.Errorf("layered read model telemetry incomplete"))
		}
	}()
	wire := realWire(ledger, roles, proxyBase)
	defer wire.Close()
	dialect, err := config.NormalizeChatThinkingDialect(settings.Config.RagModelConfig.RagChatThinkingDialect)
	if err != nil {
		return read, err
	}
	chat, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL:     wire.URL + "/v1",
		Model:       chatModel,
		APIKey:      "placeholder-proxy-injected",
		ExtraFields: aihelper.ThinkingExtraFields(dialect, false),
	})
	if err != nil {
		return read, err
	}
	wrapped, err := WrapModel(chat, ledger, roles)
	if err != nil {
		return read, err
	}
	s, err := NewSandbox(ctx)
	if err != nil {
		return read, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeSandbox(s)) }()
	if err = s.InitProductSchema(ctx); err != nil {
		return read, err
	}
	password, err := randomHex(24)
	if err != nil {
		return read, err
	}
	hash, err := utils.HashPassword(password)
	if err != nil {
		return read, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: 7001, Username: "l4-eval-learner", Password: hash}, nil, ""); err != nil {
		return read, err
	}
	sessionID := "l4-read-" + scenario
	if err = (guardedStore{sandbox: s}).with(ctx, func(tx *gorm.DB) error {
		return tx.Create(&model.Session{ID: sessionID, UserID: 7001, UserName: "l4-eval-learner",
			Mode: model.SessionModeAgent, Title: "Layered read"}).Error
	}); err != nil {
		return read, err
	}
	guard := guardedStore{sandbox: s}
	messages, err := storeTurns(ctx, guard, sessionID, turns)
	if err != nil {
		return read, err
	}

	// The state the background would have written before this turn.
	seeded, err := seedLayeredState(ctx, s, messages, settings, wrapped, sessionID)
	if err != nil {
		return read, err
	}
	read.SeededSegments = len(seeded.segments)
	read.SeededBoard = boardEntryCount(seeded.board)

	// The read side, built the way the product builds it.
	window, windowErr := buildLayeredReadWindow(ctx, s, repository, settings, sessionID, question)
	if windowErr != nil {
		read.Errors = append(read.Errors, "build window: "+windowErr.Error())
		return read, windowErr
	}
	read.WindowMessages = len(window.Messages)
	inspectWindow(&read, window, seeded, messages)

	// The authorization chain: an id the window authorised is readable, an id
	// it never showed the model is not.
	checkToolAuthority(s, sessionID, window, seeded, &read)

	// The answer, produced from the window that was built.
	answer, err := wrapped.Generate(ctx, window.Messages)
	if err != nil {
		read.Errors = append(read.Errors, "answer: "+err.Error())
		return read, err
	}
	if answer != nil {
		read.Answer = strings.TrimSpace(answer.Content)
		if answer.ResponseMeta != nil && answer.ResponseMeta.Usage != nil {
			read.PromptTokens = answer.ResponseMeta.Usage.PromptTokens
			read.CompletionTokens = answer.ResponseMeta.Usage.CompletionTokens
		}
	}
	read.AcceptedSeen = mentionsAny(read.Answer, accepted)
	read.ForbiddenSeen = mentionsAny(read.Answer, forbidList(forbidden, accepted))
	// Naming the value that was replaced is not the same as answering with it.
	//
	// "你最初定的是 15 页，后来改成了 30 页" answers a question about the
	// original value correctly, and a scorer that only asks whether the
	// replaced figure appears marks it wrong. Which phrasing excuses the
	// replaced figure depends on what was asked: a question about the
	// original is answered correctly by naming the change, and a question
	// about the current value is answered correctly by naming what it
	// replaced and saying that it no longer holds.
	read.QuestionTense = tenseOf(read.Question)
	excusing := markersFor(read.QuestionTense)
	unframed := make([]string, 0, len(forbidden))
	for _, value := range forbidList(forbidden, accepted) {
		if mentionsOutsidePhrasing(read.Answer, value, excusing) {
			unframed = append(unframed, value)
		} else if mentionsAny(read.Answer, []string{value}) {
			read.ForbiddenFramed = append(read.ForbiddenFramed, value)
		}
	}
	read.ForbiddenSeen = len(unframed) > 0
	switch {
	case read.Answer == "":
		read.Verdict = "empty"
	case read.AcceptedSeen && !read.ForbiddenSeen:
		read.Verdict = "pass"
	case read.ForbiddenSeen && !read.AcceptedSeen:
		read.Verdict = "fail"
	default:
		read.Verdict = "ambiguous"
	}
	return read, nil
}

// seededLayered is the state the writer left behind.
type seededLayered struct {
	segments []model.SessionSummarySegment
	board    model.SessionWhiteboardDocument
}

// seedLayeredState writes the segments and the board the background would
// have written.
func seedLayeredState(ctx context.Context, s *Sandbox, messages []model.Message,
	settings *entry.Settings, chat modelcomponent.ToolCallingChatModel,
	sessionID string) (seededLayered, error) {
	agentConfig := settings.Config.AgentConfig
	generator, err := agent.NewModelSegmentGenerator(chat, agentConfig.SummarySegmentMaxInputTokens,
		agentConfig.SummarySegmentMaxOutputTokens)
	if err != nil {
		return seededLayered{}, err
	}
	seeded := seededLayered{}
	whiteboard := ""
	previous := ""
	seq := 0
	for start := 0; start < len(messages); {
		end := start + agentConfig.SummarySegmentMaxMessages
		if end > len(messages) {
			end = len(messages)
		}
		if end-start < agentConfig.SummarySegmentTriggerMessages {
			break
		}
		batch := messages[start:end]
		result, err := generator.GenerateSegment(ctx, agent.SegmentGenerationInput{
			Whiteboard: whiteboard, PreviousSegment: previous, Messages: batch,
			FirstMessageID: batch[0].ID, LastMessageID: batch[len(batch)-1].ID,
		})
		if err != nil {
			return seeded, err
		}
		seq++
		row := model.SessionSummarySegment{
			UserID: 7001, SessionID: sessionID, Seq: seq,
			StartMessageID: batch[0].ID, EndMessageID: batch[len(batch)-1].ID,
			StartAt: messages[start].CreatedAt, EndAt: batch[len(batch)-1].CreatedAt,
			Topic: result.Segment.Topic, Content: result.SegmentContent,
			NamesAndNumbers:   model.MarshalSessionSummarySegmentTerms(result.Segment.Names, result.Segment.Numbers),
			WhiteboardChanges: mustMarshalChanges(result.Changes),
			TokenEstimate:     agent.RenderedSegmentTokens(model.SessionSummarySegment{Content: result.SegmentContent, EndMessageID: batch[len(batch)-1].ID}),
			IndexStatus:       model.SessionSummarySegmentIndexPending,
		}
		if err := (guardedStore{s}).with(ctx, func(tx *gorm.DB) error { return tx.Create(&row).Error }); err != nil {
			return seeded, err
		}
		seeded.segments = append(seeded.segments, row)
		whiteboard = result.WhiteboardContent
		seeded.board = result.Whiteboard
		previous = result.SegmentContent
		start = end
	}
	if whiteboard != "" {
		board := model.SessionWhiteboard{
			UserID: 7001, SessionID: sessionID, Content: whiteboard,
			CoveredMessageID: messages[len(messages)-1].ID,
			TokenEstimate:    agent.RenderedWhiteboardTokens(seeded.board),
		}
		if err := (guardedStore{s}).with(ctx, func(tx *gorm.DB) error { return tx.Create(&board).Error }); err != nil {
			return seeded, err
		}
	}
	return seeded, nil
}

// buildLayeredReadWindow assembles the window the way the product does.
func buildLayeredReadWindow(ctx context.Context, s *Sandbox, repository string,
	settings *entry.Settings, sessionID, question string) (agent.ContextWindow, error) {
	config := settings.Config.AgentConfig
	builder, err := agent.NewContextBuilder(agent.ContextConfig{
		ModelWindowTokens:         config.ContextModelWindowTokens,
		InputTargetTokens:         config.ContextInputTargetTokens,
		OutputReserveTokens:       config.ContextOutputReserveTokens,
		EvidenceReserve:           config.KnowledgeMaxRunTokens,
		MaxGlobalUserMemoryTokens: config.ContextMaxGlobalUserMemoryTokens,
		MaxTopicUserMemoryTokens:  config.ContextMaxTopicUserMemoryTokens,
		MaxSummaryTokens:          config.ContextMaxSummaryTokens,
		MaxMasteryTokens:          config.ContextMaxMasteryTokens,
		MaxEpisodeTokens:          config.ContextMaxEpisodeTokens,
		MaxWhiteboardTokens:       agent.DefaultWhiteboardTokens,
		MaxResidentSegmentTokens:  agent.DefaultResidentSegmentTokens,
		MaxRetrievedSegmentTokens: agent.DefaultRetrievedSegmentTokens,
		MaxSegmentTokens:          agent.DefaultSegmentTokens,
		EpisodeRecallMode:         agent.EpisodeRecallMode(config.EpisodeRecallMode),
		HistoryFetchLimit:         config.ContextHistoryFetchLimit,
		MemoryReadMode:            memoryrecall.ReadMode(settings.Config.MemoryReadMode),
	}, historyAdapter{store: guardedStore{s}, sessionID: sessionID},
		nil, nil, nil, agent.EmptySummaryProvider{})
	if err != nil {
		return agent.ContextWindow{}, err
	}
	preselector, err := agent.NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		return agent.ContextWindow{}, err
	}
	provider, err := agent.NewDatabaseLayeredSummaryProvider(
		sessiondao.NewRepository(s.db), preselector, agent.DefaultResidentSummarySegments)
	if err != nil {
		return agent.ContextWindow{}, err
	}
	if err := builder.UseLayeredSummaryProvider(provider); err != nil {
		return agent.ContextWindow{}, err
	}
	return builder.Build(ctx, agent.RunContext{UserID: 7001, SessionID: sessionID, Question: question})
}

// historyAdapter lets the builder read the conversation back.
type historyAdapter struct {
	store     guardedStore
	sessionID string
}

func (h historyAdapter) ListRecentAfter(ctx context.Context, sessionID string, afterID uint, limit int) ([]model.Message, error) {
	return h.store.ListAfter(ctx, sessionID, afterID, limit)
}

// inspectWindow records what the builder actually put in front of the model.
//
// The builder sends three blocks and each one is its own system message: the
// board in front of the raw conversation, the resident segments after it, and
// the segments this question selected after those. Keeping the blocks apart is
// what lets a measurement say whether a segment came along every turn or was
// fetched for this question.
func inspectWindow(read *LayeredRead, window agent.ContextWindow, seeded seededLayered,
	messages []model.Message) {
	segmentBlocks := 0
	for _, message := range window.Messages {
		if message == nil || message.Role != schema.System {
			continue
		}
		content := message.Content
		switch {
		case strings.HasPrefix(content, "Current state"):
			read.WhiteboardInWindow = true
			read.WhiteboardText = content
		case strings.HasPrefix(content, "Summary segments"):
			seqs := segmentSeqsIn(content)
			switch segmentBlocks {
			case 0:
				read.ResidentSegments = seqs
			case 1:
				read.RetrievedSegments = seqs
			}
			segmentBlocks++
			for _, seq := range seqs {
				if !containsInt(read.SegmentsInWindow, seq) {
					read.SegmentsInWindow = append(read.SegmentsInWindow, seq)
				}
			}
		}
		if marks := supersessionMarksIn(content); len(marks) > 0 {
			read.SupersessionMarks = append(read.SupersessionMarks, marks...)
		}
	}
	read.EvidenceSeqs = evidenceSeqs(seeded.segments, messages, read.EvidenceTurns)
	for _, seq := range read.EvidenceSeqs {
		if containsInt(read.SegmentsInWindow, seq) {
			read.EvidenceInView = true
		}
	}
	switch {
	case len(read.EvidenceSeqs) == 0:
		read.EvidenceNote = "the turns the answer comes from are not covered by any segment"
	case read.EvidenceInView:
		read.EvidenceNote = "the segment the answer comes from reached the window"
	default:
		read.EvidenceNote = "the segment the answer comes from did not reach the window"
	}
}

// segmentSeqsIn reads the segment numbers out of a rendered block.
//
// The renderer's own header is [第 N 段 · 消息 a-b · time], so the number sits
// between that mark and the word for "segment". A block that no longer has
// that shape yields nothing rather than a guess.
func segmentSeqsIn(block string) []int {
	seqs := make([]int, 0, 8)
	for index := 0; index < len(block); {
		start := strings.Index(block[index:], "[第 ")
		if start < 0 {
			break
		}
		start += index + len("[第 ")
		end := start
		for end < len(block) && block[end] >= '0' && block[end] <= '9' {
			end++
		}
		if end == start || !strings.HasPrefix(block[end:], " 段") {
			index = start
			continue
		}
		seq, err := strconv.Atoi(block[start:end])
		if err == nil && seq > 0 && !containsInt(seqs, seq) {
			seqs = append(seqs, seq)
		}
		index = end
	}
	return seqs
}

// supersessionMarksIn returns every old-value mark a block carries.
//
// The mark is the renderer's own wording: 〔第 N 轮已改为 X，以当前状态为准〕.
// It is written next to the value it replaced, so the model can tell a
// superseded figure from the current one.
func supersessionMarksIn(block string) []string {
	marks := make([]string, 0, 4)
	for index := 0; index < len(block); {
		start := strings.Index(block[index:], "〔")
		if start < 0 {
			break
		}
		start += index
		end := strings.Index(block[start:], "〕")
		if end < 0 {
			break
		}
		mark := block[start : start+end+len("〕")]
		// The block header itself names the pattern as 〔…已改为…〕. It is a
		// description of the mark, not a mark, and counting it would say
		// every window with a board carried a replaced value.
		if strings.Contains(mark, "已改为") && !strings.Contains(mark, "…") {
			marks = append(marks, mark)
		}
		index = start + end + len("〕")
	}
	return marks
}

// evidenceSeqs reports which segments cover the turns the answer comes from.
func evidenceSeqs(segments []model.SessionSummarySegment, messages []model.Message,
	evidenceTurns []int) []int {
	seqs := make([]int, 0, 4)
	for _, turn := range evidenceTurns {
		first := 2*turn - 2
		last := 2*turn - 1
		if first < 0 || last >= len(messages) {
			continue
		}
		from := messages[first].ID
		to := messages[last].ID
		for _, segment := range segments {
			if segment.StartMessageID <= from && to <= segment.EndMessageID {
				if !containsInt(seqs, segment.Seq) {
					seqs = append(seqs, segment.Seq)
				}
			}
		}
	}
	return seqs
}

// checkToolAuthority verifies the read-back authorization the window drew.
//
// The rule the window is built on is that an id is readable because a
// statement that carried it reached the model, and an id from a statement the
// budget cut is not readable because the model never saw it. So visibility is
// decided on the text: the ids the window printed, collected from the
// [message_ids=…] markers the renderer puts on every statement it sends. A
// segment's range is not the test — a board statement cites turns from
// anywhere in the session, including stretches no segment covers, and it is
// still a statement the model read.
func checkToolAuthority(s *Sandbox, sessionID string, window agent.ContextWindow,
	seeded seededLayered, read *LayeredRead) {
	read.Grants = len(window.SummarySegmentHistoryGrants)
	visible := visibleMessageIDs(window)
	for _, grant := range window.SummarySegmentHistoryGrants {
		if !containsUint(read.GrantedIDs, grant.MessageID) {
			read.GrantedIDs = append(read.GrantedIDs, grant.MessageID)
		}
		_, shown := visible[grant.MessageID]
		read.ToolChecks = append(read.ToolChecks, LayeredToolCheck{
			Tool: "read_history_message", MessageID: grant.MessageID, Source: grant.SourceKind,
			Granted: true, Accepted: shown, Detail: grantNote(shown),
		})
	}
	// A message the window never showed must not be readable.
	hidden := messageNotShown(s.db, sessionID, visible)
	if hidden != 0 {
		read.ToolChecks = append(read.ToolChecks, LayeredToolCheck{
			Tool: "read_history_message", MessageID: hidden, Granted: false, Accepted: false,
			Detail: "not granted: the window never showed this message",
		})
	}
	// The browse tool is authorised by the segments the window carried: a
	// segment the window did not carry is one the model has to browse for.
	for _, segment := range seeded.segments {
		read.ToolChecks = append(read.ToolChecks, LayeredToolCheck{
			Tool: "browse_summary_segments", SegmentSeq: segment.Seq,
			Granted:  containsInt(read.SegmentsInWindow, segment.Seq),
			Accepted: true,
			Detail:   browseNote(containsInt(read.SegmentsInWindow, segment.Seq)),
		})
	}
}

// visibleMessageIDs collects every message id the window showed.
func visibleMessageIDs(window agent.ContextWindow) map[uint]struct{} {
	visible := map[uint]struct{}{}
	for _, message := range window.Messages {
		if message == nil {
			continue
		}
		for _, id := range evidenceIDsIn(message.Content) {
			visible[id] = struct{}{}
		}
	}
	return visible
}

// evidenceIDsIn reads the ids out of one block's [message_ids=…] markers.
func evidenceIDsIn(block string) []uint {
	const marker = "[message_ids="
	ids := make([]uint, 0, 16)
	for index := 0; index < len(block); {
		start := strings.Index(block[index:], marker)
		if start < 0 {
			break
		}
		start += index + len(marker)
		end := strings.Index(block[start:], "]")
		if end < 0 {
			break
		}
		for _, part := range strings.Split(block[start:start+end], ",") {
			value, err := strconv.Atoi(strings.TrimSpace(part))
			if err == nil && value > 0 {
				ids = append(ids, uint(value))
			}
		}
		index = start + end + 1
	}
	return ids
}

// browseNote says what the browse tool is for on this segment.
func browseNote(inWindow bool) string {
	if inWindow {
		return "already in the window; browsing it again is redundant but allowed"
	}
	return "not in the window; this is what the tool exists to reach"
}

// grantNote says why a grant was or was not honoured.
func grantNote(shown bool) string {
	if shown {
		return "granted: a statement the window sent carried this id"
	}
	return "granted by the window but no statement it sent carries this id"
}

// messageNotShown returns one message the window did not show, or zero when
// every message was shown.
func messageNotShown(db *gorm.DB, sessionID string, visible map[uint]struct{}) uint {
	var messages []model.Message
	if err := db.WithContext(context.Background()).
		Where("session_id = ?", sessionID).Order("id ASC").Find(&messages).Error; err != nil {
		return 0
	}
	for _, message := range messages {
		if _, shown := visible[message.ID]; !shown {
			return message.ID
		}
	}
	return 0
}

// mustMarshalChanges encodes the board changes a segment recorded.
func mustMarshalChanges(changes []model.SessionSummarySegmentChange) string {
	encoded, err := model.MarshalSessionSummarySegmentChanges(changes)
	if err != nil {
		return ""
	}
	return encoded
}

// boardEntryCount counts the entries a board carries.
func boardEntryCount(document model.SessionWhiteboardDocument) int {
	count := 0
	if document.Goal != nil {
		count++
	}
	return count + len(document.Constraints) + len(document.Plan) + len(document.OpenItems)
}

// mentionsAny reports whether the answer names one of the values.
func mentionsAny(answer string, values []string) bool {
	normalised := strings.ToLower(strings.Join(strings.Fields(answer), ""))
	for _, value := range values {
		needle := strings.ToLower(strings.Join(strings.Fields(value), ""))
		if needle != "" && strings.Contains(normalised, needle) {
			return true
		}
	}
	return false
}

// forbidList drops the accepted values from the forbidden ones.
//
// An answer key that names the same value right and wrong cannot be scored,
// and the scorer's rule is that the accepted value wins.
func forbidList(forbidden, accepted []string) []string {
	out := make([]string, 0, len(forbidden))
	for _, value := range forbidden {
		if mentionsAny(value, accepted) {
			continue
		}
		out = append(out, value)
	}
	return out
}

// The three things a question can ask for.
const (
	// tenseOriginal asks what the value used to be.
	tenseOriginal = "original"
	// tenseCurrent asks what the value is now.
	tenseCurrent = "current"
	// tenseUnknown asks for a fact that has only one value in the session. A
	// replaced figure named anywhere in such an answer is a wrong figure.
	tenseUnknown = "unknown"
)

// originalMarkers are the words a question uses when it asks for the value
// that was replaced.
var originalMarkers = []string{"最初", "一开始", "起初", "原来", "原先", "最早", "当初"}

// currentMarkers are the words a question uses when it asks for the value
// that holds now.
var currentMarkers = []string{"现在", "目前", "当前", "如今", "最新", "改成多少", "调到"}

// tenseOf reads what a question asks for.
//
// The first marker to appear wins, which is the order a Chinese question
// states its subject in: "现在要求提前几天" asks about now, and "最初每次读多少页"
// asks about the start.
func tenseOf(question string) string {
	text := strings.Join(strings.Fields(question), "")
	first, tense := len(text), tenseUnknown
	for _, marker := range originalMarkers {
		if index := strings.Index(text, marker); index >= 0 && index < first {
			first, tense = index, tenseOriginal
		}
	}
	for _, marker := range currentMarkers {
		if index := strings.Index(text, marker); index >= 0 && index < first {
			first, tense = index, tenseCurrent
		}
	}
	return tense
}

// laterMarkers frame a figure as the one a change produced.
var laterMarkers = []string{
	"改成", "改为", "改到", "换成了", "变成", "更新为", "调整为", "调整到",
	"后来", "之后", "后面",
}

// earlierMarkers frame a figure as the one a change replaced.
var earlierMarkers = []string{
	"之前", "以前", "原来", "原先", "最初", "起初", "最早", "当初",
	"不再", "不作数", "不算数", "已改", "作废", "已经改",
}

// markersFor picks the phrasing that excuses a replaced figure.
//
// A question about the original value is answered by naming the change
// ("后来改成 30 页"), and a question about the current value is answered by
// naming what it replaced ("之前说的 3 天已不作数"). The two are not
// interchangeable: the same words on the other question would excuse the
// wrong answer.
func markersFor(tense string) []string {
	switch tense {
	case tenseOriginal:
		return laterMarkers
	case tenseCurrent:
		return earlierMarkers
	default:
		return nil
	}
}

// mentionsOutsidePhrasing reports whether a value appears anywhere the answer
// is not framing it as the value that was replaced.
//
// The reach is deliberately short. A marker within a dozen characters of the
// figure is the sentence the figure sits in; a marker anywhere in the answer
// would let a single "后来" elsewhere excuse every wrong figure it names.
func mentionsOutsidePhrasing(answer, value string, excusing []string) bool {
	text := []rune(strings.ToLower(strings.Join(strings.Fields(answer), "")))
	needle := []rune(strings.ToLower(strings.Join(strings.Fields(value), "")))
	if len(needle) == 0 {
		return false
	}
	for index := 0; index+len(needle) <= len(text); index++ {
		if string(text[index:index+len(needle)]) != string(needle) {
			continue
		}
		if !framedBy(text, index, len(needle), excusing) {
			return true
		}
	}
	// Every mention was framed as the replaced value, or the answer never
	// named it. Neither is an answer given with the replaced figure.
	return false
}

// framedBy reports whether the figure at this place sits in a sentence the
// excusing words reach.
func framedBy(text []rune, from, width int, excusing []string) bool {
	if len(excusing) == 0 {
		return false
	}
	const reach = 12
	start := from - reach
	if start < 0 {
		start = 0
	}
	end := from + width + reach
	if end > len(text) {
		end = len(text)
	}
	window := string(text[start:end])
	for _, marker := range excusing {
		if strings.Contains(window, marker) {
			return true
		}
	}
	return false
}

func containsInt(values []int, value int) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func containsUint(values []uint, value uint) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

// WriteLayeredRead writes one read-side run as JSON.
func WriteLayeredRead(path string, read LayeredRead) error {
	encoded, err := json.MarshalIndent(read, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}

// LayeredReadTimeout is the deadline one read-side run is given.
const LayeredReadTimeout = 30 * time.Minute
