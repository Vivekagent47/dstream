package outbound

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

func (f *fx) publish(app store.Application, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.own(http.MethodPost, appPath(app)+"/messages", body)
}

func TestPublish_SubscriptionMatchingDecidesTheFanOut(t *testing.T) {
	f := newFx(t)
	et, other := uniq("sub"), uniq("sub.other")
	f.mkEventType(f.oid, et, nil)
	withFilter := func(types ...string) func(*store.CreateEndpointParams) {
		return func(p *store.CreateEndpointParams) { p.FilterEventTypes = types }
	}
	// Documents CURRENT behaviour, not contract: a NULL or empty filter_event_types
	// means "receive every event type"; there is no way to subscribe to nothing.
	everything := f.mkEp(f.app, "https://ex.test/null")                        // NULL filter
	emptyList := f.mkEp(f.app, "https://ex.test/empty", withFilter())          // empty list
	exact := f.mkEp(f.app, "https://ex.test/exact", withFilter(other, et))     // lists this type
	elsewhere := f.mkEp(f.app, "https://ex.test/elsewhere", withFilter(other)) // lists only another
	disabled := f.mkEp(f.app, "https://ex.test/disabled", withFilter(et))      // matches, but off
	sibling := f.mkEp(f.app2, "https://ex.test/sibling")                       // another app, same org
	if _, err := f.pool.Exec(context.Background(), `UPDATE endpoints SET disabled=true WHERE id=$1`, disabled.ID); err != nil {
		t.Fatal(err)
	}

	rec := f.publish(f.app, map[string]any{"event_type": et, "payload": map[string]any{"a": 1}})
	want(t, rec, http.StatusAccepted, "")
	b := obj(t, rec)
	msgID := uuid.MustParse(b["message_id"].(string))
	if b["idempotent_replay"] != false {
		t.Fatalf("first publish is not a replay: %v", b)
	}
	dels, err := f.q.ListDeliveriesForMessage(context.Background(), store.UUID(msgID))
	if err != nil {
		t.Fatal(err)
	}
	var got, wantIDs []string
	for _, d := range dels {
		got = append(got, uid36(d.EndpointID))
		if d.Status != "queued" {
			t.Fatalf("new delivery must be queued, got %s", d.Status)
		}
	}
	for _, e := range []store.Endpoint{everything, emptyList, exact} {
		wantIDs = append(wantIDs, uid36(e.ID))
	}
	sort.Strings(got)
	sort.Strings(wantIDs)
	if !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("deliveries went to %v, want exactly %v (not elsewhere=%s disabled=%s sibling=%s)",
			got, wantIDs, uid36(elsewhere.ID), uid36(disabled.ID), uid36(sibling.ID))
	}
	if n := drainMessageTasks(t, f.dq); n != 3 {
		t.Fatalf("one task per delivery: got %d want 3", n)
	}
	// The stored message is what was published.
	m, _ := f.q.GetMessageForApp(context.Background(), store.GetMessageForAppParams{ID: store.UUID(msgID), AppID: f.app.ID})
	if string(m.Payload) != `{"a":1}` || m.EventType != et || m.OrgID != f.app.OrgID {
		t.Fatalf("stored message: %+v", m)
	}
}

func TestPublish_NoSubscriberStillStoresTheMessage(t *testing.T) {
	f := newFx(t)
	et := uniq("lonely")
	f.mkEventType(f.oid, et, nil)
	rec := f.publish(f.app, map[string]any{"event_type": et, "payload": "str", "event_id": "e-1"})
	want(t, rec, http.StatusAccepted, "")
	if obj(t, rec)["event_id"] != "e-1" || f.msgCount(f.app) != 1 {
		t.Fatalf("message must be stored with its event id: %v", rec.Body.String())
	}
	if n := drainMessageTasks(t, f.dq); n != 0 {
		t.Fatalf("no endpoint, no task: got %d", n)
	}
}

