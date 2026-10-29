package episode

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"

	"GopherAI/model"
)

// STABILIZE-065 Phase 2-C1.1: the second semantic layer becomes production.
//
// The blocking fact this file exists for, stated before it is fixed:
//
//	1. the SupportVerifier interface already existed;
//	2. a test could inject a fake through UseSupportVerifier;
//	3. main.go called NewProjector(loader, generator, repository) and never
//	   called UseSupportVerifier, so production had no verifier at all;
//	4. verifySupport returned every assertion untouched when the verifier was
//	   nil;
//	5. so the second layer preregistered in C0 was never in the production
//	   chain - it was reachable only from a test;
//	6. and "nil is a deployment choice" is not the fail-closed contract this
//	   experiment runs under. A boundary that is off by default is not a
//	   boundary; it is an option nobody took.
//
// The probe that recorded (3) and (4) as real behaviour is sealed in
// exp033/stabilize065/phase2/repro/c11-verifier-red-before.txt.
//
// The fix is structural rather than procedural: the verifier becomes a
// constructor argument, so a Projector without one cannot be built, and
// main.go cannot compile without supplying it.

// --- a verifier that records exactly what it was asked -----------------------

type recordingVerifier struct {
	mu sync.Mutex
	// byText maps an assertion body to the verdict to return.
	byText map[string]SupportVerdict
	// fallback answers anything byText does not cover.
	fallback SupportVerdict
	// err, when set, is returned instead of a verdict.
	err error
	// requests is every request, in order.
	requests []SupportRequest
}

func (v *recordingVerifier) Verify(_ context.Context,
	request SupportRequest) (SupportVerdict, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.requests = append(v.requests, request)
	if v.err != nil {
		return "", v.err
	}
	if verdict, ok := v.byText[request.AssertionText]; ok {
		return verdict, nil
	}
	return v.fallback, nil
}

func (v *recordingVerifier) Requests() []SupportRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]SupportRequest(nil), v.requests...)
}

func (v *recordingVerifier) Calls() int { return len(v.Requests()) }

// --- fixtures ---------------------------------------------------------------

const (
	c11Entailed     = "用户决定采用长度前缀。"
	c11NotEntailed  = "用户尚未回应助手提出的演示建议。"
	c11Synopsis     = "用户排查了 RPC 粘包，助手另外提了一个演示建议。"
	c11AssistantSay = "要不要我演示一个粘包场景？"
	c11UserSay      = "采用长度前缀"
)

// c11Evidence is one user turn and one assistant turn.
func c11Evidence() ProjectionEvidence {
	evidence := validProjectionEvidence()
	evidence.TargetMessageID = 2
	evidence.Messages = []model.Message{
		{ID: 1, SessionID: "session-1", IsUser: true, Content: c11UserSay,
			CreatedAt: evidence.TargetMessageAt},
		{ID: 2, SessionID: "session-1", IsUser: false, Content: c11AssistantSay,
			CreatedAt: evidence.TargetMessageAt},
	}
	return evidence
}

func c11Document(entries ...model.EpisodeEvidence) model.EpisodeDocument {
	return model.EpisodeDocument{
		Synopsis:      c11Synopsis,
		Topics:        []model.EpisodeTopic{{Key: "rpc-粘包", Label: "RPC 粘包"}},
		Decisions:     entries,
		SourceHandles: []string{"message:1", "message:2"},
	}
}

func c11Project(t *testing.T, document model.EpisodeDocument,
	verifier SupportVerifier) (*model.EpisodeMemory, *stubGenerator, error) {
	t.Helper()
	generator := &stubGenerator{document: document}
	projector, err := NewProjector(stubEvidenceLoader{evidence: c11Evidence()},
		generator, &stubProjectionRepository{}, verifier)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	episode, perr := projector.Project(context.Background(), 7, "session-1", 2)
	return episode, generator, perr
}

// --- 1 and 2: a Projector without a verifier cannot exist -------------------

