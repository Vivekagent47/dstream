package mailer

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/Vivekagent47/dstream/internal/dqueue"
)

// emailBackoff[p.Attempt] is the delay before the next retry. len == 2 gives 3
// total attempts (send, +1m, +5m) before dead-lettering.
var emailBackoff = []time.Duration{1 * time.Minute, 5 * time.Minute}

// EmailHandler renders and sends one email task off the queue. A nil Sender
// means SMTP is unconfigured — the handler logs the task (dev fallback) instead
// of sending.
type EmailHandler struct {
	Sender  Sender
	Log     *slog.Logger
	DevMode bool // when true (and Sender==nil) log the link locally; prod never logs it
}

// Process handles one leased email task. Terminal outcomes Ack or DeadLetter
// the leased member; a transient send failure reschedules with backoff.
func (h EmailHandler) Process(ctx context.Context, p dqueue.Payload, raw string, q *dqueue.Client) error {
	var t emailTask
	if err := json.Unmarshal(p.Data, &t); err != nil {
		h.Log.Error("mailer: bad email task, dead-lettering", "err", err)
		return q.DeadLetter(ctx, raw)
	}
	msg, err := Render(t.Template, t.Vars)
	if err != nil {
		h.Log.Error("mailer: render failed, dead-lettering", "err", err, "template", t.Template)
		return q.DeadLetter(ctx, raw)
	}
	msg.To = t.To

	// No SMTP configured. In dev, log the link so local flows work without a
	// mail server. In prod we must NOT log it — the link is a valid sign-in /
	// invite URL, an auth-bypass vector in logs (mirrors server.go's DevMode
	// boot guard). Ack either way: retrying can't fix missing config.
	if h.Sender == nil {
		if h.DevMode {
			h.Log.Info("email (dev: SMTP unconfigured; link logged, not sent)", "to", t.To, "template", t.Template, "vars", t.Vars)
		} else {
			h.Log.Error("email not sent: SMTP unconfigured", "to", t.To, "template", t.Template)
		}
		return q.Ack(ctx, raw)
	}

	if err := h.Sender.Send(ctx, msg); err != nil {
		if p.Attempt < len(emailBackoff) {
			delay := emailBackoff[p.Attempt]
			p.Attempt++
			h.Log.Warn("mailer: send failed, retrying", "err", err, "to", t.To, "attempt", p.Attempt)
			// Schedule the retry, THEN Ack the leased member so it leaves
			// :processing. Otherwise Recover reinjects the original copy on
			// lease expiry — a duplicate send that also defeats the attempt cap
			// (mirrors deliver.failAttempt). On Schedule error leave it leased
			// for the recoverer.
			if err := q.Schedule(ctx, p, time.Now().Add(delay).UnixMilli()); err != nil {
				return err
			}
			return q.Ack(ctx, raw)
		}
		h.Log.Error("mailer: send failed, dead-lettering", "err", err, "to", t.To)
		return q.DeadLetter(ctx, raw)
	}
	return q.Ack(ctx, raw)
}
