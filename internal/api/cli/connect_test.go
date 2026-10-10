package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func TestConnectRejectsBeforeUpgrade(t *testing.T) {
	e := newEnv(t, nil)
	org, other := e.seedOrg(), e.seedOrg()
	src := e.seedSource(org, "mine")
	foreign := e.seedSource(other, "theirs")

	cases := []struct {
		name   string
		org    string
		query  string
		status int
		body   string
	}{
		{"no principal", "", "source_id=" + src.String(), 401, `{"error":"active org required"}`},
		{"nil org", uuid.Nil.String(), "source_id=" + src.String(), 401, `{"error":"active org required"}`},
		{"missing source", org.String(), "", 400, `{"error":"source_id required"}`},
		{"malformed source", org.String(), "source_id=not-a-uuid", 400, `{"error":"invalid source_id"}`},
		{"unknown source", org.String(), "source_id=" + uuid.NewString(), 404, `{"error":"source not found"}`},
		{"other org's source", org.String(), "source_id=" + foreign.String(), 404, `{"error":"source not found"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", e.srv.URL+"/connect?"+tc.query, nil)
			if tc.org != "" {
				req.Header.Set("X-Org", tc.org)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status || strings.TrimSpace(string(b)) != tc.body {
				t.Fatalf("got %d %q, want %d %q", resp.StatusCode, b, tc.status, tc.body)
			}
		})
	}
	if n, _ := e.rdb.Exists(context.Background(), SessionKey(foreign)).Result(); n != 0 {
		t.Fatal("rejected connect registered a session key")
	}
}

func TestConnectOriginPolicy(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		origin  string
		wantErr bool
	}{
		{"matching origin", "https://app.example.test", "https://app.example.test", false},
		{"foreign origin", "https://app.example.test", "https://evil.example", true},
		{"no origin header (CLI)", "https://app.example.test", "", false},
		// Unparseable base URL: no pin, so default same-origin check applies.
		{"unparseable base, no origin", "http://%zz", "", false},
		{"unparseable base, foreign origin", "http://%zz", "https://evil.example", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, func(h *Handlers) { h.PublicBaseURL = tc.baseURL })
			org := e.seedOrg()
			src := e.seedSource(org, "s")
			hdr := http.Header{"X-Org": {org.String()}}
			if tc.origin != "" {
				hdr.Set("Origin", tc.origin)
			}
			base := sessionsActive(t)
			c, resp, err := e.dialHdr(src.String(), hdr)
			if tc.wantErr {
				if err == nil || resp == nil || resp.StatusCode != 403 {
					t.Fatalf("want 403 handshake rejection, got resp=%v err=%v", resp, err)
				}
				if sessionsActive(t) != base {
					t.Fatal("rejected origin opened a tunnel")
				}
				return
			}
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			helloFor(t, c, src)
			c.Close(websocket.StatusNormalClosure, "")
			e.waitSessions(src, base)
		})
	}
}

func TestConnectHelloRegistersAndReleasesSession(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	closedBefore := disconnects(t, "closed")

	c := e.dial(org, src)
	// Wire format, asserted on the raw frame, not through this package's types.
	var hello map[string]json.RawMessage
	if err := json.Unmarshal([]byte(read(t, c)), &hello); err != nil {
		t.Fatal(err)
	}
	if len(hello) != 3 || string(hello["type"]) != `"hello"` || string(hello["source_id"]) != `"`+src.String()+`"` {
		t.Fatalf("hello = %v", hello)
	}
	var now time.Time
	if err := json.Unmarshal(hello["now"], &now); err != nil || time.Since(now) > time.Minute {
		t.Fatalf("hello.now = %s (%v)", hello["now"], err)
	}
	ctx := context.Background()
	if v, err := e.rdb.Get(ctx, SessionKey(src)).Result(); err != nil || v == "" {
		t.Fatalf("session key = %q, %v", v, err)
	}
	if ttl := e.rdb.TTL(ctx, SessionKey(src)).Val(); ttl <= 0 || ttl > cliSessionTTL {
		t.Fatalf("session ttl = %v", ttl)
	}
	if got := sessionsActive(t); got != base+1 {
		t.Fatalf("active sessions = %v, want %v", got, base+1)
	}

	// Client disconnect releases the key and the gauge.
	c.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base)
	poll(t, "session key released", func() bool { return e.rdb.Exists(ctx, SessionKey(src)).Val() == 0 })
	if got := disconnects(t, "closed") - closedBefore; got != 1 {
		t.Fatalf("closed disconnects = %v, want 1", got)
	}
}

func TestConnectPingKeepsSessionAlive(t *testing.T) {
	e := newEnv(t, func(h *Handlers) { h.pingEvery = 20 * time.Millisecond })
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	c := e.dial(org, src)
	helloFor(t, c, src)

	if f := read(t, c); f != `{"type":"ping"}` {
		t.Fatalf("ping frame = %s", f)
	}
	// A wiped key (a racing older teardown) must be re-established by a later
	// ping; ticks are bounded (20ms), so the poll converges.
	e.rdb.Del(context.Background(), SessionKey(src))
	poll(t, "ping re-registers the wiped session key", func() bool {
		return e.rdb.Exists(context.Background(), SessionKey(src)).Val() == 1
	})
	c.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base)
}

func TestConnectRegisterFailure(t *testing.T) {
	// A Redis client pointed at a dead port: the Set that registers the session fails.
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { dead.Close() })
	e := newEnv(t, func(h *Handlers) { h.Redis = dead })
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	before := disconnects(t, "register_failed")

	c := e.dial(org, src)
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("want a normal close from the server, got %v", err)
	}
	poll(t, "register_failed disconnect", func() bool { return disconnects(t, "register_failed")-before == 1 })
	if !strings.Contains(e.log.String(), "cli: register session") {
		t.Fatalf("register failure not logged: %s", e.log.String())
	}
}

func TestConnectDeliversEventAndRecordsAttempt(t *testing.T) {
	cases := []struct {
		name       string
		response   string // %ID% is replaced with the event id
		wantStatus *int32
		wantBody   string
		wantErr    string
		wantEvent  string
	}{
		{"2xx delivered", `{"type":"response","event_id":"%ID%","status":201,"headers":{"X-Out":["y"]},"body":"b2s="}`, i32(201), "ok", "", "delivered"},
		{"non-2xx failed", `{"type":"response","event_id":"%ID%","status":500,"headers":{},"body":"Ym9vbQ=="}`, i32(500), "boom", "", "failed"},
		{"error frame failed", `{"type":"response","event_id":"%ID%","error":"connection refused"}`, nil, "", "connection refused", "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, nil)
			org := e.seedOrg()
			src := e.seedSource(org, "s")
			base := sessionsActive(t)
			ev := e.seedEvent(org, src, []byte(`{"a":1}`), "")

			c := e.dial(org, src)
			helloFor(t, c, src)
			e.pushEvent(src, ev)
			want := `{"body":"eyJhIjoxfQ==","event_id":"` + ev.String() + `","headers":{"X-In":["1"]},"method":"POST","path":"/e/x","type":"event"}`
			if got := read(t, c); got != want {
				t.Fatalf("event frame\n got %s\nwant %s", got, want)
			}
			send(t, c, strings.ReplaceAll(tc.response, "%ID%", ev.String()))

			a := e.waitAttempt(ev)
			if a.AttemptNum != 1 || (a.ResponseStatus == nil) != (tc.wantStatus == nil) || (tc.wantStatus != nil && *a.ResponseStatus != *tc.wantStatus) {
				t.Fatalf("attempt = num %d status %v", a.AttemptNum, a.ResponseStatus)
			}
			if string(a.ResponseBody) != tc.wantBody {
				t.Fatalf("response body = %q, want %q", a.ResponseBody, tc.wantBody)
			}
			if tc.wantErr == "" && a.ErrorMessage != nil || tc.wantErr != "" && (a.ErrorMessage == nil || *a.ErrorMessage != tc.wantErr) {
				t.Fatalf("error message = %v, want %q", a.ErrorMessage, tc.wantErr)
			}
			if a.DurationMs == nil {
				t.Fatal("duration not recorded")
			}
			poll(t, "event status "+tc.wantEvent, func() bool { return e.eventStatus(ev) == tc.wantEvent })
			c.Close(websocket.StatusNormalClosure, "")
			e.waitSessions(src, base)
		})
	}
}

func i32(v int32) *int32 { return &v }

func TestConnectResponseHeadersStoredVerbatim(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	ev := e.seedEvent(org, src, []byte(`{}`), "")
	c := e.dial(org, src)
	helloFor(t, c, src)
	e.pushEvent(src, ev)
	read(t, c)
	send(t, c, `{"type":"response","event_id":"`+ev.String()+`","status":200,"headers":{"X-Out":["y"]},"body":""}`)
	a := e.waitAttempt(ev)
	var h map[string][]string
	if err := json.Unmarshal(a.ResponseHeaders, &h); err != nil || len(h) != 1 || len(h["X-Out"]) != 1 || h["X-Out"][0] != "y" {
		t.Fatalf("response headers = %s (%v)", a.ResponseHeaders, err)
	}
	c.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base)
}

func TestConnectSurvivesNoiseFramesAndBadDispatchPayloads(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	ev := e.seedEvent(org, src, []byte(`{"a":1}`), "")
	c := e.dial(org, src)
	helloFor(t, c, src)

	// Client-side noise: a non-response frame and a response for an id nobody awaits.
	send(t, c, `{"type":"pong"}`)
	send(t, c, `{"type":"response","event_id":"`+uuid.NewString()+`","status":200}`)
	// Server-side noise on the dispatch list: not JSON, and a non-UUID event id.
	e.push(src, "not json")
	e.push(src, `{"event_id":"nope"}`)
	// A real event after all of it proves the tunnel survived and kept consuming.
	e.pushEvent(src, ev)
	if f := read(t, c); !strings.Contains(f, `"event_id":"`+ev.String()+`"`) {
		t.Fatalf("event frame = %s", f)
	}
	// The stray response must not have produced an attempt for any event of the org.
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM attempts a JOIN events ev ON ev.id = a.event_id WHERE ev.org_id = $1`, org).Scan(&n); err != nil || n != 0 {
		t.Fatalf("attempts in org after stray response = %d (%v)", n, err)
	}
	c.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base)
}