func TestC11ProjectorRefusesToExistWithoutAVerifier(t *testing.T) {
	_, err := NewProjector(stubEvidenceLoader{evidence: c11Evidence()},
		&stubGenerator{document: c11Document()}, &stubProjectionRepository{}, nil)
	if err == nil {
		t.Fatal("a Projector was built with no support verifier; the second " +
			"semantic layer would then be off in exactly the deployment that " +
			"forgot to switch it on, which is the defect C1.1 exists to remove")
	}
	if !strings.Contains(err.Error(), "verifier") {
		t.Fatalf("the refusal does not name what is missing: %v", err)
	}
}

// The escape hatch is gone. A Projector that could be mutated after
// construction can be left unverified by an ordering mistake, and this one
// carries the boundary between one learner's memory and what a model made up.
func TestC11NoSetterCanLeaveAProjectorUnverified(t *testing.T) {
	if _, ok := any(&Projector{}).(interface{ UseSupportVerifier(SupportVerifier) }); ok {
		t.Fatal("UseSupportVerifier still exists; a verifier that can be attached " +
			"later can also be forgotten, which is how production ended up without one")
	}
}

// --- 3 to 7: the four verdicts, and an illegal one --------------------------

func TestC11EveryVerdictOtherThanEntailedTrims(t *testing.T) {
	for name, shape := range map[string]struct {
		verifier *recordingVerifier
		wantKept bool
	}{
		"entailed keeps it": {
			&recordingVerifier{fallback: SupportEntailed}, true},
		"not_entailed trims": {
			&recordingVerifier{fallback: SupportNotEntailed}, false},
		"uncertain trims": {
			&recordingVerifier{fallback: SupportUncertain}, false},
		"an error trims": {
			&recordingVerifier{err: fmt.Errorf("verifier unreachable")}, false},
		"an empty verdict trims": {
			&recordingVerifier{fallback: ""}, false},
		"an unknown verdict trims": {
			&recordingVerifier{fallback: SupportVerdict("probably")}, false},
	} {
		t.Run(name, func(t *testing.T) {
			document := c11Document(model.EpisodeEvidence{
				Text: c11Entailed, EvidenceMessageIDs: []uint{1}})
			episode, _, err := c11Project(t, document, shape.verifier)
			if err != nil {
				t.Fatalf("projection failed: %v", err)
			}
			kinds := []string{}
			for _, a := range model.AssertionsOf(*episode) {
				if a.Kind != model.AssertionSynopsis {
					kinds = append(kinds, string(a.Kind))
				}
			}
			if shape.wantKept {
				if len(kinds) != 1 || episode.Status != model.EpisodeStatusActive {
					t.Fatalf("an entailed assertion was not kept: status=%q kinds=%v",
						episode.Status, kinds)
				}
				return
			}
			if len(kinds) != 0 {
				t.Fatalf("an unverified assertion survived: %v", kinds)
			}
			// 8: everything trimmed abstains with the reason that says the
			// server removed them, not that the model produced nothing.
			if episode.Status != model.EpisodeStatusAbstained {
				t.Fatalf("status is %q, want abstained", episode.Status)
			}
			if episode.AbstainReason != model.AbstainAllAssertionsTrimmed {
				t.Fatalf("abstain reason is %q, want %q",
					episode.AbstainReason, model.AbstainAllAssertionsTrimmed)
			}
		})
	}
}

// --- 9 and 10: one call per assertion, and a minimal input ------------------

