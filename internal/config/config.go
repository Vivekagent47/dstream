package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

type Config struct {
	HTTPAddr       string        `mapstructure:"http_addr"`
	LogLevel       string        `mapstructure:"log_level"`
	LogFormat      string        `mapstructure:"log_format"`
	PublicBaseURL  string        `mapstructure:"public_base_url"`
	AppBaseURL     string        `mapstructure:"app_base_url"`
	SessionSecret  string        `mapstructure:"session_secret"`
	MagicLinkTTL   time.Duration `mapstructure:"magic_link_ttl"`
	TrustedProxies []string      `mapstructure:"trusted_proxies"`
	CookieSecure   bool          `mapstructure:"cookie_secure"`
	// AllowPrivateDestinations disables the outbound SSRF guard, permitting the
	// delivery worker to POST to loopback/private/link-local addresses. NEVER
	// enable this on a multi-tenant/SaaS deployment — it re-opens delivery to
	// cloud metadata endpoints and internal services. Only for self-hosters who
	// legitimately deliver to private ranges. Defaults false (blocked).
	AllowPrivateDestinations bool `mapstructure:"allow_private_destinations"`
	// IngestRateLimitRPS caps ingest requests per source per second (token
	// bucket in Redis). 0 disables. Burst defaults to 2x RPS when unset.
	// Protects Postgres/queue from a single source (or leaked token) flooding.
	IngestRateLimitRPS   int `mapstructure:"ingest_rate_limit_rps"`
	IngestRateLimitBurst int `mapstructure:"ingest_rate_limit_burst"`
	// DevMode unlocks dev-only conveniences that are NEVER safe in
	// production — most importantly, logging plaintext magic-link and
	// invite tokens to the server log so the developer can copy-paste them
	// out of stdout instead of wiring SMTP. Must be explicitly opted into
	// via DSTREAM_DEV_MODE=true.
	DevMode bool `mapstructure:"dev_mode"`

	// Loop guard. SelfHosts are dstream's own hostnames; a webhook target
	// (endpoint or destination) pointing at one is rejected. Derived from
	// PublicBaseURL + AppBaseURL plus explicit DSTREAM_SELF_HOSTS (comma list).
	SelfHostsRaw   string   `mapstructure:"self_hosts"`
	MaxWebhookHops int      `mapstructure:"max_webhook_hops"`
	SelfHosts      []string `mapstructure:"-"` // computed in Load

	// WebhookSecretGrace is how long a rotated endpoint's previous signing
	// secret stays valid after a rotate, so receivers can cut over without a
	// missed delivery.
	WebhookSecretGrace time.Duration `mapstructure:"webhook_secret_grace"`

	// PortalTokenTTL is how long a minted App Portal link stays valid.
	PortalTokenTTL time.Duration `mapstructure:"portal_token_ttl"`

	// EndpointMaxConsecutiveFailures auto-disables an endpoint once this many
	// deliveries dead-letter back-to-back; a successful delivery resets the run.
	EndpointMaxConsecutiveFailures int `mapstructure:"endpoint_max_consecutive_failures"`

	// PayloadRetention nulls stored payloads older than this window — outbound
	// message payloads + delivery attempt bodies, and inbound request bodies +
	// attempt bodies. 0 / unset = keep forever.
	PayloadRetention time.Duration `mapstructure:"payload_retention"`

	// TransformTimeout bounds a single delivery-time JS transform's wall-clock
	// run; TransformMaxOutput caps its output in bytes. Both flow into the
	// outbound + deliver handlers.
	TransformTimeout   time.Duration `mapstructure:"transform_timeout"`
	TransformMaxOutput int           `mapstructure:"transform_max_output"`

	DB     DBConfig     `mapstructure:"db"`
	Redis  RedisConfig  `mapstructure:"redis"`
	Worker WorkerConfig `mapstructure:"worker"`
	SMTP   SMTPConfig   `mapstructure:"smtp"`
	OIDC   OIDCConfig   `mapstructure:"oidc"`

	Tracing TracingConfig `mapstructure:"tracing"`
}

type DBConfig struct {
	URL      string `mapstructure:"url"`
	MaxConns int    `mapstructure:"max_conns"`
}

