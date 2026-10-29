package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// apiResponder records the responder.Request each path receives so tests can
// assert the server, not the client, decided the mode and strategy.
type apiResponder struct {
	streamErr      error
	respondRequest chan responder.Request
	streamRequest  chan responder.Request
	streamCalled   chan struct{}
	// steps makes this double the agent harness for a step stream: it reports progress,
	// and it only claims the capability for a turn whose strategy has one.
	steps []responder.Step
}

// SupportsSteps mirrors what the real responder answers: the strategy resolved for this
// turn decides, never what the client asked for.
func (a apiResponder) SupportsSteps(request responder.Request) bool {
	return request.AnswerStrategy == domain.AnswerStrategyAgent
}

// StreamSteps reports the prepared progress and then answers exactly the way Stream does,
// so a test that compares the two cannot be fooled by a double that streams differently.
func (a apiResponder) StreamSteps(ctx context.Context, request responder.Request, emit func(string) error,
	onStep responder.StepSink,
) (responder.Answer, error) {
	for _, step := range a.steps {
		if err := onStep(step); err != nil {
			return responder.Answer{}, err
		}
	}
	return a.Stream(ctx, request, emit)
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (r *flushRecorder) Flush() {
	r.flushes++
}

func (a apiResponder) Respond(ctx context.Context, request responder.Request) (responder.Answer, error) {
	if a.respondRequest != nil {
		a.respondRequest <- request
	}
	if a.streamErr != nil {
		return responder.Answer{}, a.streamErr
	}
	return responder.Answer{Content: "safe answer"}, nil
}

func (a apiResponder) Stream(ctx context.Context, request responder.Request, emit func(string) error) (responder.Answer, error) {
	if a.streamCalled != nil {
		select {
		case a.streamCalled <- struct{}{}:
		default:
		}
	}
	if a.streamRequest != nil {
		a.streamRequest <- request
	}
	if err := emit("safe "); err != nil {
		return responder.Answer{}, err
	}
	if err := emit("answer"); err != nil {
		return responder.Answer{}, err
	}
	if a.streamErr != nil {
		return responder.Answer{}, a.streamErr
	}
	return responder.Answer{Content: "safe answer"}, nil
}

func newAPIFixture(t *testing.T, assistant responder.Responder) (*Server, string, string, string) {
	t.Helper()
	fixture := newFixture(t, assistant)
	return fixture.server, fixture.owner, fixture.other, fixture.session
}

func doJSON(t *testing.T, server *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func requireStatus(t *testing.T, response *httptest.ResponseRecorder, want int, context string) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("%s status=%d, want %d, body=%s", context, response.Code, want, response.Body.String())
	}
}

