package hybrid

import (
	"GopherAI/model"
	"errors"
	"gorm.io/gorm"
	"testing"
)

func TestHTTPOriginalUserRevalidationDatabaseFailureAndUnknownStoredMode(t *testing.T) {
	app, db, _, _, token, _ := httpFixture(t)
	if err := db.Create(&model.Session{ID: "unknown-mode", UserID: 1, UserName: "learner", Mode: model.SessionMode("unexpected")}).Error; err != nil {
		t.Fatal(err)
	}
	status, e, _ := requestHTTP(t, app.Handler, "GET", "/api/v1/sessions", "", token)
	if status != 400 || e["error"].(map[string]any)["code"] != "invalid_request" {
		t.Fatalf("unknown stored mode was defaulted/hidden %d %v", status, e)
	}
	if err := db.Callback().Query().Before("gorm:query").Register("test:auth-database-error", func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.AddError(errors.New("synthetic user revalidation SQL failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	status, e, _ = requestHTTP(t, app.Handler, "GET", "/api/v1/me", "", token)
	if status != 503 || e["error"].(map[string]any)["code"] != "internal_error" {
		t.Fatalf("original Auth database error changed %d %v", status, e)
	}
}
