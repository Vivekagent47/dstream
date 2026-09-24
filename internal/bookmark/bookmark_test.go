package bookmark

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// mockQ embeds store.Querier so only the two methods Reinject calls need bodies;
// any other call would panic (Reinject calls only these two).
type mockQ struct {
	store.Querier
	conns   []store.Connection
	created store.CreateEventsBatchParams
	events  []store.Event
}

func (m *mockQ) ListEnabledConnectionsBySource(ctx context.Context, sourceID pgtype.UUID) ([]store.Connection, error) {
	return m.conns, nil
}

func (m *mockQ) CreateEventsBatch(ctx context.Context, arg store.CreateEventsBatchParams) ([]store.Event, error) {
	m.created = arg
	return m.events, nil
}

type fakeEnq struct{ payloads []dqueue.Payload }

func (f *fakeEnq) Enqueue(_ context.Context, p dqueue.Payload) error {
	f.payloads = append(f.payloads, p)
	return nil
}

func TestReinject(t *testing.T) {
	// two enabled connections
	c1 := store.Connection{ID: store.UUID(uuid.New()), RetryStrategy: "exponential", RetryBaseMs: 1000, RetryCapMs: 60000, RetryJitterPct: 20}
	c2 := store.Connection{ID: store.UUID(uuid.New()), RetryStrategy: "fixed", RetryBaseMs: 500, RetryCapMs: 500, RetryJitterPct: 0}
	orgID, reqID, srcID := uuid.New(), uuid.New(), uuid.New()
	// events the batch insert "returns", one per connection, in REVERSED
	// order vs conns — RETURNING order is unspecified, so this catches a
	// buggy positional (rather than connection_id-keyed) retry-policy lookup.
	e1 := store.Event{ID: store.UUID(uuid.New()), OrgID: store.UUID(orgID), ConnectionID: c1.ID}
	e2 := store.Event{ID: store.UUID(uuid.New()), OrgID: store.UUID(orgID), ConnectionID: c2.ID}
	m := &mockQ{conns: []store.Connection{c1, c2}, events: []store.Event{e2, e1}}
	enq := &fakeEnq{}

	ids, err := Reinject(context.Background(), m, enq, reqID, srcID, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 event ids, got %d", len(ids))
	}
	if !m.created.IsTest {
		t.Error("CreateEventsBatch must set IsTest=true")
	}
	if store.GoUUID(m.created.RequestID) != reqID {
		t.Error("batch must reuse the original request_id")
	}
	if len(enq.payloads) != 2 {
		t.Fatalf("want 2 enqueues, got %d", len(enq.payloads))
	}
	for _, p := range enq.payloads {
		if p.Attempt != 0 {
			t.Error("enqueue Attempt must be 0")
		}
	}
	// retry policy must be threaded from the connection matching each
	// event's ConnectionID (not RETURNING position) — c1/c2 have distinct
	// policies so a positional bug fails this.
	connByEvent := map[uuid.UUID]store.Connection{
		store.GoUUID(e1.ID): c1,
		store.GoUUID(e2.ID): c2,
	}
	for _, p := range enq.payloads {
		want, ok := connByEvent[p.EventID]
		if !ok {
			t.Fatalf("enqueued unknown event id %s", p.EventID)
		}
		if p.RetryStrategy != want.RetryStrategy || p.RetryBaseMs != want.RetryBaseMs ||
			p.RetryCapMs != want.RetryCapMs || p.RetryJitterPct != want.RetryJitterPct {
			t.Errorf("retry policy mismatch for event %s: got %+v, want connection retry fields %+v", p.EventID, p, want)
		}
	}
}

func TestReinjectNoConnections(t *testing.T) {
	m := &mockQ{conns: nil}
	enq := &fakeEnq{}
	ids, err := Reinject(context.Background(), m, enq, uuid.New(), uuid.New(), uuid.New())
	if err != nil || len(ids) != 0 || len(enq.payloads) != 0 {
		t.Fatalf("no connections → no events/enqueues; got ids=%d enq=%d err=%v", len(ids), len(enq.payloads), err)
	}
}

func TestReplayTo(t *testing.T) {
	var gotMethod, gotCT, gotKeep, gotAuth, gotHops, gotConn, gotXFF string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotKeep = r.Header.Get("X-Keep")
		gotAuth = r.Header.Get("Authorization")
		gotHops = r.Header.Get("Dstream-Webhook-Hops")
		gotConn = r.Header.Get("Connection")
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	req := Request{
		Method: "POST", Body: []byte(`{"a":1}`), ContentType: "application/json",
		Headers: map[string][]string{
			"X-Keep":               {"v"},
			"Authorization":        {"[redacted]"},
			"Dstream-Webhook-Hops": {"1"},
			"Connection":           {"keep-alive"},
			"X-Forwarded-For":      {"1.2.3.4"},
		},
	}
	resp, err := ReplayTo(context.Background(), http.DefaultClient, req, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || string(resp.Body) != "ok" {
		t.Fatalf("resp = %+v", resp)
	}
	if gotMethod != "POST" || string(gotBody) != `{"a":1}` || gotCT != "application/json" || gotKeep != "v" {
		t.Fatalf("forwarded wrong: method=%s body=%s ct=%s keep=%s", gotMethod, gotBody, gotCT, gotKeep)
	}
	if gotAuth != "" {
		t.Error("redacted Authorization must NOT be forwarded")
	}
	if gotHops != "" {
		t.Error("Dstream-Webhook-Hops must NOT be forwarded")
	}
	if gotConn != "" {
		t.Error("Connection must NOT be forwarded")
	}
	if gotXFF != "" {
		t.Error("X-Forwarded-For must NOT be forwarded")
	}
}

func TestExport(t *testing.T) {
	req := Request{Method: "POST", Path: "/e/x", ContentType: "application/json",
		Headers: map[string][]string{"X-A": {"1"}}, Body: []byte(`{"k":"v"}`)}
	out, err := Export(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Method      string              `json:"method"`
		Path        string              `json:"path"`
		ContentType string              `json:"content_type"`
		Body        string              `json:"body_base64"`
		Headers     map[string][]string `json:"headers"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Method != "POST" || got.Path != "/e/x" || got.ContentType != "application/json" {
		t.Fatalf("export fields wrong: %+v", got)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != `{"k":"v"}` {
		t.Fatalf("body did not round-trip: %s", decoded)
	}
}
