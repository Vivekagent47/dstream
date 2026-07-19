package config

import (
	"fmt"
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

	DB     DBConfig     `mapstructure:"db"`
	Redis  RedisConfig  `mapstructure:"redis"`
	Worker WorkerConfig `mapstructure:"worker"`
	SMTP   SMTPConfig   `mapstructure:"smtp"`
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
	Concurrency       int `mapstructure:"concurrency"`
	PerOrgMaxInflight int `mapstructure:"per_org_max_inflight"`
}

type SMTPConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
	User string `mapstructure:"user"`
	Pass string `mapstructure:"pass"`
	From string `mapstructure:"from"`
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
	v.SetDefault("session_secret", "")
	v.SetDefault("magic_link_ttl", "15m")
	v.SetDefault("trusted_proxies", []string{})
	// Secure by default: session/CSRF cookies get the Secure attribute unless
	// explicitly opted out for local HTTP dev (DSTREAM_COOKIE_SECURE=false).
	v.SetDefault("cookie_secure", true)
	v.SetDefault("dev_mode", false)
	v.SetDefault("allow_private_destinations", false)
	v.SetDefault("ingest_rate_limit_rps", 100)
	v.SetDefault("ingest_rate_limit_burst", 200)

	v.SetDefault("db.url", "postgres://dstream:dstream@localhost:5432/dstream?sslmode=disable")
	v.SetDefault("db.max_conns", 20)

	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)

	v.SetDefault("worker.concurrency", 50)
	// 0 = disabled: no per-org cap (single-tenant self-host uses the full pool).
	// Set > 0 (e.g. 20) in multi-tenant deployments so one org can't starve others.
	v.SetDefault("worker.per_org_max_inflight", 0)
	v.SetDefault("smtp.host", "")
	v.SetDefault("smtp.port", 587)
	v.SetDefault("smtp.user", "")
	v.SetDefault("smtp.pass", "")
	v.SetDefault("smtp.from", "noreply@localhost")

	var c Config
	if err := v.Unmarshal(&c, viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToSliceHookFunc(","),
		mapstructure.StringToTimeDurationHookFunc(),
	))); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}
	return c, nil
}
