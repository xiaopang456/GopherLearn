package agenttool

import (
	"context"
	"encoding/json"
	"testing"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"github.com/cloudwego/eino/components/tool"
)

// The card path is the only mode in which the browse tool may open another
// conversation, and it is opened only through a card this turn showed.
//
// These are the two halves of that: the session a card opened is browsable,
// and a session no card opened is refused with a reason that says so rather
// than with an empty result.

// cardBrowseRun is a run that was shown a card about an earlier conversation.
func cardBrowseRun() serviceagent.RunContext {
	run := serviceagent.RunContext{UserID: 7, SessionID: "current-session", Question: "上次那道题"}
	run.HistoryGrants = []serviceagent.HistoryMessageGrant{{
		MessageID: 12, SourceKind: serviceagent.HistorySourceSessionCard,
		SourceSessionID: "source-session", CardID: "card-1", CardVersion: 2, CardWatermark: 144,
	}}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, run.HistoryGrants)
	// The scope is what opens the conversation. A grant set that derived
	// "which sessions are reachable" from the cited message ids would deny a
	// card whose entries point at summary segments and at no raw message.
	run.GrantSet.SetCardScope(serviceagent.CardScope{
		SessionID: "source-session", CardID: "card-1", CardVersion: 2, CardWatermark: 144,
	})
	return run
}

// callBrowseIn runs the browse tool against a named session.
func callBrowseIn(t *testing.T, browse tool.BaseTool, session, query string) *BrowseSummarySegmentsOutput {
	t.Helper()
	invokable, ok := browse.(tool.InvokableTool)
	if !ok {
		t.Fatal("the browse tool is not invokable")
	}
	payload := `{"query":"` + query + `"`
	if session != "" {
		payload += `,"session":"` + session + `"`
	}
	payload += `}`
	raw, err := invokable.InvokableRun(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	out := &BrowseSummarySegmentsOutput{}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The conversation a card opened may be browsed, and the segments it returns
// are the ones of that conversation rather than of the run's own.
func TestBrowseSummarySegmentsOpensTheConversationACardPointedAt(t *testing.T) {
	browser := &fakeSegmentBrowser{segments: []model.SessionSummarySegment{
		browseSegment(t, 1, 1, 30, "RPC 拆包", []model.SessionSummarySegmentPoint{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{12}},
		}),
	}}
	registry, err := NewBrowseSummarySegmentsRegistry(browser)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSessionParameter(true)
	run := cardBrowseRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callBrowseIn(t, tools[0], "source-session", "拆包")
	if out.Status != browseStatusOK {
		t.Fatalf("the browse reported %q/%q", out.Status, out.Reason)
	}
	if len(out.Segments) != 1 || out.Segments[0].Topic != "RPC 拆包" {
		t.Fatalf("the browse returned %+v", out.Segments)
	}
	// The grant the browse tool issued inside that conversation carries the
	// card that opened it and the card's watermark, so the read-back is
	// bounded by what the card saw.
	var segmentGrant *serviceagent.HistoryMessageGrant
	for _, grant := range run.GrantSet.Grants(12) {
		if grant.SourceKind == serviceagent.HistorySourceSummarySegment {
			found := grant
			segmentGrant = &found
		}
	}
	if segmentGrant == nil {
		t.Fatalf("the segment's own grant is missing: %+v", run.GrantSet.Grants(12))
	}
	if segmentGrant.CardID != "card-1" || segmentGrant.CardVersion != 2 || segmentGrant.CardWatermark != 144 {
		t.Fatalf("the grant does not name the card that opened the conversation: %+v", segmentGrant)
	}
	if segmentGrant.SegmentSeq != 1 {
		t.Fatalf("the grant does not name the segment it came from: %+v", segmentGrant)
	}
	if !segmentGrant.UsableIn(run) {
		t.Fatalf("the grant the browse tool made is not usable: %+v", segmentGrant)
	}
}

// A conversation no card opened is refused, and the refusal says why.
//
// "There are no segments" and "you may not look there" are different facts,
// and a model told the first will try the same call again with a different
// query.
func TestBrowseSummarySegmentsRefusesAConversationNoCardPointedAt(t *testing.T) {
	browser := &fakeSegmentBrowser{segments: []model.SessionSummarySegment{
		browseSegment(t, 1, 1, 30, "RPC 拆包", []model.SessionSummarySegmentPoint{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{12}},
		}),
	}}
	registry, err := NewBrowseSummarySegmentsRegistry(browser)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSessionParameter(true)
	tools, err := registry.Tools(context.Background(), cardBrowseRun())
	if err != nil {
		t.Fatal(err)
	}
	out := callBrowseIn(t, tools[0], "other-session", "拆包")
	if out.Status != browseStatusNoEvidence || out.Reason != browseReasonSessionNotAllowed {
		t.Fatalf("the browse reported %q/%q", out.Status, out.Reason)
	}
	if len(out.Segments) != 0 {
		t.Fatalf("a refused browse returned %+v", out.Segments)
	}
}