func TestC11VerifierIsCalledOncePerAssertionWithMinimalInput(t *testing.T) {
	document := c11Document(
		model.EpisodeEvidence{Text: c11Entailed, EvidenceMessageIDs: []uint{1}},
		model.EpisodeEvidence{Text: c11NotEntailed, EvidenceMessageIDs: []uint{1}},
	)
	verifier := &recordingVerifier{byText: map[string]SupportVerdict{
		c11Entailed:    SupportEntailed,
		c11NotEntailed: SupportNotEntailed,
	}}
	episode, _, err := c11Project(t, document, verifier)
	if err != nil {
		t.Fatalf("projection failed: %v", err)
	}

	requests := verifier.Requests()
	if len(requests) != 2 {
		t.Fatalf("the verifier was called %d times for 2 assertions", len(requests))
	}
	seen := map[string]int{}
	for _, request := range requests {
		seen[request.AssertionText]++
	}
	if seen[c11Entailed] != 1 || seen[c11NotEntailed] != 1 {
		t.Fatalf("calls were not one per assertion: %v", seen)
	}

	for _, request := range requests {
		if request.AssertionKind != model.AssertionDecision {
			t.Fatalf("the request carries kind %q", request.AssertionKind)
		}
		// The whole point of the second layer is that it sees less than the
		// generator did. A verifier handed the assistant turn, the synopsis or
		// a sibling assertion can reproduce the generator's mistake for the
		// generator's reasons.
		for _, body := range request.UserMessages {
			if body != c11UserSay {
				t.Fatalf("the verifier was shown %q, which is not a cited user message", body)
			}
		}
		blob := request.AssertionText + "\n" + strings.Join(request.UserMessages, "\n")
		for label, forbidden := range map[string]string{
			"the assistant turn": c11AssistantSay,
			"the synopsis":       c11Synopsis,
			"a topic label":      "RPC 粘包",
		} {
			if strings.Contains(blob, forbidden) {
				t.Fatalf("the verifier was shown %s", label)
			}
		}
		// A sibling assertion must not travel either.
		other := c11NotEntailed
		if request.AssertionText == c11NotEntailed {
			other = c11Entailed
		}
		if strings.Contains(strings.Join(request.UserMessages, "\n"), other) {
			t.Fatal("the verifier was shown a sibling assertion")
		}
	}

	// The entailed one survives; the other does not.
	kept := []string{}
	for _, a := range model.AssertionsOf(*episode) {
		if a.Kind != model.AssertionSynopsis {
			kept = append(kept, a.Text)
		}
	}
	if len(kept) != 1 || kept[0] != c11Entailed {
		t.Fatalf("surviving assertions are %v", kept)
	}
}

// --- 11: an abstained outcome never reaches the verifier --------------------

func TestC11AbstainedOutcomeDoesNotCallTheVerifier(t *testing.T) {
	verifier := &recordingVerifier{fallback: SupportEntailed}
	// No assertions at all, so the generator abstains before the second layer
	// is relevant. Calling a model to check nothing would be a wasted request
	// on every question a learner ever asks.
	episode, _, err := c11Project(t, c11Document(), verifier)
	if err != nil {
		t.Fatalf("projection failed: %v", err)
	}
	if episode.Status != model.EpisodeStatusAbstained {
		t.Fatalf("status is %q, want abstained", episode.Status)
	}
	if episode.AbstainReason != model.AbstainNoDurableAssertion {
		t.Fatalf("abstain reason is %q, want %q",
			episode.AbstainReason, model.AbstainNoDurableAssertion)
	}
	if verifier.Calls() != 0 {
		t.Fatalf("the verifier was called %d times for a session with no assertions",
			verifier.Calls())
	}
}

// --- 12: a repeat at the same watermark calls neither ------------------------

