package ingest

import (
	"context"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

// BodyStore persists raw webhook payloads, keyed by request id.
//
// Phase 1 ships a Postgres-backed implementation. The interface lets us swap
// to S3/MinIO without touching ingest or delivery logic.
type BodyStore interface {
	Put(ctx context.Context, requestID uuid.UUID, body []byte) (ref string, err error)
	Get(ctx context.Context, ref string) ([]byte, error)
}

type pgBodyStore struct {
	q *store.Queries
}

func NewPostgresBodyStore(q *store.Queries) BodyStore {
	return &pgBodyStore{q: q}
}

func (s *pgBodyStore) Put(ctx context.Context, requestID uuid.UUID, body []byte) (string, error) {
	if err := s.q.InsertRequestBody(ctx, store.InsertRequestBodyParams{
		RequestID: store.UUID(requestID),
		Body:      body,
	}); err != nil {
		return "", err
	}
	return "pg:" + requestID.String(), nil
}

func (s *pgBodyStore) Get(ctx context.Context, ref string) ([]byte, error) {
	// ref format: "pg:<uuid>"
	const prefix = "pg:"
	if len(ref) <= len(prefix) || ref[:len(prefix)] != prefix {
		return nil, ErrUnknownBodyRef
	}
	id, err := uuid.Parse(ref[len(prefix):])
	if err != nil {
		return nil, err
	}
	return s.q.GetRequestBody(ctx, store.UUID(id))
}
