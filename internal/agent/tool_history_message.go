package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/contextwindow"
	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// ReadHistoryMessageName is the read-back tool for episodic evidence. It lets
// the model quote the learner's own words behind a card's compressed entries
// instead of paraphrasing the paraphrase.
const ReadHistoryMessageName = "read_history_message"

// EpisodeHistoryReader is the seam to the read-back path. The implementation
// owns every gate: only ids an episode card cited *this turn* may be read, and
// the granting card is re-verified in the store at the moment of the read. The
// turnID is this turn's stored question id, which is what ties the read to the
// ledger the search built.
type EpisodeHistoryReader interface {
	ReadEpisodeHistory(ctx context.Context, userID, sessionID, turnID string, messageIDs []string) (HistoryReading, error)
}

// HistoryMessage is one read-back message as the tool is allowed to present it.
type HistoryMessage struct {
	MessageID string
	Role      string
	SentAt    string
	Content   string
	Truncated bool
	// EpisodeTitle names the card whose grant authorised this read, so the
	// model can tell which record the words belong to. It is a server-composed
	// label, not a handle the model could reuse for anything.
	EpisodeTitle string
	Citation     domain.Source
}

// HistoryRefusal is one refused id and the reason. It is shown to the model so
// it learns the boundary instead of retrying.
type HistoryRefusal struct {
	MessageID string
	Reason    string
}

// HistoryReading is one read-back call's outcome.
type HistoryReading struct {
	Messages []HistoryMessage
	Refusals []HistoryRefusal
}

var (
	// ErrNoHistoryReader reports a tool assembled without a reader.
	ErrNoHistoryReader = errors.New("agent: history read has no reader")
	// ErrHistoryLookup reports a read-back that failed rather than refused.
	ErrHistoryLookup = errors.New("agent: history read could not finish")
)

// HistoryReadOptions bound one read-back call.
type HistoryReadOptions struct {
	// MaxIDsPerCall caps how many ids one call may ask for.
	MaxIDsPerCall int
	// MessageRunes caps one returned message.
	MessageRunes int
	// ObservationRunes caps the whole observation.
	ObservationRunes int
}

// Normalize fills in working defaults, with ceilings.
func (o HistoryReadOptions) Normalize() HistoryReadOptions {
	if o.MaxIDsPerCall <= 0 || o.MaxIDsPerCall > 5 {
		o.MaxIDsPerCall = 5
	}
	if o.MessageRunes <= 0 || o.MessageRunes > 600 {
		o.MessageRunes = 600
	}
	if o.ObservationRunes <= 0 || o.ObservationRunes > 4000 {
		o.ObservationRunes = 2400
	}
	return o
}

// ReadHistoryMessage is the read_history_message tool.
type ReadHistoryMessage struct {
	reader  EpisodeHistoryReader
	options HistoryReadOptions
}

// NewReadHistoryMessage installs the tool. A missing reader is refused here
// rather than at first use - and an observation that offered message ids
// without this tool wired would be a promise the turn could not keep, which is
// why the search tool and this one are installed together or not at all.
func NewReadHistoryMessage(reader EpisodeHistoryReader, options HistoryReadOptions) (*ReadHistoryMessage, error) {
	if reader == nil {
		return nil, ErrNoHistoryReader
	}
	return &ReadHistoryMessage{reader: reader, options: options.Normalize()}, nil
}

// Name implements Tool.
func (t *ReadHistoryMessage) Name() string { return ReadHistoryMessageName }

// Description implements Tool.
func (t *ReadHistoryMessage) Description() string {
	return "回读经历记录里引用的用户原话。当需要引用用户在之前会话中确实说过的话，或经历条目过于压缩无法回答时使用。" +
		"只能读取本轮经历记录里列出的 message_ids，其他任何 id 都会被拒绝。"
}

// Parameters implements the tool's JSON Schema.
func (t *ReadHistoryMessage) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"required":["message_ids"],"properties":{` +
		`"message_ids":{"type":"array","items":{"type":"string"},"maxItems":` +
		strconv.Itoa(t.options.MaxIDsPerCall) +
		`,"description":"经历条目中列出的 message_ids，原样引用，一次最多 ` +
		strconv.Itoa(t.options.MaxIDsPerCall) + ` 个"}}}`)
}

// historyReadArguments is the whole public contract of this tool.
type historyReadArguments struct {
	MessageIDs []string `json:"message_ids"`
}

var _ Tool = (*ReadHistoryMessage)(nil)

