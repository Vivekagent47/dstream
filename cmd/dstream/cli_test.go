package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	testKey  = "k-test"
	uuidA    = "11111111-1111-1111-1111-111111111111"
	uuidB    = "22222222-2222-2222-2222-222222222222"
	uuidSrc  = "33333333-3333-3333-3333-333333333333"
	wsWait   = 5 * time.Second // safety net only, never a synchronisation delay
	jsonType = "application/json"
)

// apiServer serves routes keyed by "METHOD /path", rejecting any request that
// lacks the test bearer token so every helper's auth header is asserted.
func apiServer(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonRoute(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", jsonType)
		_ = json.NewEncoder(w).Encode(v)
	}
}

func cliEnv(t *testing.T, base string) {
	t.Helper()
	t.Setenv("DSTREAM_API_KEY", testKey)
	t.Setenv("DSTREAM_API_URL", base)
}

// deadURL returns the address of a server that has been shut down.
func deadURL(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.NotFoundHandler())
	u := s.URL
	s.Close()
	return u
}

// --- pure helpers ---

func TestIsUUID(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{uuidA, true},
		{"", false},
		{"my-source", false},
		{uuidA + "0", false},
		{"11111111_1111-1111-1111-111111111111", false},
		{"11111111-1111_1111-1111-111111111111", false},
		{"11111111-1111-1111_1111-111111111111", false},
		{"11111111-1111-1111-1111_111111111111", false},
	} {
		if got := isUUID(tc.in); got != tc.want {
			t.Errorf("isUUID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestTruncateStr(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"below n", "abc", 5, "abc"},
		{"exactly n", "abcde", 5, "abcde"},
		{"above n", "abcdef", 5, "abcde…"},
		{"empty", "", 3, ""},
		{"multibyte cut on rune boundary", "日本語", 3, "日…"},
		{"multibyte counted in bytes", "日本語", 9, "日本語"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateStr(tc.in, tc.n); got != tc.want {
				t.Errorf("truncateStr(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

func TestCliSkipHeader(t *testing.T) {
	for _, tc := range []struct {
		key  string
		vals []string
		want bool
	}{
		{"Host", []string{"x"}, true},
		{"content-length", []string{"3"}, true},
		{"Content-Type", []string{"a/b"}, true},
		{"Dstream-Webhook-Hops", []string{"1"}, true},
		{"connection", []string{"close"}, true},
		{"Transfer-Encoding", []string{"chunked"}, true},
		{"x-forwarded-for", []string{"1.2.3.4"}, true},
		{"X-Real-Ip", []string{"1.2.3.4"}, true},
		{"X-Custom", []string{"ok"}, false},
		{"X-Custom", []string{"ok", "[redacted]"}, true},
		{"Authorization", []string{"[redacted]"}, true},
		{"X-Empty", nil, false},
		{"Keep-Alive", []string{"timeout=5"}, true},
		{"Proxy-Authenticate", []string{"Basic"}, true},
		{"Proxy-Authorization", []string{"Basic x"}, true},
		{"Te", []string{"trailers"}, true},
		{"Trailer", []string{"X-T"}, true},
		{"Upgrade", []string{"websocket"}, true},
		{"X-Forwarded-Proto", []string{"https"}, true},
		{"X-Forwarded-Host", []string{"h.example"}, true},
	} {
		if got := cliSkipHeader(tc.key, tc.vals); got != tc.want {
			t.Errorf("cliSkipHeader(%q, %v) = %v, want %v", tc.key, tc.vals, got, tc.want)
		}
	}
}

func TestBuildWSURL(t *testing.T) {
	for _, tc := range []struct {
		name, base, want string
	}{
		{"http becomes ws", "http://localhost:8080", "ws://localhost:8080/api/cli/connect?source_id=" + uuidSrc},
		{"https becomes wss", "https://dstream.example", "wss://dstream.example/api/cli/connect?source_id=" + uuidSrc},
		{"trailing slash and prefix", "https://h.example/prefix/", "wss://h.example/prefix/api/cli/connect?source_id=" + uuidSrc},
		{"other scheme untouched", "ws://h.example", "ws://h.example/api/cli/connect?source_id=" + uuidSrc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildWSURL(tc.base, uuidSrc)
			if err != nil || got != tc.want {
				t.Errorf("buildWSURL(%q) = %q, %v; want %q", tc.base, got, err, tc.want)
			}
		})
	}
	if _, err := buildWSURL("http://[::1", uuidSrc); err == nil || !strings.Contains(err.Error(), "missing ']'") {
		t.Errorf("unparseable base: err = %v, want a url parse error", err)
	}
}

func TestCliAuth(t *testing.T) {
	t.Run("missing key", func(t *testing.T) {
		t.Setenv("DSTREAM_API_KEY", "")
		_, _, err := cliAuth("")
		if err == nil || err.Error() != "DSTREAM_API_KEY env var required" {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("default base", func(t *testing.T) {
		t.Setenv("DSTREAM_API_KEY", "k")
		t.Setenv("DSTREAM_API_URL", "")
		key, base, err := cliAuth("")
		if err != nil || key != "k" || base != "http://localhost:8080" {
			t.Errorf("got %q %q %v", key, base, err)
		}
	})
	t.Run("env base, trailing slash trimmed", func(t *testing.T) {
		t.Setenv("DSTREAM_API_KEY", "k")
		t.Setenv("DSTREAM_API_URL", "http://env.example/")
		_, base, err := cliAuth("")
		if err != nil || base != "http://env.example" {
			t.Errorf("base = %q, err = %v", base, err)
		}
	})
	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv("DSTREAM_API_KEY", "k")
		t.Setenv("DSTREAM_API_URL", "http://env.example")
		_, base, err := cliAuth("http://flag.example//")
		if err != nil || base != "http://flag.example" {
			t.Errorf("base = %q, err = %v", base, err)
		}
	})
}

// --- HTTP helpers ---

func TestCliGetJSON(t *testing.T) {
	srv := apiServer(t, map[string]http.HandlerFunc{
		"GET /ok":    jsonRoute(map[string]string{"a": "b"}),
		"GET /boom":  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "kaput", 500) },
		"GET /bad":   func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{not json") },
		"GET /empty": func(http.ResponseWriter, *http.Request) {},
	})
	t.Run("ok", func(t *testing.T) {
		var out map[string]string
		if err := cliGetJSON(srv.URL+"/ok", testKey, &out); err != nil || out["a"] != "b" {
			t.Errorf("out = %v, err = %v", out, err)
		}
	})
	t.Run("non-200 carries status and body", func(t *testing.T) {
		var out any
		err := cliGetJSON(srv.URL+"/boom", testKey, &out)
		if err == nil || !strings.Contains(err.Error(), "/boom: 500 kaput") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("wrong key is a 401", func(t *testing.T) {
		var out any
		err := cliGetJSON(srv.URL+"/ok", "nope", &out)
		if err == nil || !strings.Contains(err.Error(), ": 401 unauthorized") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		var out any
		if err := cliGetJSON(srv.URL+"/bad", testKey, &out); err == nil || !strings.Contains(err.Error(), "invalid character") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("empty body", func(t *testing.T) {
		var out any
		if err := cliGetJSON(srv.URL+"/empty", testKey, &out); err != io.EOF {
			t.Errorf("err = %v, want io.EOF", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		var out any
		if err := cliGetJSON(deadURL(t)+"/ok", testKey, &out); err == nil || !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestCliPostJSON(t *testing.T) {
	var (
		mu       sync.Mutex
		gotBody  map[string]any
		gotCType string
	)
	srv := apiServer(t, map[string]http.HandlerFunc{
		"POST /ok": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			gotCType = r.Header.Get("Content-Type")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"x1"}`)
		},
		"POST /bad":  func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "nope") },
		"POST /422":  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "name taken", 422) },
		"POST /json": func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"ignored":true}`) },
	})
	t.Run("2xx decodes and sends json", func(t *testing.T) {
		var out struct{ ID string }
		if err := cliPostJSON(srv.URL+"/ok", testKey, map[string]int{"n": 7}, &out); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if out.ID != "x1" || gotCType != jsonType || gotBody["n"] != float64(7) {
			t.Errorf("out=%+v ctype=%q body=%v", out, gotCType, gotBody)
		}
	})
	t.Run("nil out ignores body", func(t *testing.T) {
		if err := cliPostJSON(srv.URL+"/json", testKey, 1, nil); err != nil {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("non-2xx carries status and body", func(t *testing.T) {
		err := cliPostJSON(srv.URL+"/422", testKey, 1, nil)
		if err == nil || !strings.Contains(err.Error(), "/422: 422 name taken") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("malformed response", func(t *testing.T) {
		var out any
		if err := cliPostJSON(srv.URL+"/bad", testKey, 1, &out); err == nil || !strings.Contains(err.Error(), "invalid character") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unmarshalable payload", func(t *testing.T) {
		err := cliPostJSON(srv.URL+"/ok", testKey, make(chan int), nil)
		if err == nil || !strings.Contains(err.Error(), "unsupported type: chan int") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		if err := cliPostJSON(deadURL(t)+"/ok", testKey, 1, nil); err == nil || !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestResolveSource(t *testing.T) {
	srv := apiServer(t, map[string]http.HandlerFunc{
		"GET /api/cli/sources": jsonRoute([]cliSource{{ID: uuidSrc, Name: "stripe"}, {ID: uuidB, Name: "github"}}),
	})
	t.Run("uuid passes through without a request", func(t *testing.T) {
		got, err := resolveSource("http://unused.invalid", testKey, uuidA)
		if err != nil || got != uuidA {
			t.Errorf("got %q, %v", got, err)
		}
	})
	t.Run("name resolves", func(t *testing.T) {
		got, err := resolveSource(srv.URL, testKey, "github")
		if err != nil || got != uuidB {
			t.Errorf("got %q, %v", got, err)
		}
	})
	t.Run("unknown name", func(t *testing.T) {
		_, err := resolveSource(srv.URL, testKey, "nope")
		if err == nil || err.Error() != `no source named "nope" in your project` {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("non-200", func(t *testing.T) {
		_, err := resolveSource(srv.URL, "wrong", "github")
		if err == nil || err.Error() != "list sources: 401 unauthorized\n" {
			t.Errorf("err = %q", err)
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		bad := apiServer(t, map[string]http.HandlerFunc{
			"GET /api/cli/sources": func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "[") },
		})
		if _, err := resolveSource(bad.URL, testKey, "x"); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		if _, err := resolveSource(deadURL(t), testKey, "x"); err == nil || !strings.HasPrefix(err.Error(), "list sources: ") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestResolveFixtureAndScenarioID(t *testing.T) {
	srv := apiServer(t, map[string]http.HandlerFunc{
		"GET /api/bookmarks": jsonRoute([]cliFixture{{ID: uuidA, Name: "charge"}}),
		"GET /api/scenarios": jsonRoute([]cliScenario{{ID: uuidB, Name: "checkout"}}),
	})
	resolvers := []struct {
		name    string
		fn      func(base, key, ref string) (string, error)
		known   string
		wantID  string
		missing string
	}{
		{"fixture", resolveFixtureID, "charge", uuidA, `no fixture named "nope"`},
		{"scenario", resolveScenarioID, "checkout", uuidB, `no scenario named "nope"`},
	}
	for _, r := range resolvers {
		t.Run(r.name, func(t *testing.T) {
			if got, err := r.fn("http://unused.invalid", testKey, uuidSrc); err != nil || got != uuidSrc {
				t.Errorf("uuid passthrough: %q, %v", got, err)
			}
			if got, err := r.fn(srv.URL, testKey, r.known); err != nil || got != r.wantID {
				t.Errorf("by name: %q, %v", got, err)
			}
			if _, err := r.fn(srv.URL, testKey, "nope"); err == nil || err.Error() != r.missing {
				t.Errorf("no match: %v", err)
			}
			if _, err := r.fn(srv.URL, "wrong", "x"); err == nil || !strings.Contains(err.Error(), ": 401 unauthorized") {
				t.Errorf("api failure: %v", err)
			}
		})
	}
}

// --- forwardExport ---

type seenReq struct {
	method, ctype, body string
	header              http.Header
}

func recorder(t *testing.T, status int, resp string) (*httptest.Server, chan seenReq) {
	t.Helper()
	ch := make(chan seenReq, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- seenReq{r.Method, r.Header.Get("Content-Type"), string(b), r.Header.Clone()}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestForwardExport(t *testing.T) {
	t.Run("defaults to POST, filters headers, sets content type", func(t *testing.T) {
		srv, seen := recorder(t, 201, "made")
		status, dur, body, err := forwardExport(cliExport{
			Headers: map[string][]string{
				"X-Keep":          {"a", "b"},
				"X-Secret":        {"[redacted]"},
				"X-Forwarded-For": {"9.9.9.9"},
				"Content-Type":    {"text/ignored"},
			},
			ContentType: "application/x-test",
			Body:        b64("payload"),
		}, srv.URL)
		if err != nil || status != 201 || body != "made" || dur < 0 {
			t.Fatalf("status=%d dur=%v body=%q err=%v", status, dur, body, err)
		}
		got := <-seen
		if got.method != "POST" || got.body != "payload" || got.ctype != "application/x-test" {
			t.Errorf("request = %+v", got)
		}
		if v := got.header.Values("X-Keep"); len(v) != 2 || v[0] != "a" || v[1] != "b" {
			t.Errorf("X-Keep = %v", v)
		}
		if got.header.Get("X-Secret") != "" || got.header.Get("X-Forwarded-For") != "" {
			t.Errorf("skipped headers leaked: %v", got.header)
		}
	})
	t.Run("explicit method", func(t *testing.T) {
		srv, seen := recorder(t, 204, "")
		if status, _, _, err := forwardExport(cliExport{Method: "PUT"}, srv.URL); err != nil || status != 204 {
			t.Fatalf("status=%d err=%v", status, err)
		}
		if got := <-seen; got.method != "PUT" || got.body != "" {
			t.Errorf("request = %+v", got)
		}
	})
	t.Run("target 500 is reported, not an error", func(t *testing.T) {
		srv, _ := recorder(t, 500, "target exploded")
		status, _, body, err := forwardExport(cliExport{}, srv.URL)
		if err != nil || status != 500 || body != "target exploded" {
			t.Errorf("status=%d body=%q err=%v", status, body, err)
		}
	})
	t.Run("response body capped at 64KiB", func(t *testing.T) {
		srv, _ := recorder(t, 200, strings.Repeat("x", 70000))
		_, _, body, err := forwardExport(cliExport{}, srv.URL)
		if err != nil || len(body) != 1<<16 {
			t.Errorf("len(body)=%d err=%v", len(body), err)
		}
	})
	t.Run("bad base64", func(t *testing.T) {
		_, _, _, err := forwardExport(cliExport{Body: "!!!"}, "http://unused.invalid")
		if err == nil || !strings.HasPrefix(err.Error(), "decode fixture body: ") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("bad method", func(t *testing.T) {
		_, _, _, err := forwardExport(cliExport{Method: "BAD METHOD"}, "http://unused.invalid")
		if err == nil || !strings.Contains(err.Error(), "invalid method") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("target unreachable", func(t *testing.T) {
		status, _, body, err := forwardExport(cliExport{}, deadURL(t))
		if err == nil || !strings.Contains(err.Error(), "connection refused") || status != 0 || body != "" {
			t.Errorf("status=%d body=%q err=%v", status, body, err)
		}
	})
	t.Run("target drops the connection mid-request", func(t *testing.T) {
		// http.DefaultClient has no timeout to inject, so a hung target is
		// modelled as one that cuts the connection without answering.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			c, _, _ := w.(http.Hijacker).Hijack()
			_ = c.Close()
		}))
		defer srv.Close()
		status, _, _, err := forwardExport(cliExport{}, srv.URL)
		if err == nil || status != 0 || !(strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "reset")) {
			t.Errorf("status=%d err=%v", status, err)
		}
	})
}

// --- tunnel ---

func wsURLOf(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }

// tunnelServer accepts one websocket and hands it to script. The connection is
// closed normally when script returns.
func tunnelServer(t *testing.T, script func(ctx context.Context, c *websocket.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		script(r.Context(), c)
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// roundTrip pushes ev through a real tunnel to forwardURL and returns the
// response frame the client sent back, plus runTunnel's own return value
// (the server closes right after reading the response).
func roundTrip(t *testing.T, ev tunnelEvent, forwardURL string) (tunnelResponse, error) {
	t.Helper()
	got := make(chan tunnelResponse, 1)
	srv := tunnelServer(t, func(ctx context.Context, c *websocket.Conn) {
		// hello matches the real server: source_id + now, no event_id.
		hello := map[string]any{"type": "hello", "source_id": uuidSrc, "now": time.Now().UTC()}
		for _, f := range []any{hello, tunnelEvent{Type: "ping"}, tunnelEvent{Type: "mystery"}, ev} {
			if err := wsjson.Write(ctx, c, f); err != nil {
				return
			}
		}
		var r tunnelResponse
		if err := wsjson.Read(ctx, c, &r); err == nil {
			got <- r
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), wsWait)
	defer cancel()
	err := runTunnel(ctx, wsURLOf(srv), testKey, forwardURL)
	select {
	case r := <-got:
		return r, err
	default:
		t.Fatalf("no response frame reached the server; runTunnel err = %v", err)
		return tunnelResponse{}, nil
	}
}

func TestRunTunnel_ForwardsEventAndReportsResponse(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo", r.Method+"|"+r.Header.Get("X-In")+"|"+string(b))
		w.WriteHeader(202)
		_, _ = io.WriteString(w, "accepted")
	}))
	defer target.Close()

	resp, err := roundTrip(t, tunnelEvent{
		Type: "event", EventID: "ev-1", Method: "POST",
		Headers: map[string][]string{"X-In": {"yes"}}, Body: []byte("hi"),
	}, target.URL)

	if resp.Type != "response" || resp.EventID != "ev-1" || resp.Status != 202 || string(resp.Body) != "accepted" || resp.Error != "" {
		t.Errorf("response = %+v", resp)
	}
	if got := resp.Headers["X-Echo"]; len(got) != 1 || got[0] != "POST|yes|hi" {
		t.Errorf("target saw %v", got)
	}
	if err == nil || !strings.Contains(err.Error(), "ws read") {
		t.Errorf("runTunnel after server close: err = %v", err)
	}
}

func TestRunTunnel_ReportsForwardFailures(t *testing.T) {
	t.Run("target unreachable", func(t *testing.T) {
		resp, _ := roundTrip(t, tunnelEvent{Type: "event", EventID: "ev-2", Method: "POST"}, deadURL(t))
		if resp.Type != "response" || resp.EventID != "ev-2" || resp.Status != 0 || !strings.Contains(resp.Error, "connection refused") {
			t.Errorf("response = %+v", resp)
		}
	})
	t.Run("invalid method", func(t *testing.T) {
		resp, _ := roundTrip(t, tunnelEvent{Type: "event", EventID: "ev-3", Method: "BAD METHOD"}, "http://unused.invalid")
		if resp.Type != "response" || resp.EventID != "ev-3" || resp.Status != 0 || !strings.Contains(resp.Error, "invalid method") {
			t.Errorf("response = %+v", resp)
		}
	})
}

func TestRunTunnel_ClientCancelReturnsNil(t *testing.T) {
	hit := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit <- struct{}{}
	}))
	defer target.Close()
	answered := make(chan tunnelResponse, 1)
	srv := tunnelServer(t, func(ctx context.Context, c *websocket.Conn) {
		_ = wsjson.Write(ctx, c, tunnelEvent{Type: "event", EventID: "e", Method: "GET"})
		var r tunnelResponse
		if wsjson.Read(ctx, c, &r) == nil {
			answered <- r
		}
		// Park until the client goes away.
		_ = wsjson.Read(ctx, c, &r)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runTunnel(ctx, wsURLOf(srv), testKey, target.URL) }()

	select {
	case <-hit:
	case <-time.After(wsWait):
		t.Fatal("event never reached the target")
	}
	select {
	case r := <-answered:
		if r.Status != 200 || r.EventID != "e" {
			t.Errorf("response = %+v", r)
		}
	case <-time.After(wsWait):
		t.Fatal("no response frame")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runTunnel on client cancel: %v", err)
		}
	case <-time.After(wsWait):
		t.Fatal("runTunnel did not return after cancel")
	}
}

func TestRunTunnel_ServerClosesImmediately(t *testing.T) {
	srv := tunnelServer(t, func(context.Context, *websocket.Conn) {})
	err := runTunnel(context.Background(), wsURLOf(srv), testKey, "http://unused.invalid")
	if err == nil || !strings.Contains(err.Error(), "ws read") || !strings.Contains(err.Error(), "StatusNormalClosure") {
		t.Errorf("err = %v", err)
	}
}

func TestRunTunnel_DialFailure(t *testing.T) {
	err := runTunnel(context.Background(), "ws"+strings.TrimPrefix(deadURL(t), "http"), testKey, "x")
	if err == nil || !strings.HasPrefix(err.Error(), "ws dial: ") {
		t.Errorf("err = %v", err)
	}
}

// --- cobra commands ---

func TestListen_ForwardsOneEventEndToEnd(t *testing.T) {
	target, seen := recorder(t, 200, "pong")
	answered := make(chan map[string]json.RawMessage, 1)
	var mu sync.Mutex
	var auth, source string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/cli/sources", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, "no", 401)
			return
		}
		jsonRoute([]cliSource{{ID: uuidSrc, Name: "stripe"}})(w, r)
	})
	mux.HandleFunc("/api/cli/connect", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth, source = r.Header.Get("Authorization"), r.URL.Query().Get("source_id")
		mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		// Raw literal in the shape of internal/api/cli's frame map, so a tag
		// typo in the client's structs cannot hide behind a symmetric fake.
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"event","event_id":"ev-9","method":"POST","path":"/","headers":{"X-In":["1"]},"body":"e30="}`))
		var resp map[string]json.RawMessage
		if wsjson.Read(ctx, c, &resp) == nil {
			answered <- resp
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	api := httptest.NewServer(mux)
	defer api.Close()
	cliEnv(t, api.URL)

	_, stderr, err := execCmd(t, cliCmd(), "listen", "--source", "stripe", "--forward", target.URL)

	if err == nil || !strings.Contains(err.Error(), "ws read") {
		t.Errorf("listen ends when the server closes: err = %v", err)
	}
	if !strings.Contains(stderr, "→ tunneling source "+uuidSrc+" to "+target.URL) {
		t.Errorf("stderr = %q", stderr)
	}
	if got := <-seen; got.method != "POST" || got.body != "{}" || got.header.Get("X-In") != "1" {
		t.Errorf("target saw %+v", got)
	}
	select {
	case r := <-answered:
		want := map[string]string{"type": `"response"`, "event_id": `"ev-9"`, "status": `200`, "body": `"` + b64("pong") + `"`}
		for k, v := range want {
			if string(r[k]) != v {
				t.Errorf("frame[%q] = %s, want %s", k, r[k], v)
			}
		}
		if _, ok := r["headers"]; !ok {
			t.Errorf("frame has no headers key: %v", r)
		}
	default:
		t.Error("response never reported back over the socket")
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "Bearer "+testKey || source != uuidSrc {
		t.Errorf("connect saw auth=%q source_id=%q", auth, source)
	}
}

func TestListen_Errors(t *testing.T) {
	t.Run("missing key", func(t *testing.T) {
		t.Setenv("DSTREAM_API_KEY", "")
		_, _, err := execCmd(t, cliCmd(), "listen", "--source", "x")
		if err == nil || err.Error() != "DSTREAM_API_KEY env var required" {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("source flag required", func(t *testing.T) {
		_, _, err := execCmd(t, cliCmd(), "listen")
		if err == nil || !strings.Contains(err.Error(), `"source" not set`) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unknown source", func(t *testing.T) {
		api := apiServer(t, map[string]http.HandlerFunc{"GET /api/cli/sources": jsonRoute([]cliSource{})})
		cliEnv(t, api.URL)
		_, _, err := execCmd(t, cliCmd(), "listen", "--source", "ghost")
		if err == nil || err.Error() != `no source named "ghost" in your project` {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("--url wins over env and its trailing slash is trimmed", func(t *testing.T) {
		api := apiServer(t, map[string]http.HandlerFunc{"GET /api/cli/sources": jsonRoute([]cliSource{})})
		t.Setenv("DSTREAM_API_KEY", testKey)
		t.Setenv("DSTREAM_API_URL", "")
		_, _, err := execCmd(t, cliCmd(), "listen", "--source", "ghost", "--url", api.URL+"/")
		if err == nil || err.Error() != `no source named "ghost" in your project` {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unparseable base url", func(t *testing.T) {
		// A UUID source skips the lookup so the URL reaches buildWSURL.
		t.Setenv("DSTREAM_API_KEY", testKey)
		_, _, err := execCmd(t, cliCmd(), "listen", "--source", uuidSrc, "--url", "http://[::1")
		if err == nil || !strings.Contains(err.Error(), "missing ']'") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestFixturesCmd(t *testing.T) {
	t.Run("lists rows with truncated names", func(t *testing.T) {
		long := strings.Repeat("n", 40)
		api := apiServer(t, map[string]http.HandlerFunc{"GET /api/bookmarks": jsonRoute([]cliFixture{
			{ID: uuidA, Name: "charge", HTTPMethod: "POST", SourceID: uuidSrc, Tags: []string{"a", "b"}},
			{ID: uuidB, Name: long, HTTPMethod: "PUT", SourceID: uuidSrc},
		})})
		cliEnv(t, api.URL)
		out, _, err := execCmd(t, cliCmd(), "fixtures")
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") {
			t.Fatalf("out = %q", out)
		}
		if f := strings.Fields(lines[1]); len(f) != 4 || f[0] != "charge" || f[1] != "POST" || f[2] != uuidSrc || f[3] != "a,b" {
			t.Errorf("row 1 = %q", lines[1])
		}
		if !strings.HasPrefix(lines[2], strings.Repeat("n", 28)+"…") || strings.Contains(lines[2], strings.Repeat("n", 29)) {
			t.Errorf("row 2 not truncated: %q", lines[2])
		}
	})
}

func TestFixturesCmd_EmptyAndErrors(t *testing.T) {
	api := apiServer(t, map[string]http.HandlerFunc{"GET /api/bookmarks": jsonRoute([]cliFixture{})})
	t.Setenv("DSTREAM_API_KEY", testKey)
	out, _, err := execCmd(t, cliCmd(), "fixtures", "--url", api.URL)
	if err != nil || out != "no fixtures\n" {
		t.Errorf("out = %q, err = %v", out, err)
	}

	t.Setenv("DSTREAM_API_KEY", "")
	if _, _, err := execCmd(t, cliCmd(), "fixtures"); err == nil || err.Error() != "DSTREAM_API_KEY env var required" {
		t.Errorf("missing key: %v", err)
	}

	t.Setenv("DSTREAM_API_KEY", "wrong")
	if _, _, err := execCmd(t, cliCmd(), "fixtures", "--url", api.URL); err == nil || !strings.Contains(err.Error(), ": 401 unauthorized") {
		t.Errorf("bad key: %v", err)
	}
}

func exportRoute(exp cliExport) http.HandlerFunc { return jsonRoute(exp) }

func TestReplayCmd(t *testing.T) {
	target, seen := recorder(t, 200, "replayed")
	api := apiServer(t, map[string]http.HandlerFunc{
		"GET /api/bookmarks":                      jsonRoute([]cliFixture{{ID: uuidA, Name: "charge"}}),
		"GET /api/bookmarks/" + uuidA + "/export": exportRoute(cliExport{Method: "PUT", ContentType: "text/plain", Body: b64("body")}),
	})
	cliEnv(t, api.URL)
	line := regexp.MustCompile(`^(\d)/2  200  \d+ms  replayed$`)

	t.Run("by name, count times", func(t *testing.T) {
		out, _, err := execCmd(t, cliCmd(), "replay", "charge", "--forward", target.URL, "--count", "2")
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) != 2 || !line.MatchString(lines[0]) || !line.MatchString(lines[1]) ||
			!strings.HasPrefix(lines[0], "1/2  200  ") || !strings.HasPrefix(lines[1], "2/2  200  ") {
			t.Fatalf("out = %q", out)
		}
		for range 2 {
			if got := <-seen; got.method != "PUT" || got.body != "body" || got.ctype != "text/plain" {
				t.Errorf("target saw %+v", got)
			}
		}
	})
	t.Run("by uuid", func(t *testing.T) {
		out, _, err := execCmd(t, cliCmd(), "replay", uuidA, "--forward", target.URL)
		if err != nil || !strings.HasPrefix(out, "1/1  200  ") {
			t.Errorf("out = %q, err = %v", out, err)
		}
		<-seen
	})
	t.Run("forward failure is reported per attempt and does not abort", func(t *testing.T) {
		out, stderr, err := execCmd(t, cliCmd(), "replay", uuidA, "--forward", deadURL(t), "--count", "2")
		if err != nil || out != "" {
			t.Errorf("out = %q, err = %v", out, err)
		}
		if strings.Count(stderr, "connection refused") != 2 || !strings.Contains(stderr, "replay 1/2: ") || !strings.Contains(stderr, "replay 2/2: ") {
			t.Errorf("stderr = %q", stderr)
		}
	})
	t.Run("unknown fixture", func(t *testing.T) {
		_, _, err := execCmd(t, cliCmd(), "replay", "ghost", "--forward", target.URL)
		if err == nil || err.Error() != `no fixture named "ghost"` {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("export fetch fails", func(t *testing.T) {
		_, _, err := execCmd(t, cliCmd(), "replay", uuidB, "--forward", target.URL)
		if err == nil || !strings.Contains(err.Error(), "/export: 404") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("missing key", func(t *testing.T) {
		t.Setenv("DSTREAM_API_KEY", "")
		_, _, err := execCmd(t, cliCmd(), "replay", "charge", "--forward", target.URL)
		if err == nil || err.Error() != "DSTREAM_API_KEY env var required" {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("forward flag required", func(t *testing.T) {
		_, _, err := execCmd(t, cliCmd(), "replay", "charge")
		if err == nil || !strings.Contains(err.Error(), `"forward" not set`) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestImportCmd(t *testing.T) {
	var (
		mu  sync.Mutex
		got map[string]any
	)
	api := apiServer(t, map[string]http.HandlerFunc{
		"GET /api/cli/sources": jsonRoute([]cliSource{{ID: uuidSrc, Name: "stripe"}}),
		"POST /api/bookmarks/import": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			got = nil
			_ = json.NewDecoder(r.Body).Decode(&got)
			if got["name"] == "dupe" {
				http.Error(w, "name taken", 409)
				return
			}
			jsonRoute(map[string]string{"id": uuidA, "name": got["name"].(string)})(w, r)
		},
	})
	cliEnv(t, api.URL)
	dir := t.TempDir()
	file := filepath.Join(dir, "fx.json")
	if err := os.WriteFile(file, []byte(`{"method":"POST","path":"/hook","headers":{"X-A":["1"]},"content_type":"application/json","body_base64":"e30="}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("imports with resolved source and split tags", func(t *testing.T) {
		out, _, err := execCmd(t, cliCmd(), "import", file, "--source", "stripe", "--name", "charge", "--description", "d", "--tags", "a,b")
		if err != nil || out != "imported fixture \"charge\" (id "+uuidA+")\n" {
			t.Fatalf("out = %q, err = %v", out, err)
		}
		mu.Lock()
		defer mu.Unlock()
		tags, _ := got["tags"].([]any)
		hdrs, _ := got["headers"].(map[string]any)
		if got["source_id"] != uuidSrc || got["description"] != "d" || got["method"] != "POST" || got["path"] != "/hook" ||
			got["content_type"] != "application/json" || got["body_base64"] != "e30=" ||
			len(tags) != 2 || tags[0] != "a" || tags[1] != "b" || hdrs["X-A"] == nil {
			t.Errorf("posted body = %v", got)
		}
	})
	t.Run("no tags sends null", func(t *testing.T) {
		if _, _, err := execCmd(t, cliCmd(), "import", file, "--source", uuidSrc, "--name", "plain"); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if v, ok := got["tags"]; !ok || v != nil || got["source_id"] != uuidSrc {
			t.Errorf("posted body = %v", got)
		}
	})
	t.Run("server rejects", func(t *testing.T) {
		_, _, err := execCmd(t, cliCmd(), "import", file, "--source", uuidSrc, "--name", "dupe")
		if err == nil || !strings.Contains(err.Error(), "/api/bookmarks/import: 409 name taken") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("empty name", func(t *testing.T) {
		_, _, err := execCmd(t, cliCmd(), "import", file, "--source", uuidSrc, "--name", "")
		if err == nil || err.Error() != "--name required" {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		_, _, err := execCmd(t, cliCmd(), "import", filepath.Join(dir, "nope.json"), "--source", uuidSrc, "--name", "x")
		if err == nil || !strings.HasPrefix(err.Error(), "read "+filepath.Join(dir, "nope.json")+": ") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("malformed fixture", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := execCmd(t, cliCmd(), "import", bad, "--source", uuidSrc, "--name", "x")
		if err == nil || !strings.HasPrefix(err.Error(), "parse fixture json: ") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unknown source", func(t *testing.T) {
		_, _, err := execCmd(t, cliCmd(), "import", file, "--source", "ghost", "--name", "x")
		if err == nil || err.Error() != `no source named "ghost" in your project` {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("missing key", func(t *testing.T) {
		t.Setenv("DSTREAM_API_KEY", "")
		_, _, err := execCmd(t, cliCmd(), "import", file, "--source", uuidSrc, "--name", "x")
		if err == nil || err.Error() != "DSTREAM_API_KEY env var required" {
			t.Errorf("err = %v", err)
		}
	})
}

func TestScenarioRunCmd(t *testing.T) {
	target, seen := recorder(t, 200, "")
	steps := []cliScenarioStep{
		{Position: 1, BookmarkID: uuidA, BookmarkName: "first"},
		{Position: 2, BookmarkID: uuidB, BookmarkName: "second", DelayMs: 1},
	}
	routes := map[string]http.HandlerFunc{
		"GET /api/scenarios":                      jsonRoute([]cliScenario{{ID: uuidSrc, Name: "checkout"}}),
		"GET /api/scenarios/" + uuidSrc:           jsonRoute(cliScenarioDetail{Steps: steps}),
		"GET /api/bookmarks/" + uuidA + "/export": exportRoute(cliExport{Method: "POST", Body: b64("one")}),
		"GET /api/bookmarks/" + uuidB + "/export": exportRoute(cliExport{Method: "POST", Body: b64("two")}),
	}
	api := apiServer(t, routes)
	cliEnv(t, api.URL)

	t.Run("runs steps in order", func(t *testing.T) {
		out, _, err := execCmd(t, cliCmd(), "scenario", "run", "checkout", "--forward", target.URL)
		if err != nil {
			t.Fatal(err)
		}
		re := regexp.MustCompile(`^step 1 first: 200 \d+ms\nstep 2 second: 200 \d+ms\n$`)
		if !re.MatchString(out) {
			t.Errorf("out = %q", out)
		}
		if a, b := <-seen, <-seen; a.body != "one" || b.body != "two" {
			t.Errorf("order = %q then %q", a.body, b.body)
		}
	})
	t.Run("unknown scenario", func(t *testing.T) {
		_, _, err := execCmd(t, cliCmd(), "scenario", "run", "ghost", "--forward", target.URL)
		if err == nil || err.Error() != `no scenario named "ghost"` {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("scenario fetch fails", func(t *testing.T) {
		_, _, err := execCmd(t, cliCmd(), "scenario", "run", uuidB, "--forward", target.URL)
		if err == nil || !strings.Contains(err.Error(), "/api/scenarios/"+uuidB+": 404") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("step export fails and aborts", func(t *testing.T) {
		broken := apiServer(t, map[string]http.HandlerFunc{
			"GET /api/scenarios/" + uuidSrc: jsonRoute(cliScenarioDetail{Steps: steps[:1]}),
		})
		_, stderr, err := execCmd(t, cliCmd(), "scenario", "run", uuidSrc, "--forward", target.URL, "--url", broken.URL)
		if err == nil || !strings.Contains(err.Error(), "/export: 404") || !strings.HasPrefix(stderr, "step 1 first: ") {
			t.Errorf("err = %v, stderr = %q", err, stderr)
		}
	})
	t.Run("step forward fails and aborts", func(t *testing.T) {
		out, stderr, err := execCmd(t, cliCmd(), "scenario", "run", uuidSrc, "--forward", deadURL(t))
		if err == nil || !strings.Contains(err.Error(), "connection refused") || out != "" || !strings.HasPrefix(stderr, "step 1 first: ") {
			t.Errorf("out = %q, stderr = %q, err = %v", out, stderr, err)
		}
	})
	t.Run("missing key", func(t *testing.T) {
		t.Setenv("DSTREAM_API_KEY", "")
		_, _, err := execCmd(t, cliCmd(), "scenario", "run", uuidSrc, "--forward", target.URL)
		if err == nil || err.Error() != "DSTREAM_API_KEY env var required" {
			t.Errorf("err = %v", err)
		}
	})
}

func TestForward_ClientTimeoutIsReportedToServer(t *testing.T) {
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer hung.Close()
	t.Cleanup(func() { close(release) }) // runs before hung.Close

	got := make(chan tunnelResponse, 1)
	srv := tunnelServer(t, func(ctx context.Context, c *websocket.Conn) {
		var r tunnelResponse
		if wsjson.Read(ctx, c, &r) == nil {
			got <- r
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), wsWait)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURLOf(srv), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	forward(ctx, conn, &http.Client{Timeout: 50 * time.Millisecond}, hung.URL, tunnelEvent{Type: "event", EventID: "ev-t", Method: "GET"})

	select {
	case r := <-got:
		if r.Type != "response" || r.EventID != "ev-t" || r.Status != 0 || !strings.Contains(r.Error, "Client.Timeout") {
			t.Errorf("response = %+v", r)
		}
	case <-time.After(wsWait):
		t.Fatal("no response frame")
	}
}

// With neither --url nor DSTREAM_API_URL the CLI talks to localhost:8080. This
// is the one test that needs a fixed port. "localhost" may resolve to either
// loopback family, so both are bound. No IPv6 on the host (EADDRNOTAVAIL or
// EAFNOSUPPORT) just
// means only 127.0.0.1 is bound; a port that is already taken, on either
// family, could route the request to the wrong server, so that skips.
func TestListen_DefaultsToLocalhost8080(t *testing.T) {
	var gotPath, gotAuth string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		http.Error(w, "short and stout", http.StatusTeapot)
	})}
	t.Cleanup(func() { srv.Close() })
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080"} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			if addr[0] == '[' && (errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT)) {
				continue // no IPv6 here
			}
			t.Skipf("%s is busy: %v", addr, err)
		}
		go srv.Serve(ln)
	}
	t.Setenv("DSTREAM_API_KEY", testKey)
	t.Setenv("DSTREAM_API_URL", "")

	_, _, err := execCmd(t, listenCmd(), "--source", "by-name")

	if err == nil || !strings.Contains(err.Error(), "list sources: 418 short and stout") {
		t.Fatalf("err = %v, want the default server's 418 reported", err)
	}
	if gotPath != "/api/cli/sources" || gotAuth != "Bearer "+testKey {
		t.Errorf("default server saw %q with auth %q", gotPath, gotAuth)
	}
}
