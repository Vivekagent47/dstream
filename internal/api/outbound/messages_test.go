package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

func msgRoutes(r chi.Router, h Handlers) {
	r.Route("/applications", func(r chi.Router) {
		r.Post("/", h.CreateApplication)
		r.Route("/{app_id}/endpoints", func(r chi.Router) { r.Post("/", h.CreateEndpoint) })
		r.Route("/{app_id}/messages", func(r chi.Router) { r.Post("/", h.CreateMessage) })
	})
	r.Route("/event-types", func(r chi.Router) { r.Post("/", h.CreateEventType) })
}

// drainMessageTasks counts queued Kind=="message" tasks by leasing until empty.
func drainMessageTasks(t *testing.T, dq *dqueue.Client) int {
	t.Helper()
	ctx := context.Background()
	n := 0
	for {
		raw, p, ok, err := dq.FairPick(ctx, 5000)
		if err != nil {
			t.Fatalf("fairpick: %v", err)
		}
		if !ok {
			return n
		}
		if p.Kind == "message" {
			n++
		}
		_ = dq.Ack(ctx, raw)
	}
}

func TestSendMessageFansOutWithFilter(t *testing.T) {
	q := store.New(testPool(t))
	rdb := testRedis(t)
	dq := dqueue.NewClient(rdb).WithPrefix("obtest-" + uuidNewShort())
	uid, oid := seedOrg(t, q)
	r := newRouter(q, dq, sign(t), msgRoutes)

	post := func(path string, body any) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, path, uid, oid, body))
		return rec
	}

	// event type + app
	post("/api/event-types", map[string]any{"name": "invoice.paid"})
	var app map[string]any
	_ = json.Unmarshal(post("/api/applications", map[string]any{"name": "A"}).Body.Bytes(), &app)
	appID := app["id"].(string)
	base := "/api/applications/" + appID

	// 3 endpoints: matching filter, non-matching filter, matching-all
	post(base+"/endpoints", map[string]any{"url": "https://ex.test/a", "filter_event_types": []string{"invoice.paid"}})
	post(base+"/endpoints", map[string]any{"url": "https://ex.test/b", "filter_event_types": []string{"user.created"}})
	post(base+"/endpoints", map[string]any{"url": "https://ex.test/c"}) // no filter = all

	rec := post(base+"/messages", map[string]any{"event_type": "invoice.paid", "payload": map[string]any{"x": 1}, "event_id": "evt_1"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send: got %d body=%s", rec.Code, rec.Body.String())
	}
	// endpoints a and c match; b is filtered out → 2 deliveries enqueued.
	if got := drainMessageTasks(t, dq); got != 2 {
		t.Fatalf("fan-out: got %d message tasks, want 2", got)
	}

	// idempotent replay: same event_id → no new fan-out.
	rec = post(base+"/messages", map[string]any{"event_type": "invoice.paid", "payload": map[string]any{"x": 1}, "event_id": "evt_1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("replay: got %d want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["idempotent_replay"] != true {
		t.Fatalf("expected idempotent_replay=true, got %v", body)
	}
	if got := drainMessageTasks(t, dq); got != 0 {
		t.Fatalf("replay must not re-fan-out, got %d tasks", got)
	}
}
