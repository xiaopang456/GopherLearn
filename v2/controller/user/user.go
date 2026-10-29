package user

import (
	"GopherAI/common/code"
	"GopherAI/controller"
	"GopherAI/service/user"
	"net/http"

	"github.com/gin-gonic/gin"
)

type (
	//这里的Username只能是账号登录，和我做的另一个项目有区别（邮箱账号均可)
	LoginRequest struct {
		Username string `json:"username" binding:"required"` // 兼容字段名，语义为账号或邮箱
		Password string `json:"password" binding:"required"`
	}
	// omitempty当字段为空的时候，不返回这个东西
	LoginResponse struct {
		controller.Response
		Token string `json:"token,omitempty"`
	}
	//验证码由后端生成，存放到redis中，固然需要先发送一次请求CaptchaRequest,然后用返回的验证码
	//邮箱以及密码进行注册，后续再将账号进行返回
	RegisterRequest struct {
		Email    string `json:"email" binding:"required"`
		Captcha  string `json:"captcha" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	//注册成功之后，直接让其进行登录状态
	RegisterResponse struct {
		controller.Response
		Token    string `json:"token,omitempty"`
		Username string `json:"username,omitempty"`
	}

	CaptchaRequest struct {
		Email string `json:"email" binding:"required"`
	}

	CaptchaResponse struct {
		controller.Response
	}
)

func Login(c *gin.Context) {

	req := new(LoginRequest)
	res := new(LoginResponse)
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}

	token, code_ := user.Login(req.Username, req.Password)
	if code_ != code.CodeSuccess {
		c.JSON(http.StatusOK, res.CodeOf(code_))
		return
	}

	res.Success()
	res.Token = token
	c.JSON(http.StatusOK, res)

}

func Register(c *gin.Context) { registerWith(c, user.Register) }

func registerWith(c *gin.Context, run func(string, string, string) (string, string, code.Code)) {

	req := new(RegisterRequest)
	res := new(RegisterResponse)
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}

	token, username, code_ := run(req.Email, req.Password, req.Captcha)
	if code_ != code.CodeSuccess {
		c.JSON(http.StatusOK, res.CodeOf(code_))
		return
	}

	res.Success()
	res.Token = token
	res.Username = username
	c.JSON(http.StatusOK, res)
}

func HandleCaptcha(c *gin.Context) { captchaWith(c, user.SendCaptcha) }

func captchaWith(c *gin.Context, run func(string) code.Code) {
	req := new(CaptchaRequest)
	res := new(CaptchaResponse)
	//解析参数
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}

	//给service层进行处理
	code_ := run(req.Email)
	if code_ != code.CodeSuccess {
		c.JSON(http.StatusOK, res.CodeOf(code_))
		return
	}
	//匿名字段，其实本身res.Success()调用就是res.Response.Success()
	//res.Response.Success()
	res.Success()
	c.JSON(http.StatusOK, res)
}

// RegistrationHandler binds the same controller flow to one explicit instance.
type RegistrationHandler struct{ Service *user.RegistrationService }

func (h RegistrationHandler) Register(c *gin.Context) { registerWith(c, h.Service.Register) }
func (h RegistrationHandler) Captcha(c *gin.Context)  { captchaWith(c, h.Service.SendCaptcha) }
