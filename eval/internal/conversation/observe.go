package conversation

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

type jobRow struct {
	ID                                                          string
	Status                                                      string
	Attempts                                                    int
	AvailableAt, CreatedAt, UpdatedAt, LeaseEndsAt, CompletedAt *time.Time
}

// ObserveJobs reads all jobs from both tables in one transaction. The caller
// must already have verified the instance is an isolated evaluation database.
// A snapshot is not a substitute for continuous execution-event coverage.
func ObserveJobs(ctx context.Context, db *gorm.DB) ([]JobState, map[string]JobCounts, error) {
	if db == nil {
		return nil, nil, fmt.Errorf("database is nil")
	}
	var jobs []JobState
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, item := range []struct{ table, kind string }{{"episode_projection_jobs", "episode"}, {"memory_extraction_jobs", "memory"}} {
			var rows []jobRow
			if err := tx.Table(item.table).Order("id").Find(&rows).Error; err != nil {
				return err
			}
			for _, r := range rows {
				jobs = append(jobs, JobState{ID: r.ID, Kind: item.kind, Status: r.Status, Attempts: r.Attempts, AvailableAt: productJobTime(r.AvailableAt, tx.Dialector.Name()), CreatedAt: productJobTime(r.CreatedAt, tx.Dialector.Name()), UpdatedAt: productJobTime(r.UpdatedAt, tx.Dialector.Name()), LeaseEndsAt: productJobTime(r.LeaseEndsAt, tx.Dialector.Name()), CompletedAt: productJobTime(r.CompletedAt, tx.Dialector.Name())})
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	counts, err := CountJobs(jobs)
	if err != nil {
		return nil, nil, err
	}
	return jobs, counts, nil
}

// The standard product MySQL pool uses loc=Local and writes naive DATETIME
// fields from its local clock. The independent observer pool uses UTC for
// snapshot data and trigger UTC_TIMESTAMP values. Reinterpret only product task
// wall times, using the same process zone as the unchanged standard entry.
func productJobTime(value *time.Time, dialect string) *time.Time {
	if value == nil {
		return nil
	}
	t := *value
	if dialect == "mysql" {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
	}
	utc := t.UTC()
	return &utc
}
