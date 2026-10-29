package router

import (
	"GopherAI/controller/user"
	userService "GopherAI/service/user"

	"github.com/gin-gonic/gin"
)

func RegisterUserRouter(r *gin.RouterGroup) {
	{
		r.POST("/register", user.Register)
		r.POST("/login", user.Login)
		r.POST("/captcha", user.HandleCaptcha)
	}
}

func RegisterUserRouterWithRegistration(r *gin.RouterGroup, service *userService.RegistrationService) {
	if service == nil {
		RegisterUserRouter(r)
		return
	}
	h := user.RegistrationHandler{Service: service}
	r.POST("/register", h.Register)
	r.POST("/login", user.Login)
	r.POST("/captcha", h.Captcha)
}
