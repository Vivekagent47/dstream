package mailer

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/dqueue"
)

const emailKind = "email"

// emailTask is the JSON carried in dqueue.Payload.Data for Kind "email".
type emailTask struct {
	Template string         `json:"template"`
	To       string         `json:"to"`
	Vars     map[string]any `json:"vars,omitempty"`
}

func buildEmailTask(name, to string, vars map[string]any, orgID uuid.UUID) (dqueue.Payload, error) {
	data, err := json.Marshal(emailTask{Template: name, To: to, Vars: vars})
	if err != nil {
		return dqueue.Payload{}, err
	}
	return dqueue.Payload{Kind: emailKind, OrgID: orgID, Data: data}, nil
}

// Enqueue pushes an email task onto the delivery queue for the worker to send.
// orgID is uuid.Nil for pre-login mail (magic links); the queue treats nil-org
// as a single fair-ring entry.
func Enqueue(ctx context.Context, q *dqueue.Client, name, to string, vars map[string]any, orgID uuid.UUID) error {
	p, err := buildEmailTask(name, to, vars, orgID)
	if err != nil {
		return err
	}
	return q.Enqueue(ctx, p)
}