func TestConnectEventQueuedBeforeCLIAttachesIsDelivered(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	ev := e.seedEvent(org, src, []byte(`{"a":1}`), "")
	e.pushEvent(src, ev) // no CLI attached yet: it waits on the list

	if n := e.rdb.LLen(context.Background(), DispatchKey(src)).Val(); n != 1 {
		t.Fatalf("queued = %d", n)
	}
	c := e.dial(org, src)
	helloFor(t, c, src)
	if f := read(t, c); !strings.Contains(f, `"event_id":"`+ev.String()+`"`) {
		t.Fatalf("event frame = %s", f)
	}
	c.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base)
}

func TestConnectMalformedFrameTearsDownTunnel(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	closedBefore := disconnects(t, "closed")
	c := e.dial(org, src)
	helloFor(t, c, src)

	send(t, c, `{not json`)
	// The server closes the socket; reading is also what lets the client answer
	// the close handshake so the server's teardown is not held up.
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusInvalidFramePayloadData {
		t.Fatalf("want close status InvalidFramePayloadData after a malformed frame, got %v", err)
	}
	e.waitSessions(src, base)
	poll(t, "session key released", func() bool { return e.rdb.Exists(context.Background(), SessionKey(src)).Val() == 0 })
	if got := disconnects(t, "closed") - closedBefore; got != 1 {
		t.Fatalf("closed disconnects = %v, want 1", got)
	}
}

