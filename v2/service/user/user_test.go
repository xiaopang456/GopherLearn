package user

import (
	"GopherAI/common/code"
	"GopherAI/model"
	"errors"
	"testing"
)

var (
	errNotFound  = errors.New("not found")
	errDuplicate = errors.New("duplicate")
)

func stubDependencies() dependencies {
	return dependencies{
		findByLogin: func(string) (*model.User, error) { return nil, errNotFound },
		emailExists: func(string) (bool, error) { return false, nil },
		register: func(username, email, passwordHash string) (*model.User, error) {
			return &model.User{ID: 7, Username: username, Email: email, Password: passwordHash}, nil
		},
		upgradePassword: func(int64, string, string) (bool, error) { return true, nil },
		isNotFound:      func(err error) bool { return errors.Is(err, errNotFound) },
		isDuplicate:     func(err error) bool { return errors.Is(err, errDuplicate) },
		verifyCaptcha:   func(string, string) (bool, error) { return true, nil },
		consumeCaptcha:  func(string, string) (bool, error) { return true, nil },
		setCaptcha:      func(string, string) error { return nil },
		deleteCaptcha:   func(string) error { return nil },
		reserveSend:     func(string) (bool, error) { return true, nil },
		releaseSend:     func(string) error { return nil },
		sendEmail:       func(string, string, string) error { return nil },
		generateToken:   func(int64, string) (string, error) { return "token", nil },
		randomDigits:    func(int) (string, error) { return "12345678901", nil },
		hashPassword:    func(string) (string, error) { return "bcrypt", nil },
		hashVerified:    func(string) (string, error) { return "upgraded", nil },
		verifyPassword:  func(string, string) (bool, bool) { return true, false },
	}
}

func TestLoginUsesUniformCredentialFailure(t *testing.T) {
	deps := stubDependencies()
	if _, got := login(deps, "missing", "password"); got != code.CodeInvalidPassword {
		t.Fatalf("missing user code=%d", got)
	}
	deps.findByLogin = func(string) (*model.User, error) {
		return &model.User{ID: 1, Username: "user", Password: "hash"}, nil
	}
	deps.verifyPassword = func(string, string) (bool, bool) { return false, false }
	if _, got := login(deps, "user", "wrong"); got != code.CodeInvalidPassword {
		t.Fatalf("wrong password code=%d", got)
	}
}

func TestLoginMigratesLegacyHashConditionally(t *testing.T) {
	deps := stubDependencies()
	deps.findByLogin = func(identifier string) (*model.User, error) {
		if identifier != "user@example.com" {
			t.Fatalf("identifier=%q", identifier)
		}
		return &model.User{ID: 9, Username: "123", Password: "legacy"}, nil
	}
	deps.verifyPassword = func(stored, password string) (bool, bool) {
		return stored == "legacy" && password == "old-pass", true
	}
	upgraded := false
	deps.upgradePassword = func(id int64, oldHash, newHash string) (bool, error) {
		upgraded = true
		if id != 9 || oldHash != "legacy" || newHash != "upgraded" {
			t.Fatalf("upgrade args id=%d old=%q new=%q", id, oldHash, newHash)
		}
		return true, nil
	}
	token, got := login(deps, " USER@EXAMPLE.COM ", "old-pass")
	if got != code.CodeSuccess || token != "token" || !upgraded {
		t.Fatalf("token=%q code=%d upgraded=%v", token, got, upgraded)
	}
}

func TestLoginRechecksConcurrentPasswordChange(t *testing.T) {
	deps := stubDependencies()
	reads := 0
	deps.findByLogin = func(string) (*model.User, error) {
		reads++
		if reads == 1 {
			return &model.User{ID: 9, Username: "user", Password: "legacy"}, nil
		}
		return &model.User{ID: 9, Username: "user", Password: "current"}, nil
	}
	deps.verifyPassword = func(stored, password string) (bool, bool) {
		if stored == "legacy" {
			return true, true
		}
		return stored == "current" && password == "old-pass", false
	}
	deps.upgradePassword = func(int64, string, string) (bool, error) { return false, nil }
	if _, got := login(deps, "user", "old-pass"); got != code.CodeSuccess || reads != 2 {
		t.Fatalf("code=%d reads=%d", got, reads)
	}
}