func TestPublish_Refusals(t *testing.T) {
	f := newFx(t)
	et, archived, strict := uniq("ok"), uniq("arch"), uniq("strict")
	f.mkEventType(f.oid, et, nil)
	f.mkEventType(f.oid, archived, nil)
	f.mkEventType(f.oid, strict, []byte(`{"type":"object","required":["id"]}`))
	if _, err := f.pool.Exec(context.Background(), `UPDATE event_types SET archived=true WHERE org_id=$1 AND name=$2`, f.oid, archived); err != nil {
		t.Fatal(err)
	}
	// An invalid schema that predates validation: it can only be stored directly.
	broken := uniq("broken")
	f.mkEventType(f.oid, broken, []byte(`{"type":"bogus"}`))
	base := appPath(f.app) + "/messages"
	for _, c := range []struct {
		name string
		body any
		code int
		msg  string
	}{
		{"no body", nil, 400, "invalid json"},
		{"no event_type", map[string]any{"payload": 1}, 400, "event_type required"},
		{"no payload", map[string]any{"event_type": et}, 400, "payload must be a valid json value"},
		{"unknown type", map[string]any{"event_type": uniq("nope"), "payload": 1}, 422, "unknown or archived event_type"},
		{"archived type", map[string]any{"event_type": archived, "payload": 1}, 422, "unknown or archived event_type"},
		{"bad channel", map[string]any{"event_type": et, "payload": 1, "channels": []string{"x y"}}, 422, `invalid channel "x y"`},
		{"schema mismatch", map[string]any{"event_type": strict, "payload": map[string]any{"nope": 1}}, 422, ""},
		{"stored schema invalid", map[string]any{"event_type": broken, "payload": 1}, 422, ""},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rec := f.own(http.MethodPost, base, c.body)
			want(t, rec, c.code, c.msg)
			if c.msg == "" {
				prefix := "payload does not match schema: "
				if c.name == "stored schema invalid" {
					prefix = "stored schema invalid: "
				}
				if e := obj(t, rec)["error"].(string); !strings.HasPrefix(e, prefix) {
					t.Fatalf("error %q lacks prefix %q", e, prefix)
				}
			}
			if n := f.msgCount(f.app); n != 0 {
				t.Fatalf("refused publish stored %d messages", n)
			}
		})
	}
	// A conforming payload to the strict type is accepted.
	want(t, f.own(http.MethodPost, base, map[string]any{"event_type": strict, "payload": map[string]any{"id": 1}}), http.StatusAccepted, "")
	want(t, f.own(http.MethodPost, "/api/applications/not-a-uuid/messages", map[string]any{}), http.StatusBadRequest, "invalid app id")
}

func TestPublish_BodyOverFiveMiBIsRefused(t *testing.T) {
	f := newFx(t)
	et := uniq("big")
	f.mkEventType(f.oid, et, nil)
	big := `{"event_type":"` + et + `","payload":"` + strings.Repeat("x", 5<<20) + `"}`
	rec := httpDo(f, http.MethodPost, appPath(f.app)+"/messages", strings.NewReader(big))
	want(t, rec, http.StatusBadRequest, "body too large or unreadable")
	if f.msgCount(f.app) != 0 {
		t.Fatalf("oversized publish stored a message")
	}
}

func TestPublish_IdempotentReplayReturnsTheFirstMessage(t *testing.T) {
	f := newFx(t)
	et := uniq("idem")
	f.mkEventType(f.oid, et, nil)
	f.mkEp(f.app, "https://ex.test/idem")
	body := map[string]any{"event_type": et, "payload": 1, "event_id": uniq("evt")}
	first := obj(t, f.publish(f.app, body))
	rec := f.publish(f.app, body)
	want(t, rec, http.StatusOK, "")
	second := obj(t, rec)
	if second["message_id"] != first["message_id"] || second["idempotent_replay"] != true || second["event_id"] != body["event_id"] {
		t.Fatalf("replay must echo the first message: first=%v second=%v", first, second)
	}
	if f.msgCount(f.app) != 1 {
		t.Fatalf("replay stored a second message")
	}
	// The same event_id in the sibling app is a different message.
	want(t, f.publish(f.app2, body), http.StatusAccepted, "")
}

