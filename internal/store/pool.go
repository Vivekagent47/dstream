package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func NewPool(ctx context.Context, dsn string, maxConns int) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse db dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = int32(maxConns)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect db: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return pool, nil
}

// UUID wraps a google/uuid.UUID into pgtype.UUID. uuid.Nil maps to SQL
// NULL (Valid=false) — NOT to the all-zeros UUID. Any code path that
// accidentally lets uuid.Nil slip into a sqlc parameter then fails fast
// against NOT NULL / FK constraints instead of silently matching the
// all-zeros sentinel. Callers that genuinely need a "no row" predicate
// already pass pgtype.UUID{} directly.
func UUID(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

// GoUUID unwraps pgtype.UUID into google/uuid.UUID. Returns uuid.Nil if invalid.
func GoUUID(p pgtype.UUID) uuid.UUID {
	if !p.Valid {
		return uuid.Nil
	}
	return uuid.UUID(p.Bytes)
}