func TestRegisterLoginMeAndSanitizedErrors(t *testing.T) {
	server, _, _, _ := newAPIFixture(t, apiResponder{})
	register := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"email":"new@example.com","password":"correct horse","display_name":"New"}`))
	registerResponse := httptest.NewRecorder()
	server.ServeHTTP(registerResponse, register)
	if registerResponse.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", registerResponse.Code, registerResponse.Body.String())
	}
	if strings.Contains(registerResponse.Body.String(), "password") || strings.Contains(registerResponse.Body.String(), "hash") {
		t.Fatalf("register response leaked sensitive data: %s", registerResponse.Body.String())
	}

	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"new@example.com","password":"correct horse"}`))
	loginResponse := httptest.NewRecorder()
	server.ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
	}
	var envelope struct {
		Data struct {
			Token string `json:"token"`
			User  struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"data"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(loginResponse.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Token == "" || envelope.Data.User.ID == "" || envelope.Error != nil {
		t.Fatalf("invalid login envelope: %#v", envelope)
	}
	me := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	me.Header.Set("Authorization", "Bearer "+envelope.Data.Token)
	meResponse := httptest.NewRecorder()
	server.ServeHTTP(meResponse, me)
	if meResponse.Code != http.StatusOK {
		t.Fatalf("me status=%d body=%s", meResponse.Code, meResponse.Body.String())
	}

	badLogin := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"new@example.com","password":"not-the-password"}`))
	badResponse := httptest.NewRecorder()
	server.ServeHTTP(badResponse, badLogin)
	if badResponse.Code != http.StatusUnauthorized || strings.Contains(badResponse.Body.String(), "not-the-password") {
		t.Fatalf("unsafe bad-login response: %d %s", badResponse.Code, badResponse.Body.String())
	}
}

func TestSessionModesAreRequiredAndReturned(t *testing.T) {
	server, userID, _, _ := newAPIFixture(t, apiResponder{})
	token := issueTestToken(t, server, userID)
	modes := []domain.SessionMode{
		domain.SessionModeChat,
		domain.SessionModeLearningAgent,
	}
	for index, mode := range modes {
		body := `{"title":"Mode ` + string(rune('A'+index)) + `","mode":"` + string(mode) + `"}`
		response := doJSON(t, server, http.MethodPost, "/api/v1/sessions", token, body)
		requireStatus(t, response, http.StatusOK, "create mode "+string(mode))
		if !strings.Contains(response.Body.String(), `"mode":"`+string(mode)+`"`) {
			t.Fatalf("mode %q missing from response: %s", mode, response.Body.String())
		}
	}

	listed := listSessions(t, server, token)
	if len(listed) != 1+len(modes) {
		t.Fatalf("listed %d sessions, want %d", len(listed), 1+len(modes))
	}
	seen := make(map[domain.SessionMode]bool)
	for _, item := range listed {
		seen[item.Mode] = true
	}
	for _, mode := range modes {
		if !seen[mode] {
			t.Errorf("mode %q missing from list", mode)
		}
	}

	invalidBodies := []string{
		`{"title":"missing mode"}`,
		`{"title":"bad mode","mode":"unsupported"}`,
		`{"title":"retired mode","mode":"fast_chat"}`,
		`{"title":"strategy is not a mode","mode":"knowledge_qa"}`,
		`{"title":"strategy is not a mode","mode":"direct_chat"}`,
		`{"title":"strategy is not a mode","mode":"agent"}`,
		`{"title":"empty mode","mode":""}`,
		`{"title":"wrong type","mode":1}`,
	}
	for _, body := range invalidBodies {
		response := doJSON(t, server, http.MethodPost, "/api/v1/sessions", token, body)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("invalid mode body=%s status/body=%d %s", body, response.Code, response.Body.String())
		}
	}
	if after := listSessions(t, server, token); len(after) != len(listed) {
		t.Fatalf("invalid mode wrote a session: before=%d after=%d", len(listed), len(after))
	}
}

type sessionListItem struct {
	ID    string             `json:"id"`
	Title string             `json:"title"`
	Mode  domain.SessionMode `json:"mode"`
}

func listSessions(t *testing.T, server *Server, token string) []sessionListItem {
	t.Helper()
	response := doJSON(t, server, http.MethodGet, "/api/v1/sessions", token, "")
	requireStatus(t, response, http.StatusOK, "list sessions")
	var envelope struct {
		Data struct {
			Items []sessionListItem `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data.Items
}

func TestSessionModeHasNoUpdateRoute(t *testing.T) {
	server, userID, _, sessionID := newAPIFixture(t, apiResponder{})
	token := issueTestToken(t, server, userID)
	body := `{"mode":"learning_agent"}`
	methods := []string{http.MethodPut, http.MethodPatch, http.MethodPost}
	for _, method := range methods {
		response := doJSON(t, server, method, "/api/v1/sessions/"+sessionID, token, body)
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s on a session status=%d, want 405: %s", method, response.Code, response.Body.String())
		}
	}
	response := doJSON(t, server, http.MethodGet, "/api/v1/sessions/"+sessionID, token, "")
	if response.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on a session status=%d, want 405: %s", response.Code, response.Body.String())
	}
	for _, item := range listSessions(t, server, token) {
		if item.ID == sessionID && item.Mode != domain.SessionModeChat {
			t.Fatalf("stored mode changed to %q", item.Mode)
		}
	}
}