func TestPublish_DatabaseFailures(t *testing.T) {
	type tc struct {
		name, marker string
		msgs         int // messages expected in the store afterwards
	}
	for _, c := range []tc{
		{"insert message", "insert into messages", 0},
		{"match endpoints", "cardinality(filter_event_types)", 1},
		{"insert deliveries", "insert into message_deliveries", 1},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t, withQueries(tracedQueries(t, failAll(c.marker))))
			et := uniq("dbfail")
			f.mkEventType(f.oid, et, nil)
			f.mkEp(f.app, "https://ex.test/x")
			rec := f.publish(f.app, map[string]any{"event_type": et, "payload": 1})
			msg := map[string]string{"insert message": "create message", "match endpoints": "fan-out", "insert deliveries": "fan-out"}[c.name]
			want(t, rec, http.StatusInternalServerError, msg)
			if got := f.msgCount(f.app); got != c.msgs {
				t.Fatalf("stored messages: got %d want %d", got, c.msgs)
			}
			if n := drainMessageTasks(t, f.dq); n != 0 {
				t.Fatalf("failed fan-out enqueued %d tasks", n)
			}
		})
	}
	t.Run("idempotency lookup", func(t *testing.T) {
		f := newFx(t, withQueries(tracedQueries(t, failAll("from messages where app_id = $1 and event_id = $2"))))
		et := uniq("idemfail")
		f.mkEventType(f.oid, et, nil)
		body := map[string]any{"event_type": et, "payload": 1, "event_id": "same"}
		want(t, f.publish(f.app, body), http.StatusAccepted, "")
		want(t, f.publish(f.app, body), http.StatusInternalServerError, "idempotency lookup")
		if f.msgCount(f.app) != 1 {
			t.Fatalf("collision stored a second message")
		}
	})
}

// An enqueue failure after the rows are committed is not an error to the caller:
// the delivery stays 'queued' for the reaper to pick up.
func TestPublish_EnqueueFailureLeavesTheDeliveryQueued(t *testing.T) {
	f := newFx(t)
	f.h.Queue = closedQueue(t)
	f.build()
	et := uniq("enq")
	f.mkEventType(f.oid, et, nil)
	ep := f.mkEp(f.app, "https://ex.test/x")
	rec := f.publish(f.app, map[string]any{"event_type": et, "payload": 1})
	want(t, rec, http.StatusAccepted, "")
	msg, _ := f.q.GetMessageForApp(context.Background(), store.GetMessageForAppParams{ID: store.UUID(uuid.MustParse(obj(t, rec)["message_id"].(string))), AppID: f.app.ID})
	dels := f.deliveries(msg)
	if len(dels) != 1 || dels[0].EndpointID != ep.ID || dels[0].Status != "queued" {
		t.Fatalf("delivery must be stored queued: %+v", dels)
	}
	if !strings.Contains(f.logs.String(), "enqueue delivery") {
		t.Fatalf("enqueue failure must be logged")
	}
}

// ---- replay ----

func replayPath(app store.Application, msg store.Message, ep store.Endpoint) string {
	return appPath(app) + "/messages/" + uid36(msg.ID) + "/endpoints/" + uid36(ep.ID) + "/replay"
}

func TestReplay_Refusals(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/r")
	msg := f.mkMsg(f.app, "x")
	m, e := uid36(msg.ID), uid36(ep.ID)
	want(t, f.own(http.MethodPost, "/api/applications/not-a-uuid/messages/"+m+"/endpoints/"+e+"/replay", nil), http.StatusBadRequest, "invalid app id")
	want(t, f.own(http.MethodPost, appPath(f.app)+"/messages/not-a-uuid/endpoints/"+e+"/replay", nil), http.StatusBadRequest, "invalid message id")
	want(t, f.own(http.MethodPost, appPath(f.app)+"/messages/"+m+"/endpoints/not-a-uuid/replay", nil), http.StatusBadRequest, "invalid endpoint id")
	want(t, f.own(http.MethodPost, appPath(f.app)+"/messages/"+uuid.NewString()+"/endpoints/"+e+"/replay", nil), http.StatusNotFound, "message not found")
	want(t, f.own(http.MethodPost, appPath(f.app)+"/messages/"+m+"/endpoints/"+uuid.NewString()+"/replay", nil), http.StatusNotFound, "endpoint not found")
	if len(f.deliveries(msg)) != 0 {
		t.Fatalf("refused replays created a delivery")
	}
	if n := drainMessageTasks(t, f.dq); n != 0 {
		t.Fatalf("refused replays enqueued %d tasks", n)
	}
}

