package summaryconvert

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	messageDAO "GopherAI/dao/message"
	"GopherAI/model"

	"gorm.io/gorm"
)

// Action names what one row is going to get.
type Action string

const (
	// ActionUnchanged means the row is left exactly as it is: it is already
	// readable, it holds nothing, or this run cannot convert it.
	ActionUnchanged Action = "unchanged"
	// ActionWouldConvert means the row is convertible and a run that writes
	// would rewrite it. A dry run stops here: nothing is written.
	ActionWouldConvert Action = "would_convert"
	// ActionConverted means the row was read, re-expressed and written.
	ActionConverted Action = "converted"
	// ActionFailed means the row was going to be written and the write did not
	// happen or did not land. The row keeps its old content either way.
	ActionFailed Action = "failed"
)

// Outcome is one row's verdict, in the words an operator has to act on.
type Outcome struct {
	Row   Row
	Shape Shape
	// Action is what this run did or would do. Reason is why, and for a row
	// that is left alone it names the thing that would have to change first.
	Action Action
	Reason string
	// ModelCalls is the number of provider calls this row spent. A mechanical
	// conversion spends none, so a non-zero count means the row's prose was
	// re-expressed by the model.
	ModelCalls int
	// DroppedEvidenceIDs and LegacyTextRunes report what the re-expression
	// could not carry. They are never a silent part of a converted row.
	DroppedEvidenceIDs []string
	LegacyTextRunes    int
}

// Report is the whole run.
type Report struct {
	Outcomes []Outcome
	// RowsRead counts every summary row the run looked at, including ones it
	// could not read.
	RowsRead int
	// RowsWithMore reports whether rows exist beyond the limit the run read, so
	// a partial pass never reads as a complete count.
	RowsWithMore bool
}

// CountByShape totals the rows by shape, which is the number the plan asked for
// and no one could produce from the repository: how many old shapes are in a
// live database is only knowable by reading it.
func (r Report) CountByShape() map[Shape]int {
	counts := make(map[Shape]int, 5)
	for _, outcome := range r.Outcomes {
		counts[outcome.Shape]++
	}
	return counts
}

// CountByAction totals the rows by what the run did to them.
func (r Report) CountByAction() map[Action]int {
	counts := make(map[Action]int, 4)
	for _, outcome := range r.Outcomes {
		counts[outcome.Action]++
	}
	return counts
}

// ProseGenerator re-expresses one prose summary as a document of this schema.
// It is the only arm of this tool that can spend a provider call, so it is
// injected rather than built: a dry run never needs one, and the tests decide
// what the model answers.
type ProseGenerator interface {
	GenerateFromProse(ctx context.Context, prose string, messages []model.Message) (string, error)
}

// CoveredMessages reads the messages one summary already covers: everything at
// or below its watermark.
type CoveredMessages interface {
	Covered(ctx context.Context, userID int64, sessionID string, watermark uint, limit int) ([]model.Message, bool, error)
}

// defaultCoveredMessages reads the covered range through the message DAO, which
// is the same ownership-boundary join the runtime reads through.
type defaultCoveredMessages struct{ repository *messageDAO.Repository }

func (m defaultCoveredMessages) Covered(ctx context.Context, userID int64, sessionID string, watermark uint, limit int) ([]model.Message, bool, error) {
	if watermark == 0 {
		return nil, false, nil
	}
	page, err := m.repository.ListOwnedHistoryPage(ctx, userID, sessionID, watermark+1, limit)
	if err != nil {
		return nil, false, err
	}
	return page.Messages, page.HasMore, nil
}

// Options configures one run.
type Options struct {
	// Apply writes converted rows. Without it the run is a preview: no row is
	// written, and no provider call is made.
	Apply bool
	// Limit bounds how many rows one run reads.
	Limit int
	// MessageLimit bounds how many covered messages a prose conversion may put
	// in front of the model. A session whose covered range is larger than this
	// is reported rather than half-read.
	MessageLimit int
	// Budget is the compression budget a converted document has to fit.
	Budget model.SummaryBudget
	// Prose is the generator for prose rows. A nil Prose leaves every prose row
	// alone and says why.
	Prose ProseGenerator
}

