package episode

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fakeHistoryReader struct{ value bool }

func (f fakeHistoryReader) HasSessionHistory(context.Context, int64, string) (bool, error) {
	return f.value, nil
}

func TestShadowObserverLogsMetadataOnly(t *testing.T) {
	search := &fakeEpisodeSearch{}
	loader := &fakeEpisodeLoader{rows: nil}
	retriever, _ := NewRetriever(NewQueryRouter(), search, loader, nil, DefaultRetrieverConfig())
	observer, _ := NewShadowObserver(fakeHistoryReader{}, retriever)
	var line string
	observer.logf = func(format string, args ...any) { line = fmt.Sprintf(format, args...) }
	secret := "PRIVATE_EPISODE_BODY"
	if err := observer.Observe(context.Background(), 7, "current", "普通问题 "+secret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(line, secret) || !strings.Contains(line, "intent=none") || !strings.Contains(line, "abstained=true") {
		t.Fatalf("unsafe shadow log=%q", line)
	}
}
