package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/filter"
	"github.com/Vivekagent47/dstream/internal/store"
)

const (
	defaultCaptureRuleCap = 50
	minCaptureRuleCap     = 1
	maxCaptureRuleCap     = 1000
)

// captureRuleView is the API shape for a capture rule.
func captureRuleView(cr store.CaptureRule) map[string]any {
	return map[string]any{
		"id":          store.GoUUID(cr.ID).String(),
		"source_id":   store.GoUUID(cr.SourceID).String(),
		"name":        cr.Name,
		"filter_expr": cr.FilterExpr,
		"cap":         cr.Cap,
		"enabled":     cr.Enabled,
		"created_at":  cr.CreatedAt.Time,
		"updated_at":  cr.UpdatedAt.Time,
	}
}

// invalidateSourceCache best-effort evicts a source's ingest cache entry so a
// capture-rule change (filter/cap/enabled) takes effect immediately instead
// of waiting for the cache's own TTL. Lookup/evict failures are swallowed:
// this is a freshness nicety, not a correctness requirement.
func (d Handlers) invalidateSourceCache(ctx context.Context, orgID uuid.UUID, sourceID pgtype.UUID) {
	if d.EvictSourceCache == nil {
		return
	}
	src, err := d.Queries.GetSourceForOrg(ctx, store.GetSourceForOrgParams{ID: sourceID, OrgID: store.UUID(orgID)})
	if err != nil {
		return
	}
	d.EvictSourceCache(src.IngestToken)
}

func validCaptureRuleCap(cap int32) bool {
	return cap >= minCaptureRuleCap && cap <= maxCaptureRuleCap
}

type createCaptureRuleReq struct {
	SourceID   string  `json:"source_id"`
	Name       string  `json:"name"`
	FilterExpr *string `json:"filter_expr,omitempty"`
	Cap        *int32  `json:"cap,omitempty"`
}

func (d Handlers) CreateCaptureRule(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	var body createCaptureRuleReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Name == "" || body.SourceID == "" {
		httpx.Err(w, http.StatusBadRequest, "name and source_id are required")
		return
	}
	srcID, err := uuid.Parse(body.SourceID)
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid source_id")
		return
	}
	src, err := d.Queries.GetSourceForOrg(r.Context(), store.GetSourceForOrgParams{
		ID: store.UUID(srcID), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "source not found")
			return
		}
		d.Log.Error("create capture rule: lookup source", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "lookup source")
		return
	}
	if body.FilterExpr != nil && *body.FilterExpr != "" {
		if _, err := filter.Compile(*body.FilterExpr, false); err != nil {
			httpx.Err(w, http.StatusBadRequest, "invalid filter_expr: "+err.Error())
			return
		}
	}
	cap := int32(defaultCaptureRuleCap)
	if body.Cap != nil {
		cap = *body.Cap
	}
	if !validCaptureRuleCap(cap) {
		httpx.Err(w, http.StatusBadRequest, fmt.Sprintf("cap must be between %d and %d", minCaptureRuleCap, maxCaptureRuleCap))
		return
	}
	row, err := d.Queries.CreateCaptureRule(r.Context(), store.CreateCaptureRuleParams{
		OrgID: store.UUID(p.OrgID), SourceID: store.UUID(srcID),
		Name: body.Name, FilterExpr: body.FilterExpr, Cap: cap, Enabled: true,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Err(w, http.StatusConflict, "capture rule name already in use")
			return
		}
		d.Log.Error("create capture rule", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create capture rule")
		return
	}
	if d.EvictSourceCache != nil {
		d.EvictSourceCache(src.IngestToken)
	}
	httpx.WriteJSON(w, http.StatusCreated, captureRuleView(row))
}

func (d Handlers) ListCaptureRules(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	params := store.ListCaptureRulesForOrgParams{OrgID: store.UUID(p.OrgID)}
	if s := r.URL.Query().Get("source_id"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			httpx.Err(w, http.StatusBadRequest, "invalid source_id")
			return
		}
		params.SourceID = store.UUID(id)
	}
	rows, err := d.Queries.ListCaptureRulesForOrg(r.Context(), params)
	if err != nil {
		d.Log.Error("list capture rules", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "list capture rules")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, captureRuleView(row))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (d Handlers) GetCaptureRule(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid capture rule id")
		return
	}
	row, err := d.Queries.GetCaptureRuleForOrg(r.Context(), store.GetCaptureRuleForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "capture rule not found")
			return
		}
		d.Log.Error("get capture rule", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "get capture rule")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, captureRuleView(row))
}

type patchCaptureRuleReq struct {
	Name       *string `json:"name,omitempty"`
	FilterExpr *string `json:"filter_expr,omitempty"`
	Cap        *int32  `json:"cap,omitempty"`
	Enabled    *bool   `json:"enabled,omitempty"`
}

// PatchCaptureRule loads the current row, merges only the provided fields
// onto it, and writes the full merged set back — UpdateCaptureRule takes all
// columns (no partial SET), so a load-merge-update is required.
func (d Handlers) PatchCaptureRule(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid capture rule id")
		return
	}
	current, err := d.Queries.GetCaptureRuleForOrg(r.Context(), store.GetCaptureRuleForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "capture rule not found")
			return
		}
		d.Log.Error("patch capture rule: lookup", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "lookup capture rule")
		return
	}
	var body patchCaptureRuleReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	name, filterExpr, cap, enabled := current.Name, current.FilterExpr, current.Cap, current.Enabled
	if body.Name != nil {
		name = *body.Name
	}
	if body.FilterExpr != nil {
		filterExpr = body.FilterExpr
	}
	if body.Cap != nil {
		cap = *body.Cap
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	if filterExpr != nil && *filterExpr != "" {
		if _, err := filter.Compile(*filterExpr, false); err != nil {
			httpx.Err(w, http.StatusBadRequest, "invalid filter_expr: "+err.Error())
			return
		}
	}
	if !validCaptureRuleCap(cap) {
		httpx.Err(w, http.StatusBadRequest, fmt.Sprintf("cap must be between %d and %d", minCaptureRuleCap, maxCaptureRuleCap))
		return
	}
	row, err := d.Queries.UpdateCaptureRule(r.Context(), store.UpdateCaptureRuleParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
		Name: name, FilterExpr: filterExpr, Cap: cap, Enabled: enabled,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Err(w, http.StatusConflict, "capture rule name already in use")
			return
		}
		d.Log.Error("patch capture rule", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "patch capture rule")
		return
	}
	d.invalidateSourceCache(r.Context(), p.OrgID, row.SourceID)
	httpx.WriteJSON(w, http.StatusOK, captureRuleView(row))
}

func (d Handlers) DeleteCaptureRule(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid capture rule id")
		return
	}
	current, err := d.Queries.GetCaptureRuleForOrg(r.Context(), store.GetCaptureRuleForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "capture rule not found")
			return
		}
		d.Log.Error("delete capture rule: lookup", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "lookup capture rule")
		return
	}
	if _, err := d.Queries.DeleteCaptureRuleForOrg(r.Context(), store.DeleteCaptureRuleForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	}); err != nil {
		d.Log.Error("delete capture rule", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "delete capture rule")
		return
	}
	d.invalidateSourceCache(r.Context(), p.OrgID, current.SourceID)
	w.WriteHeader(http.StatusNoContent)
}
