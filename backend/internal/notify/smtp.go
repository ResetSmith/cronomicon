package notify

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SMTP holds the resolved SMTP transport settings (loaded from notification_config;
// the password is already decrypted by the dispatcher).
type SMTP struct {
	Host       string
	Port       int
	Encryption string // none | starttls | tls
	Username   string
	Password   string
	From       string
	FromName   string
}

// Send delivers one message to the recipients using native net/smtp. Encryption:
//   - "tls":      implicit TLS (typically port 465) — dial a TLS socket directly.
//   - "starttls": plaintext connect then upgrade with STARTTLS (typically 587).
//   - "none":     plaintext (dev/relay only).
//
// Returns an error (never panics) so the dispatcher can log a failed send.
func (s SMTP) Send(to []string, subject, body string) error {
	if s.Host == "" {
		return fmt.Errorf("smtp host not configured")
	}
	if len(to) == 0 {
		return fmt.Errorf("no recipients")
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.port()))
	msg := s.buildMessage(to, subject, body)

	client, err := s.dial(addr)
	if err != nil {
		return err
	}
	defer client.Close()

	if s.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := client.Mail(s.fromAddr()); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(strings.TrimSpace(rcpt)); err != nil {
			return fmt.Errorf("smtp RCPT %q: %w", rcpt, err)
		}
	}
	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := wc.Write([]byte(msg)); err != nil {
		_ = wc.Close()
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return client.Quit()
}

func (s SMTP) dial(addr string) (*smtp.Client, error) {
	if strings.EqualFold(s.Encryption, "tls") {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr,
			&tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12})
		if err != nil {
			return nil, fmt.Errorf("smtp tls dial: %w", err)
		}
		c, err := smtp.NewClient(conn, s.Host)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("smtp client: %w", err)
		}
		return c, nil
	}

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("smtp dial: %w", err)
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("smtp client: %w", err)
	}
	if !strings.EqualFold(s.Encryption, "none") {
		// STARTTLS (default). Require the server to advertise the extension —
		// fall-through to plaintext auth is never acceptable (PP-L16).
		ok, _ := c.Extension("STARTTLS")
		if !ok {
			_ = c.Close()
			return nil, fmt.Errorf("smtp server does not advertise STARTTLS; refusing plaintext auth")
		}
		if err := c.StartTLS(&tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("smtp starttls: %w", err)
		}
	}
	return c, nil
}

func (s SMTP) port() int {
	if s.Port > 0 {
		return s.Port
	}
	return 587
}

func (s SMTP) fromAddr() string {
	if s.From != "" {
		return s.From
	}
	return s.Username
}

func (s SMTP) buildMessage(to []string, subject, body string) string {
	from := s.fromAddr()
	if s.FromName != "" {
		from = fmt.Sprintf("%s <%s>", s.FromName, s.fromAddr())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}
