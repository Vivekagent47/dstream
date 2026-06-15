package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/streamingo/dstream/internal/store"
)

type ctxKey int

const (
	ctxKeyProjectID ctxKey = iota
	ctxKeyAPIKey
	ctxKeyUserID
)

// Principal carries the authenticated identity attached to a request.
// Exactly one of ProjectID or UserID will be set per request, depending on
// which credential the caller presented.
type Principal struct {
	ProjectID uuid.UUID // set when authenticated via API key
	APIKeyID  uuid.UUID
	UserID    uuid.UUID // set when authenticated via session cookie
}

// APIKeyOnly requires a valid API key. Use for ingest/admin/control endpoints
// that don't accept session cookies.
func APIKeyOnly(q *store.Queries) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := ExtractAPIKey(r.Header.Get("Authorization"))
			row, err := VerifyAPIKey(r.Context(), q, raw)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := contextWithPrincipal(r.Context(), Principal{
				ProjectID: store.GoUUID(row.ProjectID),
				APIKeyID:  store.GoUUID(row.ID),
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// SessionOnly requires a valid session cookie. Use for dashboard routes.
func SessionOnly(s *SessionSigner) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, err := s.Parse(r)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := contextWithPrincipal(r.Context(), Principal{UserID: uid})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// APIKeyOrSession accepts either credential type. Useful for shared API
// endpoints that the dashboard and CI scripts both call.
func APIKeyOrSession(q *store.Queries, s *SessionSigner) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if raw := ExtractAPIKey(r.Header.Get("Authorization")); raw != "" {
				if row, err := VerifyAPIKey(r.Context(), q, raw); err == nil {
					ctx := contextWithPrincipal(r.Context(), Principal{
						ProjectID: store.GoUUID(row.ProjectID),
						APIKeyID:  store.GoUUID(row.ID),
					})
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
			if uid, err := s.Parse(r); err == nil {
				ctx := contextWithPrincipal(r.Context(), Principal{UserID: uid})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
}

// SuperAdminOnly requires a session belonging to a user with is_super_admin.
func SuperAdminOnly(q *store.Queries, s *SessionSigner) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, err := s.Parse(r)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			u, err := q.GetUserByID(r.Context(), store.UUID(uid))
			if err != nil || !u.IsSuperAdmin {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			ctx := contextWithPrincipal(r.Context(), Principal{UserID: uid})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func contextWithPrincipal(parent context.Context, p Principal) context.Context {
	if p.ProjectID != uuid.Nil {
		parent = context.WithValue(parent, ctxKeyProjectID, p.ProjectID)
	}
	if p.UserID != uuid.Nil {
		parent = context.WithValue(parent, ctxKeyUserID, p.UserID)
	}
	if p.APIKeyID != uuid.Nil {
		parent = context.WithValue(parent, ctxKeyAPIKey, p.APIKeyID)
	}
	return parent
}

// FromContext recovers the principal attached by an auth middleware.
func FromContext(ctx context.Context) (Principal, error) {
	p := Principal{}
	if v, ok := ctx.Value(ctxKeyProjectID).(uuid.UUID); ok {
		p.ProjectID = v
	}
	if v, ok := ctx.Value(ctxKeyUserID).(uuid.UUID); ok {
		p.UserID = v
	}
	if v, ok := ctx.Value(ctxKeyAPIKey).(uuid.UUID); ok {
		p.APIKeyID = v
	}
	if p.ProjectID == uuid.Nil && p.UserID == uuid.Nil {
		return p, errors.New("no principal in context")
	}
	return p, nil
}
