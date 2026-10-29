package learning

import (
	"GopherAI/common/code"
	"GopherAI/controller"
	learningdao "GopherAI/dao/learning"
	sessiondao "GopherAI/dao/session"
	agentservice "GopherAI/service/agent"
	learningservice "GopherAI/service/learning"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type startRequest struct {
	SessionID string `json:"sessionId" binding:"required"`
	Topic     string `json:"topic" binding:"required"`
}

type cancelRequest struct {
	SessionID         string `json:"sessionId" binding:"required"`
	LearningSessionID string `json:"learningSessionId" binding:"required"`
}

type answerRequest struct {
	SessionID         string `json:"sessionId" binding:"required"`
	LearningSessionID string `json:"learningSessionId" binding:"required"`
	QuestionID        string `json:"questionId" binding:"required"`
	SubmissionID      string `json:"submissionId" binding:"required"`
	Answer            string `json:"answer" binding:"required"`
}

type actionRequest struct {
	SessionID         string `json:"sessionId" binding:"required"`
	LearningSessionID string `json:"learningSessionId" binding:"required"`
	QuestionID        string `json:"questionId"`
	SubmissionID      string `json:"submissionId"`
	Action            string `json:"action" binding:"required"`
}

type correctionRequest struct {
	AttemptID      string `json:"attemptId" binding:"required"`
	EffectiveTotal int    `json:"effectiveTotal"`
	Reason         string `json:"reason" binding:"required"`
}

type sessionResponse struct {
	controller.Response
	Learning *learningservice.SessionView `json:"learning,omitempty"`
}

type answerResponse struct {
	controller.Response
	Result *learningservice.AnswerView `json:"result,omitempty"`
}

type progressResponse struct {
	controller.Response
	Progress interface{}                  `json:"progress,omitempty"`
	Attempt  *learningservice.AttemptView `json:"attempt,omitempty"`
}

func Start(c *gin.Context) {
	request := new(startRequest)
	response := new(sessionResponse)
	if err := c.ShouldBindJSON(request); err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := learningservice.DefaultService()
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeServerBusy))
		return
	}
	view, err := service.Start(c.Request.Context(), learningservice.StartRequest{
		UserID: c.GetInt64("userID"), UserName: c.GetString("userName"),
		SessionID: request.SessionID, Topic: request.Topic,
	}, agentservice.DiscardEvents)
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(errorCode(err)))
		return
	}
	response.Success()
	response.Learning = &view
	c.JSON(http.StatusOK, response)
}

func StartStream(c *gin.Context) {
	prepareStream(c)
	sink, err := agentservice.NewSSEEventSink(c.Writer)
	if err != nil {
		return
	}
	request := new(startRequest)
	if err := c.ShouldBindJSON(request); err != nil {
		_ = sink.Send(c.Request.Context(), agentservice.Event{
			Type: agentservice.EventRunFailed, Message: "请求参数错误", TerminationReason: "invalid_request",
		})
		return
	}
	service, err := learningservice.DefaultService()
	if err != nil {
		_ = sink.Send(c.Request.Context(), agentservice.Event{
			Type: agentservice.EventRunFailed, Message: "学习服务尚未初始化", TerminationReason: "not_configured",
		})
		return
	}
	_, _ = service.Start(c.Request.Context(), learningservice.StartRequest{
		UserID: c.GetInt64("userID"), UserName: c.GetString("userName"),
		SessionID: request.SessionID, Topic: request.Topic,
	}, sink)
}

func Current(c *gin.Context) {
	response := new(sessionResponse)
	sessionID := strings.TrimSpace(c.Query("sessionId"))
	if sessionID == "" {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := learningservice.DefaultService()
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeServerBusy))
		return
	}
	view, err := service.Current(c.Request.Context(), learningservice.CurrentRequest{
		UserID: c.GetInt64("userID"), SessionID: sessionID,
	})
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(errorCode(err)))
		return
	}
	response.Success()
	response.Learning = &view
	c.JSON(http.StatusOK, response)
}

func Cancel(c *gin.Context) {
	request := new(cancelRequest)
	response := new(sessionResponse)
	if err := c.ShouldBindJSON(request); err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := learningservice.DefaultService()
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeServerBusy))
		return
	}
	view, err := service.Cancel(c.Request.Context(), learningservice.CancelRequest{
		UserID: c.GetInt64("userID"), SessionID: request.SessionID,
		LearningSessionID: request.LearningSessionID,
	})
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(errorCode(err)))
		return
	}
	response.Success()
	response.Learning = &view
	c.JSON(http.StatusOK, response)
}

