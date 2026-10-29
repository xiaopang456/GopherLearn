package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	serviceagent "GopherAI/service/agent"
	"GopherAI/service/agent/topicconsent"

	"github.com/gin-gonic/gin"
)

// The confirmation must arrive identically whether the client streams or not.
// A field accepted on one transport and dropped on the other is how the same
// question gets two different answers depending on a checkbox.
func TestBothTransportsBindTheSameConsentField(t *testing.T) {
	body := `{"sessionId":"s-1","question":"登录态怎么校验？","requestId":"req-1",
		"topicConsent":{"ticket":"abc.def","selectedTopicKeys":["jwt-鉴权","session-状态机"]}}`

	var sync RunRequest
	if err := json.Unmarshal([]byte(body), &sync); err != nil {
		t.Fatalf("sync bind: %v", err)
	}
	if sync.TopicConsent.Ticket != "abc.def" || len(sync.TopicConsent.SelectedTopicKeys) != 2 {
		t.Fatalf("sync consent = %+v", sync.TopicConsent)
	}

	// RunStream binds the very same struct, so the two cannot drift apart.
	var stream RunRequest
	if err := json.Unmarshal([]byte(body), &stream); err != nil {
		t.Fatalf("stream bind: %v", err)
	}
	if stream.TopicConsent.Ticket != sync.TopicConsent.Ticket ||
		strings.Join(stream.TopicConsent.SelectedTopicKeys, ",") != strings.Join(sync.TopicConsent.SelectedTopicKeys, ",") {
		t.Fatal("the two transports bound different consents")
	}
}

func TestAbsentConsentIsEmptyRatherThanInvalid(t *testing.T) {
	var request RunRequest
	if err := json.Unmarshal([]byte(`{"sessionId":"s-1","question":"q","requestId":"r"}`), &request); err != nil {
		t.Fatal(err)
	}
	if !request.TopicConsent.Empty() {
		t.Fatalf("an omitted consent must be empty, got %+v", request.TopicConsent)
	}
}

// A ticket with an empty selection is a real answer and must not read as
// "nothing was submitted".
func TestTicketWithNoSelectionIsNotEmpty(t *testing.T) {
	submission := serviceagent.TopicConsentSubmission{Ticket: "abc.def"}
	if submission.Empty() {
		t.Fatal("a ticket with no selection is still a submission")
	}
}

// A deployment that does not run the confirmed mode answers the suggestion
// endpoint successfully with no candidates, so the client proceeds without a
// confirmation rather than showing an error.
func TestSuggestionsDegradeToNoCandidatesWhenDisabled(t *testing.T) {
	serviceagent.SetDefaultTopicSuggestionService(nil)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/topic-suggestions", func(c *gin.Context) {
		c.Set("userID", int64(11))
		TopicSuggestions(c)
	})
	request := httptest.NewRequest(http.MethodPost, "/topic-suggestions",
		strings.NewReader(`{"sessionId":"s-1","question":"登录态怎么校验？","requestId":"req-1"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	var response struct {
		StatusCode    int      `json:"status_code"`
		Candidates    []string `json:"candidates"`
		Ticket        string   `json:"ticket"`
		MaxSelectable int      `json:"maxSelectable"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v body=%s", err, recorder.Body.String())
	}
	if response.StatusCode != 1000 {
		t.Fatalf("status = %d, want a success", response.StatusCode)
	}
	if len(response.Candidates) != 0 || response.Ticket != "" || response.MaxSelectable != 0 {
		t.Fatalf("a disabled deployment must offer nothing: %+v", response)
	}
}

func TestSuggestionsRejectAMalformedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/topic-suggestions", func(c *gin.Context) {
		c.Set("userID", int64(11))
		TopicSuggestions(c)
	})
	request := httptest.NewRequest(http.MethodPost, "/topic-suggestions", strings.NewReader(`{"question":"q"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	var response struct {
		StatusCode int `json:"status_code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == 1000 {
		t.Fatal("a request without a session or request id must be refused")
	}
}

