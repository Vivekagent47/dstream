package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/bookmark"
	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/store"
)

const (
	minStepDelayMs = 0
	maxStepDelayMs = 60000
	// maxScenarioSteps bounds a scenario so a replay can't tie up a handler
	// goroutine for hours (worst case = steps × maxStepDelayMs of sleeping).
	maxScenarioSteps = 50
)

type scenarioStepInput struct {
	BookmarkID string `json:"bookmark_id"`
	DelayMs    int32  `json:"delay_ms"`
}

type createScenarioReq struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Steps       []scenarioStepInput `json:"steps"`
}

type patchScenarioReq struct {
	Name        *string              `json:"name"`
	Description *string              `json:"description"`
	Steps       *[]scenarioStepInput `json:"steps"` // nil = leave steps unchanged
}

// scenarioView is the API shape for a scenario, with its ordered steps.
func scenarioView(sc store.Scenario, steps []store.ListScenarioStepsRow) map[string]any {
	stepsOut := make([]map[string]any, 0, len(steps))
	for _, s := range steps {
		stepsOut = append(stepsOut, map[string]any{
			"position":      s.Position,
			"bookmark_id":   store.GoUUID(s.BookmarkID).String(),
			"bookmark_name": s.BookmarkName,
			"delay_ms":      s.DelayMs,
		})
	}
	return map[string]any{
		"id":          store.GoUUID(sc.ID).String(),
		"name":        sc.Name,
		"description": sc.Description,
		"created_at":  sc.CreatedAt.Time,
		"updated_at":  sc.UpdatedAt.Time,
		"steps":       stepsOut,
	}
}

// validateSteps confirms every step's bookmark_id parses and is in-org, and
// its delay is in range. Returns the parsed bookmark ids in step order; on
// failure returns the HTTP status the caller should write plus the message.
func validateSteps(ctx context.Context, d Handlers, orgID uuid.UUID, steps []scenarioStepInput) ([]uuid.UUID, int, error) {
	if len(steps) > maxScenarioSteps {
		return nil, http.StatusBadRequest, errors.New("scenario has too many steps (max 50)")
	}
	bids := make([]uuid.UUID, len(steps))
	for i, s := range steps {
		bid, err := uuid.Parse(s.BookmarkID)
		if err != nil {
			return nil, http.StatusBadRequest, errors.New("invalid bookmark_id in steps")
		}
		if _, err := d.Queries.GetBookmarkForOrg(ctx, store.GetBookmarkForOrgParams{
			ID: store.UUID(bid), OrgID: store.UUID(orgID),
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, http.StatusNotFound, errors.New("bookmark not found in scenario steps")
			}
			return nil, http.StatusInternalServerError, err
		}
		if s.DelayMs < minStepDelayMs || s.DelayMs > maxStepDelayMs {
			return nil, http.StatusBadRequest, errors.New("delay_ms must be between 0 and 60000")
		}
		bids[i] = bid
	}
	return bids, 0, nil
}