// A message that never reached this endpoint (it subscribed afterwards) can
// still be replayed to it: the delivery row is created.
func TestReplay_CreatesTheDeliveryWhenThereWasNone(t *testing.T) {
	f := newFx(t)
	msg := f.mkMsg(f.app, "x")
	ep := f.mkEp(f.app, "https://ex.test/late")
	rec := f.own(http.MethodPost, replayPath(f.app, msg, ep), nil)
	want(t, rec, http.StatusAccepted, "")
	dels := f.deliveries(msg)
	if len(dels) != 1 || dels[0].EndpointID != ep.ID || dels[0].Status != "queued" || uid36(dels[0].DeliveryID) != obj(t, rec)["delivery_id"] {
		t.Fatalf("replay must create one queued delivery and return its id: %+v body=%s", dels, rec.Body.String())
	}
	if n := drainMessageTasks(t, f.dq); n != 1 {
		t.Fatalf("want 1 task, got %d", n)
	}
	var audits int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE org_id=$1 AND action='message.replay' AND target_id=$2`, f.oid, dels[0].DeliveryID).Scan(&audits)
	if audits != 1 {
		t.Fatalf("replay must be audited once, got %d", audits)
	}
}

func TestReplay_DatabaseAndQueueFailures(t *testing.T) {
	type tc struct {
		name   string
		tr     func() tracerFn
		seed   bool // pre-existing dead delivery
		useBad bool // closed queue
		msg    string
	}
	for _, c := range []tc{
		{"load delivery", func() tracerFn { return failAll("from message_deliveries where message_id = $1 and endpoint_id = $2") }, true, false, "load delivery"},
		{"reset delivery", func() tracerFn { return failAll("set status = 'queued', attempt_count = 0") }, true, false, "reset delivery"},
		{"create delivery", func() tracerFn { return failAll("insert into message_deliveries") }, false, false, "create delivery"},
		{"enqueue", func() tracerFn { return func(ctx context.Context, _ string) context.Context { return ctx } }, true, true, "enqueue"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t, withQueries(tracedQueries(t, c.tr())))
			if c.useBad {
				f.h.Queue = closedQueue(t)
				f.build()
			}
			msg := f.mkMsg(f.app, "x")
			ep := f.mkEp(f.app, "https://ex.test/x")
			if c.seed {
				id := f.mkDel(msg, ep)
				if err := f.q.MarkDeliveryDead(context.Background(), id); err != nil {
					t.Fatal(err)
				}
				if _, err := f.pool.Exec(context.Background(), `UPDATE message_deliveries SET attempt_count=4 WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			want(t, f.own(http.MethodPost, replayPath(f.app, msg, ep), nil), http.StatusInternalServerError, c.msg)
			dels := f.deliveries(msg)
			switch {
			case c.name == "create delivery":
				if len(dels) != 0 {
					t.Fatalf("failed create left a delivery")
				}
			case c.name == "enqueue":
				if len(dels) != 1 || dels[0].Status != "queued" || dels[0].AttemptCount != 0 {
					t.Fatalf("enqueue failure: the reset is already committed, got %+v", dels)
				}
			default:
				if len(dels) != 1 || dels[0].Status != "dead" || dels[0].AttemptCount != 4 {
					t.Fatalf("failed %s must leave the dead delivery as it was, got %+v", c.name, dels)
				}
			}
		})
	}
}

// Two replays race to create the same (message, endpoint) delivery: the loser's
// INSERT hits ON CONFLICT DO NOTHING and returns no row, so it must load the
// winner's row, reset it and re-enqueue that one. The tracer plays the winner by
// inserting the row, as a dead delivery, just before the handler's own INSERT.
func raceInserting(f *fx, msg store.Message, ep store.Endpoint, failAfter tracerFn) tracerFn {
	done := false
	return func(ctx context.Context, sql string) context.Context {
		if !done && strings.Contains(sql, "insert into message_deliveries") {
			done = true
			if _, err := f.pool.Exec(context.Background(),
				`INSERT INTO message_deliveries (message_id, endpoint_id, org_id, status, attempt_count) VALUES ($1,$2,$3,'dead',6)`,
				msg.ID, ep.ID, msg.OrgID); err != nil {
				f.t.Errorf("simulate the winning replay: %v", err)
			}
			return ctx
		}
		if failAfter != nil {
			return failAfter(ctx, sql)
		}
		return ctx
	}
}

