package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/streamingo/dstream/internal/store"
)

var ErrInvalidMagicToken = errors.New("auth: invalid or expired magic link")

const magicTokenBytes = 32

// IssueMagicLink generates a token, persists its hash, and returns the
// human-deliverable token (caller embeds it in an email link).
func IssueMagicLink(ctx context.Context, q *store.Queries, email string, ttl time.Duration) (token string, err error) {
	b := make([]byte, magicTokenBytes)
	if _, err = rand.Read(b); err != nil {
		return "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(token))

	if _, err = q.CreateMagicLinkToken(ctx, store.CreateMagicLinkTokenParams{
		Email:     email,
		TokenHash: h[:],
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(ttl), Valid: true},
	}); err != nil {
		return "", err
	}
	return token, nil
}

// ConsumeMagicLink validates a presented token and returns the user it
// belongs to, creating the user if they're new. The token is single-use.
func ConsumeMagicLink(ctx context.Context, q *store.Queries, token string) (store.User, error) {
	h := sha256.Sum256([]byte(token))
	row, err := q.GetActiveMagicLinkToken(ctx, h[:])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.User{}, ErrInvalidMagicToken
		}
		return store.User{}, err
	}
	if err := q.MarkMagicLinkUsed(ctx, row.ID); err != nil {
		return store.User{}, err
	}
	u, err := q.GetUserByEmail(ctx, row.Email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			u, err = q.CreateUser(ctx, store.CreateUserParams{Email: row.Email})
			if err != nil {
				return store.User{}, err
			}
		} else {
			return store.User{}, err
		}
	}
	return u, nil
}
