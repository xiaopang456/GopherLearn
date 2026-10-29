package user

import (
	"reflect"
	"testing"
)

func TestRegistrationExternalBoundaryValidationAndCopy(t *testing.T) {
	if _, err := NewRegistrationService(&RegistrationExternal{}); err == nil {
		t.Fatal("incomplete boundary accepted")
	}
	s, err := NewRegistrationService(nil)
	if err != nil {
		t.Fatal(err)
	}
	original := defaultDependencies()
	// The only overridable fields are external captcha/mail, never DAO/hash/JWT.
	for _, pair := range [][2]any{{s.deps.register, original.register}, {s.deps.emailExists, original.emailExists}, {s.deps.hashPassword, original.hashPassword}, {s.deps.randomDigits, original.randomDigits}, {s.deps.generateToken, original.generateToken}} {
		if reflect.ValueOf(pair[0]).Pointer() != reflect.ValueOf(pair[1]).Pointer() {
			t.Fatal("original implementation replaced")
		}
	}
	called := false
	e := &RegistrationExternal{
		VerifyCaptcha: func(string, string) (bool, error) { return true, nil }, ConsumeCaptcha: func(string, string) (bool, error) { return true, nil },
		SetCaptcha: func(string, string) error { return nil }, DeleteCaptcha: func(string) error { return nil },
		ReserveSend: func(string) (bool, error) { return true, nil }, ReleaseSend: func(string) error { return nil },
		SendEmail: func(string, string, string) error { called = true; return nil },
	}
	instance, err := NewRegistrationService(e)
	if err != nil {
		t.Fatal(err)
	}
	e.SendEmail = func(string, string, string) error { t.Fatal("pointer mutation leaked into instance"); return nil }
	if err := instance.deps.sendEmail("", "", ""); err != nil || !called {
		t.Fatal("instance boundary not copied")
	}
}
