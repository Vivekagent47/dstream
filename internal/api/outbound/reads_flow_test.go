package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/store"
)

func (f *fx) mkAttempt(del pgtype.UUID, num int32, status *int32, headers []byte, errMsg *string) store.MessageDeliveryAttempt {
	f.t.Helper()
	a, err := f.q.CreateMessageDeliveryAttempt(context.Background(), store.CreateMessageDeliveryAttemptParams{
		DeliveryID: del, AttemptNum: num, ResponseStatus: status, ResponseHeaders: headers,
		ResponseBody: []byte("body-" + string(rune('0'+num))), ErrorMessage: errMsg,
	})
	if err != nil {
		f.t.Fatalf("seed attempt: %v", err)
	}
	return a
}

func TestListMessages_ViewFieldsAndBadCursorFallsBackToFirstPage(t *testing.T) {
	f := newFx(t)
	msg, err := f.q.CreateMessage(context.Background(), store.CreateMessageParams{
		AppID: f.app.ID, OrgID: f.app.OrgID, EventType: "inv.paid", Payload: []byte(`{"n":1}`), PayloadHash: "abc",
		EventID: ptr("evt-9"), Channels: []string{"acme"},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := appPath(f.app) + "/messages"
	for _, cur := range []string{"", "?cursor=%25%25%25", "?cursor=bm90LWEtY3Vyc29y", "?cursor=YmFkLXRpbWV8YmFkLXV1aWQ"} {
		rec := f.own(http.MethodGet, base+cur, nil)
		want(t, rec, http.StatusOK, "")
		var body struct {
			Data []map[string]any `json:"data"`
			Next any              `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Data) != 1 || body.Next != nil {
			t.Fatalf("cursor %q: want the single message and no next, got %s", cur, rec.Body.String())
		}
		m := body.Data[0]
		if m["id"] != uid36(msg.ID) || m["app_id"] != uid36(f.app.ID) || m["event_type"] != "inv.paid" ||
			m["payload_hash"] != "abc" || m["event_id"] != "evt-9" || m["created_at"] == nil {
			t.Fatalf("cursor %q: view %v", cur, m)
		}
		if ch, _ := m["channels"].([]any); len(ch) != 1 || ch[0] != "acme" {
			t.Fatalf("channels: %v", m["channels"])
		}
		if _, leaked := m["payload"]; leaked {
			t.Fatalf("the list view must not carry payloads")
		}
	}
	want(t, f.own(http.MethodGet, "/api/applications/not-a-uuid/messages", nil), http.StatusBadRequest, "invalid app id")
}

func ptr[T any](v T) *T { return &v }

func TestListMessages_DatabaseFailureIs500(t *testing.T) {
	f := newFx(t, withQueries(tracedQueries(t, failAll("from messages", "(created_at, id) <"))))
	want(t, f.own(http.MethodGet, appPath(f.app)+"/messages", nil), http.StatusInternalServerError, "list messages")
}

func TestGetMessage_ReturnsPayloadAndRefusesBadIDs(t *testing.T) {
	f := newFx(t)
	msg := f.mkMsg(f.app, "got.it")
	rec := f.own(http.MethodGet, appPath(f.app)+"/messages/"+uid36(msg.ID), nil)
	want(t, rec, http.StatusOK, "")
	b := obj(t, rec)
	if p, _ := b["payload"].(map[string]any); p["n"] != float64(1) || b["id"] != uid36(msg.ID) || b["event_type"] != "got.it" || b["event_id"] != nil {
		t.Fatalf("message view: %v", b)
	}
	want(t, f.own(http.MethodGet, appPath(f.app)+"/messages/not-a-uuid", nil), http.StatusBadRequest, "invalid message id")
	want(t, f.own(http.MethodGet, appPath(f.app)+"/messages/"+uuid.NewString(), nil), http.StatusNotFound, "message not found")
}

func TestMessageAttempts_ListedNewestFirstWithViews(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/at")
	msg := f.mkMsg(f.app, "x")
	del := f.mkDel(msg, ep)
	st200, st500 := int32(200), int32(503)
	emsg := "boom"
	a1 := f.mkAttempt(del, 1, &st500, []byte(`{"Retry-After":["5"]}`), &emsg)
	if _, err := f.pool.Exec(context.Background(), `UPDATE message_delivery_attempts SET attempted_at = now() - interval '1 minute' WHERE id=$1`, a1.ID); err != nil {
		t.Fatal(err)
	}
	a2 := f.mkAttempt(del, 2, &st200, nil, nil)

	for _, c := range []struct{ name, path string }{
		{"message", appPath(f.app) + "/messages/" + uid36(msg.ID) + "/attempts"},
		{"endpoint", epPath(f.app, ep) + "/attempts"},
	} {
		rec := f.own(http.MethodGet, c.path, nil)
		want(t, rec, http.StatusOK, "")
		got := list(t, rec)
		if len(got) != 2 || got[0]["id"] != uid36(a2.ID) || got[1]["id"] != uid36(a1.ID) {
			t.Fatalf("%s attempts must be newest first: %v", c.name, got)
		}
		if got[0]["response_status"] != float64(200) || got[0]["response_headers"] != nil || got[0]["response_body"] != "body-2" || got[0]["error_message"] != nil {
			t.Fatalf("%s attempt 2 view: %v", c.name, got[0])
		}
		h, _ := got[1]["response_headers"].(map[string]any)
		if got[1]["response_status"] != float64(503) || h["Retry-After"] == nil || got[1]["error_message"] != "boom" || got[1]["attempt_num"] != float64(1) || got[1]["delivery_id"] != uid36(del) {
			t.Fatalf("%s attempt 1 view: %v", c.name, got[1])
		}
	}
}

func TestMessageAttemptsAndDeliveries_Refusals(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/x")
	for _, suffix := range []string{"attempts", "deliveries"} {
		want(t, f.own(http.MethodGet, appPath(f.app)+"/messages/not-a-uuid/"+suffix, nil), http.StatusBadRequest, "invalid message id")
		want(t, f.own(http.MethodGet, appPath(f.app)+"/messages/"+uuid.NewString()+"/"+suffix, nil), http.StatusNotFound, "message not found")
		want(t, f.own(http.MethodGet, "/api/applications/not-a-uuid/messages/"+uuid.NewString()+"/"+suffix, nil), http.StatusBadRequest, "invalid app id")
	}
	want(t, f.own(http.MethodGet, appPath(f.app)+"/endpoints/not-a-uuid/attempts", nil), http.StatusBadRequest, "invalid endpoint id")
	want(t, f.own(http.MethodGet, appPath(f.app)+"/endpoints/"+uuid.NewString()+"/attempts", nil), http.StatusNotFound, "endpoint not found")
	if rec := f.own(http.MethodGet, epPath(f.app, ep)+"/attempts", nil); rec.Body.String() != "[]\n" && strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("no attempts must be an empty array, got %q", rec.Body.String())
	}
}

func TestMessageDeliveries_ShowTimestampsOnceAttempted(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/d")
	msg := f.mkMsg(f.app, "x")
	del := f.mkDel(msg, ep)
	path := appPath(f.app) + "/messages/" + uid36(msg.ID) + "/deliveries"

	row := list(t, f.own(http.MethodGet, path, nil))[0]
	if row["next_retry_at"] != nil || row["last_attempt_at"] != nil || row["status"] != "queued" || row["attempt_count"] != float64(0) {
		t.Fatalf("unattempted delivery: %v", row)
	}
	if err := f.q.MarkDeliveryInFlight(context.Background(), del); err != nil {
		t.Fatal(err)
	}
	if err := f.q.MarkDeliveryForRetry(context.Background(), store.MarkDeliveryForRetryParams{ID: del, NextRetryAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}}); err != nil {
		t.Fatal(err)
	}
	row = list(t, f.own(http.MethodGet, path, nil))[0]
	if row["next_retry_at"] == nil || row["last_attempt_at"] == nil || row["attempt_count"] != float64(1) || row["endpoint_id"] != uid36(ep.ID) || row["delivery_id"] != uid36(del) {
		t.Fatalf("attempted delivery: %v", row)
	}
}

func TestReads_DatabaseFailuresAre500(t *testing.T) {
	for _, c := range []struct {
		name, path, msg string
		markers         []string
	}{
		{"message attempts", "/messages/%m/attempts", "list attempts", []string{"from message_delivery_attempts a", "d.message_id = $1"}},
		{"endpoint attempts", "/endpoints/%e/attempts", "list attempts", []string{"from message_delivery_attempts a", "d.endpoint_id = $1"}},
		{"deliveries", "/messages/%m/deliveries", "list deliveries", []string{"from message_deliveries d", "join endpoints e"}},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t, withQueries(tracedQueries(t, failAll(c.markers...))))
			ep := f.mkEp(f.app, "https://ex.test/x")
			msg := f.mkMsg(f.app, "x")
			path := appPath(f.app) + strings.NewReplacer("%m", uid36(msg.ID), "%e", uid36(ep.ID)).Replace(c.path)
			want(t, f.own(http.MethodGet, path, nil), http.StatusInternalServerError, c.msg)
		})
	}
}