func raceFx(t *testing.T, failAfter func() tracerFn) (*fx, store.Message, store.Endpoint) {
	t.Helper()
	// The fixture needs the real pool before the traced one can reference it.
	real := newFx(t)
	msg := real.mkMsg(real.app, "x")
	ep := real.mkEp(real.app, "https://ex.test/race")
	var fa tracerFn
	if failAfter != nil {
		fa = failAfter()
	}
	real.h.Queries = tracedQueries(t, raceInserting(real, msg, ep, fa))
	real.build()
	return real, msg, ep
}

func TestReplay_LostCreateRaceResetsTheWinnersRow(t *testing.T) {
	f, msg, ep := raceFx(t, nil)
	rec := f.own(http.MethodPost, replayPath(f.app, msg, ep), nil)
	want(t, rec, http.StatusAccepted, "")
	dels := f.deliveries(msg)
	if len(dels) != 1 || dels[0].Status != "queued" || dels[0].AttemptCount != 0 || uid36(dels[0].DeliveryID) != obj(t, rec)["delivery_id"] {
		t.Fatalf("loser must reset and return the winner's row: %+v body=%s", dels, rec.Body.String())
	}
	if n := drainMessageTasks(t, f.dq); n != 1 {
		t.Fatalf("want 1 task, got %d", n)
	}
}

func TestReplay_LostCreateRaceFailures(t *testing.T) {
	// Second lookup of the (message, endpoint) row fails.
	f, msg, ep := raceFx(t, func() tracerFn {
		return failNth("from message_deliveries where message_id = $1 and endpoint_id = $2", 2)
	})
	want(t, f.own(http.MethodPost, replayPath(f.app, msg, ep), nil), http.StatusInternalServerError, "load delivery")
	if d := f.deliveries(msg); len(d) != 1 || d[0].Status != "dead" || d[0].AttemptCount != 6 {
		t.Fatalf("winner's row must be untouched: %+v", d)
	}
	// The reset of the winner's row fails.
	g, msg2, ep2 := raceFx(t, func() tracerFn { return failAll("set status = 'queued', attempt_count = 0") })
	want(t, g.own(http.MethodPost, replayPath(g.app, msg2, ep2), nil), http.StatusInternalServerError, "reset delivery")
	if d := g.deliveries(msg2); len(d) != 1 || d[0].Status != "dead" || d[0].AttemptCount != 6 {
		t.Fatalf("winner's row must be untouched: %+v", d)
	}
}

func TestReplay_ExpungedPayloadIs422AndKeepsTheDeliveryDead(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/exp")
	msg := f.mkMsg(f.app, "x")
	id := f.mkDel(msg, ep)
	_ = f.q.MarkDeliveryDead(context.Background(), id)
	if _, err := f.pool.Exec(context.Background(), `UPDATE messages SET payload = NULL WHERE id=$1`, msg.ID); err != nil {
		t.Fatal(err)
	}
	want(t, f.own(http.MethodPost, replayPath(f.app, msg, ep), nil), http.StatusUnprocessableEntity,
		"message payload has been expunged and can no longer be delivered")
	if d := f.deliveries(msg); len(d) != 1 || d[0].Status != "dead" {
		t.Fatalf("expunged replay must not reset the delivery: %+v", d)
	}
	if n := drainMessageTasks(t, f.dq); n != 0 {
		t.Fatalf("expunged replay enqueued %d tasks", n)
	}
}

// ---- test-send ----

