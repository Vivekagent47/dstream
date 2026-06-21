package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Vivekagent47/dstream/internal/store"
)

const (
	APIKeyPrefixLen = 12
	APIKeySecretLen = 32

	apiKeyHeader = "Authorization"
	apiKeyScheme = "Bearer "
	apiKeyMark   = "dsk_"
)

var (
	ErrInvalidAPIKey = errors.New("auth: invalid api key")
	ErrMissingAPIKey = errors.New("auth: missing api key")
)

// NewAPIKey returns a freshly generated key, the prefix (lookup), and the
// sha256 hash of the secret (to be stored).
func NewAPIKey() (full string, prefix string, hash []byte, err error) {
	pb := make([]byte, APIKeyPrefixLen)
	if _, err = rand.Read(pb); err != nil {
		return "", "", nil, err
	}
	prefix = base64.RawURLEncoding.EncodeToString(pb)[:APIKeyPrefixLen]

	sb := make([]byte, APIKeySecretLen)
	if _, err = rand.Read(sb); err != nil {
		return "", "", nil, err
	}
	secret := base64.RawURLEncoding.EncodeToString(sb)

	h := sha256.Sum256([]byte(secret))
	full = apiKeyMark + prefix + "_" + secret
	return full, prefix, h[:], nil
}

// parseAPIKey splits a raw key into its prefix and secret components.
func parseAPIKey(raw string) (prefix, secret string, ok bool) {
	if !strings.HasPrefix(raw, apiKeyMark) {
		return "", "", false
	}
	rest := strings.TrimPrefix(raw, apiKeyMark)
	parts := strings.SplitN(rest, "_", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ExtractAPIKey reads "Authorization: Bearer dsk_<prefix>_<secret>" from the
// header. Returns "" if no header was sent.
func ExtractAPIKey(authHeader string) string {
	if authHeader == "" {
		return ""
	}
	if !strings.HasPrefix(authHeader, apiKeyScheme) {
		return ""
	}
	return strings.TrimPrefix(authHeader, apiKeyScheme)
}

// VerifyAPIKey validates a raw key against the DB and returns the api key row.
// The store query lookups by prefix; we constant-time compare the secret hash.
func VerifyAPIKey(ctx context.Context, q *store.Queries, raw string) (store.ApiKey, error) {
	if raw == "" {
		return store.ApiKey{}, ErrMissingAPIKey
	}
	prefix, secret, ok := parseAPIKey(raw)
	if !ok {
		return store.ApiKey{}, ErrInvalidAPIKey
	}

	row, err := q.GetAPIKeyByPrefix(ctx, prefix)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ApiKey{}, ErrInvalidAPIKey
		}
		return store.ApiKey{}, fmt.Errorf("get api key: %w", err)
	}

	got := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(got[:], row.KeyHash) != 1 {
		return store.ApiKey{}, ErrInvalidAPIKey
	}

	_ = q.TouchAPIKey(ctx, row.ID)
	return row, nil
}
