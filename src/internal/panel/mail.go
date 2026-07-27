package panel

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
	"unicode"
)

// smtpCfg is the operator-configured outbound mail transport.
type smtpCfg struct {
	Host string
	Port string
	User string
	Pass string
	From string
	TLS  string // starttls | tls | none
}

// mailVerifyEnabled reports whether registration requires a 6-digit email code.
func (s *Server) mailVerifyEnabled(ctx context.Context) bool {
	return s.db.Setting(ctx, "mail_verify_enabled", "0") == "1"
}

// loadSMTP reads SMTP settings. ok is false when host or from is missing
// (minimum required to actually send mail).
func (s *Server) loadSMTP(ctx context.Context) (smtpCfg, bool) {
	cfg := smtpCfg{
		Host: strings.TrimSpace(s.db.Setting(ctx, "smtp_host", "")),
		Port: strings.TrimSpace(s.db.Setting(ctx, "smtp_port", "587")),
		User: s.db.Setting(ctx, "smtp_user", ""),
		Pass: s.db.Setting(ctx, "smtp_pass", ""),
		From: strings.TrimSpace(s.db.Setting(ctx, "smtp_from", "")),
		TLS:  strings.ToLower(strings.TrimSpace(s.db.Setting(ctx, "smtp_tls", "starttls"))),
	}
	if cfg.Port == "" {
		cfg.Port = "587"
	}
	switch cfg.TLS {
	case "tls", "none", "starttls":
	default:
		cfg.TLS = "starttls"
	}
	if cfg.Host == "" || cfg.From == "" {
		return cfg, false
	}
	return cfg, true
}

// sendVerifyCode emails a registration verification code.
func (s *Server) sendVerifyCode(ctx context.Context, to, code, site string) error {
	cfg, ok := s.loadSMTP(ctx)
	if !ok {
		return fmt.Errorf("SMTP 未配置完整（需要主机与发件人）")
	}
	if site == "" {
		site = s.cfg.SiteName
	}
	subject := fmt.Sprintf("[%s] 注册验证码", site)
	body := fmt.Sprintf("您的注册验证码是：%s\n\n有效期 15 分钟。如非本人操作，请忽略本邮件。\n", code)
	return s.sendMail(cfg, to, subject, body)
}

// sendMail delivers a plain-text message via the configured SMTP transport.
func (s *Server) sendMail(cfg smtpCfg, to, subject, body string) error {
	addr := net.JoinHostPort(cfg.Host, cfg.Port)
	msg := buildMailMessage(cfg.From, to, subject, body)

	var auth smtp.Auth
	if cfg.User != "" {
		auth = smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)
	}

	switch cfg.TLS {
	case "tls":
		return sendSMTPS(addr, cfg.Host, auth, cfg.From, to, msg)
	case "none":
		return sendSMTPPlain(addr, auth, cfg.From, to, msg)
	default: // starttls
		return sendSMTPStartTLS(addr, cfg.Host, auth, cfg.From, to, msg)
	}
}

// encodeSubject returns s unchanged when pure ASCII, otherwise RFC 2047
// Base64 UTF-8 encoded word form so Chinese subjects survive SMTP hops.
func encodeSubject(s string) string {
	ascii := true
	for _, r := range s {
		if r > unicode.MaxASCII || r == '\r' || r == '\n' {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

func buildMailMessage(from, to, subject, body string) []byte {
	var b strings.Builder
	b.WriteString("From: ")
	b.WriteString(from)
	b.WriteString("\r\nTo: ")
	b.WriteString(to)
	b.WriteString("\r\nSubject: ")
	b.WriteString(encodeSubject(subject))
	b.WriteString("\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

func smtpDial(addr string) (*smtp.Client, error) {
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func sendSMTPStartTLS(addr, host string, auth smtp.Auth, from, to string, msg []byte) error {
	c, err := smtpDial(addr)
	if err != nil {
		return fmt.Errorf("SMTP: %w", err)
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		tlsCfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}
	return smtpData(c, from, to, msg)
}

func sendSMTPS(addr, host string, auth smtp.Auth, from, to string, msg []byte) error {
	tlsCfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("SMTPS: %w", err)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("SMTPS: %w", err)
	}
	defer c.Close()
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}
	return smtpData(c, from, to, msg)
}

func sendSMTPPlain(addr string, auth smtp.Auth, from, to string, msg []byte) error {
	c, err := smtpDial(addr)
	if err != nil {
		return fmt.Errorf("SMTP: %w", err)
	}
	defer c.Close()
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}
	return smtpData(c, from, to, msg)
}

func smtpData(c *smtp.Client, from, to string, msg []byte) error {
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