func TestResponderReceivesServerResolvedStrategy(t *testing.T) {
	cases := []struct {
		name         string
		sessionMode  domain.SessionMode
		body         string
		wantStrategy domain.AnswerStrategy
		wantIDs      []string
		// wantStatus is the HTTP status for the synchronous call; zero means the
		// turn is expected to succeed.
		wantStatus int
		// wantBodyContains switches a grounded case to citation assertions,
		// because the knowledge strategy answers it instead of the double.
		wantBodyContains []string
	}{
		{
			name:         "chat defaults to direct_chat",
			sessionMode:  domain.SessionModeChat,
			body:         `{"content":"question"}`,
			wantStrategy: domain.AnswerStrategyDirectChat,
		},
		{
			name:         "chat with an explicit disabled toggle",
			sessionMode:  domain.SessionModeChat,
			body:         `{"content":"question","knowledge":{"enabled":false,"collection_ids":[]}}`,
			wantStrategy: domain.AnswerStrategyDirectChat,
		},
		{
			name:         "chat with knowledge trims and deduplicates collection ids",
			sessionMode:  domain.SessionModeChat,
			body:         `{"content":"Redis 持久化 AOF","knowledge":{"enabled":true,"collection_ids":["  {{collection}}  ","{{collection}}"]}}`,
			wantStrategy: domain.AnswerStrategyKnowledgeQA,
			// The scope must name a collection the caller owns: an unknown or
			// foreign id is refused before anything is retrieved.
			wantIDs: []string{"{{collection}}"},
			wantBodyContains: []string{
				`"sources"`,
				`"collection_id":"` + collectionPlaceholder + `"`,
				`"title":"redis-persistence.md"`,
				`"document_id":"` + documentPlaceholder + `"`,
			},
		},
		{
			name:         "chat with knowledge refuses a collection it does not own",
			sessionMode:  domain.SessionModeChat,
			body:         `{"content":"question","knowledge":{"enabled":true,"collection_ids":["{{foreign}}"]}}`,
			wantStrategy: domain.AnswerStrategyKnowledgeQA,
			wantStatus:   http.StatusNotFound,
		},
		{
			name:         "learning agent always uses the agent strategy",
			sessionMode:  domain.SessionModeLearningAgent,
			body:         `{"content":"question"}`,
			wantStrategy: domain.AnswerStrategyAgent,
		},
	}

	replacements := []string{sessionPlaceholder, "", collectionPlaceholder, "", documentPlaceholder, "", foreignPlaceholder, "", foreignDocumentPlaceholder, ""}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assistant := apiResponder{
				respondRequest: make(chan responder.Request, 4),
				streamRequest:  make(chan responder.Request, 4),
			}
			fixture := newFixture(t, assistant)
			replacements[1] = fixture.session
			replacements[3] = fixture.collection
			replacements[5] = fixture.document
			replacements[7] = fixture.foreignCollection
			replacements[9] = fixture.foreignDocument
			replacer := strings.NewReplacer(replacements...)
			body := replacer.Replace(testCase.body)
			wantIDs := []string(nil)
			for _, want := range testCase.wantIDs {
				wantIDs = append(wantIDs, replacer.Replace(want))
			}
			server, token := fixture.server, issueTestToken(t, fixture.server, fixture.owner)
			sessionID := createSession(t, server, token, testCase.sessionMode)

			wantStatus := testCase.wantStatus
			if wantStatus == 0 {
				wantStatus = http.StatusOK
			}
			syncResponse := doJSON(t, server, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", token, body)
			requireStatus(t, syncResponse, wantStatus, "sync message")
			if wantStatus != http.StatusOK {
				return
			}
			if len(testCase.wantBodyContains) > 0 {
				// A grounded turn is answered by the knowledge strategy, not by
				// the capturing double, so the observable proof is the citation set
				// the response carries: it must name the collection and document the
				// trimmed, deduplicated scope pointed at.
				responseBody := syncResponse.Body.String()
				for _, want := range testCase.wantBodyContains {
					resolved := replacer.Replace(want)
					if !strings.Contains(responseBody, resolved) {
						t.Fatalf("grounded response is missing %q: %s", resolved, responseBody)
					}
				}
				if strings.Contains(responseBody, "{{") {
					t.Fatalf("response leaked an unresolved placeholder: %s", responseBody)
				}
				return
			}
			sync := receiveRequest(t, assistant.respondRequest)
			assertRequest(t, "sync", sync, testCase.sessionMode, testCase.wantStrategy, wantIDs)

			recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/stream", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+token)
			server.ServeHTTP(recorder, request)
			requireStatus(t, recorder.ResponseRecorder, http.StatusOK, "stream message")
			streamed := receiveRequest(t, assistant.streamRequest)
			assertRequest(t, "stream", streamed, testCase.sessionMode, testCase.wantStrategy, wantIDs)

			// Both routes must resolve the same turn. History is one field allowed to
			// differ: by the time the stream runs, the synchronous call has stored its
			// own pair, so seeing more conversation is correct. The stored id of the
			// question being answered is the other - these are two different turns, so
			// agreeing on it would be the bug, not the fix.
			syncContext, streamContext := sync, streamed
			syncContext.History, streamContext.History = nil, nil
			syncContext.UserMessageID, streamContext.UserMessageID = "", ""
			if !reflect.DeepEqual(syncContext, streamContext) {
				t.Fatalf("sync and stream resolved different requests: %#v vs %#v", sync, streamed)
			}
			if sync.UserMessageID == "" || streamed.UserMessageID == "" || sync.UserMessageID == streamed.UserMessageID {
				t.Fatalf("the two routes must each cite their own stored question, got %q and %q",
					sync.UserMessageID, streamed.UserMessageID)
			}
			if len(sync.History) != 0 {
				t.Fatalf("a first turn was handed %d history turns: %#v", len(sync.History), sync.History)
			}
			if len(streamed.History) != 2 {
				t.Fatalf("the streamed turn saw %d history turns, want the pair the sync turn stored: %#v", len(streamed.History), streamed.History)
			}
		})
	}
}

