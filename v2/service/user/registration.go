package user

import (
	"GopherAI/common/code"
	"errors"
)

// RegistrationExternal replaces only the original Redis captcha and mail
// boundaries. All fields are required; DAO, hashing, randomness and JWT remain
// the original implementations. Values are copied into each service instance.
type RegistrationExternal struct {
	VerifyCaptcha  func(string, string) (bool, error)
	ConsumeCaptcha func(string, string) (bool, error)
	SetCaptcha     func(string, string) error
	DeleteCaptcha  func(string) error
	ReserveSend    func(string) (bool, error)
	ReleaseSend    func(string) error
	SendEmail      func(string, string, string) error
}

type RegistrationService struct{ deps dependencies }

func NewRegistrationService(external *RegistrationExternal) (*RegistrationService, error) {
	d := defaultDependencies()
	if external != nil {
		if external.VerifyCaptcha == nil || external.ConsumeCaptcha == nil || external.SetCaptcha == nil || external.DeleteCaptcha == nil || external.ReserveSend == nil || external.ReleaseSend == nil || external.SendEmail == nil {
			return nil, errors.New("incomplete registration external boundaries")
		}
		d.verifyCaptcha, d.consumeCaptcha = external.VerifyCaptcha, external.ConsumeCaptcha
		d.setCaptcha, d.deleteCaptcha = external.SetCaptcha, external.DeleteCaptcha
		d.reserveSend, d.releaseSend, d.sendEmail = external.ReserveSend, external.ReleaseSend, external.SendEmail
	}
	return &RegistrationService{deps: d}, nil
}

func (s *RegistrationService) Register(email, password, captcha string) (string, string, code.Code) {
	return register(s.deps, email, password, captcha)
}
func (s *RegistrationService) SendCaptcha(email string) code.Code {
	return sendCaptcha(s.deps, email)
}
