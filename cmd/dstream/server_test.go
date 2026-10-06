package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/config"
	mw "github.com/Vivekagent47/dstream/internal/middleware"
	"github.com/Vivekagent47/dstream/internal/store"
)

const testSessionSecret = "0123456789abcdef0123456789abcdef"

func TestIsLocalBaseURL(t *testing.T) {
	cases := []struct {
		name, raw string
		want      bool
	}{
		{"localhost", "http://localhost:8080", true},
		{"ipv4 loopback", "http://127.0.0.1:8080", true},
		{"ipv6 loopback", "http://[::1]:8080", true},
		{".localhost suffix", "http://app.localhost", true},
		{"public host", "https://dstream.example.com", false},
		{"suffix lookalike", "https://evil-localhost.example.com", false},
		{"unparseable", "http://%zz", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isLocalBaseURL(c.raw); got != c.want {
				t.Fatalf("isLocalBaseURL(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// serverEnv sets the minimum environment for a bootable server and returns
// the loaded config.
func serverEnv(t *testing.T) config.Config {
	t.Helper()
	useTestDB(t)
	t.Setenv("DSTREAM_SESSION_SECRET", testSessionSecret)
	t.Setenv("DSTREAM_COOKIE_SECURE", "false")
	t.Setenv("DSTREAM_LOG_LEVEL", "error")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestServerCmd_BootRefusals(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"short session secret",
			map[string]string{"DSTREAM_SESSION_SECRET": "too-short"},
			"DSTREAM_SESSION_SECRET must be at least 32 bytes"},
		{"insecure cookie on a public origin",
			map[string]string{"DSTREAM_COOKIE_SECURE": "false", "DSTREAM_PUBLIC_BASE_URL": "https://dstream.example.com"},
			"DSTREAM_COOKIE_SECURE must be true when DSTREAM_PUBLIC_BASE_URL is not localhost"},
		{"dev mode on a public origin",
			map[string]string{"DSTREAM_DEV_MODE": "true", "DSTREAM_PUBLIC_BASE_URL": "https://dstream.example.com"},
			"DSTREAM_DEV_MODE must be false when DSTREAM_PUBLIC_BASE_URL is not localhost"},
		{"secret guard wins over the dev mode guard",
			map[string]string{"DSTREAM_SESSION_SECRET": "too-short", "DSTREAM_DEV_MODE": "true", "DSTREAM_PUBLIC_BASE_URL": "https://dstream.example.com"},
			"DSTREAM_SESSION_SECRET must be at least 32 bytes"},
		{"oidc enforce without an issuer",
			map[string]string{"DSTREAM_OIDC_ENFORCE": "true"},
			"DSTREAM_OIDC_ENFORCE is set without DSTREAM_OIDC_ISSUER"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("DSTREAM_SESSION_SECRET", testSessionSecret)
			t.Setenv("DSTREAM_COOKIE_SECURE", "true")
			// Ambient shell values must not leak in; and an unreachable DB
			// makes a guard that runs after dependency construction surface
			// "ping db" instead of its own message.
			for _, k := range []string{"DSTREAM_DEV_MODE", "DSTREAM_PUBLIC_BASE_URL", "DSTREAM_OIDC_ISSUER",
				"DSTREAM_OIDC_CLIENT_ID", "DSTREAM_OIDC_CLIENT_SECRET", "DSTREAM_OIDC_ENFORCE", "DSTREAM_OIDC_DEFAULT_ROLE"} {
				t.Setenv(k, "")
			}
			t.Setenv("DSTREAM_DB_URL", "postgres://nobody:nopass@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			_, _, err := execCmd(t, serverCmd())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestServerCmd_ListenFailureIsReturned(t *testing.T) {
	serverEnv(t)
	t.Setenv("DSTREAM_HTTP_ADDR", "127.0.0.1:notaport")
	_, _, err := execCmd(t, serverCmd())
	if err == nil || !strings.Contains(err.Error(), "notaport") {
		t.Fatalf("err = %v, want a listen error naming the bad port", err)
	}
}

func TestServerCmd_BuildFailureIsReturned(t *testing.T) {
	serverEnv(t)
	t.Setenv("DSTREAM_DB_URL", "postgres://nobody:nopass@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	_, _, err := execCmd(t, serverCmd())
	if err == nil || !strings.Contains(err.Error(), "ping db") {
		t.Fatalf("err = %v, want a ping db error", err)
	}
}

func TestBuildServer_FailuresReleaseWhatTheyBuilt(t *testing.T) {
	t.Run("bad trusted proxy", func(t *testing.T) {
		cfg := serverEnv(t)
		cfg.TrustedProxies = []string{"not-a-cidr"}
		srv, cleanup, err := buildServer(context.Background(), cfg, quietLog())
		if err == nil || !strings.Contains(err.Error(), "not-a-cidr") || srv != nil || cleanup != nil {
			t.Fatalf("got srv=%v cleanup=%v err=%v, want only an error naming not-a-cidr", srv, cleanup != nil, err)
		}
		// The collector registered before the failure must have been released:
		// a good build right after must not panic on a duplicate registration.
		cfg.TrustedProxies = nil
		_, cleanup, err = buildServer(context.Background(), cfg, quietLog())
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		cleanup()
	})
}

func get(t *testing.T, url string, cookie *http.Cookie) (int, string, http.Header) {
	t.Helper()
	return do(t, http.MethodGet, url, cookie)
}

func do(t *testing.T, method, url string, cookie *http.Cookie) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	// Don't follow redirects: SSO start answers with one.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestBuildServer_RoutesAreWired(t *testing.T) {
	cfg := serverEnv(t)
	pool := testPool(t)
	q := store.New(pool)

	srv, cleanup, err := buildServer(context.Background(), cfg, quietLog())
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	defer cleanup()
	if srv.Addr != cfg.HTTPAddr || srv.ReadHeaderTimeout != 10*time.Second || srv.ReadTimeout != 60*time.Second || srv.WriteTimeout != 0 {
		t.Fatalf("server timeouts/addr wrong: %+v", srv)
	}
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	if code, body, _ := get(t, ts.URL+"/healthz", nil); code != 200 || body != "ok" {
		t.Fatalf("/healthz = %d %q", code, body)
	}
	if code, body, _ := get(t, ts.URL+"/readyz", nil); code != 200 || body != "ready" {
		t.Fatalf("/readyz = %d %q", code, body)
	}
	if code, _, _ := get(t, ts.URL+"/no/such/route", nil); code != http.StatusNotFound {
		t.Fatalf("unknown route = %d, want 404", code)
	}

	// Routing evidence beyond the negative control: each of these would 404
	// if its Mount call were dropped from buildServer.
	if code, body, _ := do(t, http.MethodPost, ts.URL+"/e/bogus-"+uuid.NewString(), nil); code != http.StatusNotFound || !strings.Contains(body, "unknown source") {
		t.Fatalf("ingest unknown token = %d %q, want the ingest handler's 404 'unknown source'", code, body)
	}
	if code, _, _ := get(t, ts.URL+"/admin/usage", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous /admin/usage = %d, want 401 (a missing mount would 404)", code)
	}

	// /metrics sits behind SuperAdminOnly: anonymous 401, a plain user 403,
	// a super admin 200 with the Prometheus exposition.
	if code, _, _ := get(t, ts.URL+"/metrics", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous /metrics = %d, want 401", code)
	}
	user := mustUser(t, q, newEmail(t, pool))
	signer := &auth.SessionSigner{Secret: []byte(testSessionSecret)}
	cookieFor := func() *http.Cookie {
		rec := httptest.NewRecorder()
		signer.Issue(rec, uuid.UUID(user.ID.Bytes), uuid.Nil, int64(user.SessionEpoch))
		return rec.Result().Cookies()[0]
	}
	if code, _, _ := get(t, ts.URL+"/metrics", cookieFor()); code != http.StatusForbidden {
		t.Fatalf("non-admin /metrics = %d, want 403", code)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE users SET is_super_admin = true WHERE id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	code, body, hdr := get(t, ts.URL+"/metrics", cookieFor())
	if code != 200 || !strings.Contains(body, "# HELP") || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Fatalf("admin /metrics = %d %q (%s)", code, body[:min(len(body), 80)], hdr.Get("Content-Type"))
	}
}

func TestServeUntil_ShutsDownOnSignal(t *testing.T) {
	cfg := serverEnv(t)
	srv, cleanup, err := buildServer(context.Background(), cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() { done <- serveUntil(srv, ln, quietLog(), stop) }()

	url := "http://" + ln.Addr().String() + "/healthz"
	deadline := time.Now().Add(5 * time.Second)
	for {
		if code, _, _ := tryGet(url); code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}

	stop <- os.Interrupt
	if err := <-done; err != nil {
		t.Fatalf("serveUntil after signal: %v", err)
	}
	if _, _, err := tryGet(url); err == nil {
		t.Fatal("server still accepting after shutdown")
	}
}

func tryGet(url string) (int, string, error) {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func TestServeUntil_ReturnsServeError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // Serve on a closed listener fails immediately
	err = serveUntil(&http.Server{}, ln, quietLog(), make(chan os.Signal))
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("err = %v, want net.ErrClosed", err)
	}
}

// fakeIdP serves just the OIDC discovery document.
func fakeIdP(t *testing.T) *httptest.Server {
	t.Helper()
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 ts.URL,
			"authorization_endpoint": ts.URL + "/authorize",
			"token_endpoint":         ts.URL + "/token",
			"jwks_uri":               ts.URL + "/jwks",
		})
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestBuildServer_SSODiscovery(t *testing.T) {
	t.Run("wires the authenticator", func(t *testing.T) {
		cfg := serverEnv(t)
		idp := fakeIdP(t)
		cfg.OIDC = config.OIDCConfig{Issuer: idp.URL, ClientID: "cid", ClientSecret: "sec", Scopes: []string{"openid"}}
		srv, cleanup, err := buildServer(context.Background(), cfg, quietLog())
		if err != nil {
			t.Fatalf("buildServer: %v", err)
		}
		defer cleanup()
		ts := httptest.NewServer(srv.Handler)
		defer ts.Close()
		code, _, hdr := get(t, ts.URL+"/api/auth/sso/start", nil)
		if code != http.StatusFound || !strings.HasPrefix(hdr.Get("Location"), idp.URL+"/authorize?") {
			t.Fatalf("sso start = %d Location %q, want a redirect to the IdP", code, hdr.Get("Location"))
		}
	})
	t.Run("discovery failure", func(t *testing.T) {
		cfg := serverEnv(t)
		idp := fakeIdP(t)
		bad := idp.URL + "/nope" // discovery 404s under this path
		cfg.OIDC = config.OIDCConfig{Issuer: bad, ClientID: "cid", ClientSecret: "sec"}
		_, _, err := buildServer(context.Background(), cfg, quietLog())
		if err == nil || !strings.Contains(err.Error(), "sso discovery failed; check DSTREAM_OIDC_ISSUER and IdP reachability") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestServerCmd_ReportsBadConfig(t *testing.T) {
	t.Setenv("DSTREAM_DB_MAX_CONNS", "not-a-number")
	_, _, err := execCmd(t, serverCmd())
	if err == nil || !strings.Contains(err.Error(), "unmarshal config") {
		t.Fatalf("err = %v, want the config unmarshal failure", err)
	}
}

// The real command serves until SIGTERM, then shuts down and returns nil.
func TestServerCmd_RunsUntilSigterm(t *testing.T) {
	serverEnv(t)
	t.Setenv("DSTREAM_HTTP_ADDR", "127.0.0.1:0")
	// Our own handler guarantees an early SIGTERM can never kill the test
	// binary, whatever the timing against the command's own registration.
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(sigs) })

	done := make(chan error, 1)
	go func() {
		_, _, err := execCmd(t, serverCmd())
		done <- err
	}()
	deadline := time.After(30 * time.Second)
	for first := true; ; first = false {
		select {
		case err := <-done:
			if first {
				t.Fatalf("server exited before it was signalled: %v", err)
			}
			if err != nil {
				t.Fatalf("server returned %v on SIGTERM, want a clean nil", err)
			}
			return
		case <-deadline:
			t.Fatal("server did not exit after SIGTERM")
		case <-time.After(100 * time.Millisecond):
			// Re-sent until the command is up and has registered its handler.
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		}
	}
}

func TestBuildServer_LogsTracingEnabled(t *testing.T) {
	cfg := serverEnv(t)
	cfg.Tracing.Enabled = true
	cfg.Tracing.OTLPEndpoint = "http://127.0.0.1:1"
	cfg.Tracing.SampleRatio = 1
	// Init installs a global provider; put the previous one back afterwards.
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	logs := &logBuf{}
	// The exporter connects lazily, so an unreachable collector must not fail the build.
	_, cleanup, err := buildServer(context.Background(), cfg, logs.logger())
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(cleanup)
	if out := logs.String(); !strings.Contains(out, "tracing enabled") || !strings.Contains(out, "127.0.0.1:1") {
		t.Errorf("no tracing-enabled line naming the endpoint:\n%s", out)
	}
}

// A quota-load failure must not stop the server from booting: it logs the
// cause and serves unlimited until a reload lands.
func TestBuildServer_QuotaLoadFailureOnlyWarns(t *testing.T) {
	cfg := serverEnv(t)
	pool, dsn := migratedScratchDB(t)
	cfg.DB.URL = dsn
	renameColumn := `ALTER TABLE organizations RENAME COLUMN quota_period TO quota_period_gone`
	breakDB(t, pool, renameColumn, `ALTER TABLE organizations RENAME COLUMN quota_period_gone TO quota_period`)

	logs := &logBuf{}
	srv, cleanup, err := buildServer(context.Background(), cfg, logs.logger())
	if err != nil {
		t.Fatalf("buildServer: %v, want the boot to survive a failed quota load", err)
	}
	t.Cleanup(cleanup)
	if srv == nil {
		t.Fatal("no server returned")
	}
	out := logs.String()
	if !strings.Contains(out, "usage: initial quota limits load failed") || !strings.Contains(out, `column \"quota_period\" does not exist`) {
		t.Errorf("no quota-load warning with its cause:\n%s", out)
	}
}

// --- security controls are installed by the production bootstrap ---
//
// Each control below is unit-tested where it lives. These tests prove the
// composition root actually installs it: drop the wiring line in buildServer
// or api.Mount and the matching test fails.

// wired is a real buildServer behind httptest, plus an org with an owner
// session and one ingest source.
type wired struct {
	t       *testing.T
	url     string
	pool    *pgxpool.Pool
	orgID   uuid.UUID
	srcID   uuid.UUID
	token   string // ingest token
	session *http.Cookie
	csrf    string
}

// newWired seeds the org (with the given hard quotas, 0 = unlimited) BEFORE
// building the server, because the quota gate loads its limits at boot.
func newWired(t *testing.T, cfg config.Config, eventsHard, messagesHard int64) *wired {
	t.Helper()
	pool := testPool(t)
	q := store.New(pool)
	rdb := testRedis(t)
	cfg.Redis.Addr = rdb.Options().Addr
	ctx := context.Background()

	user := mustUser(t, q, newEmail(t, pool)) // its cleanup also deletes the org
	org, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: "wired", Slug: "wired-" + uuid.NewString()})
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{OrgID: org.ID, UserID: user.ID, Role: "owner"}); err != nil {
		t.Fatalf("member: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE organizations SET quota_events_hard = $2, quota_messages_hard = $3 WHERE id = $1`,
		org.ID, eventsHard, messagesHard); err != nil {
		t.Fatalf("quotas: %v", err)
	}
	src, err := q.CreateSource(ctx, store.CreateSourceParams{
		OrgID: org.ID, Name: "s-" + uuid.NewString(), Type: "generic",
		IngestToken: uuid.NewString(), SigningConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	w := &wired{t: t, pool: pool, orgID: store.GoUUID(org.ID), srcID: store.GoUUID(src.ID), token: src.IngestToken}

	srv, cleanup, err := buildServer(ctx, cfg, quietLog())
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(cleanup)
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	w.url = ts.URL

	// Registered last, so it runs first: the gate publishes its quota alert off
	// the request path, and it must land (or time out) before the org is
	// deleted and the keys swept, or it would recreate them afterwards.
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if count(t, pool, `SELECT count(*) FROM messages WHERE org_id = $1 AND event_type LIKE 'usage.quota_%'`, org.ID) > 0 ||
				(eventsHard == 0 && messagesHard == 0) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		for _, id := range []uuid.UUID{w.orgID, w.srcID} {
			if keys, _ := rdb.Keys(ctx, "*"+id.String()+"*").Result(); len(keys) > 0 {
				rdb.Del(ctx, keys...)
			}
		}
	})

	rec := httptest.NewRecorder()
	(&auth.SessionSigner{Secret: []byte(testSessionSecret)}).Issue(rec, uuid.UUID(user.ID.Bytes), w.orgID, int64(user.SessionEpoch))
	w.session = rec.Result().Cookies()[0]
	return w
}

// send issues a request with the owner session; withCSRF adds the token the
// SPA would echo, fetched the way the SPA gets it (a GET refreshes the cookie).
func (w *wired) send(method, path string, body any, withCSRF bool) (int, string) {
	w.t.Helper()
	if withCSRF && w.csrf == "" {
		req, _ := http.NewRequest(http.MethodGet, w.url+"/api/me", nil)
		req.AddCookie(w.session)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.t.Fatal(err)
		}
		resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Name == mw.CSRFCookieName {
				w.csrf = c.Value
			}
		}
		if w.csrf == "" {
			w.t.Fatal("GET did not mint a csrf cookie")
		}
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, w.url+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(w.session)
	if withCSRF {
		req.Header.Set(mw.CSRFHeaderName, w.csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// ingest POSTs a distinct body (so dedup never short-circuits) to the source.
func (w *wired) ingest(hdr map[string]string) (int, string) {
	w.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, w.url+"/e/"+w.token, strings.NewReader(`{"n":"`+uuid.NewString()+`"}`))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func TestBuildServer_CSRFIsInstalled(t *testing.T) {
	w := newWired(t, serverEnv(t), 0, 0)
	code, body := w.send(http.MethodPost, "/api/event-types", map[string]any{"name": "e.a"}, false)
	if code != http.StatusForbidden || strings.TrimSpace(body) != "csrf token mismatch" {
		t.Fatalf("session POST without a token = %d %q, want 403 \"csrf token mismatch\"", code, body)
	}
	if code, body := w.send(http.MethodPost, "/api/event-types", map[string]any{"name": "e.a"}, true); code >= 300 {
		t.Fatalf("session POST with a valid token = %d %q, want success", code, body)
	}
}

// The limiter keys on the address the trusted-proxy middleware resolved from
// X-Forwarded-For, not on the loopback peer.
func TestBuildServer_TrustedProxyRewriteReachesTheLimiter(t *testing.T) {
	cfg := serverEnv(t)
	cfg.TrustedProxies = []string{"127.0.0.1"}
	rdb := testRedis(t)
	cfg.Redis.Addr = rdb.Options().Addr
	pool := testPool(t)
	email := newEmail(t, pool) // cleanup also drops any token a failed run minted

	srv, cleanup, err := buildServer(context.Background(), cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	id := uuid.New()
	xff := fmt.Sprintf("10.%d.%d.%d", id[0], id[1], id[2])
	ctx := context.Background()
	t.Cleanup(func() {
		for _, pat := range []string{"*" + xff + "*", "*" + email + "*"} {
			if keys, _ := rdb.Keys(ctx, pat).Result(); len(keys) > 0 {
				rdb.Del(ctx, keys...)
			}
		}
	})
	// Spend the forwarded address's whole hourly allowance (30), so the one
	// real request is refused if, and only if, it is counted against that key.
	lim := redis_rate.NewLimiter(rdb)
	for i := 0; i < 30; i++ {
		if _, err := lim.Allow(ctx, "magic_link:ip:"+xff, redis_rate.PerHour(30)); err != nil {
			t.Fatal(err)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/magic-link/request", strings.NewReader(`{"email":"`+email+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", xff)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: the limiter did not see the forwarded address %s", resp.StatusCode, xff)
	}
}

func TestBuildServer_IngestControlsAreInstalled(t *testing.T) {
	t.Run("hop limit", func(t *testing.T) {
		cfg := serverEnv(t)
		cfg.MaxWebhookHops = 2
		cfg.IngestRateLimitRPS = 0
		w := newWired(t, cfg, 0, 0)
		if code, body := w.ingest(map[string]string{"Dstream-Webhook-Hops": "1"}); code >= 300 {
			t.Fatalf("below the limit = %d %q, want success", code, body)
		}
		if code, body := w.ingest(map[string]string{"Dstream-Webhook-Hops": "2"}); code != http.StatusForbidden || !strings.Contains(body, "loop detected") {
			t.Fatalf("at the limit = %d %q, want 403 loop detected", code, body)
		}
	})
	t.Run("rate limit", func(t *testing.T) {
		cfg := serverEnv(t)
		cfg.IngestRateLimitRPS = 1
		cfg.IngestRateLimitBurst = 1
		w := newWired(t, cfg, 0, 0)
		// Burst 1 at 1/s: three back-to-back requests cannot all pass unless
		// the limiter is missing (each would have to take over a second).
		limited := false
		for i := 0; i < 3 && !limited; i++ {
			code, body := w.ingest(nil)
			limited = code == http.StatusTooManyRequests && strings.Contains(body, "rate limited")
		}
		if !limited {
			t.Fatal("three back-to-back requests at 1 rps burst 1 were never rate limited")
		}
	})
	t.Run("quota", func(t *testing.T) {
		cfg := serverEnv(t)
		cfg.IngestRateLimitRPS = 0
		w := newWired(t, cfg, 2, 0) // the gate refuses the request that reaches the ceiling
		if code, body := w.ingest(nil); code >= 300 {
			t.Fatalf("first = %d %q, want success", code, body)
		}
		if code, body := w.ingest(nil); code != http.StatusTooManyRequests || !strings.Contains(body, "quota exceeded") {
			t.Fatalf("second = %d %q, want 429 quota exceeded", code, body)
		}
	})
}

// The publish gate is wired inside api.Mount, so this reaches it through the
// real server and the real session + CSRF chain.
func TestBuildServer_PublishQuotaIsInstalled(t *testing.T) {
	w := newWired(t, serverEnv(t), 0, 2)
	if code, body := w.send(http.MethodPost, "/api/event-types", map[string]any{"name": "invoice.paid"}, true); code >= 300 {
		t.Fatalf("event type = %d %q", code, body)
	}
	code, body := w.send(http.MethodPost, "/api/applications", map[string]any{"name": "A"}, true)
	var app struct {
		ID string `json:"id"`
	}
	if code >= 300 || json.Unmarshal([]byte(body), &app) != nil || app.ID == "" {
		t.Fatalf("application = %d %q", code, body)
	}
	publish := func() (int, string) {
		return w.send(http.MethodPost, "/api/applications/"+app.ID+"/messages",
			map[string]any{"event_type": "invoice.paid", "payload": map[string]any{"n": 1}}, true)
	}
	if code, body := publish(); code != http.StatusAccepted {
		t.Fatalf("first publish = %d %q, want 202", code, body)
	}
	if code, body := publish(); code != http.StatusTooManyRequests || !strings.Contains(body, "quota exceeded") {
		t.Fatalf("second publish = %d %q, want 429 quota exceeded", code, body)
	}
}

// Deps.AllowPrivateDestinations reaches the bookmark replay-to client: with
// the opt-in the loopback target is hit, without it the guard refuses.
func TestBuildServer_PrivateDestinationOptInReachesReplayClient(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow=%v", allow), func(t *testing.T) {
			var hits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				rw.WriteHeader(http.StatusNoContent)
			}))
			defer target.Close()

			cfg := serverEnv(t)
			cfg.IngestRateLimitRPS = 0
			cfg.AllowPrivateDestinations = allow
			w := newWired(t, cfg, 0, 0)
			if code, body := w.ingest(nil); code >= 300 {
				t.Fatalf("capture = %d %q", code, body)
			}
			var bookmarkID uuid.UUID
			if err := w.pool.QueryRow(context.Background(), `
WITH r AS (SELECT id FROM requests WHERE source_id = $1 LIMIT 1)
INSERT INTO bookmarks (org_id, request_id, name) SELECT $2, r.id, 'bm' FROM r RETURNING id`,
				w.srcID, w.orgID).Scan(&bookmarkID); err != nil {
				t.Fatalf("bookmark: %v", err)
			}
			code, body := w.send(http.MethodPost, "/api/bookmarks/"+bookmarkID.String()+"/replay-to", map[string]any{"url": target.URL}, true)
			if allow && (code != http.StatusOK || hits.Load() != 1) {
				t.Fatalf("opt-in: %d %q hits=%d, want 200 and one hit", code, body, hits.Load())
			}
			if !allow && (code != http.StatusBadGateway || !strings.Contains(body, "ssrf-guard") || hits.Load() != 0) {
				t.Fatalf("guarded: %d %q hits=%d, want 502 ssrf-guard and no hit", code, body, hits.Load())
			}
		})
	}
}
