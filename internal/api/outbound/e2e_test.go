package outbound

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/webhook"
)

func fullRoutes(r chi.Router, h Handlers) {
	r.Route("/event-types", func(r chi.Router) { r.Post("/", h.CreateEventType) })
	r.Route("/applications", func(r chi.Router) {
		r.Post("/", h.CreateApplication)
		r.Route("/{app_id}/endpoints", func(r chi.Router) { r.Post("/", h.CreateEndpoint) })
		r.Route("/{app_id}/messages", func(r chi.Router) {
			r.Post("/", h.CreateMessage)
			r.Get("/{id}/attempts", h.ListMessageAttempts)
		})
	})
}

// TestEndToEndSignedDelivery: send via the HTTP API, then run the delivery
// handler off the queue, and confirm the customer endpoint received a valid
// Standard-Webhooks-signed request.
func TestEndToEndSignedDelivery(t *testing.T) {
	q := store.New(testPool(t))
	rdb := testRedis(t)
	prefix := "obe2e-" + uuidNewShort()
	dq := dqueue.NewClient(rdb).WithPrefix(prefix)
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+":*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
	})
	uid, oid := seedOrg(t, q)
	r := newRouter(q, dq, sign(t), fullRoutes)
	ctx := context.Background()

	type capture struct {
		id, ts, sig string
		body        []byte
		hits        int
	}
	var cap capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		cap.hits++
		cap.id = req.Header.Get("webhook-id")
		cap.ts = req.Header.Get("webhook-timestamp")
		cap.sig = req.Header.Get("webhook-signature")
		cap.body, _ = io.ReadAll(req.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

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
	_ = json.Unmarshal(post(base+"/endpoints", map[string]any{"url": srv.URL}).Body.Bytes(), &ep)
	secret := ep["secret"].(string)

	var sent map[string]any
	_ = json.Unmarshal(post(base+"/messages", map[string]any{
		"event_type": "invoice.paid", "payload": map[string]any{"amount": 42},
	}).Body.Bytes(), &sent)
	msgID := sent["message_id"].(string)

	// Drain the one delivery through the real handler (allowPrivate → 127.0.0.1).
	h := webhook.Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true)}
	raw, p, ok, err := dq.FairPick(ctx, 10000)
	if err != nil || !ok {
		t.Fatalf("fairpick ok=%v err=%v", ok, err)
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatalf("process: %v", err)
	}

	if cap.hits != 1 {
		t.Fatalf("endpoint hits: got %d want 1", cap.hits)
	}
	if cap.id != msgID {
		t.Fatalf("webhook-id: got %q want %q", cap.id, msgID)
	}
	tsN, _ := strconv.ParseInt(cap.ts, 10, 64)
	want, _ := webhook.Sign(secret, msgID, tsN, cap.body)
	if cap.sig != want {
		t.Fatalf("signature mismatch: got %q want %q", cap.sig, want)
	}

	// Attempts endpoint shows the delivered attempt.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, base+"/messages/"+msgID+"/attempts", uid, oid, nil))
	var attempts []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &attempts)
	if len(attempts) != 1 || attempts[0]["response_status"].(float64) != 200 {
		t.Fatalf("attempts: %v", attempts)
	}
}
