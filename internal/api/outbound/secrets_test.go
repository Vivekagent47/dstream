package outbound

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/webhook"
)

func TestEndpointSecret_OnlyAdminsOfTheOwningAppRead(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/s")
	member := seedMember(t, f.q, f.oid)
	path := epPath(f.app, ep) + "/secret"

	rec := f.own(http.MethodGet, path, nil)
	want(t, rec, http.StatusOK, "")
	if got := obj(t, rec)["secret"]; got != ep.Secret {
		t.Fatalf("owner must read the stored secret, got %v", got)
	}

	rec = f.as(member, f.oid, http.MethodGet, path, nil)
	want(t, rec, http.StatusForbidden, "")
	if strings.Contains(rec.Body.String(), ep.Secret) {
		t.Fatalf("403 body leaked the secret: %s", rec.Body.String())
	}
	bUID, bOrg := seedOrg(t, f.q)
	rec = f.as(bUID, bOrg, http.MethodGet, path, nil)
	want(t, rec, http.StatusNotFound, "application not found")
	if strings.Contains(rec.Body.String(), ep.Secret) {
		t.Fatalf("cross-org body leaked the secret")
	}

	want(t, f.own(http.MethodGet, appPath(f.app)+"/endpoints/not-a-uuid/secret", nil), http.StatusBadRequest, "invalid endpoint id")
	want(t, f.own(http.MethodGet, appPath(f.app)+"/endpoints/"+uuid.NewString()+"/secret", nil), http.StatusNotFound, "endpoint not found")
	want(t, f.own(http.MethodGet, "/api/applications/not-a-uuid/endpoints/"+uid36(ep.ID)+"/secret", nil), http.StatusBadRequest, "invalid app id")
}

// The secret is shown once, at creation. No other view of the endpoint (get,
// list, patch) carries it, and rotation neither logs nor audits it.
func TestEndpointSecret_NeverEchoedOutsideCreateAndRotate(t *testing.T) {
	f := newFx(t)
	rec := f.own(http.MethodPost, appPath(f.app)+"/endpoints", map[string]any{"url": "https://ex.test/once"})
	want(t, rec, http.StatusCreated, "")
	created := obj(t, rec)
	s0, id := created["secret"].(string), created["id"].(string)
	if !webhook.ValidSecret(s0) {
		t.Fatalf("created secret is not a valid signing key: %q", s0)
	}
	if stored := f.epRow(f.app, store.UUID(uuid.MustParse(id))); stored.Secret != s0 {
		t.Fatalf("stored secret %q != returned %q", stored.Secret, s0)
	}

	rec = f.own(http.MethodPost, appPath(f.app)+"/endpoints/"+id+"/rotate-secret", nil)
	want(t, rec, http.StatusOK, "")
	s1 := obj(t, rec)["secret"].(string)

	for _, c := range []struct {
		name, method, path string
		body               any
	}{
		{"get", "GET", appPath(f.app) + "/endpoints/" + id, nil},
		{"list", "GET", appPath(f.app) + "/endpoints", nil},
		{"patch", "PATCH", appPath(f.app) + "/endpoints/" + id, map[string]any{"description": "d"}},
	} {
		rec := f.own(c.method, c.path, c.body)
		want(t, rec, http.StatusOK, "")
		for _, s := range []string{s0, s1} {
			if strings.Contains(rec.Body.String(), s) {
				t.Fatalf("%s view leaked a secret: %s", c.name, rec.Body.String())
			}
		}
	}
	if out := f.logs.String(); strings.Contains(out, s0) || strings.Contains(out, s1) {
		t.Fatalf("handler log contains a secret: %s", out)
	}
	var meta string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT string_agg(metadata::text, ' ') FROM audit_logs WHERE org_id=$1 AND target_id=$2`,
		f.oid, uuid.MustParse(id)).Scan(&meta); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(meta, "https://ex.test/once") || strings.Contains(meta, s0) || strings.Contains(meta, s1) {
		t.Fatalf("audit metadata must record the action but never a secret: %s", meta)
	}
	var n int
	_ = f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE org_id=$1 AND action='endpoint.rotate_secret' AND target_id=$2`,
		f.oid, uuid.MustParse(id)).Scan(&n)
	if n != 1 {
		t.Fatalf("rotation must be audited once, got %d", n)
	}
}

