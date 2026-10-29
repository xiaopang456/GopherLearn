package jwt

import (
	"GopherAI/common/code"
	"GopherAI/common/mysql"
	"GopherAI/controller"
	"GopherAI/model"
	"GopherAI/utils/myjwt"
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type UserValidator func(context.Context, int64, string) (bool, error)

// 读取jwt
func Auth() gin.HandlerFunc {
	return AuthWithUserValidator(databaseUserValidator)
}

func AuthWithUserValidator(validate UserValidator) gin.HandlerFunc {
	return func(c *gin.Context) {
		res := new(controller.Response)

		authHeader := c.GetHeader("Authorization")
		token := ""
		if strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		}

		if token == "" {
			c.JSON(http.StatusUnauthorized, res.CodeOf(code.CodeInvalidToken))
			c.Abort()
			return
		}

		claims, err := myjwt.ParseToken(token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, res.CodeOf(code.CodeInvalidToken))
			c.Abort()
			return
		}

		if validate == nil {
			c.JSON(http.StatusServiceUnavailable, res.CodeOf(code.CodeServerBusy))
			c.Abort()
			return
		}
		exists, err := validate(c.Request.Context(), claims.ID, claims.Username)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, res.CodeOf(code.CodeServerBusy))
			c.Abort()
			return
		}
		if !exists {
			c.JSON(http.StatusUnauthorized, res.CodeOf(code.CodeInvalidToken))
			c.Abort()
			return
		}

		c.Set("userID", claims.ID)
		c.Set("userName", claims.Username)
		c.Next()
	}
}

func databaseUserValidator(ctx context.Context, userID int64, username string) (bool, error) {
	if mysql.DB == nil {
		return false, errors.New("mysql is not initialized")
	}
	var user model.User
	err := mysql.DB.WithContext(ctx).
		Select("id").
		Where("id = ? AND username = ?", userID, username).
		First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
