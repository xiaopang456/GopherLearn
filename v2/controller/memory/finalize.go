package memory

import (
	"net/http"

	memoryservice "GopherAI/service/memory"

	"github.com/gin-gonic/gin"

	"GopherAI/common/code"
	"GopherAI/controller"
)

// finalizeSessionResponse is the outcome of one finalize notification. The
// counts are what the learner's page and the support logs both want: what was
// proposed, what was skipped and why, what is still waiting for a judgement,
// and whether the processing watermark moved.
type finalizeSessionResponse struct {
	controller.Response
	SessionID         string `json:"session_id"`
	CoveredMessageID  uint   `json:"covered_message_id"`
	Advanced          bool   `json:"advanced"`
	NoNewContent      bool   `json:"no_new_content"`
	BoardMissing      bool   `json:"board_missing"`
	PersistedCount    int    `json:"persisted"`
	SkippedCount      int    `json:"skipped"`
	RetryPendingCount int    `json:"retry_pending"`
	FailedCount       int    `json:"failed"`
}

// FinalizeSession is the session-close notification: the frontend fires it
// when the learner closes a conversation, and the server runs the finalize
// extraction once - the watermark makes a repeated notification harmless, so
// the call is safe to retry and safe to fire and forget. The learner keeps
// chatting either way; nothing here blocks a turn.
func FinalizeSession(c *gin.Context) {
	response := new(finalizeSessionResponse)
	runner, err := memoryservice.DefaultFinalizeRunner()
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeServerBusy))
		return
	}
	sessionID := c.Param("id")
	userID := c.GetInt64("userID")
	if sessionID == "" || userID <= 0 {
		c.JSON(http.StatusBadRequest, response.CodeOf(code.CodeInvalidParams))
		return
	}
	outcome, err := runner.RunSession(c.Request.Context(), userID, sessionID)
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeServerBusy))
		return
	}
	response.Success()
	response.SessionID = outcome.SessionID
	response.CoveredMessageID = outcome.Cursor.CoveredMessageID
	response.Advanced = outcome.Advanced
	response.NoNewContent = outcome.NoNewContent
	response.BoardMissing = outcome.BoardMissing
	response.PersistedCount = len(outcome.Persisted)
	response.SkippedCount = len(outcome.Skipped)
	response.RetryPendingCount = len(outcome.RetryPending)
	response.FailedCount = len(outcome.Failed)
	c.JSON(http.StatusOK, response)
}