// The card's watermark bounds the segments of the conversation it opened.
//
// The card described that session as far as it had reached. A segment that
// reaches past that line describes a stretch of the conversation the card
// never saw, and the message ids it carries are not readable back.
func TestBrowseSummarySegmentsBoundsTheOtherConversationByTheCardWatermark(t *testing.T) {
	browser := &fakeSegmentBrowser{segments: []model.SessionSummarySegment{
		browseSegment(t, 1, 1, 30, "RPC 拆包", []model.SessionSummarySegmentPoint{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{12}},
		}),
	}}
	registry, err := NewBrowseSummarySegmentsRegistry(browser)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSessionParameter(true)
	run := cardBrowseRun()
	// A card whose coverage stops at 20 cannot vouch for a segment that ends
	// at 30.
	run.GrantSet.SetCardScope(serviceagent.CardScope{
		SessionID: "source-session", CardID: "card-1", CardVersion: 2, CardWatermark: 20,
	})
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := callBrowseIn(t, tools[0], "source-session", "拆包")
	if out.Status != browseStatusNoEvidence {
		t.Fatalf("the browse reported %q/%q", out.Status, out.Reason)
	}
	if len(out.Segments) != 0 {
		t.Fatalf("a segment above the card's watermark was shown: %+v", out.Segments)
	}
	// The card's own citation of the message is still there - it is the
	// card's citation, and the card did cite it. What must not happen is a
	// segment above the card's coverage producing a new authorisation.
	for _, grant := range run.GrantSet.Grants(12) {
		if grant.SourceKind == serviceagent.HistorySourceSummarySegment {
			t.Fatalf("a segment above the card's watermark was authorised: %+v", grant)
		}
	}
}

// The old mode's schema is the one that was measured, and it does not grow a
// session parameter.
//
// The card path attaches its own input type instead, so a deployment whose
// measurements were taken with the old schema keeps showing the model the
// old schema. The two contracts are compared as digests of what the model
// actually receives: the name, the description and the parameter schema.
func TestBrowseSummarySegmentsKeepsTheOldSchemaWithoutTheCardPath(t *testing.T) {
	registry, err := NewBrowseSummarySegmentsRegistry(&fakeSegmentBrowser{})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.Tools(context.Background(), browseRun())
	if err != nil {
		t.Fatal(err)
	}
	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := toolContractDigest(BrowseSummarySegmentsName, browseSummarySegmentsDescription, info)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" {
		t.Fatal("the tool contract has no digest")
	}
	// The card path's own contract is a different one, and it says so.
	cardRegistry, err := NewBrowseSummarySegmentsRegistry(&fakeSegmentBrowser{})
	if err != nil {
		t.Fatal(err)
	}
	cardRegistry.UseSessionParameter(true)
	cardTools, err := cardRegistry.Tools(context.Background(), browseRun())
	if err != nil {
		t.Fatal(err)
	}
	cardInfo, err := cardTools[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cardDigest, err := toolContractDigest(BrowseSummarySegmentsName, browseSummarySegmentsSessionDescription, cardInfo)
	if err != nil {
		t.Fatal(err)
	}
	if cardDigest == digest {
		t.Fatal("the card path did not change the tool contract")
	}
}

