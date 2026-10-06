package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

func TestKeys(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	if got, want := SessionKey(id), "cli:source:11111111-2222-3333-4444-555555555555"; got != want {
		t.Fatalf("SessionKey = %q, want %q", got, want)
	}
	if got, want := DispatchKey(id), "cli:dispatch:11111111-2222-3333-4444-555555555555"; got != want {
		t.Fatalf("DispatchKey = %q, want %q", got, want)
	}
}

func TestOriginHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://hooks.example.com", "hooks.example.com"},
		{"http://localhost:8080/base", "localhost:8080"},
		{"", ""},
		{"no-scheme-no-host", ""},
		{"http://%zz", ""}, // does not parse
	} {
		if got := originHost(tc.in); got != tc.want {
			t.Errorf("originHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPendingMap(t *testing.T) {
	p := newPendingMap()
	if _, ok := p.pop("never-added"); ok {
		t.Fatal("pop of an unknown id reported ok")
	}
	p.remove("never-added") // must not panic

	ch := p.add("a")
	got, ok := p.pop("a")
	if !ok || got != ch {
		t.Fatalf("pop(a) = %v, %v; want the added channel", got, ok)
	}
	if _, ok := p.pop("a"); ok {
		t.Fatal("second pop of the same id reported ok")
	}
	p.add("b")
	p.remove("b")
	if _, ok := p.pop("b"); ok {
		t.Fatal("pop after remove reported ok")
	}
	// A popped channel is buffered: the reader can hand over without blocking.
	ch <- map[string]json.RawMessage{"k": json.RawMessage(`1`)}
	if v := <-ch; string(v["k"]) != "1" {
		t.Fatalf("buffered handoff = %v", v)
	}
}

func TestPendingMapConcurrent(t *testing.T) {
	p := newPendingMap()
	const n = 200
	var wg sync.WaitGroup
	popped := make(chan string, 2*n)
	ids := make([]string, n)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	for _, id := range ids {
		wg.Add(1)
		go func() { defer wg.Done(); p.add(id) }()
	}
	wg.Wait()
	// Two goroutines race to pop every id: exactly one may win.
	for _, id := range ids {
		for r := 0; r < 2; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, ok := p.pop(id); ok {
					popped <- id
				}
			}()
		}
		wg.Add(1)
		go func() { defer wg.Done(); p.remove(id) }()
	}
	wg.Wait()
	close(popped)
	seen := map[string]int{}
	for id := range popped {
		seen[id]++
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("id %s popped %d times", id, c)
		}
	}
	if len(p.chs) != 0 {
		t.Fatalf("%d entries left after pop/remove", len(p.chs))
	}
}

func TestListSources(t *testing.T) {
	e := newEnv(t, nil)
	org, other := e.seedOrg(), e.seedOrg()
	a := e.seedSource(org, "alpha")
	e.seedSource(other, "not-mine")

	get := func(h Handlers, ctx context.Context) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ListSources(w, httptest.NewRequest("GET", "/sources", nil).WithContext(ctx))
		return w
	}
	t.Run("no principal", func(t *testing.T) {
		w := get(e.h, context.Background())
		if w.Code != 401 || strings.TrimSpace(w.Body.String()) != `{"error":"active org required"}` {
			t.Fatalf("got %d %s", w.Code, w.Body)
		}
	})
	t.Run("nil org", func(t *testing.T) {
		w := get(e.h, auth.WithPrincipal(context.Background(), auth.Principal{}))
		if w.Code != 401 {
			t.Fatalf("got %d", w.Code)
		}
	})
	t.Run("lists only the org's sources with wire keys", func(t *testing.T) {
		w := get(e.h, auth.WithPrincipal(context.Background(), auth.Principal{OrgID: org}))
		if w.Code != 200 {
			t.Fatalf("got %d %s", w.Code, w.Body)
		}
		want := `[{"id":"` + a.String() + `","name":"alpha","type":"generic"}]`
		if got := strings.TrimSpace(w.Body.String()); got != want {
			t.Fatalf("body\n got %s\nwant %s", got, want)
		}
	})
	t.Run("empty org yields an empty array", func(t *testing.T) {
		w := get(e.h, auth.WithPrincipal(context.Background(), auth.Principal{OrgID: e.seedOrg()}))
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `[]` {
			t.Fatalf("got %d %s", w.Code, w.Body)
		}
	})
	t.Run("store failure", func(t *testing.T) {
		dead := testPool(t)
		h := e.h
		h.Queries = store.New(dead)
		dead.Close()
		w := get(h, auth.WithPrincipal(context.Background(), auth.Principal{OrgID: org}))
		if w.Code != 500 || strings.TrimSpace(w.Body.String()) != `{"error":"list"}` {
			t.Fatalf("got %d %s", w.Code, w.Body)
		}
	})
}

