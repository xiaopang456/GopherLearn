package router

import (
	"GopherAI/config"
	"GopherAI/middleware/jwt"
	"GopherAI/utils/myjwt"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	if err := config.Use(&config.Config{JwtConfig: config.JwtConfig{ExpireDuration: 1, Issuer: "o3-test", Subject: "o3-test", Key: "test-only-jwt-secret-with-at-least-32-bytes"}}); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func opsRequest(t *testing.T, engine http.Handler, path string, user string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != "" {
		id := map[string]int64{"learner": 73, "operator": 74}[user]
		token, err := myjwt.GenerateToken(id, user)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec.Code
}

// This build has no cross-user document-processing routes: it processes documents
// in-process from the start, so those paths were never routed and every request
// to them is a 404.
func TestO3OpsAccessControl(t *testing.T) {
	gin.SetMode(gin.TestMode)
	valid := jwt.AuthWithUserValidator(func(_ context.Context, id int64, username string) (bool, error) {
		return (username == "learner" && id == 73) || (username == "operator" && id == 74), nil
	})
	conf := &config.Config{}
	paths := []string{"/api/v1/ops/document-processing/status", "/api/v1/ops/document-processing/events", "/api/v1/ops/document-processing/metrics"}

	t.Run("local processing mode: not routed at all", func(t *testing.T) {
		engine := initIsolatedRouter(nil, conf, valid)
		for _, path := range paths {
			for _, user := range []string{"", "learner", "operator"} {
				if code := opsRequest(t, engine, path, user); code != http.StatusNotFound {
					t.Fatalf("local %s as %q = %d, want 404", path, user, code)
				}
			}
		}
	})
}