func TestConnectDisconnectMidEventLeavesNoAttempt(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	ev := e.seedEvent(org, src, []byte(`{}`), "")
	c := e.dial(org, src)
	helloFor(t, c, src)
	e.pushEvent(src, ev)
	read(t, c) // event delivered to the CLI, which never answers

	c.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base)
	// A late attempt from a buggy dispatch goroutine would land shortly after
	// teardown; observe a short window rather than a single instant.
	for i := 0; i < 20; i++ {
		if n := len(e.attempts(ev)); n != 0 {
			t.Fatalf("abandoned event recorded %d attempts", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Left in_flight, not failed or delivered: the stuck-event reaper owns it.
	if st := e.eventStatus(ev); st != "in_flight" {
		t.Fatalf("event status = %q, want in_flight", st)
	}
}

func TestConnectSecondConnectionOwnsSession(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	ctx := context.Background()

	a := e.dial(org, src)
	helloFor(t, a, src)
	tokenA := e.rdb.Get(ctx, SessionKey(src)).Val()
	b := e.dial(org, src)
	helloFor(t, b, src)
	tokenB := e.rdb.Get(ctx, SessionKey(src)).Val()
	if tokenA == "" || tokenB == "" || tokenA == tokenB {
		t.Fatalf("tokens A=%q B=%q: second connection must overwrite with its own token", tokenA, tokenB)
	}

	// The older connection goes away; its teardown must not wipe B's key.
	a.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base+1)
	if got := e.rdb.Get(ctx, SessionKey(src)).Val(); got != tokenB {
		t.Fatalf("after A's teardown session key = %q, want B's %q", got, tokenB)
	}

	// B still receives events.
	ev := e.seedEvent(org, src, []byte(`{"a":1}`), "")
	e.pushEvent(src, ev)
	if f := read(t, b); !strings.Contains(f, `"event_id":"`+ev.String()+`"`) {
		t.Fatalf("B event frame = %s", f)
	}
	b.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base)
	poll(t, "B's key released", func() bool { return e.rdb.Exists(ctx, SessionKey(src)).Val() == 0 })
}

func TestConnectConcurrentEventsAllRecorded(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	c := e.dial(org, src)
	helloFor(t, c, src)

	const n = 8
	ids := map[string]uuid.UUID{}
	for i := 0; i < n; i++ {
		ev := e.seedEvent(org, src, []byte(`{}`), "")
		ids[ev.String()] = ev
		e.pushEvent(src, ev)
	}
	for i := 0; i < n; i++ {
		var f struct {
			EventID string `json:"event_id"`
		}
		if err := json.Unmarshal([]byte(read(t, c)), &f); err != nil || ids[f.EventID] == uuid.Nil {
			t.Fatalf("unexpected event frame (%v): %+v", err, f)
		}
		send(t, c, `{"type":"response","event_id":"`+f.EventID+`","status":200}`)
	}
	for _, ev := range ids {
		ev := ev
		a := e.waitAttempt(ev)
		if a.ResponseStatus == nil || *a.ResponseStatus != 200 {
			t.Fatalf("event %s attempt status = %v", ev, a.ResponseStatus)
		}
		poll(t, "delivered", func() bool { return e.eventStatus(ev) == "delivered" })
	}
	c.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base)
}