// The handler also serves through the real mux (route + principal wiring).
func TestListSourcesOverHTTP(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	req, _ := http.NewRequest("GET", e.srv.URL+"/sources", nil)
	req.Header.Set("X-Org", org.String())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// dispatch drives dispatchEventToCLI directly. write receives the frame the
// server would send; it gets the pending map to play the CLI's reply.
func (e *env) dispatch(ctx context.Context, h Handlers, ev uuid.UUID, write func(any) error) *pendingMap {
	e.t.Helper()
	p := newPendingMap()
	h.dispatchEventToCLI(ctx, write, ingest.NewPostgresBodyStore(e.q), p, ev)
	if _, ok := p.pop(ev.String()); ok {
		e.t.Fatal("pending entry leaked after dispatch returned")
	}
	return p
}

func TestDispatchLoadEventError(t *testing.T) {
	e := newEnv(t, nil)
	wrote := false
	e.dispatch(context.Background(), e.h, uuid.New(), func(any) error { wrote = true; return nil })
	if wrote || !strings.Contains(e.log.String(), "cli dispatch: load event") {
		t.Fatalf("wrote=%v log=%s", wrote, e.log.String())
	}
}

func TestDispatchBodyError(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	ev := e.seedEvent(org, src, []byte(`x`), "bogus-ref")
	wrote := false
	e.dispatch(context.Background(), e.h, ev, func(any) error { wrote = true; return nil })
	if wrote || !strings.Contains(e.log.String(), "cli dispatch: body") {
		t.Fatalf("wrote=%v log=%s", wrote, e.log.String())
	}
	if st := e.eventStatus(ev); st != "queued" || len(e.attempts(ev)) != 0 {
		t.Fatalf("a body that cannot load must leave the event untouched: status %q", st)
	}
}

// recordCLIFailure's row, reached through a failed socket write.
func TestDispatchWriteFailureRecordsAttempt(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	ev := e.seedEvent(org, src, []byte(`{}`), "")
	e.dispatch(context.Background(), e.h, ev, func(any) error { return errors.New("socket gone") })

	as := e.attempts(ev)
	if len(as) != 1 || as[0].AttemptNum != 1 || as[0].ErrorMessage == nil || *as[0].ErrorMessage != "socket gone" ||
		as[0].ResponseStatus != nil || as[0].ResponseBody != nil {
		t.Fatalf("attempts = %+v", as)
	}
	if st := e.eventStatus(ev); st != "failed" {
		t.Fatalf("event status = %q, want failed", st)
	}
}

func TestDispatchTimeoutRecordsAttempt(t *testing.T) {
	e := newEnv(t, func(h *Handlers) { h.respTimeout = 30 * time.Millisecond })
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	ev := e.seedEvent(org, src, []byte(`{}`), "")
	var frame map[string]any
	e.dispatch(context.Background(), e.h, ev, func(v any) error { frame = v.(map[string]any); return nil })

	if frame["type"] != "event" || frame["event_id"] != ev.String() {
		t.Fatalf("frame = %v", frame)
	}
	as := e.attempts(ev)
	if len(as) != 1 || as[0].ErrorMessage == nil || *as[0].ErrorMessage != "cli response timeout" {
		t.Fatalf("attempts = %+v", as)
	}
	if st := e.eventStatus(ev); st != "failed" {
		t.Fatalf("event status = %q, want failed", st)
	}
}

func TestDispatchCancelledWhileWaiting(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	ev := e.seedEvent(org, src, []byte(`{}`), "")
	ctx, cancel := context.WithCancel(context.Background())
	e.dispatch(ctx, e.h, ev, func(any) error { cancel(); return nil })
	if n := len(e.attempts(ev)); n != 0 {
		t.Fatalf("cancelled dispatch recorded %d attempts", n)
	}
	if st := e.eventStatus(ev); st != "in_flight" {
		t.Fatalf("event status = %q, want in_flight", st)
	}
}

// replyWith plays the CLI: when the server's frame is written, hand the
// response straight to the pending channel, exactly as the reader goroutine does.
func replyWith(p **pendingMap, id uuid.UUID, resp string) func(any) error {
	return func(any) error {
		var frame map[string]json.RawMessage
		if err := json.Unmarshal([]byte(resp), &frame); err != nil {
			return err
		}
		ch, ok := (*p).pop(id.String())
		if !ok {
			return errors.New("no pending entry for event")
		}
		ch <- frame
		return nil
	}
}

func TestDispatchAttemptInsertFailureStillSettlesEvent(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	ev := e.seedEvent(org, src, []byte(`{}`), "")
	// attempts(event_id, attempt_num) is unique: occupy attempt 1 so the insert conflicts.
	if _, err := e.q.CreateAttempt(context.Background(), store.CreateAttemptParams{EventID: store.UUID(ev), AttemptNum: 1}); err != nil {
		t.Fatal(err)
	}
	p := newPendingMap()
	e.h.dispatchEventToCLI(context.Background(), replyWith(&p, ev, `{"type":"response","status":200}`), ingest.NewPostgresBodyStore(e.q), p, ev)
	if !strings.Contains(e.log.String(), "cli dispatch: attempt") {
		t.Fatalf("insert failure not logged: %s", e.log.String())
	}
	if st := e.eventStatus(ev); st != "delivered" {
		t.Fatalf("event status = %q, want delivered", st)
	}
}

func TestRecordCLIFailureInsertFailureStillFailsEvent(t *testing.T) {
	e := newEnv(t, nil)
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	ev := e.seedEvent(org, src, []byte(`{}`), "")
	if _, err := e.q.CreateAttempt(context.Background(), store.CreateAttemptParams{EventID: store.UUID(ev), AttemptNum: 1}); err != nil {
		t.Fatal(err)
	}
	e.dispatch(context.Background(), e.h, ev, func(any) error { return errors.New("gone") })
	if !strings.Contains(e.log.String(), "cli dispatch: record failure") {
		t.Fatalf("record failure not logged: %s", e.log.String())
	}
	if st := e.eventStatus(ev); st != "failed" {
		t.Fatalf("event status = %q, want failed", st)
	}
}

func TestDispatchHandlersDefaults(t *testing.T) {
	var h Handlers
	// Literals, not the constants: this guards the no-behaviour-change claim.
	if h.pingInterval() != 10*time.Second || h.responseTimeout() != 35*time.Second {
		t.Fatalf("defaults = %v, %v", h.pingInterval(), h.responseTimeout())
	}
}