func (o Options) withDefaults() Options {
	if o.Limit <= 0 {
		o.Limit = 512
	}
	if o.MessageLimit <= 0 {
		o.MessageLimit = 512
	}
	if o.Budget.GroupItemLimit == 0 {
		o.Budget = model.DefaultSummaryBudget()
	}
	return o
}

// Runner plans and performs conversions against one deployment's store.
type Runner struct {
	store    *Store
	messages CoveredMessages
	options  Options
}

// NewRunner builds a runner over one deployment's summary store. The covered
// messages are read from the same database through the message DAO, which is the
// ownership-boundary read the runtime itself uses.
func NewRunner(store *Store, db *gorm.DB, options Options) *Runner {
	return &Runner{
		store:    store,
		messages: defaultCoveredMessages{repository: messageDAO.NewRepository(db)},
		options:  options.withDefaults(),
	}
}

// NewRunnerWithMessages is the seam the tests use: the store, the covered
// message reader, and the options, with nothing else implied.
func NewRunnerWithMessages(store *Store, messages CoveredMessages, options Options) *Runner {
	return &Runner{store: store, messages: messages, options: options.withDefaults()}
}

// Run reads the rows and returns what each one gets. It never returns a partial
// verdict: a row that could not be read is an outcome with a reason.
func (r *Runner) Run(ctx context.Context) (Report, error) {
	rows, err := r.store.List(ctx, r.options.Limit)
	if err != nil {
		return Report{}, err
	}
	more, err := r.store.HasMore(ctx, r.options.Limit)
	if err != nil {
		return Report{}, err
	}
	report := Report{RowsRead: len(rows), RowsWithMore: more}
	for _, row := range rows {
		report.Outcomes = append(report.Outcomes, r.runRow(ctx, row))
	}
	return report, nil
}

func (r *Runner) runRow(ctx context.Context, row Row) Outcome {
	shape := Classify(row.Content)
	outcome := Outcome{Row: row, Shape: shape, Action: ActionUnchanged}
	switch shape {
	case ShapeEmpty:
		outcome.Reason = "the row holds no summary"
		return outcome
	case ShapeCurrent:
		outcome.Reason = "already readable by this build"
		return outcome
	case ShapeDamaged:
		outcome.Reason = "the content is neither shape and cannot be read"
		return outcome
	case ShapeLegacyJSON:
		conversion, err := ConvertLegacyDocument(row.Content, r.options.Budget)
		if err != nil {
			outcome.Reason = fmt.Sprintf("not converted: %v", err)
			return outcome
		}
		outcome.DroppedEvidenceIDs = conversion.DroppedEvidenceIDs
		outcome.LegacyTextRunes = conversion.LegacyTextRunes
		return r.commit(ctx, outcome, conversion)
	case ShapeProse:
		return r.runProse(ctx, outcome, row)
	default:
		outcome.Reason = fmt.Sprintf("unknown shape %q", shape)
		return outcome
	}
}

