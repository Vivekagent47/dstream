package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/config"
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
