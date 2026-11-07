package conversation

import (
	"testing"

	agent "GopherAI/service/agent"
)

// The harness has to report the authorisation set the product's runner would
// consult for the same window.
//
// It used to read the summary segments' list alone, so an arm whose grants
// came from the rolling summary - the truncated arm - was reported as
// unauthorised whatever its window carried, and a run that found nothing
// readable could not say whether the window was empty or the harness was
// looking in the wrong place.
func TestTheDriverReportsTheWholeProductAuthorisationSet(t *testing.T) {
	window := agent.ContextWindow{
		EpisodeHistoryGrants: []agent.HistoryMessageGrant{{
			MessageID: 11, SourceKind: agent.HistorySourceEpisode, SourceSessionID: "session-1",
			EpisodeID: "episode-1", EpisodeVersion: 1,
		}},
		SummaryHistoryGrants: []agent.HistoryMessageGrant{{
			MessageID: 13, SourceKind: agent.HistorySourceSummary, SourceSessionID: "session-1",
			SummaryVersion: 2, SummaryWatermark: 40,
		}},
		SummarySegmentHistoryGrants: []agent.HistoryMessageGrant{{
			MessageID: 17, SourceKind: agent.HistorySourceSummarySegment, SourceSessionID: "session-1",
			SegmentSeq: 3, SegmentEndMessageID: 30,
		}},
	}
	run := agent.RunContext{UserID: 7, SessionID: "session-1"}

	grants, set := windowGrantSet(window, run)
	if len(grants) != 3 {
		t.Fatalf("grants=%+v want one per card", grants)
	}
	for _, id := range []uint{11, 13, 17} {
		if !set.Covers(id, "session-1") {
			t.Fatalf("the id %d is not authorised", id)
		}
	}
	// The reference space is what an answer's number is resolved against, and
	// it carries the segment number next to the message ids.
	references := set.References()
	for _, want := range []int{11, 13, 17, 3} {
		found := false
		for _, reference := range references {
			if reference == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("references=%v want %d among them", references, want)
		}
	}

	// The per-source breakdown says which card authorised what, so an empty
	// set and a set of one kind are different facts.
	sources := grantSources(grants)
	if sources[agent.HistorySourceEpisode] != 1 || sources[agent.HistorySourceSummary] != 1 ||
		sources[agent.HistorySourceSummarySegment] != 1 {
		t.Fatalf("sources=%v want one per card", sources)
	}
}

// A window with nothing in it authorises nothing, and the report has to say
// so rather than count the cards it did not find.
func TestAnEmptyWindowAuthorisesNothing(t *testing.T) {
	run := agent.RunContext{UserID: 7, SessionID: "session-1"}

	grants, set := windowGrantSet(agent.ContextWindow{}, run)
	if len(grants) != 0 || set.Len() != 0 {
		t.Fatalf("grants=%+v set=%d want none", grants, set.Len())
	}
	if sources := grantSources(grants); len(sources) != 0 {
		t.Fatalf("sources=%v want none", sources)
	}
}
