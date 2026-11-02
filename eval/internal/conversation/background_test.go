package conversation

import (
	"testing"
	"time"
)

func TestBackgroundAuditFailsClosed(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	answer := start.Add(time.Second)
	base := BackgroundAudit{Started: start, Answered: answer, Complete: true, Before: []JobState{{ID: "j", Kind: "episode", Status: "pending"}}}
	if err := base.ValidateBackground(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*BackgroundAudit){
		"incomplete":       func(a *BackgroundAudit) { a.Complete = false },
		"in progress":      func(a *BackgroundAudit) { a.Before[0].Status = "processing" },
		"previous failure": func(a *BackgroundAudit) { a.Before[0].Attempts = 1 },
		"execution event":  func(a *BackgroundAudit) { a.ExecutedBeforeAnswer = true },
		"background call": func(a *BackgroundAudit) {
			a.Calls = []BackgroundCall{{Role: "background", Started: start.Add(time.Millisecond)}}
		},
		"unknown role":         func(a *BackgroundAudit) { a.Calls = []BackgroundCall{{Role: "unknown", Started: answer}} },
		"unrecognised role":    func(a *BackgroundAudit) { a.Calls = []BackgroundCall{{Role: "other", Started: answer}} },
		"unknown after status": func(a *BackgroundAudit) { a.After = []JobState{{ID: "x", Kind: "memory", Status: "unexpected"}} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			a := base
			a.Before = append([]JobState(nil), base.Before...)
			mutate(&a)
			if a.ValidateBackground() == nil {
				t.Fatal("must reject")
			}
		})
	}
	base.Calls = []BackgroundCall{{Role: "background", Started: answer.Add(time.Nanosecond)}}
	if err := base.ValidateBackground(); err != nil {
		t.Fatal(err)
	}
}
