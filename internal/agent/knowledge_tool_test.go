package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

const (
	ownerUser   = "user-owner"
	otherUser   = "user-other"
	aofMarkdown = "# AOF 持久化\n\nAOF 的 fsync 策略可以选 always、everysec 或 no。\n"
)

// newKnowledgeFixture builds the same collaborators production wires: an in-memory
// knowledge store, the service that owns ingestion and the authorization gate, and a
// BM25-only pipeline over the real chunks.
func newKnowledgeFixture(t *testing.T) (knowledge.Store, *service.KnowledgeService, retrieval.Retriever, string, string) {
	t.Helper()
	ctx := context.Background()
	store := knowledge.NewMemoryStore()
	ingest := service.NewKnowledgeService(store)
	pipeline := retrieval.NewPipeline(knowledge.NewChunkSource(store),
		[]retrieval.Scorer{retrieval.NewLexicalScorer(retrieval.BM25Options{})}, retrieval.Options{})

	owned, err := ingest.CreateCollection(ctx, ownerUser, "Redis 手册")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.AddDocument(ctx, ownerUser, owned.ID, service.DocumentInput{
		Title: "aof.md", Format: string(domain.DocumentFormatMarkdown), Content: aofMarkdown,
	}); err != nil {
		t.Fatal(err)
	}
	foreign, err := ingest.CreateCollection(ctx, otherUser, "别人的手册")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.AddDocument(ctx, otherUser, foreign.ID, service.DocumentInput{
		Title: "secreto.md", Format: string(domain.DocumentFormatMarkdown),
		Content: "# 别人的 AOF\n\n别人的 fsync 策略也写作 always、everysec 或 no。\n",
	}); err != nil {
		t.Fatal(err)
	}
	return store, ingest, pipeline, owned.ID, foreign.ID
}

func newSearchTool(t *testing.T, scope Scope, retriever retrieval.Retriever, options KnowledgeSearchOptions) *KnowledgeSearch {
	t.Helper()
	tool, err := NewKnowledgeSearch(retriever, scope, options)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func arguments(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(raw)) {
		t.Fatalf("the fixture arguments are not valid JSON: %s", raw)
	}
	return json.RawMessage(raw)
}

// compile-time proof that the production knowledge service is what the tool's Scope
// needs; if it ever stops satisfying the interface, wiring fails here rather than at
// the first agent turn.
var _ Scope = (*service.KnowledgeService)(nil)