// writeSteps replaces a scenario's steps within a transaction: delete-all
// then insert-by-position. bids are the already-validated bookmark ids,
// aligned by index with steps.
func writeSteps(ctx context.Context, qtx *store.Queries, scenarioID pgtype.UUID, steps []scenarioStepInput, bids []uuid.UUID) error {
	if err := qtx.DeleteScenarioSteps(ctx, scenarioID); err != nil {
		return err
	}
	for i := range steps {
		if _, err := qtx.InsertScenarioStep(ctx, store.InsertScenarioStepParams{
			ScenarioID: scenarioID,
			Position:   int32(i),
			BookmarkID: store.UUID(bids[i]),
			DelayMs:    steps[i].DelayMs,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (d Handlers) CreateScenario(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	var body createScenarioReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Name == "" {
		httpx.Err(w, http.StatusBadRequest, "name required")
		return
	}
	bids, status, err := validateSteps(r.Context(), d, p.OrgID, body.Steps)
	if err != nil {
		if status == http.StatusInternalServerError {
			d.Log.Error("create scenario: validate steps", "err", err)
			httpx.Err(w, status, "validate steps")
		} else {
			httpx.Err(w, status, err.Error())
		}
		return
	}
	tx, err := d.Pool.Begin(r.Context())
	if err != nil {
		d.Log.Error("create scenario: begin tx", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create scenario")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := d.Queries.WithTx(tx)
	sc, err := qtx.CreateScenario(r.Context(), store.CreateScenarioParams{
		OrgID: store.UUID(p.OrgID), Name: body.Name, Description: body.Description,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Err(w, http.StatusConflict, "scenario name already in use")
			return
		}
		d.Log.Error("create scenario", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create scenario")
		return
	}
	if err := writeSteps(r.Context(), qtx, sc.ID, body.Steps, bids); err != nil {
		d.Log.Error("create scenario: write steps", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create scenario")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		if isUniqueViolation(err) {
			httpx.Err(w, http.StatusConflict, "scenario name already in use")
			return
		}
		d.Log.Error("create scenario: commit", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create scenario")
		return
	}
	steps, err := d.Queries.ListScenarioSteps(r.Context(), sc.ID)
	if err != nil {
		d.Log.Error("create scenario: list steps", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create scenario")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, scenarioView(sc, steps))
}

func (d Handlers) ListScenarios(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	rows, err := d.Queries.ListScenariosForOrg(r.Context(), store.UUID(p.OrgID))
	if err != nil {
		d.Log.Error("list scenarios", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "list scenarios")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// ponytail: one step query per scenario (N+1). Fine for a small list;
		// swap for a COUNT column if scenario counts grow large. Without the
		// steps the list can't show a per-scenario step count.
		steps, err := d.Queries.ListScenarioSteps(r.Context(), row.ID)
		if err != nil {
			d.Log.Error("list scenarios: steps", "err", err, "scenario", store.GoUUID(row.ID).String())
			httpx.Err(w, http.StatusInternalServerError, "list scenarios")
			return
		}
		out = append(out, scenarioView(row, steps))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (d Handlers) GetScenario(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid scenario id")
		return
	}
	sc, err := d.Queries.GetScenarioForOrg(r.Context(), store.GetScenarioForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "scenario not found")
			return
		}
		d.Log.Error("get scenario", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "get scenario")
		return
	}
	steps, err := d.Queries.ListScenarioSteps(r.Context(), sc.ID)
	if err != nil {
		d.Log.Error("get scenario: list steps", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "get scenario")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, scenarioView(sc, steps))
}

// PatchScenario loads the current row, merges name/description onto it, and
// optionally replaces the step list. When Steps is provided the rename (if
// any) and the step-replace commit atomically in one transaction.
func (d Handlers) PatchScenario(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid scenario id")
		return
	}
	current, err := d.Queries.GetScenarioForOrg(r.Context(), store.GetScenarioForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "scenario not found")
			return
		}
		d.Log.Error("patch scenario: lookup", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "lookup scenario")
		return
	}
	var body patchScenarioReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	name, desc := current.Name, current.Description
	if body.Name != nil {
		name = *body.Name
	}
	if body.Description != nil {
		desc = *body.Description
	}
	if name == "" {
		httpx.Err(w, http.StatusBadRequest, "name required")
		return
	}
	var steps []scenarioStepInput
	var bids []uuid.UUID
	if body.Steps != nil {
		steps = *body.Steps
		var status int
		bids, status, err = validateSteps(r.Context(), d, p.OrgID, steps)
		if err != nil {
			if status == http.StatusInternalServerError {
				d.Log.Error("patch scenario: validate steps", "err", err)
				httpx.Err(w, status, "validate steps")
			} else {
				httpx.Err(w, status, err.Error())
			}
			return
		}
	}
	tx, err := d.Pool.Begin(r.Context())
	if err != nil {
		d.Log.Error("patch scenario: begin tx", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "patch scenario")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := d.Queries.WithTx(tx)
	sc, err := qtx.UpdateScenario(r.Context(), store.UpdateScenarioParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID), Name: name, Description: desc,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Err(w, http.StatusConflict, "scenario name already in use")
			return
		}
		d.Log.Error("patch scenario", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "patch scenario")
		return
	}
	if body.Steps != nil {
		if err := writeSteps(r.Context(), qtx, sc.ID, steps, bids); err != nil {
			d.Log.Error("patch scenario: write steps", "err", err)
			httpx.Err(w, http.StatusInternalServerError, "patch scenario")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		if isUniqueViolation(err) {
			httpx.Err(w, http.StatusConflict, "scenario name already in use")
			return
		}
		d.Log.Error("patch scenario: commit", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "patch scenario")
		return
	}
	outSteps, err := d.Queries.ListScenarioSteps(r.Context(), sc.ID)
	if err != nil {
		d.Log.Error("patch scenario: list steps", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "patch scenario")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, scenarioView(sc, outSteps))
}

type scenarioReplayResult struct {
	Position   int32  `json:"position"`
	Status     int    `json:"status,omitempty"`
	DurationMs int    `json:"duration_ms,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ReplayScenarioTo replays a scenario's bookmarked steps, in order, to a
// caller-supplied URL through the SSRF-guarded Replayer client. Stops at the
// first step that fails to load or replay, returning results-so-far.
func (d Handlers) ReplayScenarioTo(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid scenario id")
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
	if _, err := d.Queries.GetScenarioForOrg(r.Context(), store.GetScenarioForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "scenario not found")
			return
		}
		d.Log.Error("scenario replay: get", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "replay")
		return
	}
	steps, err := d.Queries.ListScenarioSteps(r.Context(), store.UUID(id))
	if err != nil {
		d.Log.Error("scenario replay: steps", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "replay")
		return
	}

	results := make([]scenarioReplayResult, 0, len(steps))
	for _, step := range steps {
		if step.DelayMs > 0 {
			delay := step.DelayMs
			if delay > maxStepDelayMs {
				delay = maxStepDelayMs
			}
			// Cancelable: a client disconnect (or shutdown) stops the replay
			// instead of sleeping out the full delay on a dead request.
			select {
			case <-time.After(time.Duration(delay) * time.Millisecond):
			case <-r.Context().Done():
				httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
				return
			}
		}
		bm, err := d.Queries.GetBookmarkForOrg(r.Context(), store.GetBookmarkForOrgParams{
			ID: step.BookmarkID, OrgID: store.UUID(p.OrgID),
		})
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				d.Log.Error("scenario replay: get bookmark", "err", err, "position", step.Position)
			}
			results = append(results, scenarioReplayResult{Position: step.Position, Error: "bookmark unavailable"})
			break
		}
		req, status, lerr := d.loadBookmarkRequestCore(r.Context(), bm.RequestID, p.OrgID)
		if lerr != nil {
			results = append(results, scenarioReplayResult{Position: step.Position, Status: status, Error: lerr.Error()})
			break
		}
		resp, rerr := bookmark.ReplayTo(r.Context(), d.Replayer, req, body.URL)
		if rerr != nil {
			results = append(results, scenarioReplayResult{Position: step.Position, Error: "replay target failed: " + rerr.Error()})
			break
		}
		results = append(results, scenarioReplayResult{Position: step.Position, Status: resp.Status, DurationMs: resp.DurationMs})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (d Handlers) DeleteScenario(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid scenario id")
		return
	}
	n, err := d.Queries.DeleteScenarioForOrg(r.Context(), store.DeleteScenarioForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		d.Log.Error("delete scenario", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "delete scenario")
		return
	}
	if n == 0 {
		httpx.Err(w, http.StatusNotFound, "scenario not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
