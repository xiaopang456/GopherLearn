package agent

import (
	"GopherAI/common/code"
	"GopherAI/controller"
	agentdao "GopherAI/dao/agent"
	sessiondao "GopherAI/dao/session"
	serviceagent "GopherAI/service/agent"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type SessionRunsResponse struct {
	controller.Response
	Runs      []serviceagent.RunView `json:"runs"`
	Truncated bool                   `json:"truncated"`
}

type RunStepsResponse struct {
	controller.Response
	Run serviceagent.RunView `json:"run"`
}

// SessionRuns lists the Agent runs of one session with the steps each took.
//
// The session is the caller's or the answer says the session was not found.
// A session of another account and a session that does not exist answer
// exactly the same way, so the answer never says which of the two it was.
func SessionRuns(c *gin.Context) {
	response := new(SessionRunsResponse)
	sessionID := strings.TrimSpace(c.Param("sessionId"))
	if sessionID == "" {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := serviceagent.DefaultRunReadService()
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeServerBusy))
		return
	}
	view, err := service.ListSessionRuns(c.Request.Context(), c.GetInt64("userID"), sessionID)
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(runReadErrorCode(err)))
		return
	}
	response.Success()
	response.Runs = view.Runs
	response.Truncated = view.Truncated
	c.JSON(http.StatusOK, response)
}

// RunSteps reads the steps of one run of one session.
//
// The run is the caller's or it is not found. A run of another account, a run
// of another session and a run that never existed answer exactly the same
// way.
func RunSteps(c *gin.Context) {
	response := new(RunStepsResponse)
	sessionID := strings.TrimSpace(c.Param("sessionId"))
	runID := strings.TrimSpace(c.Param("runId"))
	if sessionID == "" || runID == "" {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeInvalidParams))
		return
	}
	service, err := serviceagent.DefaultRunReadService()
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeServerBusy))
		return
	}
	view, err := service.ListRunSteps(c.Request.Context(), c.GetInt64("userID"), sessionID, runID)
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(runReadErrorCode(err)))
		return
	}
	response.Success()
	response.Run = view
	c.JSON(http.StatusOK, response)
}

// runReadErrorCode classifies a read failure.
//
// A run or a session that is not the caller's is reported as "not found"
// rather than "forbidden": naming the difference would tell an outsider that
// an id it may not read exists.
func runReadErrorCode(err error) code.Code {
	switch {
	case errors.Is(err, sessiondao.ErrSessionNotFound), errors.Is(err, agentdao.ErrRunNotFound):
		return code.CodeRecordNotFound
	default:
		return code.CodeServerBusy
	}
}
