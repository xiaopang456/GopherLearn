package hybrid

import (
	"GopherAI/common/code"
	"GopherAI/config"
	userDAO "GopherAI/dao/user"
	"GopherAI/model"
	userService "GopherAI/service/user"
	"GopherAI/utils"
	"GopherAI/utils/myjwt"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cloudwego/eino/schema"
	"github.com/golang-jwt/jwt/v4"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"testing"
	"time"
)

type registrationBoundary struct {
	mu                       sync.Mutex
	captcha, mail            map[string]string
	reserved                 map[string]bool
	sendErr, consumeErr      bool
	deleted, released, sends int
}

func newRegistrationBoundary() *registrationBoundary {
	return &registrationBoundary{captcha: map[string]string{}, mail: map[string]string{}, reserved: map[string]bool{}}
}
func (b *registrationBoundary) external() *userService.RegistrationExternal {
	return &userService.RegistrationExternal{
		VerifyCaptcha: func(e, c string) (bool, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			return b.captcha[e] == c && c != "", nil
		},
		ConsumeCaptcha: func(e, c string) (bool, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.consumeErr {
				return false, errors.New("offline consume failure")
			}
			if b.captcha[e] != c {
				return false, nil
			}
			delete(b.captcha, e)
			return true, nil
		},
		SetCaptcha:    func(e, c string) error { b.mu.Lock(); defer b.mu.Unlock(); b.captcha[e] = c; return nil },
		DeleteCaptcha: func(e string) error { b.mu.Lock(); defer b.mu.Unlock(); delete(b.captcha, e); b.deleted++; return nil },
		ReserveSend: func(e string) (bool, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.reserved[e] {
				return false, nil
			}
			b.reserved[e] = true
			return true, nil
		},
		ReleaseSend: func(e string) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			delete(b.reserved, e)
			b.released++
			return nil
		},
		SendEmail: func(e, c, msg string) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.sends++
			if b.sendErr {
				return errors.New("offline mail failure")
			}
			b.mail[e] = c
			return nil
		},
	}
}
func (b *registrationBoundary) captured(e string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mail[e]
}
func registrationFixture(t *testing.T, b *registrationBoundary) (*Application, *gorm.DB) {
	t.Helper()
	db := sqliteDatabase(t)
	m := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		t.Error("registration called model")
		return nil, errors.New("unexpected model")
	}}
	migrations := 0
	deps := dependencies(t, db, m, &testChunks{}, &migrations)
	deps.Registration = b.external()
	app, err := Assemble(context.Background(), testSettings(t), deps, &ordinaryModel{}, conversation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if migrations != 0 {
		t.Fatal("migration invoked")
	}
	return app, db
}
func registrationCall(t *testing.T, app *Application, action string, body map[string]string, want code.Code) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	status, r, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/user/"+action, string(raw), "")
	if status != 200 || r["status_code"] != float64(want) {
		t.Fatalf("%s status=%d code=%v expected=%d", action, status, r["status_code"], want)
	}
	return r
}
func registrationUser(t *testing.T, email string, result map[string]any) *model.User {
	t.Helper()
	u, err := userDAO.FindByLogin(email)
	if err != nil {
		t.Fatal(err)
	}
	if result["username"] != u.Username || !regexp.MustCompile(`^[0-9]{11}$`).MatchString(u.Username) || u.Name != u.Username || u.Email != email || u.ID <= 0 {
		t.Fatal("original user identity mismatch")
	}
	valid, upgrade := utils.VerifyPassword(u.Password, "password-123")
	if !valid || upgrade || u.Password == "password-123" {
		t.Fatal("original password hash not verifiable")
	}
	return u
}
func TestRegistrationOriginalSuccessAndFailureChain(t *testing.T) {
	b := newRegistrationBoundary()
	app, db := registrationFixture(t, b)
	email := "chain@invalid.example"
	registrationCall(t, app, "captcha", map[string]string{"email": " CHAIN@INVALID.EXAMPLE "}, code.CodeSuccess)
	captcha := b.captured(email)
	if !regexp.MustCompile(`^[0-9]{6}$`).MatchString(captcha) {
		t.Fatal("not original generated captcha")
	}
	registrationCall(t, app, "captcha", map[string]string{"email": email}, code.CodeCaptchaTooFrequent)
	wrong := "000000"
	if wrong == captcha {
		wrong = "000001"
	}
	body := map[string]string{"email": email, "password": "password-123", "captcha": wrong}
	registrationCall(t, app, "register", body, code.CodeInvalidCaptcha)
	var count int64
	db.Model(&model.User{}).Count(&count)
	if count != 0 {
		t.Fatal("wrong captcha wrote user")
	}
	body["captcha"] = captcha
	r := registrationCall(t, app, "register", body, code.CodeSuccess)
	u := registrationUser(t, email, r)
	token := r["token"].(string)
	claims, err := myjwt.ParseToken(token)
	if err != nil || claims.ID != u.ID || claims.Username != u.Username {
		t.Fatal("original JWT mismatch")
	}
	status, me, _ := requestHTTP(t, app.Handler, "GET", "/api/v1/me", "", token)
	profile, ok := me["data"].(map[string]any)
	if !ok {
		t.Fatal("me profile envelope missing")
	}
	if status != 200 || profile["id"] != fmt.Sprint(u.ID) || profile["username"] != u.Username || profile["email"] != u.Email {
		t.Fatal("authenticated me identity mismatch")
	}
	registrationCall(t, app, "register", body, code.CodeUserExist)
	db.Model(&model.User{}).Count(&count)
	if count != 1 {
		t.Fatal("duplicate wrote user")
	}
	expiredClaims := *claims
	expiredClaims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
	expired, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, &expiredClaims).SignedString([]byte(config.GetConfig().JwtConfig.Key))
	for _, bad := range []string{token + "forged", expired} {
		status, _, _ = requestHTTP(t, app.Handler, "GET", "/api/v1/me", "", bad)
		if status != 401 {
			t.Fatal("invalid token accepted")
		}
	}
	b.mu.Lock()
	_, remaining := b.captcha[email]
	b.mu.Unlock()
	if remaining {
		t.Fatal("success did not consume captcha")
	}
}
func TestRegistrationCommittedCleanupFailure(t *testing.T) {
	b := newRegistrationBoundary()
	b.consumeErr = true
	app, _ := registrationFixture(t, b)
	e := "cleanup@invalid.example"
	registrationCall(t, app, "captcha", map[string]string{"email": e}, code.CodeSuccess)
	r := registrationCall(t, app, "register", map[string]string{"email": e, "password": "password-123", "captcha": b.captured(e)}, code.CodeSuccess)
	u := registrationUser(t, e, r)
	status, me, _ := requestHTTP(t, app.Handler, "GET", "/api/v1/me", "", r["token"].(string))
	profile, ok := me["data"].(map[string]any)
	if !ok {
		t.Fatal("me profile envelope missing")
	}
	if status != 200 || profile["id"] != fmt.Sprint(u.ID) {
		t.Fatal("committed cleanup failure lost success")
	}
}
func TestRegistrationMailFailureCleanup(t *testing.T) {
	b := newRegistrationBoundary()
	b.sendErr = true
	app, db := registrationFixture(t, b)
	e := "mailfail@invalid.example"
	// A relay that refuses has its own answer now, so an operator can tell a
	// mail problem from a broken server. The cleanup below is unchanged: the
	// stored captcha is removed and the send slot released either way.
	registrationCall(t, app, "captcha", map[string]string{"email": e}, code.CodeEmailSendFailed)
	b.mu.Lock()
	clean := len(b.captcha) == 0 && len(b.reserved) == 0 && b.deleted == 1 && b.released == 1 && b.sends == 1
	b.sendErr = false
	b.mu.Unlock()
	if !clean {
		t.Fatal("original mail failure cleanup missing")
	}
	var count int64
	db.Model(&model.User{}).Count(&count)
	if count != 0 {
		t.Fatal("mail failure wrote user")
	}
	registrationCall(t, app, "captcha", map[string]string{"email": e}, code.CodeSuccess)
}
func TestRegistrationExternalAssemblyInstances(t *testing.T) {
	a, b := newRegistrationBoundary(), newRegistrationBoundary()
	one, _ := registrationFixture(t, a)
	two, _ := registrationFixture(t, b)
	email := "instances@invalid.example"
	registrationCall(t, one, "captcha", map[string]string{"email": email}, code.CodeSuccess)
	if a.captured(email) == "" || b.captured(email) != "" {
		t.Fatal("boundary replaced by other assembly")
	}
	registrationCall(t, two, "captcha", map[string]string{"email": email}, code.CodeSuccess)
	registrationCall(t, one, "captcha", map[string]string{"email": email}, code.CodeCaptchaTooFrequent)
	if b.captured(email) == "" {
		t.Fatal("second boundary missing")
	}
}
func TestRegistrationFrontendSuccessSocket(t *testing.T) {
	b := newRegistrationBoundary()
	app, _ := registrationFixture(t, b)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/test-mail-boundary" {
			json.NewEncoder(w).Encode(map[string]string{"captcha": b.captured("frontend@invalid.example")})
			return
		}
		app.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	command := exec.Command("node", "../web/test/helpers/hybridRegistrationSuccessClient.mjs")
	command.Env = append(os.Environ(), "HYBRID_TEST_URL="+server.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("frontend success socket: %v\n%s", err, output)
	}
	u, err := userDAO.FindByLogin("frontend@invalid.example")
	if err != nil {
		t.Fatal(err)
	}
	registrationUser(t, u.Email, map[string]any{"username": u.Username})
}
