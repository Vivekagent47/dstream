package pipeline

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/bookmark"
	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/store"
)

// isUniqueViolation reports a Postgres 23505 (unique_violation). (pipeline has
// no copy of this yet; outbound/identity define their own.)
func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

type createBookmarkReq struct {
	RequestID   string   `json:"request_id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// bookmarkView is the API shape for a single bookmark (create/get).
func bookmarkView(b store.Bookmark) map[string]any {
	return map[string]any{
		"id":          store.GoUUID(b.ID).String(),
		"request_id":  store.GoUUID(b.RequestID).String(),
		"name":        b.Name,
		"description": b.Description,
		"tags":        b.Tags,
		"created_at":  b.CreatedAt.Time,
	}
}

// bookmarkListView adds the captured-request summary (list).
func bookmarkListView(row store.ListBookmarksForOrgRow) map[string]any {
	return map[string]any{
		"id":          store.GoUUID(row.ID).String(),
		"request_id":  store.GoUUID(row.RequestID).String(),
		"name":        row.Name,
		"description": row.Description,
		"tags":        row.Tags,
		"created_at":  row.CreatedAt.Time,
		"source_id":   store.GoUUID(row.SourceID).String(),
		"http_method": row.HTTPMethod,
		"http_path":   row.HTTPPath,
		"captured_at": row.CapturedAt.Time,
	}
}

func (d Handlers) CreateBookmark(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	var body createBookmarkReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Name == "" {
		httpx.Err(w, http.StatusBadRequest, "name required")
		return
	}
	reqID, err := uuid.Parse(body.RequestID)
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid request_id")
		return
	}
	// Confirm the request is in the caller's org (source→org join). ErrNoRows =
	// not found / not theirs → 404 (do not leak other orgs' requests).
	if _, err := d.Queries.GetRequestForReplay(r.Context(), store.GetRequestForReplayParams{
		ID: store.UUID(reqID), OrgID: store.UUID(p.OrgID),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "request not found")
			return
		}
		d.Log.Error("bookmark: lookup request", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "lookup request")
		return
	}
	tags := body.Tags
	if tags == nil {
		tags = []string{}
	}
	row, err := d.Queries.CreateBookmark(r.Context(), store.CreateBookmarkParams{
		OrgID:       store.UUID(p.OrgID),
		RequestID:   store.UUID(reqID),
		Name:        body.Name,
		Description: body.Description,
		Tags:        tags,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Err(w, http.StatusConflict, "bookmark name already in use")
			return
		}
		d.Log.Error("create bookmark", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create bookmark")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, bookmarkView(row))
}

func (d Handlers) ListBookmarks(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	params := store.ListBookmarksForOrgParams{OrgID: store.UUID(p.OrgID)}
	if s := r.URL.Query().Get("source_id"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			httpx.Err(w, http.StatusBadRequest, "invalid source_id")
			return
		}
		params.SourceID = store.UUID(id)
	}
	if tag := r.URL.Query().Get("tag"); tag != "" {
		params.Tag = &tag
	}
	rows, err := d.Queries.ListBookmarksForOrg(r.Context(), params)
	if err != nil {
		d.Log.Error("list bookmarks", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "list bookmarks")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, bookmarkListView(row))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (d Handlers) GetBookmark(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid bookmark id")
		return
	}
	row, err := d.Queries.GetBookmarkForOrg(r.Context(), store.GetBookmarkForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "bookmark not found")
			return
		}
		d.Log.Error("get bookmark", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "get bookmark")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, bookmarkView(row))
}

func (d Handlers) DeleteBookmark(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid bookmark id")
		return
	}
	n, err := d.Queries.DeleteBookmarkForOrg(r.Context(), store.DeleteBookmarkForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		d.Log.Error("delete bookmark", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "delete bookmark")
		return
	}
	if n == 0 {
		httpx.Err(w, http.StatusNotFound, "bookmark not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ReplayBookmark fans a bookmarked (captured) request back out through its
// source's enabled connections as new test events.
func (d Handlers) ReplayBookmark(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid bookmark id")
		return
	}
	bm, err := d.Queries.GetBookmarkForOrg(r.Context(), store.GetBookmarkForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "bookmark not found")
			return
		}
		d.Log.Error("replay: get bookmark", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "replay")
		return
	}
	// Look up the request to get its source_id (and confirm still in-org).
	req, err := d.Queries.GetRequestForReplay(r.Context(), store.GetRequestForReplayParams{
		ID: bm.RequestID, OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "captured request no longer exists")
			return
		}
		d.Log.Error("replay: get request", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "replay")
		return
	}
	ids, err := bookmark.Reinject(r.Context(), d.Queries, d.Queue,
		store.GoUUID(bm.RequestID), store.GoUUID(req.SourceID), p.OrgID)
	if err != nil {
		d.Log.Error("replay: reinject", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "replay")
		return
	}
	out := make([]string, 0, len(ids))
	for _, eid := range ids {
		out = append(out, eid.String())
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"event_ids": out})
}

// loadBookmarkRequest reconstructs the captured request (headers + body) behind
// a bookmark. Writes the error response and returns ok=false on 404 (request
// gone) or 410 (body expunged).
func (d Handlers) loadBookmarkRequest(w http.ResponseWriter, r *http.Request, requestID pgtype.UUID, orgID uuid.UUID) (bookmark.Request, bool) {
	req, err := d.Queries.GetRequestForReplay(r.Context(), store.GetRequestForReplayParams{
		ID: requestID, OrgID: store.UUID(orgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "captured request no longer exists")
			return bookmark.Request{}, false
		}
		d.Log.Error("bookmark: load request", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "load request")
		return bookmark.Request{}, false
	}
	body, err := d.BodyStore.Get(r.Context(), req.BodyRef)
	if err != nil {
		// Body was expunged by the retention sweep (a pre-pin bookmark) or is
		// otherwise unavailable — cannot replay/export.
		httpx.Err(w, http.StatusGone, "captured payload no longer stored")
		return bookmark.Request{}, false
	}
	var hdrs map[string][]string
	if len(req.Headers) > 0 {
		_ = json.Unmarshal(req.Headers, &hdrs)
	}
	ct := ""
	if req.ContentType != nil {
		ct = *req.ContentType
	}
	return bookmark.Request{
		SourceID:    store.GoUUID(req.SourceID),
		Method:      req.HTTPMethod,
		Path:        req.HTTPPath,
		Headers:     hdrs,
		Body:        body,
		ContentType: ct,
	}, true
}

type replayToReq struct {
	URL string `json:"url"`
}

// ReplayBookmarkTo replays a bookmarked request to an arbitrary caller-supplied
// URL, through the SSRF-guarded Replayer client.
func (d Handlers) ReplayBookmarkTo(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid bookmark id")
		return
	}
	var body replayToReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := deliver.ValidateDestinationURL(body.URL); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid url: "+err.Error())
		return
	}
	bm, err := d.Queries.GetBookmarkForOrg(r.Context(), store.GetBookmarkForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "bookmark not found")
			return
		}
		d.Log.Error("replay-to: get bookmark", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "replay-to")
		return
	}
	breq, ok := d.loadBookmarkRequest(w, r, bm.RequestID, p.OrgID)
	if !ok {
		return
	}
	resp, err := bookmark.ReplayTo(r.Context(), d.Replayer, breq, body.URL)
	if err != nil {
		// Target unreachable or blocked by the SSRF guard (loopback/private).
		httpx.Err(w, http.StatusBadGateway, "replay target failed: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"status":        resp.Status,
		"duration_ms":   resp.DurationMs,
		"response_body": string(resp.Body),
	})
}

// safeFilename reduces a user-controlled bookmark name to a safe download
// filename: keep [A-Za-z0-9-_.], replace everything else with '_'. Falls back
// to "fixture" if nothing survives.
func safeFilename(name string) string {
	f := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, name)
	if f == "" || f == "." || f == ".." {
		return "fixture"
	}
	return f
}

// ExportBookmark returns a bookmarked request as a portable JSON fixture
// (method/path/headers/content_type/body_base64), downloadable via
// Content-Disposition.
func (d Handlers) ExportBookmark(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid bookmark id")
		return
	}
	bm, err := d.Queries.GetBookmarkForOrg(r.Context(), store.GetBookmarkForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "bookmark not found")
			return
		}
		d.Log.Error("export: get bookmark", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "export")
		return
	}
	breq, ok := d.loadBookmarkRequest(w, r, bm.RequestID, p.OrgID)
	if !ok {
		return
	}
	out, err := bookmark.Export(breq)
	if err != nil {
		d.Log.Error("export: marshal", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "export")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+safeFilename(bm.Name)+`.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
