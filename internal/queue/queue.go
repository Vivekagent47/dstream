package queue

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// Queue names — keep stable; they appear in asynqmon and as Redis keys.
const (
	QueueDeliveries = "deliveries"
	QueueDefault    = "default"
)

// Task type names.
const (
	TaskDeliver = "deliver"
)

// DeliverPayload is the task body for delivery tasks. It carries only the
// event ID — the handler reloads the full event/connection/destination from
// Postgres on every attempt so config changes (rate limit, retry policy) are
// honored on the next try.
type DeliverPayload struct {
	EventID  uuid.UUID `json:"event_id"`
	Attempt  int       `json:"attempt"`
	EnqueuedAt int64   `json:"enqueued_at_unix_ms"`
}

type Client struct {
	c *asynq.Client
}

func NewClient(redisAddr, password string, db int) *Client {
	return &Client{
		c: asynq.NewClient(asynq.RedisClientOpt{
			Addr:     redisAddr,
			Password: password,
			DB:       db,
		}),
	}
}

func (c *Client) Close() error { return c.c.Close() }

// EnqueueDeliver enqueues a delivery task with the given max retries. Pass
// `delay` > 0 to schedule (used for rate-limit deferrals and custom-schedule
// retries when we choose not to lean on asynq's built-in retry counter).
func (c *Client) EnqueueDeliver(ctx context.Context, p DeliverPayload, maxRetries int, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	task := asynq.NewTask(TaskDeliver, body)
	base := []asynq.Option{
		asynq.Queue(QueueDeliveries),
		asynq.MaxRetry(maxRetries),
	}
	base = append(base, opts...)
	return c.c.EnqueueContext(ctx, task, base...)
}
