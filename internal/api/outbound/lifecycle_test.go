package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/webhook"
)

func rotateRoutes(r chi.Router, h Handlers) {
	r.Route("/applications", func(r chi.Router) {
		r.Post("/", h.CreateApplication)
		r.Route("/{app_id}/endpoints", func(r chi.Router) {
			r.Post("/", h.CreateEndpoint)
			r.Patch("/{id}", h.PatchEndpoint)
			r.Post("/{id}/rotate-secret", h.RotateEndpointSecret)
			r.Get("/{id}/secret", h.GetEndpointSecret)
		})
	})
}

func TestRotateSecretReturnsNewAndKeepsPrev(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	h := Handlers{Log: discardLog(), Queries: q, SecretGrace: 24 * time.Hour}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, sign(t)))
		r.Group(func(r chi.Router) { r.Use(auth.RequireOrg(q)); rotateRoutes(r, h) })
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications", uid, oid, map[string]any{"name": "A"}))
	var app map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &app)
	base := "/api/applications/" + app["id"].(string) + "/endpoints"

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, base, uid, oid, map[string]any{"url": "https://ex.test/a"}))
	var ep map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ep)
	first := ep["secret"].(string)
	epID := ep["id"].(string)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, base+"/"+epID+"/rotate-secret", uid, oid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	var rot map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rot)
	got := rot["secret"].(string)
	if got == "" || got == first {
		t.Fatalf("rotate must return a new secret, got %q (was %q)", got, first)
	}
	// /secret now returns the NEW current secret
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, base+"/"+epID+"/secret", uid, oid, nil))
	var sec map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &sec)
	if sec["secret"].(string) != got {
		t.Fatalf("/secret should be the rotated secret")
	}
}

func TestReEnableResetsCounter(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	h := Handlers{Log: discardLog(), Queries: q, SecretGrace: 24 * time.Hour}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, sign(t)))
		r.Group(func(r chi.Router) { r.Use(auth.RequireOrg(q)); rotateRoutes(r, h) })
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications", uid, oid, map[string]any{"name": "A"}))
	var app map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &app)
	appID, _ := uuid.Parse(app["id"].(string))
	base := "/api/applications/" + app["id"].(string) + "/endpoints"

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, base, uid, oid, map[string]any{"url": "https://ex.test/a"}))
	var ep map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ep)
	epID, _ := uuid.Parse(ep["id"].(string))

	// Force auto-disabled state: threshold 1 → disabled + failures=1 + disabled_at set.
	if err := q.IncrEndpointFailures(context.Background(), store.IncrEndpointFailuresParams{
		ID: store.UUID(epID), Threshold: 1,
	}); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPatch, base+"/"+ep["id"].(string), uid, oid, map[string]any{"disabled": false}))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}

	row, err := q.GetEndpointForApp(context.Background(), store.GetEndpointForAppParams{
		ID: store.UUID(epID), AppID: store.UUID(appID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.Disabled || row.ConsecutiveFailures != 0 || row.DisabledAt.Valid {
		t.Fatalf("re-enable must reset: disabled=%v failures=%d disabled_at_valid=%v",
			row.Disabled, row.ConsecutiveFailures, row.DisabledAt.Valid)
	}
}

func TestRecoverReenqueuesDeadSince(t *testing.T) {
	q := store.New(testPool(t))
	rdb := testRedis(t)
	dq := dqueue.NewClient(rdb).WithPrefix("obrec-" + uuidNewShort())
	uid, oid := seedOrg(t, q)
	h := Handlers{Log: discardLog(), Queries: q, Queue: dq}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, sign(t)))
		r.Group(func(r chi.Router) { r.Use(auth.RequireOrg(q)); recoverRoutes(r, h) })
	})
	ctx := context.Background()

	app, _ := q.CreateApplication(ctx, store.CreateApplicationParams{OrgID: store.UUID(oid), Name: "A", Metadata: []byte(`{}`)})
	sec, _ := webhook.GenerateSecret()
	ep, _ := q.CreateEndpoint(ctx, store.CreateEndpointParams{AppID: app.ID, OrgID: store.UUID(oid), Url: "https://ex.test/a", Secret: sec})
	// 2 dead + 1 delivered for this endpoint
	for i := 0; i < 3; i++ {
		msg, _ := q.CreateMessage(ctx, store.CreateMessageParams{AppID: app.ID, OrgID: store.UUID(oid), EventType: "x", Payload: []byte(`{}`), PayloadHash: "h"})
		dels, _ := q.CreateMessageDeliveriesBatch(ctx, store.CreateMessageDeliveriesBatchParams{MessageID: msg.ID, OrgID: store.UUID(oid), EndpointIds: []pgtype.UUID{ep.ID}})
		if i < 2 {
			_ = q.MarkDeliveryDead(ctx, dels[0].ID)
		} else {
			_ = q.MarkDeliveryDelivered(ctx, dels[0].ID)
		}
	}
	path := "/api/applications/" + store.GoUUID(app.ID).String() + "/endpoints/" + store.GoUUID(ep.ID).String() + "/recover"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, path, uid, oid, map[string]any{"since": "2000-01-01T00:00:00Z"}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("recover: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["recovered"].(float64) != 2 {
		t.Fatalf("want 2 recovered, got %v", body["recovered"])
	}
	if n := drainMessageTasks(t, dq); n != 2 {
		t.Fatalf("want 2 tasks, got %d", n)
	}
}

