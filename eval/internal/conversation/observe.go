package conversation

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

type jobRow struct {
	ID       string
	Status   string
	Attempts int
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
			if err := tx.Table(item.table).Select("id", "status", "attempts").Order("id").Find(&rows).Error; err != nil {
				return err
			}
			for _, r := range rows {
				jobs = append(jobs, JobState{ID: r.ID, Kind: item.kind, Status: r.Status, Attempts: r.Attempts})
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
