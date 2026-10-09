package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

// mkEndpoint creates an endpoint over the API and returns its id.
func (e *quotaEnv) mkEndpoint(t *testing.T, body map[string]any) string {
	t.Helper()
	rec := e.post(e.base+"/endpoints", body)
	var ep map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ep)
	id, ok := ep["id"].(string)
	if !ok {
		t.Fatalf("create endpoint: %d %s", rec.Code, rec.Body.String())
	}
	return id
}

func (e *quotaEnv) testSend(epID string) int {
	rec := e.post(e.base+"/endpoints/"+epID+"/test", map[string]any{"event_type": "invoice.paid"})
	return rec.Code
}

// M3: a test send creates a real message + delivery, so it obeys the ceiling.
func TestTestEndpoint_OverHard_429WithRetryAfter(t *testing.T) {
	e := newQuotaEnv(t, 1, 1, "month", nil)
	epID := e.mkEndpoint(t, map[string]any{"url": "https://ex.test/a"})
	if rec := e.publish(1); rec.Code != http.StatusAccepted { // consumes the ceiling
		t.Fatalf("publish: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	rec := e.post(e.base+"/endpoints/"+epID+"/test", map[string]any{"event_type": "invoice.paid"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("test send: status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}

func TestTestEndpoint_UnderQuota_202(t *testing.T) {
	e := newQuotaEnv(t, 0, 0, "month", nil)
	epID := e.mkEndpoint(t, map[string]any{"url": "https://ex.test/a"})
	if code := e.testSend(epID); code != http.StatusAccepted {
		t.Fatalf("test send: status = %d, want 202", code)
	}
}

// M4: message row and its deliveries land together (one tx); only matching
// endpoints get a delivery.
func TestPublish_MessageAndDeliveriesPersistTogether(t *testing.T) {
	e := newQuotaEnv(t, 0, 0, "month", nil)
	hit := e.mkEndpoint(t, map[string]any{"url": "https://ex.test/a", "filter_event_types": []string{"invoice.paid"}})
	e.mkEndpoint(t, map[string]any{"url": "https://ex.test/b", "filter_event_types": []string{"user.created"}})

	rec := e.publish(1)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("publish: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	msgID, err := uuid.Parse(resp["message_id"].(string))
	if err != nil {
		t.Fatalf("message_id: %v", err)
	}

	ctx := context.Background()
	var nMsg, nDel int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE id=$1`, store.UUID(msgID)).Scan(&nMsg); err != nil {
		t.Fatal(err)
	}
	dels, err := e.q.ListDeliveriesForMessage(ctx, store.UUID(msgID))
	if err != nil {
		t.Fatal(err)
	}
	nDel = len(dels)
	if nMsg != 1 || nDel != 1 {
		t.Fatalf("messages=%d deliveries=%d, want 1 and 1", nMsg, nDel)
	}
	if got := uid36(dels[0].EndpointID); got != hit {
		t.Errorf("delivery endpoint = %s, want %s", got, hit)
	}
}
