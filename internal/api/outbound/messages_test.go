package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

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

func replayRoutes(r chi.Router, h Handlers) {
	r.Route("/applications", func(r chi.Router) {
		r.Post("/", h.CreateApplication)
		r.Route("/{app_id}", func(r chi.Router) {
			r.Post("/endpoints", h.CreateEndpoint)
			r.Route("/messages", func(r chi.Router) {
				r.Post("/", h.CreateMessage)
				r.Post("/{id}/endpoints/{endpoint_id}/replay", h.ReplayDelivery)
			})
		})
	})
	r.Route("/event-types", func(r chi.Router) { r.Post("/", h.CreateEventType) })
}

// Replaying a message whose payload was expunged by retention returns 422 and
// never creates/enqueues a delivery.
func TestReplayExpungedMessageReturns422(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	rdb := testRedis(t)
	dq := dqueue.NewClient(rdb).WithPrefix("obtest-" + uuidNewShort())
	uid, oid := seedOrg(t, q)
	r := newRouter(q, dq, sign(t), replayRoutes)

	post := func(path string, body any) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, path, uid, oid, body))
		return rec
	}
	post("/api/event-types", map[string]any{"name": "invoice.paid"})
	var app map[string]any
	_ = json.Unmarshal(post("/api/applications", map[string]any{"name": "A"}).Body.Bytes(), &app)
	base := "/api/applications/" + app["id"].(string)

	var ep map[string]any
	_ = json.Unmarshal(post(base+"/endpoints", map[string]any{"url": "https://ex.test/a"}).Body.Bytes(), &ep)
	epID := ep["id"].(string)

	var msg map[string]any
	_ = json.Unmarshal(post(base+"/messages", map[string]any{"event_type": "invoice.paid", "payload": map[string]any{"x": 1}}).Body.Bytes(), &msg)
	msgID := msg["message_id"].(string)

	// Expunge the payload directly (simulating the retention sweep).
	if _, err := pool.Exec(context.Background(), `UPDATE messages SET payload = NULL WHERE id = $1`, store.UUID(uuid.MustParse(msgID))); err != nil {
		t.Fatalf("expunge: %v", err)
	}
	drainMessageTasks(t, dq) // clear the original fan-out task

	rec := post(base+"/messages/"+msgID+"/endpoints/"+epID+"/replay", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("replay of expunged message: got %d want 422, body=%s", rec.Code, rec.Body.String())
	}
	if got := drainMessageTasks(t, dq); got != 0 {
		t.Fatalf("expunged replay must not enqueue a delivery, got %d tasks", got)
	}
}

// TestSendMessageFansOutWithChannels proves channel-overlap matching and the
// untagged→unfiltered-only semantics (a nil MsgChannels excludes channel-filtered
// endpoints but keeps unfiltered ones).
func TestSendMessageFansOutWithChannels(t *testing.T) {
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

	post("/api/event-types", map[string]any{"name": "invoice.paid"})
	var app map[string]any
	_ = json.Unmarshal(post("/api/applications", map[string]any{"name": "A"}).Body.Bytes(), &app)
	appID := app["id"].(string)
	base := "/api/applications/" + appID

	// A: no channels (matches all). B: ["acme"]. C: ["other"].
	post(base+"/endpoints", map[string]any{"url": "https://ex.test/a"})
	post(base+"/endpoints", map[string]any{"url": "https://ex.test/b", "channels": []string{"acme"}})
	post(base+"/endpoints", map[string]any{"url": "https://ex.test/c", "channels": []string{"other"}})

	deliveredURLs := func(msgID string) map[string]bool {
		rows, err := q.ListDeliveriesForMessage(context.Background(), store.UUID(uuid.MustParse(msgID)))
		if err != nil {
			t.Fatalf("list deliveries: %v", err)
		}
		out := map[string]bool{}
		for _, dl := range rows {
			out[dl.EndpointUrl] = true
		}
		return out
	}
	msgID := func(rec *httptest.ResponseRecorder) string {
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m["message_id"].(string)
	}

	// Tagged with ["acme"] → A (unfiltered) + B (overlap); C excluded.
	rec := post(base+"/messages", map[string]any{"event_type": "invoice.paid", "payload": map[string]any{"x": 1}, "channels": []string{"acme"}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send acme: got %d body=%s", rec.Code, rec.Body.String())
	}
	got := deliveredURLs(msgID(rec))
	if len(got) != 2 || !got["https://ex.test/a"] || !got["https://ex.test/b"] {
		t.Fatalf("acme fan-out: want {a,b}, got %v", got)
	}

	// Untagged → only A; channel-filtered B and C excluded.
	rec = post(base+"/messages", map[string]any{"event_type": "invoice.paid", "payload": map[string]any{"x": 2}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send untagged: got %d body=%s", rec.Code, rec.Body.String())
	}
	got = deliveredURLs(msgID(rec))
	if len(got) != 1 || !got["https://ex.test/a"] {
		t.Fatalf("untagged fan-out: want {a}, got %v", got)
	}
}
