package mailer

import (
	"testing"

	"github.com/Vivekagent47/dstream/internal/config"
)

// compile-time: smtpSender satisfies Sender.
var _ Sender = (*smtpSender)(nil)

func TestNewSenderDisabledWhenNoHost(t *testing.T) {
	s, err := NewSender(config.SMTPConfig{Host: ""})
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Fatal("want nil sender when host empty")
	}
}

func TestNewSenderEnabled(t *testing.T) {
	s, err := NewSender(config.SMTPConfig{Host: "smtp.example.com", Port: 587, User: "u", Pass: "p", From: "f@x"})
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("want non-nil sender when host set")
	}
}