// publishAndDeliver publishes through the real API and runs the one queued delivery
// through the real webhook handler against srv, returning what the receiver saw.
type seen struct {
	id, ts, sig string
	body        []byte
}

func (f *fx) publishAndDeliver(h webhook.Handler, etName string, app store.Application) {
	f.t.Helper()
	rec := f.own(http.MethodPost, appPath(app)+"/messages", map[string]any{"event_type": etName, "payload": map[string]any{"k": uuidNewShort()}})
	want(f.t, rec, http.StatusAccepted, "")
	raw, p, ok, err := f.dq.FairPick(context.Background(), 10000)
	if err != nil || !ok {
		f.t.Fatalf("fairpick ok=%v err=%v", ok, err)
	}
	if err := h.Process(context.Background(), p, raw, f.dq); err != nil {
		f.t.Fatalf("process: %v", err)
	}
}

func receiver(t *testing.T, hit *seen) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hit.id, hit.ts, hit.sig = req.Header.Get("webhook-id"), req.Header.Get("webhook-timestamp"), req.Header.Get("webhook-signature")
		hit.body, _ = io.ReadAll(req.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func sigFor(t *testing.T, secret string, hit seen) string {
	t.Helper()
	ts, _ := strconv.ParseInt(hit.ts, 10, 64)
	s, err := webhook.Sign(secret, hit.id, ts, hit.body)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// rotatedFx is an endpoint (pointing at a receiver) rotated once under grace.
func rotatedFx(t *testing.T, grace time.Duration) (f *fx, ep store.Endpoint, s0, s1, etName string, wh webhook.Handler, hit *seen) {
	t.Helper()
	f = newFx(t, withHandlers(func(h *Handlers) { h.SecretGrace = grace }))
	hit = &seen{}
	etName = uniq("grace")
	f.mkEventType(f.oid, etName, nil)
	ep = f.mkEp(f.app, receiver(t, hit))
	s0 = ep.Secret
	wh = webhook.Handler{Log: discardLog(), Queries: f.q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true)}
	rec := f.own(http.MethodPost, epPath(f.app, ep)+"/rotate-secret", nil)
	want(t, rec, http.StatusOK, "")
	s1 = obj(t, rec)["secret"].(string)
	return
}

func TestRotateSecret_PreviousSecretStillSignsInsideTheGraceWindow(t *testing.T) {
	grace := time.Hour // DSTREAM_WEBHOOK_SECRET_GRACE
	f, ep, s0, s1, etName, wh, hit := rotatedFx(t, grace)
	row := f.epRow(f.app, ep.ID)
	if row.Secret != s1 || s1 == s0 {
		t.Fatalf("stored secret must be the new one")
	}
	if row.PrevSecret == nil || *row.PrevSecret != s0 {
		t.Fatalf("stored prev_secret must be the old secret, got %v", row.PrevSecret)
	}
	if d := time.Until(row.PrevSecretExpiresAt.Time); !row.PrevSecretExpiresAt.Valid || d < grace-time.Minute || d > grace {
		t.Fatalf("stored expiry must be ~now+grace, got %v from now", d)
	}
	f.publishAndDeliver(wh, etName, f.app)
	parts := strings.Fields(hit.sig)
	if len(parts) != 2 || parts[0] != sigFor(t, s1, *hit) || parts[1] != sigFor(t, s0, *hit) {
		t.Fatalf("inside grace want [new old] signatures, got %q", hit.sig)
	}
}

// A 100ms grace stands in for the real window so the expiry is reached by
// polling the stored deadline against the clock, never by sleeping a day.
func TestRotateSecret_PreviousSecretStopsSigningAfterTheGraceWindow(t *testing.T) {
	f, ep, s0, s1, etName, wh, hit := rotatedFx(t, 100*time.Millisecond)
	row := f.epRow(f.app, ep.ID)
	if row.PrevSecret == nil || *row.PrevSecret != s0 || !row.PrevSecretExpiresAt.Valid {
		t.Fatalf("rotation must store the old secret with an expiry, got %+v", row)
	}
	giveUp := time.Now().Add(5 * time.Second)
	for !time.Now().After(row.PrevSecretExpiresAt.Time) {
		if time.Now().After(giveUp) {
			t.Fatal("grace deadline never passed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.publishAndDeliver(wh, etName, f.app)
	if hit.sig != sigFor(t, s1, *hit) || strings.Contains(hit.sig, " ") {
		t.Fatalf("after grace want only the new signature, got %q", hit.sig)
	}
	if strings.Contains(hit.sig, sigFor(t, s0, *hit)) {
		t.Fatalf("old secret still signs after the grace expired")
	}
}

func TestRotateSecret_SecondRotationDropsTheFirstKey(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/rot")
	s0 := ep.Secret
	s1 := obj(t, f.own(http.MethodPost, epPath(f.app, ep)+"/rotate-secret", nil))["secret"].(string)
	s2 := obj(t, f.own(http.MethodPost, epPath(f.app, ep)+"/rotate-secret", nil))["secret"].(string)
	row := f.epRow(f.app, ep.ID)
	if row.Secret != s2 || row.PrevSecret == nil || *row.PrevSecret != s1 || *row.PrevSecret == s0 {
		t.Fatalf("after two rotations want current=%s prev=%s, got current=%s prev=%v", s2, s1, row.Secret, row.PrevSecret)
	}
}

// SecretGrace unset (0) falls back to 24h.
func TestRotateSecret_DefaultGraceIsADay(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/def")
	before := time.Now()
	want(t, f.own(http.MethodPost, epPath(f.app, ep)+"/rotate-secret", nil), http.StatusOK, "")
	exp := f.epRow(f.app, ep.ID).PrevSecretExpiresAt.Time
	if d := exp.Sub(before); d < 24*time.Hour-time.Minute || d > 24*time.Hour+time.Minute {
		t.Fatalf("default grace: expiry %v from now, want ~24h", d)
	}
}

func TestRotateSecret_BringYourOwnAndRefusals(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/byo")
	orig := f.epRow(f.app, ep.ID)
	path := epPath(f.app, ep) + "/rotate-secret"

	for _, bad := range []string{"%%%not base64%%%", "whsec_"} {
		want(t, f.own(http.MethodPost, path, map[string]any{"secret": bad}), http.StatusBadRequest, "secret is not a valid signing key")
	}
	row := f.epRow(f.app, ep.ID)
	if row.Secret != orig.Secret || row.PrevSecret != nil {
		t.Fatalf("a refused rotation must not touch the row: %+v", row)
	}

	good, _ := webhook.GenerateSecret()
	rec := f.own(http.MethodPost, path, map[string]any{"secret": good})
	want(t, rec, http.StatusOK, "")
	row = f.epRow(f.app, ep.ID)
	if obj(t, rec)["secret"] != good || row.Secret != good || row.PrevSecret == nil || *row.PrevSecret != orig.Secret {
		t.Fatalf("BYO secret must become current and demote the old one: %+v", row)
	}

	// An empty secret string is "generate one for me".
	rec = f.own(http.MethodPost, path, map[string]any{"secret": ""})
	want(t, rec, http.StatusOK, "")
	if s := obj(t, rec)["secret"]; s == good || s == "" {
		t.Fatalf("empty secret must generate a fresh one, got %v", s)
	}

	member := seedMember(t, f.q, f.oid)
	cur := f.epRow(f.app, ep.ID)
	want(t, f.as(member, f.oid, http.MethodPost, path, nil), http.StatusForbidden, "")
	if f.epRow(f.app, ep.ID).Secret != cur.Secret {
		t.Fatalf("member rotated the secret")
	}
	want(t, f.own(http.MethodPost, appPath(f.app)+"/endpoints/not-a-uuid/rotate-secret", nil), http.StatusBadRequest, "invalid endpoint id")
	want(t, f.own(http.MethodPost, appPath(f.app)+"/endpoints/"+uuid.NewString()+"/rotate-secret", nil), http.StatusNotFound, "endpoint not found")
}

func TestRotateSecret_DatabaseFailureIs500AndKeepsTheSecret(t *testing.T) {
	f := newFx(t, withQueries(tracedQueries(t, failAll("update endpoints", "prev_secret_expires_at"))))
	ep := f.mkEp(f.app, "https://ex.test/fail")
	rec := f.own(http.MethodPost, epPath(f.app, ep)+"/rotate-secret", nil)
	want(t, rec, http.StatusInternalServerError, "rotate secret")
	if row := f.epRow(f.app, ep.ID); row.Secret != ep.Secret || row.PrevSecret != nil {
		t.Fatalf("failed rotation changed the row")
	}
	if strings.Contains(f.logs.String(), ep.Secret) {
		t.Fatalf("error log leaked the secret")
	}
}

// endpointState reads the stored auto-disable state.
func (f *fx) failureState(ep store.Endpoint) (disabled bool, failures int32, disabledAt bool) {
	r := f.epRow(f.app, ep.ID)
	return r.Disabled, r.ConsecutiveFailures, r.DisabledAt.Valid
}

// Drives IncrEndpointFailures directly: this proves the SQL and the fan-out skip.
// The handler's disable-on-exhaustion path is covered in internal/webhook.
func TestAutoDisable_ConsecutiveFailuresDisableThenPatchReenables(t *testing.T) {
	f := newFx(t)
	etName := uniq("dis")
	f.mkEventType(f.oid, etName, nil)
	ep := f.mkEp(f.app, "https://ex.test/dis")
	live := f.mkEp(f.app, "https://ex.test/live")
	ctx := context.Background()

	for i := int32(1); i <= 3; i++ {
		r, err := f.q.IncrEndpointFailures(ctx, store.IncrEndpointFailuresParams{ID: ep.ID, Threshold: 3})
		if err != nil {
			t.Fatal(err)
		}
		if got := r.JustDisabled != nil && *r.JustDisabled; got != (i == 3) {
			t.Fatalf("failure %d: just_disabled=%v", i, got)
		}
	}
	if d, n, at := f.failureState(ep); !d || n != 3 || !at {
		t.Fatalf("after 3 failures want disabled/3/disabled_at, got %v/%d/%v", d, n, at)
	}

	// A disabled endpoint is skipped by fan-out; the live one still receives.
	rec := f.own(http.MethodPost, appPath(f.app)+"/messages", map[string]any{"event_type": etName, "payload": 1})
	want(t, rec, http.StatusAccepted, "")
	msgID := uuid.MustParse(obj(t, rec)["message_id"].(string))
	dels, err := f.q.ListDeliveriesForMessage(ctx, store.UUID(msgID))
	if err != nil || len(dels) != 1 || dels[0].EndpointID != live.ID {
		t.Fatalf("fan-out must skip the disabled endpoint: err=%v dels=%+v", err, dels)
	}

	rec = f.own(http.MethodPatch, epPath(f.app, ep), map[string]any{"disabled": false})
	want(t, rec, http.StatusOK, "")
	if d, n, at := f.failureState(ep); d || n != 0 || at {
		t.Fatalf("re-enable must clear disabled/failures/disabled_at, got %v/%d/%v", d, n, at)
	}
	// Disabling by hand keeps the counter and sets no auto-disable timestamp.
	want(t, f.own(http.MethodPatch, epPath(f.app, ep), map[string]any{"disabled": true}), http.StatusOK, "")
	if d, _, at := f.failureState(ep); !d || at {
		t.Fatalf("manual disable want disabled without disabled_at, got %v/%v", d, at)
	}
}

func TestAutoDisable_SuccessfulDeliveryResetsTheCounter(t *testing.T) {
	f := newFx(t)
	var hit seen
	etName := uniq("reset")
	f.mkEventType(f.oid, etName, nil)
	ep := f.mkEp(f.app, receiver(t, &hit))
	for i := 0; i < 2; i++ {
		if _, err := f.q.IncrEndpointFailures(context.Background(), store.IncrEndpointFailuresParams{ID: ep.ID, Threshold: 3}); err != nil {
			t.Fatal(err)
		}
	}
	if d, n, _ := f.failureState(ep); d || n != 2 {
		t.Fatalf("seed: want enabled with 2 failures, got %v/%d", d, n)
	}
	wh := webhook.Handler{Log: discardLog(), Queries: f.q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true), MaxConsecutiveFailures: 3}
	f.publishAndDeliver(wh, etName, f.app)
	if hit.id == "" {
		t.Fatalf("receiver was not called")
	}
	if d, n, _ := f.failureState(ep); d || n != 0 {
		t.Fatalf("a delivered message must reset consecutive_failures, got disabled=%v failures=%d", d, n)
	}
}

func TestRecover_ResetsDeadDeliveriesOfADisabledEndpointSince(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	ep := f.mkEp(f.app, "https://ex.test/rec")
	var dead, old, delivered = f.mkMsg(f.app, "x"), f.mkMsg(f.app, "x"), f.mkMsg(f.app, "x")
	dDead, dOld, dOK := f.mkDel(dead, ep), f.mkDel(old, ep), f.mkDel(delivered, ep)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.q.MarkDeliveryDead(ctx, dDead))
	must(f.q.MarkDeliveryDead(ctx, dOld))
	must(f.q.MarkDeliveryDelivered(ctx, dOK))
	if _, err := f.pool.Exec(ctx, `UPDATE message_deliveries SET attempt_count=5, created_at = now() - interval '3 days' WHERE id=$1`, dOld); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE message_deliveries SET attempt_count=5 WHERE id=$1`, dDead); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // endpoint auto-disabled by the same failures
		_, _ = f.q.IncrEndpointFailures(ctx, store.IncrEndpointFailuresParams{ID: ep.ID, Threshold: 3})
	}

	since := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	rec := f.own(http.MethodPost, epPath(f.app, ep)+"/recover", map[string]any{"since": since})
	want(t, rec, http.StatusAccepted, "")
	if b := obj(t, rec); b["recovered"] != float64(1) || b["truncated"] != false {
		t.Fatalf("want recovered=1 truncated=false, got %v", b)
	}
	// Documents CURRENT behaviour, not contract: recover resets dead deliveries
	// only; it does not re-enable the endpoint. Re-enabling is PATCH disabled:false.
	if d, n, _ := f.failureState(ep); !d || n != 3 {
		t.Fatalf("recover must leave the endpoint disabled with 3 failures, got %v/%d", d, n)
	}
	status := func(id interface{}) (s string, n int32) {
		if err := f.pool.QueryRow(ctx, `SELECT status, attempt_count FROM message_deliveries WHERE id=$1`, id).Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		return
	}
	if s, n := status(dDead); s != "queued" || n != 0 {
		t.Fatalf("in-window dead delivery must be reset, got %s/%d", s, n)
	}
	if s, n := status(dOld); s != "dead" || n != 5 {
		t.Fatalf("older-than-since delivery must stay dead, got %s/%d", s, n)
	}
	if s, _ := status(dOK); s != "delivered" {
		t.Fatalf("delivered delivery must be untouched, got %s", s)
	}
	if n := drainMessageTasks(t, f.dq); n != 1 {
		t.Fatalf("want exactly 1 re-enqueued task, got %d", n)
	}
}
