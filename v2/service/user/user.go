package user

import (
	"errors"
	"GopherAI/common/code"
	myemail "GopherAI/common/email"
	myredis "GopherAI/common/redis"
	userdao "GopherAI/dao/user"
	"GopherAI/model"
	"GopherAI/utils"
	"GopherAI/utils/myjwt"
	"log"
	"net/mail"
	"strings"
)

const (
	generatedUsernameDigits = 11
	captchaDigits           = 6
	usernameAttempts        = 5
)

type dependencies struct {
	findByLogin     func(string) (*model.User, error)
	emailExists     func(string) (bool, error)
	register        func(string, string, string) (*model.User, error)
	upgradePassword func(int64, string, string) (bool, error)
	isNotFound      func(error) bool
	isDuplicate     func(error) bool
	verifyCaptcha   func(string, string) (bool, error)
	consumeCaptcha  func(string, string) (bool, error)
	setCaptcha      func(string, string) error
	deleteCaptcha   func(string) error
	reserveSend     func(string) (bool, error)
	releaseSend     func(string) error
	sendEmail       func(string, string, string) error
	generateToken   func(int64, string) (string, error)
	randomDigits    func(int) (string, error)
	hashPassword    func(string) (string, error)
	hashVerified    func(string) (string, error)
	verifyPassword  func(string, string) (bool, bool)
}

func defaultDependencies() dependencies {
	return dependencies{
		findByLogin:     userdao.FindByLogin,
		emailExists:     userdao.EmailExists,
		register:        userdao.Register,
		upgradePassword: userdao.UpgradePassword,
		isNotFound:      userdao.IsNotFound,
		isDuplicate:     userdao.IsDuplicate,
		verifyCaptcha:   myredis.VerifyCaptchaForEmail,
		consumeCaptcha:  myredis.ConsumeCaptchaForEmail,
		setCaptcha:      myredis.SetCaptchaForEmail,
		deleteCaptcha:   myredis.DeleteCaptchaForEmail,
		reserveSend:     myredis.ReserveCaptchaSend,
		releaseSend:     myredis.ReleaseCaptchaSend,
		sendEmail:       myemail.SendCaptcha,
		generateToken:   myjwt.GenerateToken,
		randomDigits:    utils.SecureRandomDigits,
		hashPassword:    utils.HashPassword,
		hashVerified:    utils.HashVerifiedPassword,
		verifyPassword:  utils.VerifyPassword,
	}
}

func Login(identifier, password string) (string, code.Code) {
	return login(defaultDependencies(), identifier, password)
}

func login(deps dependencies, identifier, password string) (string, code.Code) {
	identifier = normalizeLogin(identifier)
	if identifier == "" || password == "" {
		return "", code.CodeInvalidPassword
	}
	userInformation, err := deps.findByLogin(identifier)
	if err != nil {
		if deps.isNotFound(err) {
			return "", code.CodeInvalidPassword
		}
		return "", code.CodeServerBusy
	}
	valid, needsUpgrade := deps.verifyPassword(userInformation.Password, password)
	if !valid {
		return "", code.CodeInvalidPassword
	}
	if needsUpgrade {
		replacementHash, err := deps.hashVerified(password)
		if err != nil {
			return "", code.CodeServerBusy
		}
		updated, err := deps.upgradePassword(userInformation.ID, userInformation.Password, replacementHash)
		if err != nil {
			return "", code.CodeServerBusy
		}
		if !updated {
			current, currentErr := deps.findByLogin(identifier)
			if currentErr != nil {
				return "", code.CodeServerBusy
			}
			stillValid, _ := deps.verifyPassword(current.Password, password)
			if !stillValid {
				return "", code.CodeInvalidPassword
			}
		}
	}
	token, err := deps.generateToken(userInformation.ID, userInformation.Username)
	if err != nil {
		return "", code.CodeServerBusy
	}
	return token, code.CodeSuccess
}

func Register(email, password, captcha string) (string, string, code.Code) {
	return register(defaultDependencies(), email, password, captcha)
}

