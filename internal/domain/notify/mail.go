package notify

import (
	"crypto/tls"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/weave/internal/ops"
	"github.com/wneessen/go-mail"
)

// SMTPConfig is the effective SMTP configuration (config table, env overrides).
type SMTPConfig struct {
	Host, User, Password, From, Secure string
	Port                               int
	SkipVerify                         bool
}

// LoadSMTP resolves the effective SMTP settings.
func LoadSMTP(e *ops.Env) SMTPConfig {
	c := SMTPConfig{Host: Config(e, "smtp.host"), User: Config(e, "smtp.user"), Password: Config(e, "smtp.password"),
		From: Config(e, "smtp.from"), Secure: strings.ToLower(Config(e, "smtp.secure")), SkipVerify: ConfigBool(e, "smtp.skipVerify")}
	c.Port, _ = strconv.Atoi(Config(e, "smtp.port"))
	if c.Secure == "" {
		c.Secure = "starttls"
	}
	if c.Port == 0 {
		if c.Secure == "tls" {
			c.Port = 465
		} else {
			c.Port = 587
		}
	}
	return c
}

// SMTPConfigured reports whether a mail host is set.
func SMTPConfigured(e *ops.Env) bool { return Config(e, "smtp.host") != "" }

// MailSender delivers one message; replaceable in tests.
var MailSender = sendSMTP

// SendMail sends an e-mail through the configured SMTP server (synchronously).
// html may be empty. Returns an error when SMTP is not configured.
func SendMail(e *ops.Env, to []string, subject, text, html string) error {
	cfg := LoadSMTP(e)
	if cfg.Host == "" {
		return errors.New("SMTP is not configured")
	}
	if len(to) == 0 {
		return errors.New("no recipients")
	}
	return MailSender(cfg, to, subject, text, html)
}

func sendSMTP(cfg SMTPConfig, to []string, subject, text, html string) error {
	from := cfg.From
	if from == "" {
		from = "weave@" + cfg.Host
	}
	m := mail.NewMsg()
	if err := m.From(from); err != nil {
		return err
	}
	if err := m.To(to...); err != nil {
		return err
	}
	m.Subject(subject)
	m.SetMessageID()
	m.SetDate()
	switch {
	case html != "" && text != "":
		m.SetBodyString(mail.TypeTextPlain, text)
		m.AddAlternativeString(mail.TypeTextHTML, html)
	case html != "":
		m.SetBodyString(mail.TypeTextHTML, html)
	default:
		m.SetBodyString(mail.TypeTextPlain, text)
	}
	opts := []mail.Option{mail.WithPort(cfg.Port), mail.WithTimeout(20 * time.Second)}
	switch cfg.Secure {
	case "tls":
		opts = append(opts, mail.WithSSL())
	case "none":
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	default:
		opts = append(opts, mail.WithTLSPolicy(mail.TLSOpportunistic))
	}
	if cfg.SkipVerify {
		opts = append(opts, mail.WithTLSConfig(&tls.Config{InsecureSkipVerify: true, ServerName: cfg.Host}))
	}
	if cfg.User != "" {
		opts = append(opts, mail.WithSMTPAuth(mail.SMTPAuthAutoDiscover), mail.WithUsername(cfg.User), mail.WithPassword(cfg.Password))
	}
	c, err := mail.NewClient(cfg.Host, opts...)
	if err != nil {
		return err
	}
	return c.DialAndSend(m)
}