func TestTestSend_Refusals(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/t")
	ok, archived, strict := uniq("t.ok"), uniq("t.arch"), uniq("t.strict")
	f.mkEventType(f.oid, ok, nil)
	f.mkEventType(f.oid, archived, nil)
	f.mkEventType(f.oid, strict, []byte(`{"type":"object","required":["id"]}`))
	_, _ = f.pool.Exec(context.Background(), `UPDATE event_types SET archived=true WHERE org_id=$1 AND name=$2`, f.oid, archived)
	path := epPath(f.app, ep) + "/test"
	want(t, f.own(http.MethodPost, appPath(f.app)+"/endpoints/not-a-uuid/test", map[string]any{"event_type": ok}), http.StatusBadRequest, "invalid endpoint id")
	want(t, f.own(http.MethodPost, path, nil), http.StatusBadRequest, "invalid json")
	want(t, f.own(http.MethodPost, path, map[string]any{}), http.StatusBadRequest, "event_type required")
	want(t, f.own(http.MethodPost, path, map[string]any{"event_type": archived}), http.StatusUnprocessableEntity, "unknown or archived event_type")
	rec := f.own(http.MethodPost, path, map[string]any{"event_type": strict, "payload": map[string]any{"x": 1}})
	want(t, rec, http.StatusUnprocessableEntity, "")
	if !strings.HasPrefix(obj(t, rec)["error"].(string), "payload does not match schema: ") {
		t.Fatalf("schema error: %s", rec.Body.String())
	}
	if f.msgCount(f.app) != 0 {
		t.Fatalf("refused test-sends stored messages")
	}
}

func TestTestSend_StoresACompactedMessageAndOneDelivery(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/t")
	et := uniq("t.store")
	f.mkEventType(f.oid, et, nil)
	other := f.mkEp(f.app, "https://ex.test/not-targeted")
	rec := f.own(http.MethodPost, epPath(f.app, ep)+"/test", map[string]any{"event_type": et, "payload": map[string]any{"a": []int{1, 2}}})
	want(t, rec, http.StatusAccepted, "")
	msg, err := f.q.GetMessageForApp(context.Background(), store.GetMessageForAppParams{ID: store.UUID(uuid.MustParse(obj(t, rec)["message_id"].(string))), AppID: f.app.ID})
	if err != nil || string(msg.Payload) != `{"a":[1,2]}` || msg.EventType != et || msg.EventID != nil {
		t.Fatalf("stored test message: err=%v %+v", err, msg)
	}
	if d := f.deliveries(msg); len(d) != 1 || d[0].EndpointID != ep.ID || d[0].EndpointID == other.ID {
		t.Fatalf("test-send must target only the endpoint: %+v", d)
	}
	// No payload at all sends {}.
	rec = f.own(http.MethodPost, epPath(f.app, ep)+"/test", map[string]any{"event_type": et})
	want(t, rec, http.StatusAccepted, "")
	msg, _ = f.q.GetMessageForApp(context.Background(), store.GetMessageForAppParams{ID: store.UUID(uuid.MustParse(obj(t, rec)["message_id"].(string))), AppID: f.app.ID})
	if string(msg.Payload) != `{}` {
		t.Fatalf("default payload: %s", msg.Payload)
	}
	drainMessageTasks(t, f.dq)
}

func TestTestSend_DatabaseAndQueueFailures(t *testing.T) {
	for _, c := range []struct {
		name, marker, msg string
		msgs              int
		closed            bool
	}{
		{"create message", "insert into messages", "create message", 0, false},
		{"create delivery", "insert into message_deliveries", "create delivery", 1, false},
		{"enqueue", "never matches", "enqueue", 1, true},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t, withQueries(tracedQueries(t, failAll(c.marker))))
			if c.closed {
				f.h.Queue = closedQueue(t)
				f.build()
			}
			ep := f.mkEp(f.app, "https://ex.test/t")
			et := uniq("t.fail")
			f.mkEventType(f.oid, et, nil)
			want(t, f.own(http.MethodPost, epPath(f.app, ep)+"/test", map[string]any{"event_type": et}), http.StatusInternalServerError, c.msg)
			if got := f.msgCount(f.app); got != c.msgs {
				t.Fatalf("stored messages: got %d want %d", got, c.msgs)
			}
		})
	}
}

// ---- recover ----

func TestRecover_Refusals(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/rr")
	msg := f.mkMsg(f.app, "x")
	id := f.mkDel(msg, ep)
	_ = f.q.MarkDeliveryDead(context.Background(), id)
	path := epPath(f.app, ep) + "/recover"
	want(t, f.own(http.MethodPost, path, nil), http.StatusBadRequest, "invalid json")
	want(t, f.own(http.MethodPost, path, map[string]any{"since": "yesterday"}), http.StatusBadRequest, "since must be RFC3339")
	want(t, f.own(http.MethodPost, path, map[string]any{}), http.StatusBadRequest, "since must be RFC3339")
	want(t, f.own(http.MethodPost, appPath(f.app)+"/endpoints/not-a-uuid/recover", map[string]any{"since": "2000-01-01T00:00:00Z"}), http.StatusBadRequest, "invalid endpoint id")
	if d := f.deliveries(msg); d[0].Status != "dead" {
		t.Fatalf("refused recover touched the delivery: %+v", d)
	}
}