func recoverRoutes(r chi.Router, h Handlers) {
	r.Route("/applications/{app_id}/endpoints/{id}", func(r chi.Router) { r.Post("/recover", h.RecoverEndpoint) })
}

func TestTestSendTargetsOnlyThatEndpoint(t *testing.T) {
	q := store.New(testPool(t))
	rdb := testRedis(t)
	dq := dqueue.NewClient(rdb).WithPrefix("obtest-" + uuidNewShort())
	uid, oid := seedOrg(t, q)
	h := Handlers{Log: discardLog(), Queries: q, Queue: dq}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, sign(t)))
		r.Group(func(r chi.Router) { r.Use(auth.RequireOrg(q)); testRoutes(r, h) })
	})
	ctx := context.Background()

	_, _ = q.CreateEventType(ctx, store.CreateEventTypeParams{OrgID: store.UUID(oid), Name: "ping"})
	app, _ := q.CreateApplication(ctx, store.CreateApplicationParams{OrgID: store.UUID(oid), Name: "A", Metadata: []byte(`{}`)})
	sec, _ := webhook.GenerateSecret()
	// endpoint whose filter EXCLUDES "ping" — test-send must still target it
	ep, _ := q.CreateEndpoint(ctx, store.CreateEndpointParams{AppID: app.ID, OrgID: store.UUID(oid), Url: "https://ex.test/a", Secret: sec, FilterEventTypes: []string{"other.type"}})

	path := "/api/applications/" + store.GoUUID(app.ID).String() + "/endpoints/" + store.GoUUID(ep.ID).String() + "/test"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, path, uid, oid, map[string]any{"event_type": "ping"}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("test-send: %d %s", rec.Code, rec.Body.String())
	}
	if n := drainMessageTasks(t, dq); n != 1 {
		t.Fatalf("want exactly 1 delivery to the target endpoint, got %d", n)
	}
	// unregistered type → 422
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, path, uid, oid, map[string]any{"event_type": "nope"}))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unregistered type: want 422 got %d", rec.Code)
	}
}

func testRoutes(r chi.Router, h Handlers) {
	r.Route("/applications/{app_id}/endpoints/{id}", func(r chi.Router) { r.Post("/test", h.TestEndpoint) })
}