func fallbackSuggestionService(t *testing.T) (*serviceagent.TopicSuggestionService, *topicconsent.Signer) {
	t.Helper()
	suggester, err := topicconsent.NewSuggester(topicconsent.TopicListerFunc(
		func(_ context.Context, userID int64) ([]topicconsent.Topic, error) {
			if userID != 11 {
				return nil, nil
			}
			return []topicconsent.Topic{
				{Key: "jwt-鉴权", Name: "JWT 鉴权", Graded: true},
				{Key: "docker-容器编排", Name: "Docker 容器编排", Notes: 4},
			}, nil
		}), topicconsent.MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := topicconsent.NewSigner("controller-fallback-secret-that-is-long-enough", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service, err := serviceagent.NewTopicSuggestionService(suggester, signer)
	if err != nil {
		t.Fatal(err)
	}
	return service, signer
}

func TestCatalogueBrowseCarriesNoTicketAndFinalSelectionGetsOne(t *testing.T) {
	service, signer := fallbackSuggestionService(t)
	serviceagent.SetDefaultTopicSuggestionService(service)
	t.Cleanup(func() { serviceagent.SetDefaultTopicSuggestionService(nil) })
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/topic-catalogue", func(c *gin.Context) {
		c.Set("userID", int64(11))
		TopicCatalogue(c)
	})
	router.POST("/topic-consent", func(c *gin.Context) {
		c.Set("userID", int64(11))
		IssueTopicConsent(c)
	})

	browse := httptest.NewRequest(http.MethodPost, "/topic-catalogue",
		strings.NewReader(`{"query":"Docker","offset":0,"limit":20}`))
	browse.Header.Set("Content-Type", "application/json")
	browseRecorder := httptest.NewRecorder()
	router.ServeHTTP(browseRecorder, browse)
	var page struct {
		StatusCode int                      `json:"status_code"`
		Candidates []topicconsent.Candidate `json:"candidates"`
		Ticket     string                   `json:"ticket"`
	}
	if err := json.Unmarshal(browseRecorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.StatusCode != 1000 || len(page.Candidates) != 1 || page.Candidates[0].TopicKey != "docker-容器编排" || page.Ticket != "" {
		t.Fatalf("browse response = %+v", page)
	}
	if page.Candidates[0].Source != topicconsent.SourceCatalogueBrowse {
		t.Fatalf("browse leaked a personal provenance: %+v", page.Candidates[0])
	}

	issue := httptest.NewRequest(http.MethodPost, "/topic-consent", strings.NewReader(
		`{"sessionId":"s-1","question":"容器怎么启动？","requestId":"r-1","selectedTopicKeys":["docker-容器编排"]}`))
	issue.Header.Set("Content-Type", "application/json")
	issueRecorder := httptest.NewRecorder()
	router.ServeHTTP(issueRecorder, issue)
	var issued struct {
		StatusCode int    `json:"status_code"`
		Ticket     string `json:"ticket"`
	}
	if err := json.Unmarshal(issueRecorder.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.StatusCode != 1000 || issued.Ticket == "" {
		t.Fatalf("issue response = %+v body=%s", issued, issueRecorder.Body.String())
	}
	if _, err := signer.Verify(topicconsent.VerifyRequest{Ticket: issued.Ticket, Selected: []string{"docker-容器编排"},
		UserID: 11, SessionID: "s-1", RequestID: "r-1", Question: "容器怎么启动？"}); err != nil {
		t.Fatalf("verify issued ticket: %v", err)
	}
}

func TestFinalSelectionRejectsATopicOutsideTheCurrentUsersCatalogue(t *testing.T) {
	service, _ := fallbackSuggestionService(t)
	serviceagent.SetDefaultTopicSuggestionService(service)
	t.Cleanup(func() { serviceagent.SetDefaultTopicSuggestionService(nil) })
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/topic-consent", func(c *gin.Context) {
		c.Set("userID", int64(11))
		IssueTopicConsent(c)
	})
	request := httptest.NewRequest(http.MethodPost, "/topic-consent", strings.NewReader(
		`{"sessionId":"s-1","question":"q","requestId":"r-1","selectedTopicKeys":["foreign"]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	var response struct {
		StatusCode int    `json:"status_code"`
		Ticket     string `json:"ticket"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == 1000 || response.Ticket != "" {
		t.Fatalf("foreign selection was accepted: %+v", response)
	}
}
