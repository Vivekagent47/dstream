// Package mailer renders and sends dstream's transactional emails (magic-link
// sign-in, org invites), delivered durably as tasks on the shared dqueue.
package mailer

// Message is a rendered email ready to send. To is set by the caller; Render
// fills Subject/HTML/Text.
type Message struct {
	To      string
	Subject string
	HTML    string
	Text    string
}