func assertRequest(t *testing.T, path string, got responder.Request, wantMode domain.SessionMode, wantStrategy domain.AnswerStrategy, wantIDs []string) {
	t.Helper()
	if got.SessionMode != wantMode {
		t.Fatalf("%s responder mode=%q, want the stored mode %q", path, got.SessionMode, wantMode)
	}
	if got.AnswerStrategy != wantStrategy {
		t.Fatalf("%s responder strategy=%q, want %q", path, got.AnswerStrategy, wantStrategy)
	}
	wantEnabled := wantStrategy == domain.AnswerStrategyKnowledgeQA
	if got.KnowledgeEnabled != wantEnabled {
		t.Fatalf("%s knowledge enabled=%v, want %v", path, got.KnowledgeEnabled, wantEnabled)
	}
	if !reflect.DeepEqual(got.KnowledgeCollectionIDs, wantIDs) {
		t.Fatalf("%s collection ids=%#v, want %#v", path, got.KnowledgeCollectionIDs, wantIDs)
	}
}

func createSession(t *testing.T, server *Server, token string, mode domain.SessionMode) string {
	t.Helper()
	response := doJSON(t, server, http.MethodPost, "/api/v1/sessions", token, `{"title":"Seeded","mode":"`+string(mode)+`"}`)
	requireStatus(t, response, http.StatusOK, "create session")
	var envelope struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.ID == "" {
		t.Fatalf("created session has no id: %s", response.Body.String())
	}
	return envelope.Data.ID
}

func receiveRequest(t *testing.T, requests <-chan responder.Request) responder.Request {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("responder did not receive a request")
		return responder.Request{}
	}
}

