package session

import (
	"GopherAI/common/code"
	"GopherAI/controller"
	"GopherAI/service/session"
	"github.com/gin-gonic/gin"
	"net/http"
)

type (
	CreateSessionRequest struct {
		ModelType string `json:"modelType" binding:"required"`
		RequestID string `json:"requestId"`
	}

	CreateSessionResponse struct {
		SessionID string `json:"sessionId,omitempty"`
		controller.Response
	}
)

func CreateSession(c *gin.Context) {
	req := new(CreateSessionRequest)
	res := new(CreateSessionResponse)
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}
	sessionID, resultCode := session.CreateEmptySession(c.Request.Context(),
		c.GetInt64("userID"), c.GetString("userName"), req.ModelType, req.RequestID)
	if resultCode != code.CodeSuccess {
		c.JSON(http.StatusOK, res.CodeOf(resultCode))
		return
	}
	res.Success()
	res.SessionID = sessionID
	c.JSON(http.StatusOK, res)
}
