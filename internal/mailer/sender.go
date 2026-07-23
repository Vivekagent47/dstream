package mailer

import (
	"context"
	"fmt"

	"github.com/wneessen/go-mail"

	"github.com/Vivekagent47/dstream/internal/config"
)

// Sender sends a rendered Message. An interface so the worker handler is
// testable with a fake, and the go-mail impl can later be swapped for an ESP.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

type smtpSender struct {
	client *mail.Client
	from   string
}

// NewSender returns a go-mail-backed Sender, or (nil, nil) when SMTP is not
// configured (empty Host). The worker treats a nil Sender as the dev log
// fallback. Assumes STARTTLS on the configured port (587 default); port 465
// implicit TLS would need mail.WithSSL — out of scope until needed.
func NewSender(cfg config.SMTPConfig) (Sender, error) {
	if cfg.Host == "" {
		return nil, nil
	}
	port := cfg.Port
	if port == 0 {
		port = 587
	}
	opts := []mail.Option{
		mail.WithPort(port),
		mail.WithTLSPolicy(mail.TLSMandatory),
	}
	// Only negotiate AUTH when a user is set. A no-credential relay (e.g. a
	// local MailHog/Mailpit) rejects AUTH PLAIN, which would fail every send.
	if cfg.User != "" {
		opts = append(opts,
			mail.WithSMTPAuth(mail.SMTPAuthPlain),
			mail.WithUsername(cfg.User),
			mail.WithPassword(cfg.Pass),
		)
	}
	c, err := mail.NewClient(cfg.Host, opts...)
	if err != nil {
		return nil, fmt.Errorf("mailer: new client: %w", err)
	}
	return &smtpSender{client: c, from: cfg.From}, nil
}

func (s *smtpSender) Send(ctx context.Context, m Message) error {
	msg := mail.NewMsg()
	if err := msg.From(s.from); err != nil {
		return fmt.Errorf("mailer: from: %w", err)
	}
	if err := msg.To(m.To); err != nil {
		return fmt.Errorf("mailer: to: %w", err)
	}
	msg.Subject(m.Subject)
	// Plain first, HTML as the preferred alternative (multipart/alternative).
	msg.SetBodyString(mail.TypeTextPlain, m.Text)
	msg.AddAlternativeString(mail.TypeTextHTML, m.HTML)
	return s.client.DialAndSendWithContext(ctx, msg)
}
