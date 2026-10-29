package hybrid

import (
	"GopherAI/common/code"
	"GopherAI/model"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
)

// Intentionally validates only paths that stop before Redis/SMTP. The public
// original service has no injectable mail boundary; this is not success-chain
// acceptance and never installs a replacement registration service.
func TestRegistrationNativeHTTPValidationAndDuplicateEmail(t *testing.T) {
	app, db, _, _, _ := learningFixture(t, false)
	for _, tc := range []struct {
		path, body string
		expected   code.Code
	}{
		{"captcha", `{}`, code.CodeInvalidParams},
		{"captcha", `{"email":"not-an-email"}`, code.CodeInvalidParams},
		{"register", `{"email":"new@invalid.example","password":"password-123"}`, code.CodeInvalidParams},
		{"register", `{"email":"new@invalid.example","password":"password-123","captcha":"abc123"}`, code.CodeInvalidParams},
		{"register", `{"email":"new@invalid.example","password":"short","captcha":"123456"}`, code.CodeIllegalPassword},
		{"register", `{"email":" LEARNER@INVALID.EXAMPLE ","password":"password-123","captcha":"123456"}`, code.CodeUserExist},
		{"captcha", `{"email":" LEARNER@INVALID.EXAMPLE "}`, code.CodeUserExist},
	} {
		status, result, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/user/"+tc.path, tc.body, "")
		if status != 200 || result["status_code"] != float64(tc.expected) {
			t.Fatalf("native %s: %d %v", tc.path, status, result)
		}
		if result["token"] != nil || result["username"] != nil {
			t.Fatal("rejection fabricated credentials", result)
		}
	}
	var count int64
	db.Model(&model.User{}).Count(&count)
	if count != 2 {
		t.Fatalf("rejected registration wrote users: %d", count)
	}
}

func TestRegistrationNativeFrontendNegativeSocket(t *testing.T) {
	app, _, _, _, _ := learningFixture(t, false)
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	command := exec.Command("node", "../web/test/helpers/hybridRegistrationClient.mjs")
	command.Env = append(os.Environ(), "HYBRID_TEST_URL="+server.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native API negative socket: %v\n%s", err, output)
	}
}
