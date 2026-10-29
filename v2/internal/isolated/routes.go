package isolated

import (
	"GopherAI/config"
	"GopherAI/router"
	knowledgeService "GopherAI/service/knowledge"
	userService "GopherAI/service/user"
	"net/http"
)

func buildRouter(documents *knowledgeService.DocumentService, conf *config.Config, registration *userService.RegistrationService) http.Handler {
	return router.InitIsolatedRouter(documents, conf, registration)
}