func TestRecover_PartialFailuresAreSkippedNotFatal(t *testing.T) {
	since := map[string]any{"since": "2000-01-01T00:00:00Z"}
	t.Run("list fails", func(t *testing.T) {
		f := newFx(t, withQueries(tracedQueries(t, failAll("status = 'dead' and created_at >="))))
		ep := f.mkEp(f.app, "https://ex.test/x")
		want(t, f.own(http.MethodPost, epPath(f.app, ep)+"/recover", since), http.StatusInternalServerError, "list dead")
	})
	t.Run("reset fails", func(t *testing.T) {
		f := newFx(t, withQueries(tracedQueries(t, failAll("set status = 'queued', attempt_count = 0"))))
		ep := f.mkEp(f.app, "https://ex.test/x")
		msg := f.mkMsg(f.app, "x")
		_ = f.q.MarkDeliveryDead(context.Background(), f.mkDel(msg, ep))
		rec := f.own(http.MethodPost, epPath(f.app, ep)+"/recover", since)
		want(t, rec, http.StatusAccepted, "")
		if obj(t, rec)["recovered"] != float64(0) || f.deliveries(msg)[0].Status != "dead" {
			t.Fatalf("a failed reset must not count or change the row: %s", rec.Body.String())
		}
		if !strings.Contains(f.logs.String(), "recover reset") {
			t.Fatalf("reset failure must be logged")
		}
	})
	t.Run("enqueue fails", func(t *testing.T) {
		f := newFx(t)
		f.h.Queue = closedQueue(t)
		f.build()
		ep := f.mkEp(f.app, "https://ex.test/x")
		msg := f.mkMsg(f.app, "x")
		_ = f.q.MarkDeliveryDead(context.Background(), f.mkDel(msg, ep))
		rec := f.own(http.MethodPost, epPath(f.app, ep)+"/recover", since)
		want(t, rec, http.StatusAccepted, "")
		if obj(t, rec)["recovered"] != float64(0) || !strings.Contains(f.logs.String(), "recover enqueue") {
			t.Fatalf("a failed enqueue must not count and must be logged: %s", rec.Body.String())
		}
	})
}

func TestRecover_TruncatesAtTheBatchCap(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	ep := f.mkEp(f.app, "https://ex.test/cap")
	if _, err := f.pool.Exec(ctx, `
		WITH m AS (
		  INSERT INTO messages (app_id, org_id, event_type, payload, payload_hash)
		  SELECT $1, $2, 'cap', convert_to('{}','UTF8'), 'h' FROM generate_series(1, $4::int) RETURNING id)
		INSERT INTO message_deliveries (message_id, endpoint_id, org_id, status)
		SELECT id, $3, $2, 'dead' FROM m`, f.app.ID, f.app.OrgID, ep.ID, recoverMaxBatch+5); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := f.own(http.MethodPost, epPath(f.app, ep)+"/recover", map[string]any{"since": "2000-01-01T00:00:00Z"})
	want(t, rec, http.StatusAccepted, "")
	b := obj(t, rec)
	if b["recovered"] != float64(recoverMaxBatch) || b["truncated"] != true {
		t.Fatalf("want recovered=%d truncated=true, got %v", recoverMaxBatch, b)
	}
	var left int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM message_deliveries WHERE endpoint_id=$1 AND status='dead'`, ep.ID).Scan(&left)
	if left != 5 {
		t.Fatalf("the 5 beyond the cap must stay dead, got %d", left)
	}
	if n := drainMessageTasks(t, f.dq); n != recoverMaxBatch {
		t.Fatalf("want %d tasks, got %d", recoverMaxBatch, n)
	}
	if !strings.Contains(f.logs.String(), "recover truncated at cap") {
		t.Fatalf("truncation must be logged")
	}
}