func TestMessagesCursorPaginates(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	h := Handlers{Log: discardLog(), Queries: q}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, sign(t)))
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireOrg(q))
			r.Route("/applications/{app_id}/messages", func(r chi.Router) { r.Get("/", h.ListMessages) })
		})
	})
	ctx := context.Background()
	app, _ := q.CreateApplication(ctx, store.CreateApplicationParams{OrgID: store.UUID(oid), Name: "A", Metadata: []byte(`{}`)})
	total := pageSize + 5
	for i := 0; i < total; i++ {
		_, _ = q.CreateMessage(ctx, store.CreateMessageParams{AppID: app.ID, OrgID: store.UUID(oid), EventType: "x", Payload: []byte(`{}`), PayloadHash: "h"})
	}
	base := "/api/applications/" + store.GoUUID(app.ID).String() + "/messages"

	get := func(url string) (int, []any, any) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, url, uid, oid, nil))
		var body struct {
			Data       []any `json:"data"`
			NextCursor any   `json:"next_cursor"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Data, body.NextCursor
	}

	code, data, next := get(base)
	if code != 200 || len(data) != pageSize || next == nil {
		t.Fatalf("page1: code=%d len=%d next=%v", code, len(data), next)
	}
	code, data2, next2 := get(base + "?cursor=" + next.(string))
	if code != 200 || len(data2) != 5 || next2 != nil {
		t.Fatalf("page2: code=%d len=%d next=%v", code, len(data2), next2)
	}
	// disjoint: no page2 id may appear anywhere in page1 (catches boundary dup/skip)
	seen := map[string]bool{}
	for _, m := range data {
		seen[m.(map[string]any)["id"].(string)] = true
	}
	for _, m := range data2 {
		if seen[m.(map[string]any)["id"].(string)] {
			t.Fatalf("pages overlap: id %v on both pages", m.(map[string]any)["id"])
		}
	}
}

func TestReplayReenqueuesDelivery(t *testing.T) {
	q := store.New(testPool(t))
	rdb := testRedis(t)
	dq := dqueue.NewClient(rdb).WithPrefix("obrep-" + uuidNewShort())
	uid, oid := seedOrg(t, q)
	ctx := context.Background()

	// Fixtures via store: app + endpoint + message + one (dead) delivery.
	app, err := q.CreateApplication(ctx, store.CreateApplicationParams{OrgID: store.UUID(oid), Name: "A", Metadata: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := webhook.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	ep, err := q.CreateEndpoint(ctx, store.CreateEndpointParams{
		AppID: app.ID, OrgID: store.UUID(oid), Url: "https://ex.test/a", Secret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := q.CreateMessage(ctx, store.CreateMessageParams{
		AppID: app.ID, OrgID: store.UUID(oid), EventType: "invoice.paid",
		Payload: []byte(`{"x":1}`), PayloadHash: "deadbeef",
	})
	if err != nil {
		t.Fatal(err)
	}
	dels, err := q.CreateMessageDeliveriesBatch(ctx, store.CreateMessageDeliveriesBatchParams{
		MessageID: msg.ID, OrgID: store.UUID(oid), EndpointIds: []pgtype.UUID{ep.ID},
	})
	if err != nil || len(dels) != 1 {
		t.Fatalf("create deliveries: err=%v n=%d", err, len(dels))
	}
	if err := q.MarkDeliveryDead(ctx, dels[0].ID); err != nil {
		t.Fatal(err)
	}

	h := Handlers{Log: discardLog(), Queries: q, Queue: dq}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, sign(t)))
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireOrg(q))
			r.Post("/applications/{app_id}/messages/{id}/endpoints/{endpoint_id}/replay", h.ReplayDelivery)
		})
	})

	path := "/api/applications/" + store.GoUUID(app.ID).String() +
		"/messages/" + store.GoUUID(msg.ID).String() +
		"/endpoints/" + store.GoUUID(ep.ID).String() + "/replay"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, path, uid, oid, nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("replay: got %d body=%s", rec.Code, rec.Body.String())
	}
	// verify the delivery was actually reset (not a no-op): status back to 'queued', attempt_count 0
	got, gerr := q.GetDeliveryByMessageEndpoint(ctx, store.GetDeliveryByMessageEndpointParams{
		MessageID: msg.ID, EndpointID: ep.ID,
	})
	if gerr != nil {
		t.Fatalf("refetch delivery: %v", gerr)
	}
	if got.Status != "queued" {
		t.Fatalf("replay must reset status to queued, got %q", got.Status)
	}
	if got.AttemptCount != 0 {
		t.Fatalf("replay must reset attempt_count to 0, got %d", got.AttemptCount)
	}
	if got := drainMessageTasks(t, dq); got != 1 {
		t.Fatalf("replay must re-enqueue one delivery, got %d", got)
	}
}