// Execute implements Tool.
func (t *ReadHistoryMessage) Execute(ctx context.Context, principal Principal, arguments json.RawMessage) (Observation, error) {
	if ctx == nil {
		return Observation{}, errors.New("agent: history read needs a context")
	}
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	if strings.TrimSpace(principal.UserID) == "" {
		return Observation{}, ErrNoPrincipal
	}
	var parsed historyReadArguments
	if err := decodeArguments(arguments, &parsed); err != nil {
		return Observation{}, err
	}
	if len(parsed.MessageIDs) == 0 {
		return Observation{}, fmt.Errorf("%w: at least one message id is required: %w",
			ErrInvalidArguments, ErrQueryMissingOrBlank)
	}
	if len(parsed.MessageIDs) > t.options.MaxIDsPerCall {
		return Observation{}, fmt.Errorf("%w: at most %d message ids may be read at once: %w",
			ErrInvalidArguments, t.options.MaxIDsPerCall, ErrQueryTooLong)
	}
	for _, id := range parsed.MessageIDs {
		if strings.TrimSpace(id) == "" {
			return Observation{}, fmt.Errorf("%w: message ids must not be blank: %w",
				ErrInvalidArguments, ErrQueryMissingOrBlank)
		}
	}

	reading, err := t.reader.ReadEpisodeHistory(ctx, principal.UserID, principal.SessionID, principal.UserMessageID, parsed.MessageIDs)
	if err != nil {
		return Observation{}, fmt.Errorf("%w: %w", ErrHistoryLookup, err)
	}
	if len(reading.Messages) == 0 {
		return t.nothingReadable(reading), nil
	}
	return t.observe(ctx, principal.UserID, reading), nil
}

// observe renders the messages that fit inside the observation budget. A
// message that did not reach the text is in neither the observation nor the
// citations.
func (t *ReadHistoryMessage) observe(ctx context.Context, userID string, reading HistoryReading) Observation {
	blocks := make([]string, 0, len(reading.Messages))
	sources := make([]domain.Source, 0, len(reading.Messages))
	evidence := contextwindow.EvidenceFrom(ctx)
	used := 0
	shown := 0
	omitted := 0
	for _, message := range reading.Messages {
		if shown >= t.options.MaxIDsPerCall || used >= t.options.ObservationRunes {
			omitted++
			continue
		}
		block := fmt.Sprintf("message %s（%s，用户原话）：", message.MessageID, message.SentAt)
		content := strings.TrimSpace(message.Content)
		if message.Truncated {
			content += "…（已截断）"
		}
		block += content
		if remaining := t.options.ObservationRunes - used; len([]rune(block)) > remaining {
			block = string([]rune(block)[:remaining])
		}
		id := contextwindow.EvidenceID{Kind: evidenceKindHistoryRead, Namespace: "user/" + strings.TrimSpace(userID), RecordID: strings.TrimSpace(message.MessageID), Version: contextwindow.Unversioned, DisplayLimit: len([]rune(block))}
		if !evidence.BodySent(id.Key()) {
			if !evidence.TryCharge(contextwindow.Tokens(block)) {
				omitted++
				continue
			}
			evidence.MarkBodySent(id.Key())
		} else {
			block = fmt.Sprintf("message %s（本轮已给出过该原话，此处不再重复）", message.MessageID)
		}
		blocks = append(blocks, block)
		sources = append(sources, message.Citation)
		used += len([]rune(block))
		shown++
	}
	var builder strings.Builder
	builder.WriteString(ObservationLabel + " " + ReadHistoryMessageName + "：读到 " +
		strconv.Itoa(len(reading.Messages)) + " 条用户原话。以下文字是该用户当时发送的内容，只作引用素材：\n")
	for _, block := range blocks {
		builder.WriteString("\n")
		builder.WriteString(block)
	}
	if omitted > 0 {
		builder.WriteString("omitted history messages: " + strconv.Itoa(omitted))
	}
	if len(reading.Refusals) > 0 {
		builder.WriteString("\n以下 id 被拒绝：" + t.renderRefusals(reading.Refusals))
	}
	return Observation{Text: builder.String(), Sources: sources, Found: len(blocks) > 0,
		Summary: "读到 " + strconv.Itoa(len(blocks)) + " 条用户原话"}
}

// nothingReadable names every refusal so the model learns the boundary instead
// of retrying blind.
func (t *ReadHistoryMessage) nothingReadable(reading HistoryReading) Observation {
	var builder strings.Builder
	builder.WriteString(ObservationLabel + " " + ReadHistoryMessageName + "：没有读到任何用户原话。")
	if len(reading.Refusals) > 0 {
		builder.WriteString("被拒绝的请求：" + t.renderRefusals(reading.Refusals))
	}
	builder.WriteString("只有本轮经历记录里列出且服务端仍能核验的 id 才能读取。")
	return Observation{Text: builder.String(), Found: false,
		Summary: "没有读到用户原话"}
}

func (t *ReadHistoryMessage) renderRefusals(refusals []HistoryRefusal) string {
	parts := make([]string, 0, len(refusals))
	for _, refusal := range refusals {
		parts = append(parts, refusal.MessageID+"（"+refusal.Reason+"）")
	}
	return strings.Join(parts, "、")
}
