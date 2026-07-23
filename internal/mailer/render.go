package mailer

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/*
var templatesFS embed.FS

// subjects maps a template name to its email subject line.
var subjects = map[string]string{
	"magic_link": "Sign in to dstream",
	"invite":     "You've been invited to dstream",
}

var (
	htmlTemplates = htmltemplate.Must(htmltemplate.ParseFS(templatesFS, "templates/*.html"))
	textTemplates = texttemplate.Must(texttemplate.ParseFS(templatesFS, "templates/*.txt"))
)

// Render builds the Subject/HTML/Text for a named template. Returns an error
// for an unknown template name.
func Render(name string, vars map[string]any) (Message, error) {
	subj, ok := subjects[name]
	if !ok {
		return Message{}, fmt.Errorf("mailer: unknown template %q", name)
	}
	var html, text bytes.Buffer
	if err := htmlTemplates.ExecuteTemplate(&html, name+".html", vars); err != nil {
		return Message{}, fmt.Errorf("mailer: render html %q: %w", name, err)
	}
	if err := textTemplates.ExecuteTemplate(&text, name+".txt", vars); err != nil {
		return Message{}, fmt.Errorf("mailer: render text %q: %w", name, err)
	}
	return Message{Subject: subj, HTML: html.String(), Text: text.String()}, nil
}
