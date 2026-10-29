package agent

import (
	"GopherAI/common/code"
	"GopherAI/common/rag"
	"GopherAI/controller"
	sessiondao "GopherAI/dao/session"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/agent/topicconsent"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

type RunRequest struct {
	SessionID       string              `json:"sessionId" binding:"required"`
	Question        string              `json:"question" binding:"required"`
	RequestID       string              `json:"requestId" binding:"required"`
	KnowledgePolicy rag.KnowledgePolicy `json:"knowledgePolicy"`
	// TopicConsent is the user's read-side confirmation. It is optional on the
	// wire: a client that never asks for suggestions, or a deployment that does
	// not run the confirmed mode, simply omits it.
	TopicConsent serviceagent.TopicConsentSubmission `json:"topicConsent"`
}

// TopicSuggestionRequest asks which of this learner's topics might relate to a
// question they have not sent yet.
//
// It carries the same requestId the run will carry, because the ticket is bound
// to it: the suggestion and the answer are two halves of one request.
type TopicSuggestionRequest struct {
	SessionID string `json:"sessionId" binding:"required"`
	Question  string `json:"question" binding:"required"`
	RequestID string `json:"requestId" binding:"required"`
}

type TopicSuggestionResponse struct {
	controller.Response
	serviceagent.TopicSuggestion
}

type TopicCatalogueRequest struct {
	Query  string `json:"query"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type TopicCatalogueResponse struct {
	controller.Response
	serviceagent.TopicCataloguePage
}

type TopicConsentIssueRequest struct {
	SessionID         string   `json:"sessionId" binding:"required"`
	Question          string   `json:"question" binding:"required"`
	RequestID         string   `json:"requestId" binding:"required"`
	SelectedTopicKeys []string `json:"selectedTopicKeys" binding:"required"`
}

type TopicConsentIssueResponse struct {
	controller.Response
	serviceagent.TopicSuggestion
}

// TopicSuggestions proposes candidate topics and signs a consent ticket.
//
// It reads no mastery, no weak point and no note text: the whole point of the
// stage is that nothing personal leaves the server until the learner has said
// which subjects this answer may use.
func TopicSuggestions(c *gin.Context) {
	req := new(TopicSuggestionRequest)
	res := new(TopicSuggestionResponse)
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := serviceagent.DefaultTopicSuggestionService()
	if err != nil {
		// Not an error the user should see as a failure: this deployment does
		// not ask for confirmation, so the client proceeds without one.
		res.Success()
		res.TopicSuggestion = serviceagent.TopicSuggestion{Candidates: nil, MaxSelectable: 0}
		c.JSON(http.StatusOK, res)
		return
	}
	suggestion, err := service.Suggest(c.Request.Context(), c.GetInt64("userID"),
		req.SessionID, req.RequestID, req.Question)
	if err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeServerBusy))
		return
	}
	res.Success()
	res.TopicSuggestion = suggestion
	c.JSON(http.StatusOK, res)
}

// TopicCatalogue is the deterministic escape hatch when a short suggestion
// list misses. It exposes only this authenticated user's topic identities and
// never issues a memory-read ticket.
func TopicCatalogue(c *gin.Context) {
	req := new(TopicCatalogueRequest)
	res := new(TopicCatalogueResponse)
	if err := c.ShouldBindJSON(req); err != nil || req.Offset < 0 || req.Limit < 0 ||
		req.Limit > topicconsent.MaxCataloguePageSize {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := serviceagent.DefaultTopicSuggestionService()
	if err != nil {
		res.Success()
		res.TopicCataloguePage = serviceagent.TopicCataloguePage{Candidates: nil}
		c.JSON(http.StatusOK, res)
		return
	}
	page, err := service.BrowseCatalogue(c.Request.Context(), c.GetInt64("userID"), req.Query, req.Offset, req.Limit)
	if err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeServerBusy))
		return
	}
	res.Success()
	res.TopicCataloguePage = page
	c.JSON(http.StatusOK, res)
}

// IssueTopicConsent turns the learner's final catalogue selection into a
// request-bound ticket only after rechecking current ownership.
func IssueTopicConsent(c *gin.Context) {
	req := new(TopicConsentIssueRequest)
	res := new(TopicConsentIssueResponse)
	if err := c.ShouldBindJSON(req); err != nil || len(req.SelectedTopicKeys) == 0 {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := serviceagent.DefaultTopicSuggestionService()
	if err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeServerBusy))
		return
	}
	issued, err := service.IssueSelection(c.Request.Context(), c.GetInt64("userID"), req.SessionID,
		req.RequestID, req.Question, req.SelectedTopicKeys)
	if err != nil {
		if errors.Is(err, topicconsent.ErrInvalidSelection) || errors.Is(err, topicconsent.ErrTooManyTopics) {
			c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
			return
		}
		c.JSON(http.StatusOK, res.CodeOf(code.CodeServerBusy))
		return
	}
	res.Success()
	res.TopicSuggestion = issued
	c.JSON(http.StatusOK, res)
}

type RunResponse struct {
	controller.Response
	RunID           string                        `json:"runId,omitempty"`
	Answer          string                        `json:"answer,omitempty"`
	ToolCalls       int                           `json:"toolCalls"`
	ModelCalls      int                           `json:"modelCalls"`
	Sources         []rag.Source                  `json:"sources"`
	AnswerBasis     rag.AnswerBasis               `json:"answerBasis"`
	KnowledgeStatus rag.KnowledgeStatus           `json:"knowledgeStatus"`
	KnowledgeReason rag.KnowledgeReason           `json:"knowledgeReason"`
	ContextBudget   serviceagent.BudgetBreakdown  `json:"contextBudget"`
	MemoryDraft     *serviceagent.MemoryDraftCard `json:"memoryDraft,omitempty"`
	Replayed        bool                          `json:"replayed,omitempty"`
	UsedTopicKeys   []string                      `json:"usedTopicKeys,omitempty"`
}

func Run(c *gin.Context) {
	req := new(RunRequest)
	res := new(RunResponse)
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := serviceagent.DefaultService()
	if err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeServerBusy))
		return
	}
	result, err := service.Run(c.Request.Context(), serviceagent.RunRequest{
		UserID: c.GetInt64("userID"), UserName: c.GetString("userName"),
		SessionID: req.SessionID, Question: req.Question, RequestID: req.RequestID, KnowledgePolicy: req.KnowledgePolicy,
		TopicConsent: req.TopicConsent,
	}, serviceagent.DiscardEvents)
	res.RunID = result.RunID
	if err != nil {
		c.JSON(http.StatusOK, res.CodeOf(agentErrorCode(err)))
		return
	}
	res.Success()
	res.RunID = result.RunID
	res.Answer = result.Answer
	res.ToolCalls = result.ToolCalls
	res.ModelCalls = result.ModelCalls
	res.Sources = append([]rag.Source{}, result.Sources...)
	res.AnswerBasis = result.AnswerBasis
	res.KnowledgeStatus = result.KnowledgeStatus
	res.KnowledgeReason = result.KnowledgeReason
	res.ContextBudget = result.ContextBudget
	res.MemoryDraft = result.MemoryDraft
	res.Replayed = result.Replayed
	res.UsedTopicKeys = append([]string{}, result.UsedTopicKeys...)
	c.JSON(http.StatusOK, res)
}

func RunStream(c *gin.Context) {
	prepareStream(c)
	sink, err := serviceagent.NewSSEEventSink(c.Writer)
	if err != nil {
		return
	}
	req := new(RunRequest)
	if err := c.ShouldBindJSON(req); err != nil {
		_ = sink.Send(c.Request.Context(), serviceagent.Event{
			Type: serviceagent.EventRunFailed, Message: "请求参数错误", TerminationReason: "invalid_request",
		})
		return
	}
	service, err := serviceagent.DefaultService()
	if err != nil {
		_ = sink.Send(c.Request.Context(), serviceagent.Event{
			Type: serviceagent.EventRunFailed, Message: "Agent 服务尚未初始化", TerminationReason: "not_configured",
		})
		return
	}
	_, _ = service.Run(c.Request.Context(), serviceagent.RunRequest{
		UserID: c.GetInt64("userID"), UserName: c.GetString("userName"),
		SessionID: req.SessionID, Question: req.Question, RequestID: req.RequestID, KnowledgePolicy: req.KnowledgePolicy,
		TopicConsent: req.TopicConsent,
	}, sink)
}

func prepareStream(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
}

func agentErrorCode(err error) code.Code {
	switch {
	case errors.Is(err, serviceagent.ErrAgentSessionRequired):
		return code.CodeInvalidParams
	case errors.Is(err, serviceagent.ErrInvalidRunRequest):
		return code.CodeInvalidParams
	case errors.Is(err, serviceagent.ErrServiceNotConfigured):
		return code.CodeServerBusy
	case errors.Is(err, sessiondao.ErrSessionNotFound):
		return code.CodeRecordNotFound
	case errors.Is(err, serviceagent.ErrRunInProgress):
		return code.AgentRunBusy
	case errors.Is(err, serviceagent.ErrRunRequestConflict):
		return code.AgentRequestConflict
	case errors.Is(err, serviceagent.ErrRunRequestFinished):
		return code.AgentRequestFinished
	case errors.Is(err, serviceagent.ErrRunTemporarilyUnavailable):
		return code.CodeServerBusy
	default:
		return code.AIModelFail
	}
}
