package jwt

import (
	"GopherAI/config"
	"GopherAI/utils/myjwt"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	// The token helper reads the process-wide configuration. This repository
	// ships no config/config.toml, so the test installs the settings it needs
	// instead of depending on a file that is not there.
	if err := config.Use(&config.Config{JwtConfig: config.JwtConfig{
		ExpireDuration: 1,
		Issuer:         "gopherlearn-test",
		Subject:        "gopherlearn-test-user",
		Key:            "test-only-jwt-secret-with-at-least-32-bytes",
	}}); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestAuthWritesCompleteIdentityToContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	token, err := myjwt.GenerateToken(73, "learner")
	if err != nil {
		t.Fatal(err)
	}

	var gotID int64
	var gotName string
	r := gin.New()
	r.GET("/protected", AuthWithUserValidator(func(_ context.Context, id int64, username string) (bool, error) {
		return id == 73 && username == "learner", nil
	}), func(c *gin.Context) {
		gotID = c.GetInt64("userID")
		gotName = c.GetString("userName")
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, req)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if gotID != 73 || gotName != "learner" {
		t.Fatalf("context identity = (%d, %q)", gotID, gotName)
	}
}

func TestAuthRejectsMalformedToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	called := false
	r := gin.New()
	r.GET("/protected", AuthWithUserValidator(func(context.Context, int64, string) (bool, error) {
		t.Fatal("malformed token must not query the database")
		return false, nil
	}), func(c *gin.Context) { called = true })
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer malformed")
	response := httptest.NewRecorder()
	r.ServeHTTP(response, req)
	if called {
		t.Fatal("protected handler must not run")
	}
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestAuthRejectsValidTokenInQueryParameter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	token, err := myjwt.GenerateToken(73, "learner")
	if err != nil {
		t.Fatal(err)
	}
	validatorCalled := false
	r := gin.New()
	r.GET("/protected", AuthWithUserValidator(func(context.Context, int64, string) (bool, error) {
		validatorCalled = true
		return true, nil
	}), func(c *gin.Context) { t.Fatal("protected handler must not run") })
	req := httptest.NewRequest(http.MethodGet, "/protected?token="+token, nil)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, req)
	if validatorCalled {
		t.Fatal("query token must be rejected before user lookup")
	}
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestAuthRejectsTokenWhoseUserNoLongerExists(t *testing.T) {
	gin.SetMode(gin.TestMode)
	token, err := myjwt.GenerateToken(73, "deleted-user")
	if err != nil {
		t.Fatal(err)
	}

	called := false
	r := gin.New()
	r.GET("/protected", AuthWithUserValidator(func(_ context.Context, id int64, username string) (bool, error) {
		if id != 73 || username != "deleted-user" {
			t.Fatalf("validator identity = (%d, %q)", id, username)
		}
		return false, nil
	}), func(c *gin.Context) { called = true })
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, req)

	if called {
		t.Fatal("protected handler must not run")
	}
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	var body struct {
		StatusCode int64 `json:"status_code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.StatusCode != 2006 {
		t.Fatalf("business status = %d, want 2006", body.StatusCode)
	}
}

func TestAuthDoesNotMisreportDatabaseFailureAsInvalidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	token, err := myjwt.GenerateToken(73, "learner")
	if err != nil {
		t.Fatal(err)
	}

	called := false
	r := gin.New()
	r.GET("/protected", AuthWithUserValidator(func(context.Context, int64, string) (bool, error) {
		return false, errors.New("database unavailable")
	}), func(c *gin.Context) { called = true })
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, req)

	if called {
		t.Fatal("protected handler must not run")
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}