func TestC11SameWatermarkCallsNeitherGeneratorNorVerifier(t *testing.T) {
	document := c11Document(model.EpisodeEvidence{
		Text: c11Entailed, EvidenceMessageIDs: []uint{1}})
	verifier := &recordingVerifier{fallback: SupportEntailed}
	generator := &stubGenerator{document: document}
	evidence := c11Evidence()
	// The session already has an Episode covering this watermark.
	evidence.Existing = &model.EpisodeMemory{ID: "episode-stable", UserID: 7,
		SessionID: "session-1", Version: 4, SourceLastMessageID: 2}
	projector, err := NewProjector(stubEvidenceLoader{evidence: evidence},
		generator, &stubProjectionRepository{}, verifier)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	episode, err := projector.Project(context.Background(), 7, "session-1", 2)
	if err != nil {
		t.Fatalf("projection failed: %v", err)
	}
	if episode.ID != "episode-stable" {
		t.Fatalf("the repeat did not reuse the existing row: %s", episode.ID)
	}
	if generator.calls != 0 {
		t.Fatalf("the repeat called the generator %d times", generator.calls)
	}
	if verifier.Calls() != 0 {
		t.Fatalf("the repeat called the verifier %d times", verifier.Calls())
	}
}

// --- the production verifier itself ------------------------------------------

func TestC11ModelSupportVerifierRefusesAnIncompleteConfiguration(t *testing.T) {
	if _, err := NewModelSupportVerifier(SupportVerifierConfig{}); err == nil {
		t.Fatal("a verifier with no chat model was accepted")
	}
}

func TestC11ModelSupportVerifierSendsOnlyTheAssertionAndItsCitedMessages(t *testing.T) {
	chat := &episodeChat{responses: []*schema.Message{
		{Role: schema.Assistant, Content: `{"verdict":"entailed"}`}}}
	verifier, err := NewModelSupportVerifier(SupportVerifierConfig{Chat: chat})
	if err != nil {
		t.Fatal(err)
	}
	verdict, err := verifier.Verify(context.Background(), SupportRequest{
		AssertionKind: model.AssertionDecision,
		AssertionText: c11Entailed,
		UserMessages:  []string{c11UserSay},
	})
	if err != nil || verdict != SupportEntailed {
		t.Fatalf("verdict=%q err=%v", verdict, err)
	}
	if len(chat.sent) == 0 {
		t.Fatal("the verifier sent nothing")
	}
	blob := ""
	for _, message := range chat.sent {
		blob += message.Content + "\n"
	}
	if !strings.Contains(blob, c11Entailed) || !strings.Contains(blob, c11UserSay) {
		t.Fatal("the request is missing the assertion or its cited message")
	}
	for label, forbidden := range map[string]string{
		"the assistant turn": c11AssistantSay,
		"a synopsis":         c11Synopsis,
	} {
		if strings.Contains(blob, forbidden) {
			t.Fatalf("the production verifier sent %s to the model", label)
		}
	}
}

func TestC11ModelSupportVerifierDecodesStrictlyAndNeverGuesses(t *testing.T) {
	for name, shape := range map[string]struct {
		raw  string
		want SupportVerdict
	}{
		"entailed":               {`{"verdict":"entailed"}`, SupportEntailed},
		"not entailed":           {`{"verdict":"not_entailed"}`, SupportNotEntailed},
		"uncertain":              {`{"verdict":"uncertain"}`, SupportUncertain},
		"an unknown word":        {`{"verdict":"probably"}`, ""},
		"an extra field":         {`{"verdict":"entailed","why":"because"}`, ""},
		"prose after the object": {`{"verdict":"entailed"} hope that helps`, ""},
		"two objects":            {`{"verdict":"entailed"}{"verdict":"uncertain"}`, ""},
		"markdown fencing":       {"```json\n{\"verdict\":\"entailed\"}\n```", ""},
		"nothing at all":         {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			verdict, err := DecodeSupportVerdict(shape.raw)
			if shape.want == "" {
				if err == nil {
					t.Fatalf("a malformed answer decoded to %q", verdict)
				}
				// The model's own text must never travel in the error: it can
				// carry the learner's words, and this error reaches a log.
				if strings.Contains(err.Error(), shape.raw) && shape.raw != "" {
					t.Fatalf("the error quotes the model's answer: %v", err)
				}
				return
			}
			if err != nil || verdict != shape.want {
				t.Fatalf("verdict=%q err=%v want=%q", verdict, err, shape.want)
			}
		})
	}
}