func Answer(c *gin.Context) {
	request := new(answerRequest)
	response := new(answerResponse)
	if err := c.ShouldBindJSON(request); err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := learningservice.DefaultService()
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeServerBusy))
		return
	}
	view, err := service.Answer(c.Request.Context(), learningservice.AnswerRequest{
		UserID: c.GetInt64("userID"), SessionID: request.SessionID, LearningSessionID: request.LearningSessionID,
		QuestionID: request.QuestionID, SubmissionID: request.SubmissionID, Answer: request.Answer,
	})
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(errorCode(err)))
		return
	}
	response.Success()
	response.Result = &view
	c.JSON(http.StatusOK, response)
}

func Action(c *gin.Context) {
	request := new(actionRequest)
	response := new(answerResponse)
	if err := c.ShouldBindJSON(request); err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := learningservice.DefaultService()
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeServerBusy))
		return
	}
	view, err := service.Action(c.Request.Context(), learningservice.ActionRequest{
		UserID: c.GetInt64("userID"), SessionID: request.SessionID, LearningSessionID: request.LearningSessionID,
		QuestionID: request.QuestionID, SubmissionID: request.SubmissionID, Action: learningservice.LearningAction(request.Action),
	})
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(errorCode(err)))
		return
	}
	response.Success()
	response.Result = &view
	c.JSON(http.StatusOK, response)
}

func Progress(c *gin.Context) {
	response := new(progressResponse)
	topic := strings.TrimSpace(c.Query("topic"))
	if topic == "" {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := learningservice.DefaultService()
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeServerBusy))
		return
	}
	progress, err := service.Progress(c.Request.Context(), learningservice.ProgressRequest{UserID: c.GetInt64("userID"), Topic: topic})
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(errorCode(err)))
		return
	}
	response.Success()
	response.Progress = progress
	c.JSON(http.StatusOK, response)
}

func Correct(c *gin.Context) {
	request := new(correctionRequest)
	response := new(progressResponse)
	if err := c.ShouldBindJSON(request); err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := learningservice.DefaultService()
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeServerBusy))
		return
	}
	attempt, progress, err := service.Correct(c.Request.Context(), learningservice.CorrectRequest{UserID: c.GetInt64("userID"), AttemptID: request.AttemptID, EffectiveTotal: request.EffectiveTotal, Reason: request.Reason})
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(errorCode(err)))
		return
	}
	response.Success()
	response.Attempt = &attempt
	response.Progress = progress
	c.JSON(http.StatusOK, response)
}

func prepareStream(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
}

func errorCode(err error) code.Code {
	switch {
	case errors.Is(err, learningservice.ErrLearningSessionActive), errors.Is(err, learningdao.ErrActiveLearningSession):
		return code.LearningSessionActive
	case errors.Is(err, learningservice.ErrLearningSessionGone), errors.Is(err, learningdao.ErrLearningSessionNotFound):
		return code.LearningSessionNotFound
	case errors.Is(err, learningservice.ErrLearningSessionExpired):
		return code.LearningSessionExpired
	case errors.Is(err, learningservice.ErrLearningGradingFailed), errors.Is(err, learningservice.ErrInvalidGradeOutput):
		return code.LearningGradingFail
	case errors.Is(err, learningservice.ErrLearningSubmission), errors.Is(err, learningdao.ErrSubmissionConflict):
		return code.LearningSubmissionConflict
	case errors.Is(err, learningservice.ErrLearningAnswerBusy), errors.Is(err, learningdao.ErrGradingInProgress):
		return code.LearningGradingBusy
	case errors.Is(err, learningdao.ErrAttemptNotFound):
		return code.LearningProgressNotFound
	case errors.Is(err, learningservice.ErrNoLearningEvidence):
		return code.LearningNoEvidence
	case errors.Is(err, learningservice.ErrInvalidQuestionOutput), errors.Is(err, learningservice.ErrQuestionGenerationFail):
		return code.LearningGenerationFail
	case errors.Is(err, agentservice.ErrAgentSessionRequired), errors.Is(err, learningdao.ErrLearningStateConflict):
		return code.CodeInvalidParams
	case errors.Is(err, sessiondao.ErrSessionNotFound):
		return code.CodeRecordNotFound
	case errors.Is(err, learningservice.ErrServiceNotConfigured):
		return code.CodeServerBusy
	default:
		return code.CodeServerBusy
	}
}