func register(deps dependencies, email, password, captcha string) (string, string, code.Code) {
	normalizedEmail, err := normalizeEmail(email)
	if err != nil || !validCaptchaShape(captcha) {
		return "", "", code.CodeInvalidParams
	}
	if err := utils.ValidateNewPassword(password); err != nil {
		return "", "", code.CodeIllegalPassword
	}
	exists, err := deps.emailExists(normalizedEmail)
	if err != nil {
		return "", "", code.CodeServerBusy
	}
	if exists {
		return "", "", code.CodeUserExist
	}
	valid, err := deps.verifyCaptcha(normalizedEmail, captcha)
	if err != nil {
		return "", "", code.CodeServerBusy
	}
	if !valid {
		return "", "", code.CodeInvalidCaptcha
	}
	passwordHash, err := deps.hashPassword(password)
	if err != nil {
		return "", "", code.CodeIllegalPassword
	}

	var userInformation *model.User
	for attempt := 0; attempt < usernameAttempts; attempt++ {
		username, err := deps.randomDigits(generatedUsernameDigits)
		if err != nil {
			return "", "", code.CodeServerBusy
		}
		userInformation, err = deps.register(username, normalizedEmail, passwordHash)
		if err == nil {
			break
		}
		if !deps.isDuplicate(err) {
			return "", "", code.CodeServerBusy
		}
		exists, existsErr := deps.emailExists(normalizedEmail)
		if existsErr != nil {
			return "", "", code.CodeServerBusy
		}
		if exists {
			return "", "", code.CodeUserExist
		}
		userInformation = nil
	}
	if userInformation == nil {
		return "", "", code.CodeServerBusy
	}

	if consumed, consumeErr := deps.consumeCaptcha(normalizedEmail, captcha); consumeErr != nil || !consumed {
		// MySQL is already committed. Returning failure here would recreate the
		// half-registration bug; the unique email index prevents a second account.
		log.Printf("registered account but captcha cleanup did not complete")
	}
	token, err := deps.generateToken(userInformation.ID, userInformation.Username)
	if err != nil {
		return "", "", code.CodeServerBusy
	}
	return token, userInformation.Username, code.CodeSuccess
}

func SendCaptcha(email string) code.Code {
	return sendCaptcha(defaultDependencies(), email)
}

func sendCaptcha(deps dependencies, email string) code.Code {
	normalizedEmail, err := normalizeEmail(email)
	if err != nil {
		return code.CodeInvalidParams
	}
	exists, err := deps.emailExists(normalizedEmail)
	if err != nil {
		return code.CodeServerBusy
	}
	if exists {
		return code.CodeUserExist
	}
	reserved, err := deps.reserveSend(normalizedEmail)
	if err != nil {
		return code.CodeServerBusy
	}
	if !reserved {
		return code.CodeCaptchaTooFrequent
	}
	cleanup := func() {
		_ = deps.deleteCaptcha(normalizedEmail)
		_ = deps.releaseSend(normalizedEmail)
	}
	captcha, err := deps.randomDigits(captchaDigits)
	if err != nil {
		cleanup()
		return code.CodeServerBusy
	}
	if err := deps.setCaptcha(normalizedEmail, captcha); err != nil {
		cleanup()
		return code.CodeServerBusy
	}
	if err := deps.sendEmail(normalizedEmail, captcha, myemail.CodeMsg); err != nil {
		cleanup()
		// The relay error is for the operator's log; the answer to the caller
		// says what the caller can do about it.
		log.Printf("captcha send failed for %s: %v", normalizedEmail, err)
		if errors.Is(err, myemail.ErrNotConfigured) {
			return code.CodeEmailNotConfigured
		}
		return code.CodeEmailSendFailed
	}
	return code.CodeSuccess
}

func normalizeLogin(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	if strings.Contains(identifier, "@") {
		return strings.ToLower(identifier)
	}
	return identifier
}

func normalizeEmail(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 100 {
		return "", mail.ErrHeaderNotPresent
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value || !strings.Contains(value, "@") {
		return "", mail.ErrHeaderNotPresent
	}
	return value, nil
}

func validCaptchaShape(value string) bool {
	if len(value) != captchaDigits {
		return false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
