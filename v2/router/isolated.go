package router

import (
	"GopherAI/config"
	controllerknowledge "GopherAI/controller/knowledge"
	controllersession "GopherAI/controller/session"
	"GopherAI/middleware/jwt"
	knowledgeService "GopherAI/service/knowledge"
	userService "GopherAI/service/user"
	"github.com/gin-gonic/gin"
	"net/http"
)

// InitIsolatedRouter assembles only the native V2 surfaces selected for this
// entry. Ordinary AIHelper, TTS and frontend compatibility routes are absent.
func InitIsolatedRouter(documents *knowledgeService.DocumentService, conf *config.Config, registration ...*userService.RegistrationService) *gin.Engine {
	return initIsolatedRouter(documents, conf, jwt.Auth(), registration...)
}

// initIsolatedRouter takes the authentication middleware as a parameter so a test can supply one
// that does not need a database; production always passes jwt.Auth().
func initIsolatedRouter(documents *knowledgeService.DocumentService, conf *config.Config, auth gin.HandlerFunc, registration ...*userService.RegistrationService) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	api := r.Group("/api/v1")
	api.Use(limitRequestBody(defaultMaxRequestBodyBytes))
	var service *userService.RegistrationService
	if len(registration) > 0 {
		service = registration[0]
	}
	RegisterUserRouterWithRegistration(api.Group("/user"), service)
	ai := api.Group("/AI")
	// Keep the original AgentRouter intact, but exclude learning SSE from this
	// isolated surface before authentication or any learning side effects.
	ai.Use(func(c *gin.Context) {
		if c.Request.Method == http.MethodPost && c.FullPath() == "/api/v1/AI/agent/learning/start-stream" {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	})
	ai.Use(auth)
	AgentRouter(ai)
	ai.POST("/chat/session", controllersession.CreateSession)
	kb := api.Group("/knowledge")
	kb.Use(auth)
	documentHandler := controllerknowledge.NewHandler(documents, conf.KnowledgeConfig.MaxFileSizeBytes)
	KnowledgeRouter(kb, documentHandler)
	return r
}
