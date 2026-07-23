package mailer

import (
	"strings"
	"testing"
)

func TestRenderMagicLink(t *testing.T) {
	m, err := Render("magic_link", map[string]any{"Link": "https://ex/verify?token=abc"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject == "" {
		t.Fatal("empty subject")
	}
	if !strings.Contains(m.HTML, "https://ex/verify?token=abc") {
		t.Fatalf("html missing link: %s", m.HTML)
	}
	if !strings.Contains(m.Text, "https://ex/verify?token=abc") {
		t.Fatalf("text missing link: %s", m.Text)
	}
}

func TestRenderUnknownTemplate(t *testing.T) {
	if _, err := Render("nope", nil); err == nil {
		t.Fatal("want error for unknown template")
	}
}
