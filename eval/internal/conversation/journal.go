package conversation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// JobEvent is written in the same MySQL transaction as the task transition.
// Sequence numbers order transitions against the answer barrier even if a
// transition's transaction commits later. No application polling is involved.
type JobEvent struct {
	Sequence   uint64 `gorm:"primaryKey;autoIncrement"`
	Kind       string
	JobID      string
	Operation  string
	Status     string
	Attempts   int
	ObservedAt time.Time
}

func (JobEvent) TableName() string { return "eval_job_events" }

type JobJournal struct {
	sandbox     *Sandbox
	definitions map[string]string
	answer      uint64
}

// StartJobJournal must run after schema creation but before product startup.
// It refuses preexisting tasks. Triggers are installed only in an owned fresh DB.
func (s *Sandbox) StartJobJournal(ctx context.Context) (*JobJournal, error) {
	if err := s.verify(ctx); err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return s.checkMarker(tx) }); err != nil {
		return nil, err
	}
	for _, table := range []string{"episode_projection_jobs", "memory_extraction_jobs"} {
		var n int64
		if err := s.db.WithContext(ctx).Table(table).Count(&n).Error; err != nil {
			return nil, err
		}
		if n != 0 {
			return nil, fmt.Errorf("journal requires empty job tables")
		}
	}
	if s.db.Migrator().HasTable("eval_job_events") {
		return nil, fmt.Errorf("journal already exists")
	}
	if err := s.db.WithContext(ctx).Exec(`CREATE TABLE eval_job_events (sequence BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, kind VARCHAR(16) NOT NULL, job_id VARCHAR(128) NOT NULL, operation VARCHAR(16) NOT NULL, status VARCHAR(24) NOT NULL, attempts INT NOT NULL, observed_at DATETIME(6) NOT NULL) ENGINE=InnoDB`).Error; err != nil {
		return nil, err
	}
	j := &JobJournal{sandbox: s, definitions: map[string]string{}}
	for _, kind := range []string{"episode", "memory"} {
		table := "episode_projection_jobs"
		if kind == "memory" {
			table = "memory_extraction_jobs"
		}
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			name := "eval_" + kind + "_" + strings.ToLower(op)
			image := "NEW"
			if op == "DELETE" {
				image = "OLD"
			}
			// On UPDATE retain both images: resetting attempts/status cannot erase an execution.
			body := fmt.Sprintf("INSERT INTO eval_job_events (kind,job_id,operation,status,attempts,observed_at) VALUES ('%s',%s.id,'%s',%s.status,%s.attempts,UTC_TIMESTAMP(6))", kind, image, op, image, image)
			if op == "UPDATE" {
				body = fmt.Sprintf("BEGIN INSERT INTO eval_job_events (kind,job_id,operation,status,attempts,observed_at) VALUES ('%s',OLD.id,'UPDATE_OLD',OLD.status,OLD.attempts,UTC_TIMESTAMP(6)); %s; END", kind, body)
			}
			query := fmt.Sprintf("CREATE TRIGGER %s AFTER %s ON %s FOR EACH ROW %s", name, op, table, body)
			if err := s.db.WithContext(ctx).Exec(query).Error; err != nil {
				return nil, err
			}
		}
	}
	var err error
	j.definitions, err = j.readDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	if len(j.definitions) != 6 {
		return nil, fmt.Errorf("journal trigger coverage incomplete")
	}
	return j, nil
}

func (j *JobJournal) readDefinitions(ctx context.Context) (map[string]string, error) {
	var rows []struct{ TriggerName, ActionStatement string }
	err := j.sandbox.db.WithContext(ctx).Raw("SELECT TRIGGER_NAME, ACTION_STATEMENT FROM information_schema.triggers WHERE trigger_schema = DATABASE() AND trigger_name LIKE 'eval\\_%'").Scan(&rows).Error
	out := map[string]string{}
	for _, r := range rows {
		out[r.TriggerName] = r.ActionStatement
	}
	return out, err
}

// AnswerBarrier is called immediately after the final HTTP response is received.
// Pre-answer work that has not committed yet still reserves a smaller sequence.
func (j *JobJournal) AnswerBarrier(ctx context.Context) error {
	if j.answer != 0 {
		return fmt.Errorf("answer barrier already recorded")
	}
	e := JobEvent{Kind: "barrier", Operation: "answer", ObservedAt: time.Now().UTC()}
	err := guardedStore{j.sandbox}.with(ctx, func(tx *gorm.DB) error { return tx.Create(&e).Error })
	if err == nil {
		j.answer = e.Sequence
	}
	return err
}

// Finish must run after successful product shutdown, using the observer's
// separate DB pool. Missing/replaced triggers or absent barriers fail closed.
func (j *JobJournal) Finish(ctx context.Context) ([]JobEvent, bool, error) {
	if j == nil || j.answer == 0 {
		return nil, false, fmt.Errorf("journal has no answer barrier")
	}
	if err := j.sandbox.verify(ctx); err != nil {
		return nil, false, err
	}
	defs, err := j.readDefinitions(ctx)
	if err != nil {
		return nil, false, err
	}
	if len(defs) != len(j.definitions) {
		return nil, false, fmt.Errorf("journal trigger coverage changed")
	}
	for name, body := range j.definitions {
		if defs[name] != body {
			return nil, false, fmt.Errorf("journal trigger changed")
		}
	}
	var events []JobEvent
	err = guardedStore{j.sandbox}.with(ctx, func(tx *gorm.DB) error { return tx.Order("sequence").Find(&events).Error })
	if err != nil {
		return nil, false, err
	}
	barrier := false
	executed := false
	for _, e := range events {
		if e.Sequence == j.answer && e.Kind == "barrier" && e.Operation == "answer" {
			barrier = true
			continue
		}
		if e.Kind != "episode" && e.Kind != "memory" {
			return nil, false, fmt.Errorf("unexpected journal event")
		}
		if e.Sequence < j.answer && (e.Status != "pending" || e.Attempts != 0) {
			executed = true
		}
	}
	if !barrier {
		return nil, false, fmt.Errorf("answer barrier missing")
	}
	return events, executed, nil
}
