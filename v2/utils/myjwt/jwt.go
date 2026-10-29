package myjwt

import (
	"GopherAI/config"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

type Claims struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

func GenerateToken(id int64, username string) (string, error) {
	if id <= 0 || username == "" {
		return "", ErrInvalidToken
	}
	jwtConfig := config.GetConfig().JwtConfig
	claims := Claims{
		ID:       id,
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Duration(jwtConfig.ExpireDuration) * time.Hour)),
			Issuer:    jwtConfig.Issuer,
			Subject:   jwtConfig.Subject,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}

	// 生成 token
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(jwtConfig.Key))
}

var ErrInvalidToken = errors.New("invalid token")

// ParseToken verifies an HS256 token and returns the complete identity claims.
// Returning ID as well as Username lets downstream knowledge-base code use the
// immutable database identity without another user-table lookup.
func ParseToken(token string) (*Claims, error) {
	jwtConfig := config.GetConfig().JwtConfig
	claims := new(Claims)
	parser := jwt.NewParser(jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	t, err := parser.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, ErrInvalidToken
		}
		return []byte(jwtConfig.Key), nil
	})
	if err != nil || t == nil || !t.Valid || claims.ID <= 0 || claims.Username == "" ||
		claims.Issuer != jwtConfig.Issuer || claims.Subject != jwtConfig.Subject {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