func TestRegisterNormalizesEmailAndDoesNotSendSecondEmail(t *testing.T) {
	deps := stubDependencies()
	deps.register = func(username, email, passwordHash string) (*model.User, error) {
		if email != "user@example.com" || passwordHash != "bcrypt" {
			t.Fatalf("email=%q hash=%q", email, passwordHash)
		}
		return &model.User{ID: 7, Username: username, Email: email, Password: passwordHash}, nil
	}
	deps.sendEmail = func(string, string, string) error {
		t.Fatal("registration sent a second account email")
		return nil
	}
	token, username, got := register(deps, " User@Example.COM ", "password-123", "123456")
	if got != code.CodeSuccess || token != "token" || username != "12345678901" {
		t.Fatalf("token=%q username=%q code=%d", token, username, got)
	}
}

func TestRegisterDuplicateEmailWinsConcurrentRace(t *testing.T) {
	deps := stubDependencies()
	checks := 0
	deps.emailExists = func(string) (bool, error) {
		checks++
		return checks > 1, nil
	}
	deps.register = func(string, string, string) (*model.User, error) { return nil, errDuplicate }
	if _, _, got := register(deps, "user@example.com", "password-123", "123456"); got != code.CodeUserExist {
		t.Fatalf("code=%d", got)
	}
}

func TestRegisterRetriesUsernameCollision(t *testing.T) {
	deps := stubDependencies()
	attempts := 0
	deps.randomDigits = func(int) (string, error) {
		attempts++
		if attempts == 1 {
			return "11111111111", nil
		}
		return "22222222222", nil
	}
	deps.register = func(username, email, passwordHash string) (*model.User, error) {
		if username == "11111111111" {
			return nil, errDuplicate
		}
		return &model.User{ID: 8, Username: username, Email: email, Password: passwordHash}, nil
	}
	_, username, got := register(deps, "user@example.com", "password-123", "123456")
	if got != code.CodeSuccess || username != "22222222222" || attempts != 2 {
		t.Fatalf("username=%q code=%d attempts=%d", username, got, attempts)
	}
}

func TestRegisterDoesNotReportFailureAfterCaptchaCleanupError(t *testing.T) {
	deps := stubDependencies()
	deps.consumeCaptcha = func(string, string) (bool, error) { return false, errors.New("redis unavailable") }
	if _, _, got := register(deps, "user@example.com", "password-123", "123456"); got != code.CodeSuccess {
		t.Fatalf("code=%d", got)
	}
}

func TestSendCaptchaRateLimitAndFailureCleanup(t *testing.T) {
	deps := stubDependencies()
	deps.reserveSend = func(string) (bool, error) { return false, nil }
	if got := sendCaptcha(deps, "user@example.com"); got != code.CodeCaptchaTooFrequent {
		t.Fatalf("rate limit code=%d", got)
	}

	deps = stubDependencies()
	deleted, released := false, false
	deps.deleteCaptcha = func(string) error { deleted = true; return nil }
	deps.releaseSend = func(string) error { released = true; return nil }
	deps.sendEmail = func(string, string, string) error { return errors.New("smtp failed") }
	// A relay that refuses is reported as its own outcome, not as a generic
	// server error: the caller may retry, and an operator has to know that the
	// mail path ran and failed rather than never being configured. The
	// reservation is still released and the stored captcha still removed.
	if got := sendCaptcha(deps, " User@Example.com "); got != code.CodeEmailSendFailed || !deleted || !released {
		t.Fatalf("code=%d deleted=%v released=%v", got, deleted, released)
	}
}

func TestRegisterRejectsPasswordAndCaptchaShapeBeforeDependencies(t *testing.T) {
	deps := stubDependencies()
	deps.verifyCaptcha = func(string, string) (bool, error) {
		t.Fatal("invalid request reached captcha dependency")
		return false, nil
	}
	if _, _, got := register(deps, "user@example.com", "short", "abc"); got != code.CodeInvalidParams {
		t.Fatalf("code=%d", got)
	}
}
