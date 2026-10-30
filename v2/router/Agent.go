package router

import (
	controlleragent "GopherAI/controller/agent"
	controllerlearning "GopherAI/controller/learning"
	controllermemory "GopherAI/controller/memory"

	"github.com/gin-gonic/gin"
)

func AgentRouter(r *gin.RouterGroup) {
	r.POST("/agent/topic-suggestions", controlleragent.TopicSuggestions)
	r.POST("/agent/topic-catalogue", controlleragent.TopicCatalogue)
	r.POST("/agent/topic-consent", controlleragent.IssueTopicConsent)
	r.POST("/agent/run", controlleragent.Run)
	r.POST("/agent/run-stream", controlleragent.RunStream)
	// The read side of a run. A conversation that is opened again shows the
	// steps its turns took, which the run table already holds.
	r.GET("/agent/sessions/:sessionId/runs", controlleragent.SessionRuns)
	r.GET("/agent/sessions/:sessionId/runs/:runId/steps", controlleragent.RunSteps)
	r.POST("/agent/learning/start", controllerlearning.Start)
	r.POST("/agent/learning/start-stream", controllerlearning.StartStream)
	r.GET("/agent/learning/current", controllerlearning.Current)
	r.POST("/agent/learning/cancel", controllerlearning.Cancel)
	r.POST("/agent/learning/answer", controllerlearning.Answer)
	r.POST("/agent/learning/action", controllerlearning.Action)
	r.GET("/agent/learning/progress", controllerlearning.Progress)
	r.POST("/agent/learning/correct", controllerlearning.Correct)
	r.GET("/agent/memories", controllermemory.List)
	r.POST("/agent/memories", controllermemory.Create)
	r.GET("/agent/memories/:id/revisions", controllermemory.Revisions)
	r.PATCH("/agent/memories/:id", controllermemory.Correct)
	r.POST("/agent/memories/:id/archive", controllermemory.Archive)
	r.DELETE("/agent/memories/:id", controllermemory.Delete)
	r.POST("/agent/memory-drafts/:id/resolve", controllermemory.ResolveDraft)
}