func TestMessageRequestsRejectClientOwnedFields(t *testing.T) {
	// Mode is a session fact set once at creation and the answer strategy is
	// derived server-side, so both must be unknown JSON fields here.
	invalidBodies := []string{
		`{"content":"question","mode":"learning_agent"}`,
		`{"content":"question","mode":"chat"}`,
		`{"content":"question","answer_strategy":"knowledge_qa"}`,
		`{"content":"question","knowledge":{"enabled":true,"collection_ids":["a"],"collection":"a"}}`,
		`{"content":"question","knowledge":{"enabled":"true"}}`,
		`{"content":"question","knowledge":{"collection_ids":"a"}}`,
	}
	for _, body := range invalidBodies {
		t.Run(body, func(t *testing.T) {
			called := make(chan struct{}, 4)
			assistant := apiResponder{
				respondRequest: make(chan responder.Request, 8),
				streamRequest:  make(chan responder.Request, 8),
				streamCalled:   called,
			}
			server, userID, _, sessionID := newAPIFixture(t, assistant)
			token := issueTestToken(t, server, userID)

			syncResponse := doJSON(t, server, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", token, body)
			if syncResponse.Code != http.StatusBadRequest || !strings.Contains(syncResponse.Body.String(), `"code":"invalid_request"`) {
				t.Fatalf("sync body=%s status/body=%d %s", body, syncResponse.Code, syncResponse.Body.String())
			}
			streamRecorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/stream", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+token)
			server.ServeHTTP(streamRecorder, request)
			if streamRecorder.Code != http.StatusBadRequest || !strings.Contains(streamRecorder.Body.String(), `"code":"invalid_request"`) {
				t.Fatalf("stream body=%s status/body=%d %s", body, streamRecorder.Code, streamRecorder.Body.String())
			}
			if strings.HasPrefix(streamRecorder.Header().Get("Content-Type"), "text/event-stream") || strings.Contains(streamRecorder.Body.String(), "event: ") {
				t.Fatalf("rejected stream committed an SSE response: headers=%v body=%s", streamRecorder.Header(), streamRecorder.Body.String())
			}

			// The stored session fact is untouched: a valid follow-up turn still
			// resolves from the mode created earlier.
			valid := doJSON(t, server, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", token, `{"content":"question"}`)
			requireStatus(t, valid, http.StatusOK, "valid follow-up turn")
			got := receiveRequest(t, assistant.respondRequest)
			if got.SessionMode != domain.SessionModeChat || got.AnswerStrategy != domain.AnswerStrategyDirectChat {
				t.Fatalf("rejected bodies altered the resolved turn: %#v", got)
			}
			select {
			case <-called:
				t.Fatal("rejected stream body entered the responder")
			default:
			}
			if pending := len(assistant.streamRequest); pending != 0 {
				t.Fatalf("rejected stream body reached the responder %d times", pending)
			}
		})
	}
}

func TestLearningAgentRejectsTheChatKnowledgeToggle(t *testing.T) {
	assistant := apiResponder{
		respondRequest: make(chan responder.Request, 8),
		streamRequest:  make(chan responder.Request, 8),
	}
	server, userID, _, _ := newAPIFixture(t, assistant)
	token := issueTestToken(t, server, userID)
	sessionID := createSession(t, server, token, domain.SessionModeLearningAgent)

	bodies := []string{
		`{"content":"question","knowledge":{"enabled":true,"collection_ids":["collection-1"]}}`,
		`{"content":"question","knowledge":{"enabled":true}}`,
	}
	for _, body := range bodies {
		syncResponse := doJSON(t, server, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", token, body)
		if syncResponse.Code != http.StatusBadRequest || !strings.Contains(syncResponse.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("sync body=%s status/body=%d %s", body, syncResponse.Code, syncResponse.Body.String())
		}
		streamRecorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/stream", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		server.ServeHTTP(streamRecorder, request)
		if streamRecorder.Code != http.StatusBadRequest || !strings.Contains(streamRecorder.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("stream body=%s status/body=%d %s", body, streamRecorder.Code, streamRecorder.Body.String())
		}
		if strings.Contains(streamRecorder.Body.String(), "event: ") {
			t.Fatalf("rejected agent turn emitted SSE events: %s", streamRecorder.Body.String())
		}
	}
	select {
	case request := <-assistant.respondRequest:
		t.Fatalf("rejected agent turn still reached the responder: %#v", request)
	default:
	}

	// The same agent session keeps working without the toggle.
	valid := doJSON(t, server, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", token, `{"content":"question"}`)
	requireStatus(t, valid, http.StatusOK, "agent turn without the toggle")
	got := receiveRequest(t, assistant.respondRequest)
	if got.SessionMode != domain.SessionModeLearningAgent || got.AnswerStrategy != domain.AnswerStrategyAgent {
		t.Fatalf("agent turn resolved to %#v", got)
	}
}

func TestChatKnowledgeShapeRules(t *testing.T) {
	rejectedBodies := []string{
		`{"content":"question","knowledge":{"enabled":true}}`,
		`{"content":"question","knowledge":{"enabled":true,"collection_ids":[]}}`,
		`{"content":"question","knowledge":{"enabled":true,"collection_ids":["  "]}}`,
		`{"content":"question","knowledge":{"enabled":true,"collection_ids":["a",""]}}`,
		`{"content":"question","knowledge":{"enabled":false,"collection_ids":["a"]}}`,
		`{"content":"question","knowledge":{"collection_ids":["a"]}}`,
	}
	assistant := apiResponder{respondRequest: make(chan responder.Request, 32), streamRequest: make(chan responder.Request, 32)}
	server, userID, _, sessionID := newAPIFixture(t, assistant)
	token := issueTestToken(t, server, userID)

	for _, body := range rejectedBodies {
		syncResponse := doJSON(t, server, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", token, body)
		if syncResponse.Code != http.StatusBadRequest || !strings.Contains(syncResponse.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("sync body=%s status/body=%d %s", body, syncResponse.Code, syncResponse.Body.String())
		}
		streamRecorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/stream", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		server.ServeHTTP(streamRecorder, request)
		if streamRecorder.Code != http.StatusBadRequest || strings.Contains(streamRecorder.Body.String(), "event: ") {
			t.Fatalf("stream body=%s status/body=%d %s", body, streamRecorder.Code, streamRecorder.Body.String())
		}
	}
	if got := countStoredMessages(t, server, token, sessionID); got != 0 {
		t.Fatalf("rejected knowledge bodies persisted %d messages", got)
	}
}

func TestRetiredMessageAliasIsRejectedWithoutSideEffects(t *testing.T) {
	// "content" has no alias: a body carrying "message" is an unknown field, so
	// the request must be refused before any write or model call.
	invalidBodies := []string{
		`{"message":"question"}`,
		`{"content":"question","message":"question"}`,
		`{"message":"question","knowledge":{"enabled":true,"collection_ids":["collection-1"]}}`,
		`{"message":"question","mode":"chat"}`,
	}
	for _, body := range invalidBodies {
		t.Run(body, func(t *testing.T) {
			called := make(chan struct{}, 4)
			assistant := apiResponder{
				respondRequest: make(chan responder.Request, 8),
				streamRequest:  make(chan responder.Request, 8),
				streamCalled:   called,
			}
			server, userID, _, sessionID := newAPIFixture(t, assistant)
			token := issueTestToken(t, server, userID)

			syncResponse := doJSON(t, server, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", token, body)
			if syncResponse.Code != http.StatusBadRequest || !strings.Contains(syncResponse.Body.String(), `"code":"invalid_request"`) {
				t.Fatalf("sync body=%s status/body=%d %s", body, syncResponse.Code, syncResponse.Body.String())
			}

			streamRecorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/stream", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+token)
			server.ServeHTTP(streamRecorder, request)
			if streamRecorder.Code != http.StatusBadRequest || !strings.Contains(streamRecorder.Body.String(), `"code":"invalid_request"`) {
				t.Fatalf("stream body=%s status/body=%d %s", body, streamRecorder.Code, streamRecorder.Body.String())
			}
			if strings.HasPrefix(streamRecorder.Header().Get("Content-Type"), "text/event-stream") || strings.Contains(streamRecorder.Body.String(), "event: ") {
				t.Fatalf("rejected stream committed an SSE response: headers=%v body=%s", streamRecorder.Header(), streamRecorder.Body.String())
			}

			if got := countStoredMessages(t, server, token, sessionID); got != 0 {
				t.Fatalf("rejected alias body=%s persisted %d messages", body, got)
			}
			if pending := len(assistant.respondRequest); pending != 0 {
				t.Fatalf("rejected sync body reached the responder %d times", pending)
			}
			if pending := len(assistant.streamRequest); pending != 0 {
				t.Fatalf("rejected stream body reached the responder %d times", pending)
			}
			select {
			case <-called:
				t.Fatal("rejected stream body entered the responder")
			default:
			}
		})
	}
}

func countStoredMessages(t *testing.T, server *Server, token, sessionID string) int {
	t.Helper()
	response := doJSON(t, server, http.MethodGet, "/api/v1/sessions/"+sessionID+"/messages", token, "")
	requireStatus(t, response, http.StatusOK, "list messages")
	var envelope struct {
		Data struct {
			Items []json.RawMessage `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return len(envelope.Data.Items)
}

func TestMessagesUseStoredSessionMode(t *testing.T) {
	respondRequest := make(chan responder.Request, 1)
	streamRequest := make(chan responder.Request, 1)
	server, userID, _, sessionID := newAPIFixture(t, apiResponder{
		respondRequest: respondRequest,
		streamRequest:  streamRequest,
	})
	token := issueTestToken(t, server, userID)

	syncResponse := doJSON(t, server, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", token, `{"content":"question"}`)
	requireStatus(t, syncResponse, http.StatusOK, "sync message")
	if got := <-respondRequest; got.SessionMode != domain.SessionModeChat {
		t.Fatalf("sync responder mode=%q, want stored mode %q", got.SessionMode, domain.SessionModeChat)
	}

	streamRecorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/stream", strings.NewReader(`{"content":"question"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	server.ServeHTTP(streamRecorder, request)
	requireStatus(t, streamRecorder.ResponseRecorder, http.StatusOK, "stream message")
	if got := <-streamRequest; got.SessionMode != domain.SessionModeChat {
		t.Fatalf("stream responder mode=%q, want stored mode %q", got.SessionMode, domain.SessionModeChat)
	}
}

func TestSessionOwnerIsolationAndCRUD(t *testing.T) {
	server, userID, otherID, sessionID := newAPIFixture(t, apiResponder{})
	ownerToken := issueTestToken(t, server, userID)
	otherToken := issueTestToken(t, server, otherID)

	create := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(`{"title":"Created through API","mode":"chat"}`))
	create.Header.Set("Authorization", "Bearer "+ownerToken)
	createResponse := httptest.NewRecorder()
	server.ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusOK || !strings.Contains(createResponse.Body.String(), "Created through API") || !strings.Contains(createResponse.Body.String(), `"mode":"chat"`) {
		t.Fatalf("create session status/body=%d %s", createResponse.Code, createResponse.Body.String())
	}

	list := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	list.Header.Set("Authorization", "Bearer "+ownerToken)
	listResponse := httptest.NewRecorder()
	server.ServeHTTP(listResponse, list)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessionID+"/messages", nil),
		httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+sessionID, nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", strings.NewReader(`{"content":"question"}`)),
	} {
		request.Header.Set("Authorization", "Bearer "+otherToken)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("cross-owner status=%d body=%s", response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), `"code":"not_found"`) {
			t.Fatalf("cross-owner response did not use not_found: %s", response.Body.String())
		}
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+sessionID, nil)
	deleteRequest.Header.Set("Authorization", "Bearer "+ownerToken)
	deleteResponse := httptest.NewRecorder()
	server.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessionID+"/messages", nil)
	get.Header.Set("Authorization", "Bearer "+ownerToken)
	getResponse := httptest.NewRecorder()
	server.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusNotFound {
		t.Fatalf("deleted session still accessible: %d", getResponse.Code)
	}
}

func TestCrossOwnerSSEIsNotFoundBeforeResponder(t *testing.T) {
	called := make(chan struct{}, 1)
	server, ownerID, otherID, sessionID := newAPIFixture(t, apiResponder{streamCalled: called})
	_ = ownerID
	token := issueTestToken(t, server, otherID)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/stream", strings.NewReader(`{"content":"question"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"not_found"`) {
		t.Fatalf("cross-owner stream status/body=%d %s", response.Code, response.Body.String())
	}
	if strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") || strings.Contains(response.Body.String(), "event: ") {
		t.Fatalf("cross-owner stream committed SSE response: headers=%v body=%s", response.Header(), response.Body.String())
	}
	select {
	case <-called:
		t.Fatal("cross-owner stream entered responder")
	default:
	}
}

func TestSSESuccessAndFailureEvents(t *testing.T) {
	server, userID, _, sessionID := newAPIFixture(t, apiResponder{})
	token := issueTestToken(t, server, userID)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/stream", strings.NewReader(`{"content":"question"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream response headers/status: %d %#v", response.Code, response.Header())
	}
	if response.flushes < 4 {
		t.Fatalf("flush count=%d, want at least one flush per event", response.flushes)
	}
	body := response.Body.String()
	wantOrder := []string{"event: message.started", "event: message.delta", "event: message.delta", "event: message.completed"}
	last := -1
	for _, item := range wantOrder {
		index := strings.Index(body[last+1:], item)
		if index < 0 {
			t.Fatalf("SSE event missing or out of order at %q: %s", item, body)
		}
		last += 1 + index
	}

	failedServer, failedUser, _, failedSession := newAPIFixture(t, apiResponder{streamErr: errors.New("private model trace")})
	failedToken := issueTestToken(t, failedServer, failedUser)
	failedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+failedSession+"/stream", strings.NewReader(`{"content":"question"}`))
	failedRequest.Header.Set("Authorization", "Bearer "+failedToken)
	failedResponse := httptest.NewRecorder()
	failedServer.ServeHTTP(failedResponse, failedRequest)
	failedBody := failedResponse.Body.String()
	if !strings.Contains(failedBody, "event: message.failed") || strings.Contains(failedBody, "private model trace") || strings.Contains(failedBody, "event: message.completed") {
		t.Fatalf("unsafe or non-terminal SSE failure: %s", failedBody)
	}
}

func issueTestToken(t *testing.T, server *Server, userID string) string {
	t.Helper()
	token, err := server.tokens.Issue(userID)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
