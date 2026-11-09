package summaryconvert

import (
	"context"
	"fmt"

	"GopherAI/model"
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

// Options configures one run.
type Options struct {
	// Apply writes converted rows. Without it the run is a preview: no row is
	// written.
	Apply bool
	// Limit bounds how many rows one run reads.
	Limit int
	// Budget is the compression budget a converted document has to fit.
	Budget model.SummaryBudget
}

func (o Options) withDefaults() Options {
	if o.Limit <= 0 {
		o.Limit = 512
	}
	if o.Budget.GroupItemLimit == 0 {
		o.Budget = model.DefaultSummaryBudget()
	}
	return o
}

// Runner plans and performs conversions against one deployment's store.
type Runner struct {
	store   *Store
	options Options
}

// NewRunner builds a runner over one deployment's summary store.
func NewRunner(store *Store, options Options) *Runner {
	return &Runner{store: store, options: options.withDefaults()}
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
	case ShapeProse:
		conversion, err := ConvertProse(row.Content, r.options.Budget)
		if err != nil {
			outcome.Reason = fmt.Sprintf("not converted: %v", err)
			return outcome
		}
		return r.commit(ctx, outcome, conversion)
	case ShapeLegacyJSON:
		conversion, err := ConvertLegacyDocument(row.Content, r.options.Budget)
		if err != nil {
			outcome.Reason = fmt.Sprintf("not converted: %v", err)
			return outcome
		}
		return r.commit(ctx, outcome, conversion)
	default:
		outcome.Reason = fmt.Sprintf("unknown shape %q", shape)
		return outcome
	}
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
