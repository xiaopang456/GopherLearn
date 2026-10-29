package user

import (
	"GopherAI/common/code"
	"GopherAI/model"
	"errors"
	"testing"
)

// Original package-private dependency seam. These are service branch tests,
// not HTTP/original DAO/Redis/SMTP successful-registration acceptance.
func TestRegistrationBoundaryWrongCaptchaAndRedisFailure(t *testing.T) {
	for _, tc := range []struct {
		valid bool
		err   error
		want  code.Code
	}{
		{false, nil, code.CodeInvalidCaptcha},
		{false, errors.New("offline redis failure"), code.CodeServerBusy},
	} {
		deps := stubDependencies()
		deps.verifyCaptcha = func(string, string) (bool, error) { return tc.valid, tc.err }
		deps.register = func(string, string, string) (*model.User, error) {
			t.Fatal("invalid captcha reached account write")
			return nil, nil
		}
		if _, _, got := register(deps, "u@example.com", "password-123", "123456"); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
}

func TestRegistrationBoundaryTokenFailureAfterAccountCommit(t *testing.T) {
	deps := stubDependencies()
	written, consumed := false, false
	deps.register = func(username, email, hash string) (*model.User, error) {
		written = true
		return &model.User{ID: 7, Username: username, Email: email, Password: hash}, nil
	}
	deps.consumeCaptcha = func(string, string) (bool, error) { consumed = true; return true, nil }
	deps.generateToken = func(int64, string) (string, error) { return "", errors.New("offline token failure") }
	token, username, got := register(deps, "u@example.com", "password-123", "123456")
	if got != code.CodeServerBusy || !written || !consumed || token != "" || username != "" {
		t.Fatalf("partial success written=%v consumed=%v code=%d", written, consumed, got)
	}
}

func TestCaptchaBoundarySetFailureReleasesReservationAndNeverSends(t *testing.T) {
	deps := stubDependencies()
	deleted, released := false, false
	deps.setCaptcha = func(string, string) error { return errors.New("offline redis write failure") }
	deps.deleteCaptcha = func(string) error { deleted = true; return nil }
	deps.releaseSend = func(string) error { released = true; return nil }
	deps.sendEmail = func(string, string, string) error { t.Fatal("failed captcha storage reached email"); return nil }
	if got := sendCaptcha(deps, "u@example.com"); got != code.CodeServerBusy || !deleted || !released {
		t.Fatal(got, deleted, released)
	}
}
