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
	// DevMode unlocks dev-only conveniences that are NEVER safe in
	// production — most importantly, logging plaintext magic-link and
	// invite tokens to the server log so the developer can copy-paste them
	// out of stdout instead of wiring SMTP. Must be explicitly opted into
	// via DSTREAM_DEV_MODE=true.
	DevMode bool `mapstructure:"dev_mode"`

	DB     DBConfig     `mapstructure:"db"`
	Redis  RedisConfig  `mapstructure:"redis"`
	S3     S3Config     `mapstructure:"s3"`
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

type S3Config struct {
	Endpoint     string `mapstructure:"endpoint"`
	Region       string `mapstructure:"region"`
	Bucket       string `mapstructure:"bucket"`
	AccessKey    string `mapstructure:"access_key"`
	SecretKey    string `mapstructure:"secret_key"`
	UsePathStyle bool   `mapstructure:"use_path_style"`
}

type WorkerConfig struct {
	Concurrency int `mapstructure:"concurrency"`
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
	v.SetDefault("cookie_secure", false)
	v.SetDefault("dev_mode", false)

	v.SetDefault("db.url", "postgres://dstream:dstream@localhost:5432/dstream?sslmode=disable")
	v.SetDefault("db.max_conns", 20)

	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)

	v.SetDefault("s3.endpoint", "http://localhost:9000")
	v.SetDefault("s3.region", "us-east-1")
	v.SetDefault("s3.bucket", "dstream-bodies")
	v.SetDefault("s3.access_key", "minioadmin")
	v.SetDefault("s3.secret_key", "minioadmin")
	v.SetDefault("s3.use_path_style", true)

	v.SetDefault("worker.concurrency", 50)
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
