package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// signingConfig is the per-source signature-verification config stored in
// sources.signing_config (JSONB). An empty Scheme means verification is not
// configured for the source.
type signingConfig struct {
	Scheme   string `json:"scheme"`   // only "hmac_sha256" is supported
	Secret   string `json:"secret"`   // shared secret
	Header   string `json:"header"`   // request header carrying the signature
	Encoding string `json:"encoding"` // "hex" (default) | "base64"
	Prefix   string `json:"prefix"`   // optional literal prefix on the value, e.g. "sha256="
}

// verifySignature reports whether the request carries a valid HMAC signature
// over body per the source's signing config. It NEVER rejects a request — the
// caller records the result as requests.sig_verified (observability over
// enforcement, per the Phase-1 spec). Unconfigured sources (empty/invalid
// config) always return false.
func verifySignature(rawConfig []byte, headers http.Header, body []byte) bool {
	if len(rawConfig) == 0 {
		return false
	}
	var cfg signingConfig
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		return false
	}
	if cfg.Scheme != "hmac_sha256" || cfg.Secret == "" || cfg.Header == "" {
		return false
	}
	presented := strings.TrimSpace(headers.Get(cfg.Header))
	if presented == "" {
		return false
	}
	presented = strings.TrimSpace(strings.TrimPrefix(presented, cfg.Prefix))

	mac := hmac.New(sha256.New, []byte(cfg.Secret))
	mac.Write(body)
	sum := mac.Sum(nil)

	var expected string
	switch cfg.Encoding {
	case "base64":
		expected = base64.StdEncoding.EncodeToString(sum)
	default: // hex
		expected = hex.EncodeToString(sum)
	}
	return hmac.Equal([]byte(presented), []byte(expected))
}
