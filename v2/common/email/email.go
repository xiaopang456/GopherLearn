package email

import (
	"errors"
	"fmt"
	"strings"

	"GopherAI/config"

	"gopkg.in/gomail.v2"
)

const (
	CodeMsg = "GopherAI验证码如下(验证码仅限于2分钟有效): "

	// DefaultSMTPHost and DefaultSMTPPort are used when the configuration does
	// not name a relay, so a deployment that only ever had the historical
	// settings keeps sending from the same place.
	DefaultSMTPHost = "smtp.qq.com"
	DefaultSMTPPort = 587
)

var (
	// ErrNotConfigured means the mail path cannot run at all: the sender
	// address or its authorization code is missing. It is a configuration
	// problem, not a temporary failure, and callers report it as such.
	ErrNotConfigured = errors.New("email service is not configured")
	// ErrSendFailed means the configuration is complete but the relay refused
	// or could not be reached.
	ErrSendFailed = errors.New("email send failed")
)

// Settings returns the effective mail settings: the configured values, with
// the historical defaults filled in for the ones that were left out.
//
// It reads the installed configuration only. A configuration that has not been
// installed yet means the mail path cannot run, so the settings come back
// empty rather than the process reaching for a file that may not exist.
func Settings() (host string, port int, from string, authcode string) {
	current := config.Current()
	if current == nil {
		return "", 0, "", ""
	}
	c := current.EmailConfig
	host = strings.TrimSpace(c.Host)
	if host == "" {
		host = DefaultSMTPHost
	}
	port = c.Port
	if port <= 0 {
		port = DefaultSMTPPort
	}
	return host, port, strings.TrimSpace(c.Email), strings.TrimSpace(c.Authcode)
}

// Configured reports whether a captcha can be sent at all. A missing sender
// address or authorization code means the mail path is unavailable.
func Configured() bool {
	_, _, from, authcode := Settings()
	return from != "" && authcode != ""
}

// SendCaptcha delivers one captcha to the given recipient.
//
// The returned error is ErrNotConfigured when the mail path was never
// configured, and wraps ErrSendFailed when the relay refused. The underlying
// relay error is kept in the message for the operator's log; it is not the
// text a caller should show to a visitor.
func SendCaptcha(to, code, msg string) error {
	host, port, from, authcode := Settings()
	if from == "" || authcode == "" {
		return fmt.Errorf("%w: sender address and authorization code are both required", ErrNotConfigured)
	}
	m := gomail.NewMessage()

	// 发件人
	m.SetHeader("From", from)
	// 收件人
	m.SetHeader("To", to)
	// 主题
	m.SetHeader("Subject", "来自GopherAI的信息")
	// 正文内容（纯文本形式，也可以用 text/html）
	m.SetBody("text/plain", msg+" "+code)

	d := gomail.NewDialer(host, port, from, authcode)

	// 发送邮件
	if err := d.DialAndSend(m); err != nil {
		return fmt.Errorf("%w: %v", ErrSendFailed, err)
	}
	return nil
}
