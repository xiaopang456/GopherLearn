package router

import (
	controllerknowledge "GopherAI/controller/knowledge"

	"github.com/gin-gonic/gin"
)

func KnowledgeRouter(r *gin.RouterGroup, handler *controllerknowledge.Handler) {
	r.POST("/documents", handler.Create)
	r.GET("/documents", handler.List)
	r.GET("/documents/:id", handler.Get)
	r.GET("/documents/:id/processing", handler.Processing)
	r.POST("/documents/:id/retry", handler.Retry)
	r.PUT("/documents/:id", handler.Update)
	r.DELETE("/documents/:id", handler.Delete)
	r.POST("/documents/:id/reindex", handler.Reindex)
}
