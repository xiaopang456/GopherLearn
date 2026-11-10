package memory

import (
	"strings"
	"testing"
	"time"

	"GopherAI/model"
)

// A long-term memory reaches the model as one line, and the line has to say
// when the memory was last confirmed or changed.
//
// Without the date a stored value looks exactly as current as one written
// this morning, and the model has no way to tell that the learner may have
// said something different since.

func TestRenderMemoryCarriesTheDayItWasLastChanged(t *testing.T) {
	memory := model.LongTermMemory{
		Scope:   model.MemoryScopeGlobal,
		Subject: "Go 版本",
		Content: "1.22",
		Tags:    []string{"go"},
		CreatedAt: time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 12, 16, 5, 0, 0, time.UTC),
	}
	line := renderMemory(memory)
	want := "[global; tags=go; 保存于 2026-10-12] Go 版本: 1.22"
	if line != want {
		t.Fatalf("memory line = %q, want %q", line, want)
	}
}

// The date is the day the content last became what it is now.
//
// CreatedAt is when the memory first appeared and LastUsedAt is when it was
// last recalled; neither is the moment the content was last set. A memory
// that was corrected a week after it was written carries the week.

func TestRenderMemoryUsesTheLastWriteAndNotTheFirstOne(t *testing.T) {
	memory := model.LongTermMemory{
		Scope:     model.MemoryScopeTopic,
		Subject:   "学习时长",
		Content:   "48 分钟",
		CreatedAt: time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC),
		LastUsedAt: func() *time.Time {
			at := time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC)
			return &at
		}(),
	}
	line := renderMemory(memory)
	if !strings.Contains(line, "保存于 2026-10-20") {
		t.Fatalf("memory line does not carry the last write: %q", line)
	}
	if strings.Contains(line, "2026-10-25") {
		t.Fatalf("memory line carries the recall time instead: %q", line)
	}
	if strings.Contains(line, "2026-10-01") {
		t.Fatalf("memory line carries the creation time instead: %q", line)
	}
}

// A row whose write time was never stamped falls back to its creation time,
// and a row with neither carries no date at all: an invented date would look
// like a fact about when the learner said it.

func TestRenderMemoryWithoutAnyTimestampCarriesNoDate(t *testing.T) {
	memory := model.LongTermMemory{Scope: model.MemoryScopeGlobal, Subject: "偏好", Content: "先说结论"}
	line := renderMemory(memory)
	if strings.Contains(line, "保存于") {
		t.Fatalf("memory line carries a date it cannot know: %q", line)
	}
	if line != "[global; tags=] 偏好: 先说结论" {
		t.Fatalf("memory line = %q", line)
	}
}

// The paired harness maps an injected line back to its memory id by rendering
// the line it expects and finding that line in the window.
//
// The date must not break that: the line the harness renders and the line the
// window carries have to be the same bytes.

func TestRenderMemoryLineMatchesTheInjectedLine(t *testing.T) {
	memory := model.LongTermMemory{
		Scope:     model.MemoryScopeGlobal,
		Subject:   "Go 版本",
		Content:   "1.22",
		Tags:      []string{"go"},
		CreatedAt: time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 12, 16, 5, 0, 0, time.UTC),
	}
	expected := RenderMemoryLine(memory)
	if !strings.Contains(expected, "保存于 2026-10-12") {
		t.Fatalf("the exported line does not carry the date: %q", expected)
	}
	// The recall path renders the same line, so a harness that looks for the
	// exported bytes in the window finds them.
	if injected := renderMemory(memory); injected != expected {
		t.Fatalf("injected line %q and exported line %q differ", injected, expected)
	}
}