type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type WorkerConfig struct {
	Concurrency       int           `mapstructure:"concurrency"`
	PerOrgMaxInflight int           `mapstructure:"per_org_max_inflight"`
	HealthAddr        string        `mapstructure:"health_addr"`
	StallTimeout      time.Duration `mapstructure:"stall_timeout"`
}

type SMTPConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
	User string `mapstructure:"user"`
	Pass string `mapstructure:"pass"`
	From string `mapstructure:"from"`
}

// OIDCConfig configures instance-level single sign-on: one IdP for the whole
// deployment. Per-org IdP connections are deliberately out of scope — see the
// phase 5b design doc.
//
// SSO is enabled by a non-empty Issuer. There is deliberately no separate
// boolean, which could drift out of sync with the credentials.
type OIDCConfig struct {
	Issuer       string   `mapstructure:"issuer"`
	ClientID     string   `mapstructure:"client_id"`
	ClientSecret string   `mapstructure:"client_secret"`
	Scopes       []string `mapstructure:"scopes"`
	// DefaultOrgSlug, when set, joins first-time SSO users to that org at
	// DefaultRole instead of minting each one a personal workspace — the
	// common self-host shape, where everyone from the IdP works at one company.
	DefaultOrgSlug string `mapstructure:"default_org"`
	DefaultRole    string `mapstructure:"default_role"`
	// Enforce refuses magic-link requests so they cannot be used to bypass
	// IdP policy. `dstream admin magic-link` remains as a break-glass, since
	// a broken IdP would otherwise lock every human out of the deployment.
	Enforce bool `mapstructure:"enforce"`
}

// Enabled reports whether SSO is configured at all.
func (o OIDCConfig) Enabled() bool { return o.Issuer != "" }

// ValidateOIDC rejects configurations that would fail confusingly later —
// inside the auth path at someone's first login attempt, rather than at boot.
func (c Config) ValidateOIDC() error {
	o := c.OIDC
	if !o.Enabled() {
		if o.Enforce {
			return fmt.Errorf("config: DSTREAM_OIDC_ENFORCE is set without DSTREAM_OIDC_ISSUER — that disables every way to log in")
		}
		return nil
	}
	if o.ClientID == "" {
		return fmt.Errorf("config: DSTREAM_OIDC_ISSUER is set but DSTREAM_OIDC_CLIENT_ID is empty")
	}
	if o.ClientSecret == "" {
		return fmt.Errorf("config: DSTREAM_OIDC_ISSUER is set but DSTREAM_OIDC_CLIENT_SECRET is empty")
	}
	switch o.DefaultRole {
	case "", "member", "admin":
	case "owner":
		return fmt.Errorf("config: DSTREAM_OIDC_DEFAULT_ROLE cannot be 'owner' — owner is reserved for explicit promotion")
	default:
		return fmt.Errorf("config: DSTREAM_OIDC_DEFAULT_ROLE %q must be 'member' or 'admin'", o.DefaultRole)
	}
	return nil
}

type TracingConfig struct {
	Enabled      bool    `mapstructure:"enabled"`
	OTLPEndpoint string  `mapstructure:"otlp_endpoint"`
	ServiceName  string  `mapstructure:"service_name"`
	SampleRatio  float64 `mapstructure:"sample_ratio"`
}

