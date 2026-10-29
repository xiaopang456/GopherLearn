package httpapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// The public shape of a memory, and the whole of what a client may send to add one.
//
// `origin` is the field that decides which other fields are allowed to be empty, so it is
// also the one a client most usefully could lie about: hand the API a row marked
// "user_authored" and it stops needing the provenance that makes an agent's claim
// checkable. The answer here is not a validator that rejects the bad value, it is that no
// request body or response body has a place to put one - which is what these two tests pin,
// because a field list is easy to extend and hard to notice having been extended.

func jsonNames(target any) []string {
	structType := reflect.TypeOf(target)
	for structType.Kind() == reflect.Pointer {
		structType = structType.Elem()
	}
	names := make([]string, 0, structType.NumField())
	for index := 0; index < structType.NumField(); index++ {
		field := structType.Field(index)
		if !field.IsExported() {
			continue
		}
		name := strings.TrimSpace(strings.Split(field.Tag.Get("json"), ",")[0])
		if name == "" {
			name = field.Name
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestTheMemoryViewCarriesNoOriginAndNoOwner(t *testing.T) {
	want := []string{"confidence", "content", "created_at", "memory_id", "scope",
		"source_message_ids", "source_session_id", "status", "topic_key", "updated_at"}
	if got := jsonNames(memoryView{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("memory view fields = %v, want exactly %v: a new name here is a new public promise", got, want)
	}

	// Read from the live response as well as from the struct, so a change that renames a tag
	// cannot pass by matching itself.
	fixture := newMemoryFixture(t)
	owner := issueTestToken(t, fixture.server, fixture.owner)
	created := postMemory(t, fixture, owner, addMemoryBody("global", "", "我把压测留到发布前做"))
	if created.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want a hand-written add to be accepted and final", created.Code, created.Body.String())
	}
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s: %v", created.Body.String(), err)
	}
	shown := make([]string, 0, len(envelope.Data))
	for name := range envelope.Data {
		shown = append(shown, name)
	}
	sort.Strings(shown)
	// `topic_key` is omitted when the row has no subject, so this is the same list minus it.
	withoutSubject := make([]string, 0, len(want))
	for _, name := range want {
		if name != "topic_key" {
			withoutSubject = append(withoutSubject, name)
		}
	}
	if !reflect.DeepEqual(shown, withoutSubject) {
		t.Fatalf("response keys = %v, want the view's own names minus an empty topic key (%v)", shown, withoutSubject)
	}
	for _, forbidden := range []string{"origin", "user_id", "superseded_at", "deleted_at"} {
		if _, present := envelope.Data[forbidden]; present {
			t.Fatalf("the response carries %q: %s", forbidden, created.Body.String())
		}
	}
	// And the row the response describes is still the internally-consistent record: active,
	// authored by its writer, citing nothing.
	stored := decodeMemory(t, created)
	if stored.Status != domain.MemoryStatusActive || stored.Confidence != 1 ||
		stored.SourceSessionID != "" || len(stored.SourceMessageIDs) != 0 {
		t.Fatalf("the response describes %+v, want an active hand-written memory with no provenance", stored)
	}
}

func TestTheHandWrittenAddRequestHasThreeNamesAndNoOther(t *testing.T) {
	want := []string{"content", "scope", "topic_key"}
	if got := jsonNames(addUserMemoryRequest{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("request fields = %v, want exactly %v: every name added here needs a rule about who may set it", got, want)
	}
	// The decoder is the other half: an unknown key is a refusal, not something to ignore.
	// Otherwise `{"scope":"global","content":"x","origin":"user_authored"}` would be a way to
	// write a row this server believes without a confirmation behind it.
	fixture := newMemoryFixture(t)
	owner := issueTestToken(t, fixture.server, fixture.owner)
	// The fixture seeds its own memories, so the claim here is the one that matters: a
	// refused request added nothing to what was already there.
	before := listMemories(t, fixture, owner, "active")
	for _, body := range []string{
		`{"scope":"global","content":"一句话","origin":"user_authored"}`,
		`{"scope":"global","content":"一句话","status":"pending"}`,
		`{"scope":"global","content":"一句话","confidence":0.2}`,
		`{"scope":"global","content":"一句话","source_session_id":"` + fixture.session + `","source_message_ids":["message-forged"]}`,
	} {
		response := postMemory(t, fixture, owner, body)
		if response.Code != http.StatusBadRequest ||
			!strings.Contains(response.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("status=%d body=%s, want the unknown key refused for %s",
				response.Code, response.Body.String(), body)
		}
	}
	if after := listMemories(t, fixture, owner, "active"); len(after) != len(before) {
		t.Fatalf("a refused request stored something: %d active row(s) before, %d after", len(before), len(after))
	}
}