// boardWithDuration builds the board of the conversation a card opened.
//
// The board is read live, so the value it holds now is the value of this
// moment, and the message it was last written at says how far that moment is
// from the card.
func boardWithDuration(t *testing.T, value string, updatedAt uint) *model.SessionWhiteboard {
	t.Helper()
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "duration", Text: "\u6bcf\u5929\u7ec3\u591a\u4e45", Value: value,
			UpdatedAtMessageID: updatedAt,
		}},
	}
	encoded, err := model.MarshalSessionWhiteboardDocument(document)
	if err != nil {
		t.Fatalf("encode board: %v", err)
	}
	return &model.SessionWhiteboard{UserID: 7, SessionID: "source-session", Content: encoded}
}

// durationSegment is one segment that recorded a value the board has since
// replaced.
func durationSegment(t *testing.T) model.SessionSummarySegment {
	t.Helper()
	segment := browseSegment(t, 1, 1, 30, "RPC \u62c6\u5305", []model.SessionSummarySegmentPoint{
		{Text: "\u51b3\u5b9a\u4f7f\u7528\u957f\u5ea6\u5b57\u6bb5\u62c6\u5305", EvidenceMessageIDs: []uint{12}},
	})
	segment.WhiteboardChanges = `[{"key":"duration","old_value":"","new_value":"30 \u5206\u949f","new_message_ids":[12]}]`
	return segment
}

// The board of the other conversation is read live, and what it holds now may
// be a change the card never saw.
//
// The card described that conversation as far as its watermark, and every step
// inside that conversation stops at that line. A board written after the
// watermark is such a step, and the note that carries its value would hand the
// model a change from past the card's line.
func TestBrowseSummarySegmentsDoesNotShowTheOtherBoardsChangesAboveTheCardWatermark(t *testing.T) {
	browser := &fakeSegmentBrowser{segments: []model.SessionSummarySegment{durationSegment(t)},
		board: boardWithDuration(t, "48 \u5206\u949f", 200)}
	registry, err := NewBrowseSummarySegmentsRegistry(browser)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSessionParameter(true)
	tools, err := registry.Tools(context.Background(), cardBrowseRun())
	if err != nil {
		t.Fatal(err)
	}
	out := callBrowseIn(t, tools[0], "source-session", "\u62c6\u5305")
	if out.Status != browseStatusOK || len(out.Segments) != 1 {
		t.Fatalf("the browse reported %q/%q and %+v", out.Status, out.Reason, out.Segments)
	}
	if len(out.Segments[0].WhiteboardStatus) != 0 {
		t.Fatalf("a change above the card's watermark was shown: %+v", out.Segments[0].WhiteboardStatus)
	}
}

// A board written within the card's watermark is still shown.
//
// The crop is not a blanket suppression: a change the card could have seen is
// exactly the kind of warning a reader of that conversation needs, and the
// note says which value is the current one.
func TestBrowseSummarySegmentsShowsTheOtherBoardsChangesWithinTheCardWatermark(t *testing.T) {
	browser := &fakeSegmentBrowser{segments: []model.SessionSummarySegment{durationSegment(t)},
		board: boardWithDuration(t, "48 \u5206\u949f", 100)}
	registry, err := NewBrowseSummarySegmentsRegistry(browser)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSessionParameter(true)
	tools, err := registry.Tools(context.Background(), cardBrowseRun())
	if err != nil {
		t.Fatal(err)
	}
	out := callBrowseIn(t, tools[0], "source-session", "\u62c6\u5305")
	if out.Status != browseStatusOK || len(out.Segments) != 1 {
		t.Fatalf("the browse reported %q/%q and %+v", out.Status, out.Reason, out.Segments)
	}
	if len(out.Segments[0].WhiteboardStatus) != 1 {
		t.Fatalf("a change within the card's watermark was not shown: %+v", out.Segments[0].WhiteboardStatus)
	}
}