func Load() (Config, error) {
	v := viper.New()
	v.SetEnvPrefix("DSTREAM")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	v.SetDefault("http_addr", ":8080")
	v.SetDefault("log_level", "info")
	v.SetDefault("log_format", "json")
	v.SetDefault("public_base_url", "http://localhost:8080")
	// Frontend/SPA origin used to build user-facing links in emails (magic-link
	// verify, invite). Empty => falls back to public_base_url after load. Set it
	// when the web app is a separate origin from the API (e.g. dev web on :3000).
	v.SetDefault("app_base_url", "")
	v.SetDefault("session_secret", "")
	v.SetDefault("magic_link_ttl", "15m")
	v.SetDefault("portal_token_ttl", "168h")
	v.SetDefault("trusted_proxies", []string{})
	// Secure by default: session/CSRF cookies get the Secure attribute unless
	// explicitly opted out for local HTTP dev (DSTREAM_COOKIE_SECURE=false).
	v.SetDefault("cookie_secure", true)
	v.SetDefault("dev_mode", false)
	v.SetDefault("allow_private_destinations", false)
	v.SetDefault("ingest_rate_limit_rps", 100)
	v.SetDefault("ingest_rate_limit_burst", 200)
	v.SetDefault("self_hosts", "")
	v.SetDefault("max_webhook_hops", 3)
	v.SetDefault("webhook_secret_grace", "24h")
	v.SetDefault("endpoint_max_consecutive_failures", 5)
	// "0s" = keep payloads forever. The zero default is load-bearing: it registers
	// the key so viper's AutomaticEnv+Unmarshal actually reads
	// DSTREAM_PAYLOAD_RETENTION (see the tracing.otlp_endpoint note below).
	v.SetDefault("payload_retention", "0s")
	// Defaults registered so viper's AutomaticEnv reads DSTREAM_TRANSFORM_*
	// (same rationale as the payload_retention default above).
	v.SetDefault("transform_timeout", "1s")
	v.SetDefault("transform_max_output", 5242880)

	v.SetDefault("db.url", "postgres://dstream:dstream@localhost:5432/dstream?sslmode=disable")
	v.SetDefault("db.max_conns", 20)

	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)

	v.SetDefault("worker.concurrency", 50)
	// 0 = disabled: no per-org cap (single-tenant self-host uses the full pool).
	// Set > 0 (e.g. 20) in multi-tenant deployments so one org can't starve others.
	v.SetDefault("worker.per_org_max_inflight", 0)
	v.SetDefault("worker.health_addr", ":8081")
	v.SetDefault("worker.stall_timeout", "60s")
	v.SetDefault("smtp.host", "")
	v.SetDefault("smtp.port", 587)
	v.SetDefault("smtp.user", "")
	v.SetDefault("smtp.pass", "")
	v.SetDefault("smtp.from", "noreply@localhost")

	// Every oidc.* key needs a registered default, even the empty ones: viper's
	// AutomaticEnv+Unmarshal only reads env for keys it already knows (same
	// gotcha as tracing.otlp_endpoint below). Without these lines
	// DSTREAM_OIDC_CLIENT_SECRET is silently dropped and SSO fails at a user's
	// first login with an empty-credential error from the IdP.
	v.SetDefault("oidc.issuer", "")
	v.SetDefault("oidc.client_id", "")
	v.SetDefault("oidc.client_secret", "")
	v.SetDefault("oidc.scopes", "openid,email,profile")
	v.SetDefault("oidc.default_org", "")
	v.SetDefault("oidc.default_role", "member")
	v.SetDefault("oidc.enforce", false)

	v.SetDefault("tracing.enabled", false)
	// Empty default is load-bearing: viper's AutomaticEnv+Unmarshal only reads
	// env for keys it already knows (from a default/BindEnv). Without this line
	// DSTREAM_TRACING_OTLP_ENDPOINT is silently ignored and the exporter falls
	// back to localhost:4318 — so compose's http://jaeger:4318 never takes.
	v.SetDefault("tracing.otlp_endpoint", "")
	v.SetDefault("tracing.service_name", "dstream")
	v.SetDefault("tracing.sample_ratio", 1.0)

	var c Config
	if err := v.Unmarshal(&c, viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToSliceHookFunc(","),
		mapstructure.StringToTimeDurationHookFunc(),
	))); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}
	// Single-origin deployments (API also serves the SPA) need no separate app
	// URL — default it to the API origin.
	if c.AppBaseURL == "" {
		c.AppBaseURL = c.PublicBaseURL
	}
	// Loop guard: collect dstream's own hostnames from the base URLs + explicit list.
	hostSet := map[string]struct{}{}
	for _, raw := range []string{c.PublicBaseURL, c.AppBaseURL} {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			hostSet[strings.ToLower(u.Hostname())] = struct{}{}
		}
	}
	for _, h := range strings.Split(c.SelfHostsRaw, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			hostSet[h] = struct{}{}
		}
	}
	c.SelfHosts = make([]string, 0, len(hostSet))
	for h := range hostSet {
		c.SelfHosts = append(c.SelfHosts, h)
	}
	if c.MaxWebhookHops <= 0 {
		c.MaxWebhookHops = 3
	}
	if c.EndpointMaxConsecutiveFailures <= 0 {
		c.EndpointMaxConsecutiveFailures = 5
	}
	return c, nil
}
