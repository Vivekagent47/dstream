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

// DeliverPayload is the task body for delivery tasks.
//
// The handler still reloads the event/connection/destination from Postgres
// on every attempt (so live rate-limit + destination edits take effect).
// But the RETRY policy fields are stashed inline so asynq's
// RetryDelayFunc can compute the next backoff without two extra queries
// per failed delivery (events + connections). Worst-case staleness is
// bounded by the task's lifetime — a policy edit lands on the next NEW
// event, not on in-flight retries, which is acceptable.
type DeliverPayload struct {
	EventID    uuid.UUID `json:"event_id"`
	Attempt    int       `json:"attempt"`
	EnqueuedAt int64     `json:"enqueued_at_unix_ms"`

	// Retry policy snapshot, captured at enqueue time. Optional — handler
	// falls back to a 2-query DB lookup if RetryStrategy is empty (covers
	// tasks enqueued before this field shipped).
	RetryStrategy       string `json:"retry_strategy,omitempty"`
	RetryBaseMs         int32  `json:"retry_base_ms,omitempty"`
	RetryCapMs          int32  `json:"retry_cap_ms,omitempty"`
	RetryJitterPct      int32  `json:"retry_jitter_pct,omitempty"`
	CustomRetrySchedule []byte `json:"custom_retry_schedule,omitempty"`
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
