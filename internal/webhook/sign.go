// Package webhook is the outbound webhook engine: HMAC signing (Standard
// Webhooks spec) plus the queue-driven signed-delivery handler. It has no
// dependency on the HTTP API layer.
package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

const secretPrefix = "whsec_"

// Sign returns the `webhook-signature` header value for one endpoint secret:
// "v1,<base64(HMAC_SHA256(key, "{msgID}.{ts}.{payload}"))>".
func Sign(secret, msgID string, ts int64, payload []byte) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID))
	mac.Write([]byte("."))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// GenerateSecret mints a new endpoint signing secret: whsec_<base64(24 bytes)>.
func GenerateSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(b), nil
}

// ValidSecret reports whether s is a usable signing secret (decodable key).
func ValidSecret(s string) bool { _, err := decodeSecret(s); return err == nil }

// decodeSecret parses whsec_<base64> into raw HMAC key bytes. A secret without
// the prefix is treated as raw base64 (forward-compat with imported keys).
func decodeSecret(secret string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, secretPrefix))
	if err != nil {
		return nil, fmt.Errorf("decode secret: %w", err)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("empty secret key")
	}
	return key, nil
}