func (r *Runner) runProse(ctx context.Context, outcome Outcome, row Row) Outcome {
	// A preview never spends a call, whatever else is configured: whether a
	// prose row can be converted is only knowable by asking the model, and
	// asking costs. So the preview says what a writing run would have to do.
	if !r.options.Apply {
		outcome.Reason = "a prose summary takes one model call per row to re-express; this run makes no call and writes nothing"
		return outcome
	}
	if r.options.Prose == nil {
		outcome.Reason = "a prose summary cannot be split into this schema's groups without a model call; run with the model arm to convert it"
		return outcome
	}
	if r.messages == nil {
		outcome.Reason = "no covered-message reader: a prose row cannot be re-expressed without the messages the summary covered"
		return outcome
	}
	if row.CoveredMessageID == 0 {
		outcome.Reason = "the row covers no message, so there is nothing to re-express"
		return outcome
	}
	messages, more, err := r.messages.Covered(ctx, row.UserID, row.SessionID, row.CoveredMessageID, r.options.MessageLimit)
	if err != nil {
		outcome.Reason = fmt.Sprintf("cannot read the covered messages: %v", err)
		return outcome
	}
	if more {
		outcome.Reason = fmt.Sprintf("the covered range is longer than %d messages, so this run would only read part of it", r.options.MessageLimit)
		return outcome
	}
	if len(messages) == 0 {
		outcome.Reason = "the covered messages are gone, so the prose cannot be re-derived"
		return outcome
	}
	content, err := r.options.Prose.GenerateFromProse(ctx, row.Content, messages)
	if err != nil {
		outcome.ModelCalls = 1
		outcome.Reason = fmt.Sprintf("the model did not return a readable document: %v", err)
		return outcome
	}
	document, err := readProseConversion(content, row.Content, messages, r.options.Budget)
	if err != nil {
		outcome.ModelCalls = 1
		outcome.Reason = fmt.Sprintf("not converted: %v", err)
		return outcome
	}
	outcome.ModelCalls = 1
	return r.commit(ctx, outcome, document)
}

// commit is the one place a row changes, so the dry run and the writing run
// differ by exactly this branch.
func (r *Runner) commit(ctx context.Context, outcome Outcome, conversion Conversion) Outcome {
	if !r.options.Apply {
		outcome.Action = ActionWouldConvert
		outcome.Reason = "convertible; this run writes nothing"
		return outcome
	}
	if err := r.store.Write(ctx, outcome.Row, conversion.Content); err != nil {
		outcome.Action = ActionFailed
		outcome.Reason = fmt.Sprintf("the write did not land: %v", err)
		return outcome
	}
	outcome.Action = ActionConverted
	outcome.Reason = "converted"
	return outcome
}

// readProseConversion checks what the model returned against two things: the
// documents this build's own reader accepts, and the material the model was
// actually shown. A citation for a message that was not in the covered range, or
// an item that is not the prose's own text, refuses the row rather than storing
// a claim nothing backs.
func readProseConversion(content, prose string, messages []model.Message, budget model.SummaryBudget) (Conversion, error) {
	document, err := model.DecodeSessionSummaryDocument(extractJSONObject(content))
	if err != nil {
		return Conversion{}, fmt.Errorf("the answer is not a document of this schema: %w", err)
	}
	citable := make(map[uint]struct{}, len(messages))
	for _, message := range messages {
		if message.IsUser {
			citable[message.ID] = struct{}{}
		}
	}
	normalized := normalizeText(prose)
	for _, group := range []model.SessionSummaryEvidenceList{document.ConfirmedDecisions, document.OpenLoops, document.UserCorrections} {
		for _, item := range group {
			if len(item.EvidenceMessageIDs) == 0 {
				if !strings.Contains(normalized, normalizeText(item.Text)) {
					return Conversion{}, fmt.Errorf("an item without citations is not the prose's own text: %q", item.Text)
				}
				continue
			}
			for _, id := range item.EvidenceMessageIDs {
				if _, ok := citable[id]; !ok {
					return Conversion{}, fmt.Errorf("cited message %d is not a covered user message", id)
				}
			}
		}
	}
	if err := document.Validate(budget); err != nil {
		return Conversion{}, fmt.Errorf("the converted summary exceeds the budget: %w", err)
	}
	return Conversion{Content: content}, nil
}

func normalizeText(value string) string {
	return strings.Join(strings.FieldsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\u3000'
	}), "")
}

// extractJSONObject takes the object out of an answer that may have been wrapped
// in prose. It is the same rule the rolling summary applies to its own model
// answer, kept here rather than imported so the two arms cannot drift apart in
// the direction that would let a partially-extracted document into the store:
// both take the first brace to the last one, and a text with no braces is passed
// through so the strict reader is the one that refuses it.
func extractJSONObject(value string) string {
	value = strings.TrimSpace(value)
	start, end := strings.Index(value, "{"), strings.LastIndex(value, "}")
	if start < 0 || end < start {
		return value
	}
	return value[start : end+1]
}
