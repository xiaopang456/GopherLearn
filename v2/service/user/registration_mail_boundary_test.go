package user

import (
	"errors"
	"testing"

	"GopherAI/common/code"
	myemail "GopherAI/common/email"
)

// A sender that reports the mail path was never configured must produce its
// own answer, not a generic server error: the operator has to change the
// configuration, and waiting does not help. Nothing may be left behind.
func TestCaptchaBoundaryWithoutMailConfigurationIsItsOwnAnswer(t *testing.T) {
	deps := stubDependencies()
	stored, deleted, released := false, false, false
	deps.setCaptcha = func(string, string) error { stored = true; return nil }
	deps.deleteCaptcha = func(string) error { deleted = true; return nil }
	deps.releaseSend = func(string) error { released = true; return nil }
	deps.sendEmail = func(string, string, string) error {
		return myemail.ErrNotConfigured
	}

	if got := sendCaptcha(deps, "u@example.com"); got != code.CodeEmailNotConfigured {
		t.Fatalf("code = %d, want %d", got, code.CodeEmailNotConfigured)
	}
	if code.CodeEmailNotConfigured.Msg() == code.CodeServerBusy.Msg() {
		t.Fatal("the refusal must not read as a generic server error")
	}
	// The captcha that was stored for a mail that cannot be sent is removed
	// again, and the send slot is released, so a later attempt is not blocked
	// by a reservation nobody can use.
	if !stored || !deleted || !released {
		t.Fatalf("stored=%v deleted=%v released=%v", stored, deleted, released)
	}
}

// A configured mail path that the relay refuses is a different answer from a
// missing configuration: the first may be transient, the second is not.
func TestCaptchaBoundaryRelayFailureIsNotTheSameAsAMissingConfiguration(t *testing.T) {
	deps := stubDependencies()
	deps.sendEmail = func(string, string, string) error {
		return errors.Join(myemail.ErrSendFailed, errors.New("relay refused"))
	}

	if got := sendCaptcha(deps, "u@example.com"); got != code.CodeEmailSendFailed {
		t.Fatalf("code = %d, want %d", got, code.CodeEmailSendFailed)
	}
	if got := sendCaptcha(deps, "u@example.com"); got == code.CodeEmailNotConfigured {
		t.Fatal("a relay failure must not be reported as a missing configuration")
	}
}

// The sender error is wrapped, so the caller can still tell the two apart.
func TestCaptchaBoundarySenderDistinguishesNotConfiguredFromSendFailure(t *testing.T) {
	if !errors.Is(myemail.ErrNotConfigured, myemail.ErrNotConfigured) {
		t.Fatal("not configured must be matchable")
	}
	if errors.Is(myemail.ErrSendFailed, myemail.ErrNotConfigured) {
		t.Fatal("a send failure must not be reported as a missing configuration")
	}
}