func TestKnowledgeSearchHitsItsOwnCollectionAndEarnsCitations(t *testing.T) {
	_, ingest, retriever, ownedID, _ := newKnowledgeFixture(t)
	tool := newSearchTool(t, ingest, retriever, KnowledgeSearchOptions{})

	observation, err := tool.Execute(context.Background(), Principal{UserID: ownerUser},
		arguments(t, `{"query":"AOF 的 fsync 策略可以选哪些"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Found || len(observation.Sources) == 0 {
		t.Fatalf("a question the document answers found nothing: %+v", observation)
	}
	for _, source := range observation.Sources {
		if source.CollectionID != ownedID {
			t.Fatalf("citation came from outside the caller's collection: %+v", source)
		}
		if source.SourceID != "src-"+source.ChunkID {
			t.Fatalf("citation is not derived from a retrieved chunk: %+v", source)
		}
	}
	// What the model reads must carry no identifier it could echo back as a citation.
	for _, forbidden := range []string{ownedID, observation.Sources[0].ChunkID, observation.Sources[0].SourceID} {
		if strings.Contains(observation.Text, forbidden) {
			t.Fatalf("the observation leaked the identifier %q into model-visible text:\n%s", forbidden, observation.Text)
		}
	}
	if !strings.Contains(observation.Text, ObservationLabel) {
		t.Fatalf("the observation does not mark itself as tool data:\n%s", observation.Text)
	}
}

func TestKnowledgeSearchDefaultsToEverythingTheCallerOwns(t *testing.T) {
	_, ingest, retriever, ownedID, foreignID := newKnowledgeFixture(t)
	tool := newSearchTool(t, ingest, retriever, KnowledgeSearchOptions{})

	observation, err := tool.Execute(context.Background(), Principal{UserID: ownerUser}, arguments(t, `{"query":"fsync 策略"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Found {
		t.Fatalf("the default scope searched nothing: %+v", observation)
	}
	for _, source := range observation.Sources {
		if source.CollectionID == foreignID {
			t.Fatalf("a search with no named scope read another user's collection: %+v", source)
		}
	}
	if observation.Sources[0].CollectionID != ownedID {
		t.Fatalf("expected the caller's collection, got %+v", observation.Sources)
	}
}

// TestKnowledgeSearchRefusesACollectionTheCallerDoesNotOwn now covers the narrower
// contract: this tool does not accept a scope from the model at all, so naming any
// collection - somebody else's or one that has never existed - is refused as an undeclared
// field. The two answers are identical, which also keeps the tool from being an oracle for
// which collection ids exist.
func TestKnowledgeSearchRefusesACollectionTheCallerDoesNotOwn(t *testing.T) {
	_, ingest, retriever, _, foreignID := newKnowledgeFixture(t)
	tool := newSearchTool(t, ingest, retriever, KnowledgeSearchOptions{})

	for _, named := range []string{
		`{"query":"fsync 策略","collection_ids":["` + foreignID + `"]}`,
		`{"query":"fsync 策略","collection_ids":["collection-never-created"]}`,
	} {
		_, err := tool.Execute(context.Background(), Principal{UserID: ownerUser}, arguments(t, named))
		if !errors.Is(err, ErrUnknownArgumentField) || !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("named scope %q error = %v, want an undeclared-field refusal", named, err)
		}
	}
}

// TestKnowledgeSearchSchemaAdvertisesOnlyQuery pins the public shape, because a key in the
// schema is an invitation: with no list-collections tool and no id ever shown to the model,
// a scope parameter can only produce guesses that end in a refusal.
func TestKnowledgeSearchSchemaAdvertisesOnlyQuery(t *testing.T) {
	_, ingest, retriever, _, _ := newKnowledgeFixture(t)
	tool := newSearchTool(t, ingest, retriever, KnowledgeSearchOptions{})

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Properties) != 1 {
		t.Fatalf("the tool advertises %d parameters, want only query: %v", len(schema.Properties), schema.Properties)
	}
	if _, declared := schema.Properties["query"]; !declared {
		t.Fatalf("query is missing from the advertised schema: %v", schema.Properties)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "query" {
		t.Fatalf("required = %v, want exactly query", schema.Required)
	}
}

func TestKnowledgeSearchReportsNoEvidenceHonestly(t *testing.T) {
	_, ingest, retriever, _, _ := newKnowledgeFixture(t)
	tool := newSearchTool(t, ingest, retriever, KnowledgeSearchOptions{})

	observation, err := tool.Execute(context.Background(), Principal{UserID: ownerUser},
		arguments(t, `{"query":"光合作用卡尔文循环发生在哪个部位"}`))
	if err != nil {
		t.Fatalf("an honest miss must not be an error: %v", err)
	}
	if observation.Found || len(observation.Sources) != 0 {
		t.Fatalf("a question with nothing to match produced evidence: %+v", observation)
	}
	if !strings.Contains(observation.Text, "没有找到") {
		t.Fatalf("the no-evidence observation is not plain about it:\n%s", observation.Text)
	}
}

func TestKnowledgeSearchBoundsWhatItHandsToTheModel(t *testing.T) {
	ctx := context.Background()
	store := knowledge.NewMemoryStore()
	ingest := service.NewKnowledgeService(store)
	collection, err := ingest.CreateCollection(ctx, ownerUser, "长文档")
	if err != nil {
		t.Fatal(err)
	}
	// One unbroken line of text, so a passage body in the observation can be measured by
	// line without a section header getting in the way.
	sentence := "AOF fsync everysec 写入策略说明。"
	if _, err := ingest.AddDocument(ctx, ownerUser, collection.ID, service.DocumentInput{
		Title: "long.txt", Format: string(domain.DocumentFormatText), Content: strings.Repeat(sentence, 200),
	}); err != nil {
		t.Fatal(err)
	}
	pipeline := retrieval.NewPipeline(knowledge.NewChunkSource(store),
		[]retrieval.Scorer{retrieval.NewLexicalScorer(retrieval.BM25Options{})}, retrieval.Options{})

	const passageRunes = 20
	tool := newSearchTool(t, ingest, pipeline, KnowledgeSearchOptions{
		MaxResults: 2, PassageRunes: passageRunes, ObservationRunes: 200,
	})
	observation, err := tool.Execute(ctx, Principal{UserID: ownerUser}, arguments(t, `{"query":"AOF fsync 写入策略"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Found {
		t.Fatalf("a document built to match produced no evidence: %.200s", observation.Text)
	}
	// Every quoted line is a passage body, and a body may not exceed its per-chunk
	// budget - the ellipsis is the only thing allowed past it. The total observation cap
	// cannot be what enforces this, because it only limits the sum.
	var bodies int
	for _, line := range strings.Split(observation.Text, "\n") {
		if line == "" || strings.HasPrefix(line, "[") || strings.HasPrefix(line, ObservationLabel) {
			continue
		}
		bodies++
		if runes := len([]rune(line)); runes > passageRunes+1 {
			t.Fatalf("a passage body ran past its %d-rune budget: %d runes in %q", passageRunes, runes, line)
		}
	}
	if bodies == 0 {
		t.Fatalf("no passage text reached the model:\n%s", observation.Text)
	}
	// A citation is only owed for text the model actually saw.
	if len(observation.Sources) != bodies {
		t.Fatalf("%d citations for %d passages the model was shown: %+v", len(observation.Sources), bodies, observation.Sources)
	}
	if runes := len([]rune(observation.Text)); runes > 200+len([]rune(ObservationLabel))+120 {
		t.Fatalf("the observation grew past its total budget: %d runes", runes)
	}
}

// TestKnowledgeSearchCitesOnlyWhatFitInTheObservation is the citation half of the
// budget: retrieval may admit several chunks and the observation fit only the first.
// The rest were not shown to the model, so they cannot be presented as evidence for the
// answer.
func TestKnowledgeSearchCitesOnlyWhatFitInTheObservation(t *testing.T) {
	ctx := context.Background()
	store := knowledge.NewMemoryStore()
	ingest := service.NewKnowledgeService(store)
	collection, err := ingest.CreateCollection(ctx, ownerUser, "多段文档")
	if err != nil {
		t.Fatal(err)
	}
	// Three separate documents, so retrieval has three chunks to admit. Making the
	// fixture the thing under test would hide a truncation case behind a chunker that
	// decided to merge everything into one block.
	for index := 0; index < 3; index++ {
		if _, err := ingest.AddDocument(ctx, ownerUser, collection.ID, service.DocumentInput{
			Title:   "part-" + string(rune('a'+index)) + ".txt",
			Format:  string(domain.DocumentFormatText),
			Content: "AOF fsync 写入策略第 " + strconv.Itoa(index+1) + " 部分说明。",
		}); err != nil {
			t.Fatal(err)
		}
	}
	pipeline := retrieval.NewPipeline(knowledge.NewChunkSource(store),
		[]retrieval.Scorer{retrieval.NewLexicalScorer(retrieval.BM25Options{})}, retrieval.Options{TopK: 3})
	all, err := pipeline.Search(ctx, retrieval.Query{UserID: ownerUser, Text: "AOF fsync 策略", CollectionIDs: []string{collection.ID}, TopK: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 {
		t.Fatalf("the fixture must admit at least two chunks to test truncation, got %d", len(all))
	}

	tool := newSearchTool(t, ingest, pipeline, KnowledgeSearchOptions{
		MaxResults: len(all), PassageRunes: 20, ObservationRunes: 60,
	})
	observation, err := tool.Execute(ctx, Principal{UserID: ownerUser}, arguments(t, `{"query":"AOF fsync 策略"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Sources) >= len(all) {
		t.Fatalf("every retrieved chunk was cited although the budget fit %d of %d:\n%s",
			len(observation.Sources), len(all), observation.Text)
	}
	cited := map[string]bool{}
	for _, source := range observation.Sources {
		cited[source.ChunkID] = true
	}
	// Whatever is cited must appear in the observation text, and nothing that does not
	// appear may be cited.
	for _, source := range observation.Sources {
		if !strings.Contains(observation.Text, source.Snippet[:min(8, len([]rune(source.Snippet)))]) &&
			!strings.Contains(observation.Text, source.Section) {
			t.Fatalf("citation %s has no matching text in the observation:\n%s", source.ChunkID, observation.Text)
		}
	}
	if !strings.Contains(observation.Text, "没有放进本轮结果") {
		t.Fatalf("the observation hides that chunks were left out:\n%s", observation.Text)
	}
	for _, result := range all[len(observation.Sources):] {
		if cited[result.Chunk.ID] {
			t.Fatalf("a left-out chunk was still cited: %s", result.Chunk.ID)
		}
	}
}

// TestKnowledgeSearchBoundsTheDefaultScopeAndRefusesNamedOnes is item three's successor:
// a named scope is refused as an undeclared field at any count, while the scope this server
// resolves on its own is bounded and says so. A partial search that hides that it was
// partial is what makes an ungrounded "there is nothing here" look like a finding.
func TestKnowledgeSearchBoundsTheDefaultScopeAndRefusesNamedOnes(t *testing.T) {
	ctx := context.Background()
	store := knowledge.NewMemoryStore()
	ingest := service.NewKnowledgeService(store)
	ids := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		collection, err := ingest.CreateCollection(ctx, ownerUser, "手册")
		if err != nil {
			t.Fatal(err)
		}
		// Three distinct documents: ingestion now refuses a second copy of the same
		// content for the same user, which is the per-user uniqueness v2 enforces
		// with a unique index. The step under test is the scope cap, so only the
		// identity of each document has to vary - the searchable phrase does not.
		if _, err := ingest.AddDocument(ctx, ownerUser, collection.ID, service.DocumentInput{
			Title:   "aof-" + strconv.Itoa(index) + ".txt",
			Format:  string(domain.DocumentFormatText),
			Content: "AOF fsync 策略说明。第 " + strconv.Itoa(index+1) + " 篇。",
		}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, collection.ID)
	}
	pipeline := retrieval.NewPipeline(knowledge.NewChunkSource(store),
		[]retrieval.Scorer{retrieval.NewLexicalScorer(retrieval.BM25Options{})}, retrieval.Options{})
	tool := newSearchTool(t, ingest, pipeline, KnowledgeSearchOptions{MaxCollections: 2})

	// Naming the caller's own three collections is refused too: the parameter does not
	// exist, so there is no count at which it would be accepted.
	named := `{"query":"AOF fsync 策略","collection_ids":["` + strings.Join(ids, `","`) + `"]}`
	if _, err := tool.Execute(ctx, Principal{UserID: ownerUser}, arguments(t, named)); !errors.Is(err, ErrUnknownArgumentField) {
		t.Fatalf("three named collections under a cap of two produced %v, want an undeclared-field refusal", err)
	}
	// With no named scope the search still runs, bounded, and says it was partial.
	observation, err := tool.Execute(ctx, Principal{UserID: ownerUser}, arguments(t, `{"query":"AOF fsync 策略"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(observation.Text, "本轮只搜索了当前用户 3 个知识库中的前 2 个") {
		t.Fatalf("a partial default scope is not disclosed to the model:\n%s", observation.Text)
	}
	for _, source := range observation.Sources {
		if source.CollectionID == ids[2] {
			t.Fatalf("the omitted collection was searched anyway: %+v", source)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestKnowledgeSearchRejectsArgumentsItNeverDeclared also says *which* rule fired, because
// a caller that has to record a refusal has to name it without repeating the payload.
func TestKnowledgeSearchRejectsArgumentsItNeverDeclared(t *testing.T) {
	_, ingest, retriever, _, _ := newKnowledgeFixture(t)
	tool := newSearchTool(t, ingest, retriever, KnowledgeSearchOptions{})
	principal := Principal{UserID: ownerUser}

	cases := []struct {
		name  string
		raw   string
		would error
		rule  error
	}{
		{"unknown key asking for another identity", `{"query":"fsync","user_id":"` + otherUser + `"}`, ErrInvalidArguments, ErrUnknownArgumentField},
		{"unknown key asking for everything", `{"query":"fsync","all_collections":true}`, ErrInvalidArguments, ErrUnknownArgumentField},
		{"a scope parameter that no longer exists", `{"query":"fsync","collection_ids":["c"]}`, ErrInvalidArguments, ErrUnknownArgumentField},
		{"three undeclared keys", `{"query":"fsync","a":1,"b":2,"c":3}`, ErrInvalidArguments, ErrUnknownArgumentField},
		{"empty query", `{"query":"   "}`, ErrInvalidArguments, ErrQueryMissingOrBlank},
		{"query absent entirely", `{}`, ErrInvalidArguments, ErrQueryMissingOrBlank},
		{"over-long query", `{"query":"` + strings.Repeat("策", 900) + `"}`, ErrInvalidArguments, ErrQueryTooLong},
		{"wrong type", `{"query":123}`, ErrInvalidArguments, ErrInvalidArguments},
		{"an array instead of an object", `["fsync"]`, ErrInvalidArguments, ErrArgumentsNotObject},
		{"a bare string", `"fsync"`, ErrInvalidArguments, ErrArgumentsNotObject},
		{"a double-encoded object", `"{\"query\":\"fsync\"}"`, ErrInvalidArguments, ErrArgumentsNotObject},
		{"an empty payload", ``, ErrInvalidArguments, ErrArgumentsNotObject},
		{"truncated json", `{"query":"fs`, ErrInvalidArguments, ErrArgumentsMalformed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// Built raw on purpose: two of these payloads are not valid JSON, which is the
			// thing under test, so the fixture helper that rejects invalid JSON cannot be
			// used here.
			_, err := tool.Execute(context.Background(), principal, json.RawMessage(testCase.raw))
			if !errors.Is(err, testCase.would) {
				t.Fatalf("error = %v, want %v", err, testCase.would)
			}
			if !errors.Is(err, testCase.rule) {
				t.Fatalf("error = %v, want the rule %v to be identifiable without reading text", err, testCase.rule)
			}
		})
	}
}

// TestKnowledgeSearchIgnoresAnIdentityInTheArguments is the point of the principal
// argument: a model that puts a user id into a tool call changes nothing about whose
// collections are searched.
func TestKnowledgeSearchIgnoresAnIdentityInTheArguments(t *testing.T) {
	_, ingest, retriever, ownedID, foreignID := newKnowledgeFixture(t)
	tool := newSearchTool(t, ingest, retriever, KnowledgeSearchOptions{MaxCollections: 16})

	// The unknown key is refused outright, so the claim inside it is never reached.
	if _, err := tool.Execute(context.Background(), Principal{UserID: ownerUser},
		arguments(t, `{"query":"fsync","user_id":"`+otherUser+`"}`)); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("error = %v, want the whole call refused", err)
	}
	// And the same search run as the owner still reads only the owner's collection.
	observation, err := tool.Execute(context.Background(), Principal{UserID: ownerUser}, arguments(t, `{"query":"fsync 策略"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range observation.Sources {
		if source.CollectionID == foreignID {
			t.Fatalf("another user's collection was read: %+v", source)
		}
	}
	if observation.Sources[0].CollectionID != ownedID {
		t.Fatalf("expected %s, got %+v", ownedID, observation.Sources)
	}
}

// laxScope answers "yes" to every readability question. It exists to prove the second gate
// is not the only thing standing between a turn and another user's collection: if the
// readable check regressed, the scope this server resolves from the caller's own libraries
// still has to be the caller's.
type laxScope struct{ inner Scope }

func (l laxScope) ListCollections(ctx context.Context, userID string) ([]domain.Collection, error) {
	return l.inner.ListCollections(ctx, userID)
}

func (l laxScope) DocumentTitles(ctx context.Context, userID string, ids []string) (map[string]string, error) {
	return l.inner.DocumentTitles(ctx, userID, ids)
}

func (l laxScope) EnsureReadable(context.Context, string, []string) error { return nil }

func TestKnowledgeSearchOwnershipHoldsEvenIfTheReadableGateIsLax(t *testing.T) {
	_, ingest, retriever, _, foreignID := newKnowledgeFixture(t)
	tool := newSearchTool(t, laxScope{inner: ingest}, retriever, KnowledgeSearchOptions{})

	// Naming a collection is refused as an undeclared field before any scope question is
	// reached, so a lax second gate never gets the chance to widen anything.
	if _, err := tool.Execute(context.Background(), Principal{UserID: ownerUser},
		arguments(t, `{"query":"fsync 策略","collection_ids":["`+foreignID+`"]}`)); !errors.Is(err, ErrUnknownArgumentField) {
		t.Fatalf("error = %v, want an undeclared-field refusal", err)
	}
	// And the default scope still cannot reach it either.
	observation, err := tool.Execute(context.Background(), Principal{UserID: ownerUser}, arguments(t, `{"query":"别人的 AOF"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range observation.Sources {
		if source.CollectionID == foreignID {
			t.Fatalf("a lax gate plus a default scope read another user's collection: %+v", source)
		}
	}
}

// denyAfterListing is the second gate on its own: a scope that lists a collection as
// the caller's and then refuses to let it be read. Ownership is checked twice for
// independence, so a tool must not survive the second denial by having trusted the
// first one.
type denyAfterListing struct {
	inner   Scope
	denied  string
	readAny error
}

func (d *denyAfterListing) ListCollections(ctx context.Context, userID string) ([]domain.Collection, error) {
	return d.inner.ListCollections(ctx, userID)
}

func (d *denyAfterListing) DocumentTitles(ctx context.Context, userID string, ids []string) (map[string]string, error) {
	return d.inner.DocumentTitles(ctx, userID, ids)
}

func (d *denyAfterListing) EnsureReadable(ctx context.Context, userID string, ids []string) error {
	if d.readAny != nil {
		return d.readAny
	}
	for _, id := range ids {
		if id == d.denied {
			return service.ErrNotFound
		}
	}
	return d.inner.EnsureReadable(ctx, userID, ids)
}

// TestKnowledgeSearchRunsTheReadableGateAfterOwnership is the second gate on its own, now
// reachable without the model naming anything: the scope this server resolved from the
// caller's own collections has to pass the same readable check a grounded chat turn passes,
// and a denial there ends the turn rather than searching the rest quietly.
func TestKnowledgeSearchRunsTheReadableGateAfterOwnership(t *testing.T) {
	_, ingest, retriever, ownedID, _ := newKnowledgeFixture(t)
	gated := &denyAfterListing{inner: ingest, denied: ownedID}
	tool := newSearchTool(t, gated, retriever, KnowledgeSearchOptions{})

	if _, err := tool.Execute(context.Background(), Principal{UserID: ownerUser},
		arguments(t, `{"query":"fsync"}`)); !errors.Is(err, ErrForeignCollection) {
		t.Fatalf("error = %v, want the second gate to end the default-scope turn", err)
	}
	// A caller whose libraries all pass the gate searches normally, so the refusal above is
	// the gate and not the shape of the tool.
	plain := newSearchTool(t, ingest, retriever, KnowledgeSearchOptions{})
	if observation, err := plain.Execute(context.Background(), Principal{UserID: ownerUser},
		arguments(t, `{"query":"fsync"}`)); err != nil || !observation.Found {
		t.Fatalf("the ungated search failed: observation %+v err %v", observation, err)
	}
}

func TestKnowledgeSearchNeedsAPrincipal(t *testing.T) {
	_, ingest, retriever, _, _ := newKnowledgeFixture(t)
	tool := newSearchTool(t, ingest, retriever, KnowledgeSearchOptions{})
	if _, err := tool.Execute(context.Background(), Principal{}, arguments(t, `{"query":"fsync"}`)); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("error = %v, want ErrNoPrincipal", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(canceled, Principal{UserID: ownerUser}, arguments(t, `{"query":"fsync"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation to reach the caller", err)
	}
}

func TestKnowledgeSearchConfigurationIsRefusedAtWiring(t *testing.T) {
	_, ingest, retriever, _, _ := newKnowledgeFixture(t)
	if _, err := NewKnowledgeSearch(nil, ingest, KnowledgeSearchOptions{}); err == nil {
		t.Fatal("a search tool with no retriever was accepted")
	}
	if _, err := NewKnowledgeSearch(retriever, nil, KnowledgeSearchOptions{}); err == nil {
		t.Fatal("a search tool with no scope was accepted")
	}
}
