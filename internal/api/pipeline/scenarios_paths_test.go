package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

func stepIn(b uuid.UUID, delay int) map[string]any {
	return map[string]any{"bookmark_id": b.String(), "delay_ms": delay}
}

func mkScenario(t *testing.T, r http.Handler, uid, oid uuid.UUID, name string, steps ...map[string]any) string {
	t.Helper()
	body := map[string]any{"name": name, "description": "about " + name}
	if steps != nil {
		body["steps"] = steps
	}
	rec := do(t, r, http.MethodPost, "/api/scenarios", uid, oid, body)
	wantStatus(t, rec, http.StatusCreated)
	return decodeMap(t, rec)["id"].(string)
}

// dbSteps reads a scenario's steps straight from Postgres, in position order,
// as "<bookmark id>@<delay>".
func dbSteps(t *testing.T, pool *pgxpool.Pool, id string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT bookmark_id::text || '@' || delay_ms FROM scenario_steps WHERE scenario_id=$1 ORDER BY position`, id)
	if err != nil {
		t.Fatalf("read steps: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan step: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func wantSteps(t *testing.T, pool *pgxpool.Pool, id string, want ...string) {
	t.Helper()
	got := dbSteps(t, pool, id)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", got, want)
	}
}

func scenarioNameDesc(t *testing.T, pool *pgxpool.Pool, id string) (name, desc string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT name, description FROM scenarios WHERE id=$1`, id).Scan(&name, &desc); err != nil {
		t.Fatalf("read scenario %s: %v", id, err)
	}
	return
}

func scenarioCount(t *testing.T, pool *pgxpool.Pool, oid uuid.UUID) int {
	t.Helper()
	return rowCount(t, pool, `SELECT count(*) FROM scenarios WHERE org_id=$1`, oid)
}

func at(b uuid.UUID, delay int) string { return fmt.Sprintf("%s@%d", b, delay) }

func TestScenarioReadPaths(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	uidB, oidB := seedOrg(t, q)
	b1 := bmWithBody(t, q, oid, []byte(`{}`))
	b2 := bmWithBody(t, q, oid, []byte(`{}`))

	// steps given in the reverse of bookmark creation order: position, not id, rules.
	s1 := mkScenario(t, r, uid, oid, "alpha", stepIn(b2, 0), stepIn(b1, 250))
	s2 := mkScenario(t, r, uid, oid, "beta")

	t.Run("list is newest first with ordered steps, scoped to the org", func(t *testing.T) {
		l := decodeList(t, do(t, r, http.MethodGet, "/api/scenarios", uid, oid, nil))
		mustEq(t, "count", len(l), 2)
		mustEq(t, "first", l[0]["id"], s2)
		mustEq(t, "second", l[1]["id"], s1)
		mustEq(t, "beta steps", len(l[0]["steps"].([]any)), 0)
		st := l[1]["steps"].([]any)
		mustEq(t, "alpha steps", len(st), 2)
		first, second := st[0].(map[string]any), st[1].(map[string]any)
		mustEq(t, "pos0", first["position"], float64(0))
		mustEq(t, "pos0 bookmark", first["bookmark_id"], b2.String())
		mustEq(t, "pos1 bookmark", second["bookmark_id"], b1.String())
		mustEq(t, "pos1 delay", second["delay_ms"], float64(250))
		if first["bookmark_name"] == "" || first["bookmark_name"] == nil {
			t.Fatalf("step must carry its bookmark name: %v", first)
		}
		mustEq(t, "description", l[1]["description"], "about alpha")
		mustEq(t, "org B list", len(decodeList(t, do(t, r, http.MethodGet, "/api/scenarios", uidB, oidB, nil))), 0)
	})

	t.Run("get returns the same view", func(t *testing.T) {
		m := decodeMap(t, do(t, r, http.MethodGet, "/api/scenarios/"+s1, uid, oid, nil))
		mustEq(t, "name", m["name"], "alpha")
		mustEq(t, "steps", len(m["steps"].([]any)), 2)
	})

	t.Run("bad and unknown ids", func(t *testing.T) {
		for _, c := range []struct{ method, path string }{
			{http.MethodGet, "/api/scenarios/"}, {http.MethodPatch, "/api/scenarios/"},
			{http.MethodDelete, "/api/scenarios/"}, {http.MethodPost, "/api/scenarios/%s/replay-to"},
		} {
			p := c.path + "nope"
			if strings.Contains(c.path, "%s") {
				p = fmt.Sprintf(c.path, "nope")
			}
			wantErr(t, do(t, r, c.method, p, uid, oid, map[string]any{"url": "http://x.test"}), http.StatusBadRequest, "invalid scenario id")
			p = c.path + uuid.NewString()
			if strings.Contains(c.path, "%s") {
				p = fmt.Sprintf(c.path, uuid.NewString())
			}
			wantErr(t, do(t, r, c.method, p, uid, oid, map[string]any{"name": "x", "url": "http://sink.example.test"}), http.StatusNotFound, "scenario not found")
		}
	})
}

func TestScenarioHandlersRefuseAnotherOrgsRow(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	uidB, oidB := seedOrg(t, q)
	bA := bmWithBody(t, q, oid, []byte(`{"a":1}`))
	bB := bmWithBody(t, q, oidB, []byte(`{"b":1}`))
	sc := mkScenario(t, r, uid, oid, "mine", stepIn(bA, 5))
	var hits atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(sink.Close)

	wantErr(t, do(t, r, http.MethodGet, "/api/scenarios/"+sc, uidB, oidB, nil), http.StatusNotFound, "scenario not found")
	wantErr(t, do(t, r, http.MethodPatch, "/api/scenarios/"+sc, uidB, oidB, map[string]any{
		"name": "hijacked", "description": "x", "steps": []map[string]any{stepIn(bB, 0)},
	}), http.StatusNotFound, "scenario not found")
	wantErr(t, do(t, r, http.MethodDelete, "/api/scenarios/"+sc, uidB, oidB, nil), http.StatusNotFound, "scenario not found")
	wantErr(t, do(t, r, http.MethodPost, "/api/scenarios/"+sc+"/replay-to", uidB, oidB, map[string]any{"url": sink.URL}), http.StatusNotFound, "scenario not found")

	name, desc := scenarioNameDesc(t, pool, sc)
	mustEq(t, "name", name, "mine")
	mustEq(t, "description", desc, "about mine")
	wantSteps(t, pool, sc, at(bA, 5))
	mustEq(t, "sink hits", hits.Load(), int32(0))
	mustEq(t, "org B scenarios", scenarioCount(t, pool, oidB), 0)
}

func TestCreateScenarioValidation(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	b := bmWithBody(t, q, oid, []byte(`{}`))
	many := make([]map[string]any, maxScenarioSteps+1)
	for i := range many {
		many[i] = stepIn(b, 0)
	}

	for name, c := range map[string]struct {
		body   any
		status int
		msg    string
	}{
		"missing name":     {map[string]any{"description": "x"}, 400, "name required"},
		"too many steps":   {map[string]any{"name": "n1", "steps": many}, 400, "scenario has too many steps (max 50)"},
		"bad bookmark id":  {map[string]any{"name": "n2", "steps": []map[string]any{{"bookmark_id": "nope"}}}, 400, "invalid bookmark_id in steps"},
		"unknown bookmark": {map[string]any{"name": "n3", "steps": []map[string]any{stepIn(uuid.New(), 0)}}, 404, "bookmark not found in scenario steps"},
		"delay too long":   {map[string]any{"name": "n4", "steps": []map[string]any{stepIn(b, 60001)}}, 400, "delay_ms must be between 0 and 60000"},
		"negative delay":   {map[string]any{"name": "n5", "steps": []map[string]any{stepIn(b, -1)}}, 400, "delay_ms must be between 0 and 60000"},
	} {
		t.Run(name, func(t *testing.T) {
			wantErr(t, do(t, r, http.MethodPost, "/api/scenarios", uid, oid, c.body), c.status, c.msg)
		})
	}
	t.Run("malformed json", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, nil)
		req.Body = http.NoBody
		r.ServeHTTP(rec, req)
		wantErr(t, rec, http.StatusBadRequest, "invalid json")
	})
	t.Run("duplicate name", func(t *testing.T) {
		mkScenario(t, r, uid, oid, "twice")
		wantErr(t, do(t, r, http.MethodPost, "/api/scenarios", uid, oid, map[string]any{"name": "twice"}), http.StatusConflict, "scenario name already in use")
	})
	mustEq(t, "only the valid scenario exists", scenarioCount(t, pool, oid), 1)
}

func TestPatchScenarioBehaviours(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	_, oidB := seedOrg(t, q)
	b1 := bmWithBody(t, q, oid, []byte(`{}`))
	b2 := bmWithBody(t, q, oid, []byte(`{}`))
	bForeign := bmWithBody(t, q, oidB, []byte(`{}`))
	patch := func(id string, body any) *httptest.ResponseRecorder {
		return do(t, r, http.MethodPatch, "/api/scenarios/"+id, uid, oid, body)
	}

	t.Run("description alone keeps name and steps", func(t *testing.T) {
		id := mkScenario(t, r, uid, oid, "p-desc", stepIn(b1, 7))
		m := decodeMap(t, patch(id, map[string]any{"description": "fresh"}))
		mustEq(t, "desc", m["description"], "fresh")
		mustEq(t, "name", m["name"], "p-desc")
		name, desc := scenarioNameDesc(t, pool, id)
		mustEq(t, "stored name", name, "p-desc")
		mustEq(t, "stored desc", desc, "fresh")
		wantSteps(t, pool, id, at(b1, 7))
	})

	t.Run("rename and replace steps together", func(t *testing.T) {
		id := mkScenario(t, r, uid, oid, "p-both", stepIn(b1, 1))
		m := decodeMap(t, patch(id, map[string]any{"name": "p-both-2", "steps": []map[string]any{stepIn(b2, 9), stepIn(b1, 3)}}))
		mustEq(t, "name", m["name"], "p-both-2")
		wantSteps(t, pool, id, at(b2, 9), at(b1, 3))
	})

	t.Run("an empty steps list clears the steps", func(t *testing.T) {
		id := mkScenario(t, r, uid, oid, "p-clear", stepIn(b1, 1))
		wantStatus(t, patch(id, map[string]any{"steps": []map[string]any{}}), http.StatusOK)
		wantSteps(t, pool, id)
	})

	t.Run("rejections leave the scenario untouched", func(t *testing.T) {
		id := mkScenario(t, r, uid, oid, "p-keep", stepIn(b1, 4))
		other := mkScenario(t, r, uid, oid, "p-taken")
		many := make([]map[string]any, maxScenarioSteps+1)
		for i := range many {
			many[i] = stepIn(b1, 0)
		}
		for name, c := range map[string]struct {
			body   any
			status int
			msg    string
		}{
			"empty name":       {map[string]any{"name": ""}, 400, "name required"},
			"duplicate name":   {map[string]any{"name": "p-taken"}, 409, "scenario name already in use"},
			"too many steps":   {map[string]any{"name": "x1", "steps": many}, 400, "scenario has too many steps (max 50)"},
			"bad bookmark id":  {map[string]any{"name": "x2", "steps": []map[string]any{{"bookmark_id": "no"}}}, 400, "invalid bookmark_id in steps"},
			"foreign bookmark": {map[string]any{"name": "x3", "steps": []map[string]any{stepIn(bForeign, 0)}}, 404, "bookmark not found in scenario steps"},
			"delay too long":   {map[string]any{"name": "x4", "steps": []map[string]any{stepIn(b2, 99999)}}, 400, "delay_ms must be between 0 and 60000"},
		} {
			t.Run(name, func(t *testing.T) {
				wantErr(t, patch(id, c.body), c.status, c.msg)
				n, _ := scenarioNameDesc(t, pool, id)
				mustEq(t, "name", n, "p-keep")
				wantSteps(t, pool, id, at(b1, 4))
				n2, _ := scenarioNameDesc(t, pool, other)
				mustEq(t, "other name", n2, "p-taken")
			})
		}
		rec := httptest.NewRecorder()
		req := sessionReq(t, http.MethodPatch, "/api/scenarios/"+id, uid, oid, nil)
		req.Body = http.NoBody
		r.ServeHTTP(rec, req)
		wantErr(t, rec, http.StatusBadRequest, "invalid json")
		wantSteps(t, pool, id, at(b1, 4))
	})
}

func TestScenarioHandlersMidFlowDatabaseFailure(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	b1 := bmWithBody(t, q, oid, []byte(`{}`))
	b2 := bmWithBody(t, q, oid, []byte(`{}`))
	clean := fullRouter(q, pool, nil)
	failing := func(marker string) http.Handler {
		fp := failingPool(t, marker)
		return fullRouter(store.New(fp), fp, nil)
	}
	const (
		lookup    = "from scenarios where id = $1 and org_id = $2"
		bookmark  = "from bookmarks where id = $1 and org_id = $2"
		listSteps = "from scenario_steps s join bookmarks"
	)

	// each create failure rolls the whole scenario back
	for marker, msg := range map[string]string{
		bookmark:                     "validate steps",
		"insert into scenarios":      "create scenario",
		"delete from scenario_steps": "create scenario",
		"insert into scenario_steps": "create scenario",
	} {
		before := scenarioCount(t, pool, oid)
		name := "c-" + uuid.NewString()
		wantErr(t, do(t, failing(marker), http.MethodPost, "/api/scenarios", uid, oid,
			map[string]any{"name": name, "steps": []map[string]any{stepIn(b1, 0)}}), http.StatusInternalServerError, msg)
		mustEq(t, "scenarios after failed create ("+marker+")", scenarioCount(t, pool, oid), before)
	}
	t.Run("create: transaction cannot begin", func(t *testing.T) {
		rt := fullRouter(q, closedPool(t), nil)
		wantErr(t, do(t, rt, http.MethodPost, "/api/scenarios", uid, oid, map[string]any{"name": "nobegin"}), http.StatusInternalServerError, "create scenario")
		mustEq(t, "scenarios", rowCount(t, pool, `SELECT count(*) FROM scenarios WHERE org_id=$1 AND name='nobegin'`, oid), 0)
	})
	t.Run("create: commit fails", func(t *testing.T) {
		before := scenarioCount(t, pool, oid)
		wantErr(t, do(t, failing("commit"), http.MethodPost, "/api/scenarios", uid, oid,
			map[string]any{"name": "nocommit", "steps": []map[string]any{stepIn(b1, 0)}}), http.StatusInternalServerError, "create scenario")
		mustEq(t, "scenarios", scenarioCount(t, pool, oid), before)
	})
	t.Run("create: reading back fails after the commit", func(t *testing.T) {
		wantErr(t, do(t, failing(listSteps), http.MethodPost, "/api/scenarios", uid, oid, map[string]any{"name": "readback"}), http.StatusInternalServerError, "create scenario")
		mustEq(t, "committed anyway", rowCount(t, pool, `SELECT count(*) FROM scenarios WHERE org_id=$1 AND name='readback'`, oid), 1)
	})

	id := mkScenario(t, clean, uid, oid, "stable", stepIn(b1, 11))
	unchanged := func(t *testing.T) {
		t.Helper()
		name, desc := scenarioNameDesc(t, pool, id)
		mustEq(t, "name", name, "stable")
		mustEq(t, "description", desc, "about stable")
		wantSteps(t, pool, id, at(b1, 11))
	}
	change := map[string]any{"name": "renamed", "steps": []map[string]any{stepIn(b2, 22)}}

	for marker, msg := range map[string]string{
		lookup:                       "lookup scenario",
		bookmark:                     "validate steps",
		"update scenarios set":       "patch scenario",
		"delete from scenario_steps": "patch scenario",
		"insert into scenario_steps": "patch scenario",
	} {
		t.Run("patch "+marker, func(t *testing.T) {
			wantErr(t, do(t, failing(marker), http.MethodPatch, "/api/scenarios/"+id, uid, oid, change), http.StatusInternalServerError, msg)
			unchanged(t)
		})
	}
	t.Run("patch: transaction cannot begin", func(t *testing.T) {
		wantErr(t, do(t, fullRouter(q, closedPool(t), nil), http.MethodPatch, "/api/scenarios/"+id, uid, oid, change), http.StatusInternalServerError, "patch scenario")
		unchanged(t)
	})
	t.Run("patch: commit fails", func(t *testing.T) {
		wantErr(t, do(t, failing("commit"), http.MethodPatch, "/api/scenarios/"+id, uid, oid, change), http.StatusInternalServerError, "patch scenario")
		unchanged(t)
	})
	t.Run("get", func(t *testing.T) {
		wantErr(t, do(t, failing(lookup), http.MethodGet, "/api/scenarios/"+id, uid, oid, nil), http.StatusInternalServerError, "get scenario")
		wantErr(t, do(t, failing(listSteps), http.MethodGet, "/api/scenarios/"+id, uid, oid, nil), http.StatusInternalServerError, "get scenario")
	})
	t.Run("list", func(t *testing.T) {
		wantErr(t, do(t, failing("from scenarios where org_id = $1"), http.MethodGet, "/api/scenarios", uid, oid, nil), http.StatusInternalServerError, "list scenarios")
		wantErr(t, do(t, failing(listSteps), http.MethodGet, "/api/scenarios", uid, oid, nil), http.StatusInternalServerError, "list scenarios")
	})
	t.Run("delete", func(t *testing.T) {
		wantErr(t, do(t, failing("delete from scenarios where id"), http.MethodDelete, "/api/scenarios/"+id, uid, oid, nil), http.StatusInternalServerError, "delete scenario")
		unchanged(t)
	})
	t.Run("replay-to", func(t *testing.T) {
		body := map[string]any{"url": "http://sink.example.test"}
		wantErr(t, do(t, failing(lookup), http.MethodPost, "/api/scenarios/"+id+"/replay-to", uid, oid, body), http.StatusInternalServerError, "replay")
		wantErr(t, do(t, failing(listSteps), http.MethodPost, "/api/scenarios/"+id+"/replay-to", uid, oid, body), http.StatusInternalServerError, "replay")
	})
	t.Run("patch: reading back fails after the commit", func(t *testing.T) {
		wantErr(t, do(t, failing(listSteps), http.MethodPatch, "/api/scenarios/"+id, uid, oid, map[string]any{"name": "committed"}), http.StatusInternalServerError, "patch scenario")
		name, _ := scenarioNameDesc(t, pool, id)
		mustEq(t, "name", name, "committed")
	})
}

type replayResult struct {
	Position   int    `json:"position"`
	Status     int    `json:"status"`
	DurationMs int    `json:"duration_ms"`
	Error      string `json:"error"`
}

func replayResults(t *testing.T, rec *httptest.ResponseRecorder) []replayResult {
	t.Helper()
	wantStatus(t, rec, http.StatusOK)
	var out struct {
		Results []replayResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode results: %v body=%s", err, rec.Body.String())
	}
	if out.Results == nil {
		t.Fatalf("results must be [] not null: %s", rec.Body.String())
	}
	return out.Results
}

// strayBookmark is a bookmark in org oid whose request lives on another org's
// source, so the org-scoped request lookup finds nothing.
func strayBookmark(t *testing.T, q *store.Queries, oid, otherOrg uuid.UUID) uuid.UUID {
	t.Helper()
	req, _ := seedRequest(t, q, otherOrg)
	bm, err := q.CreateBookmark(context.Background(), store.CreateBookmarkParams{
		OrgID: store.UUID(oid), RequestID: store.UUID(req), Name: "stray-" + uuid.NewString(), Tags: []string{},
	})
	if err != nil {
		t.Fatalf("stray bookmark: %v", err)
	}
	return store.GoUUID(bm.ID)
}

func TestReplayScenarioToStepOutcomes(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	_, oidB := seedOrg(t, q)
	good1 := bmWithBody(t, q, oid, []byte(`one`))
	good2 := bmWithBody(t, q, oid, []byte(`two`))
	stray := strayBookmark(t, q, oid, oidB)
	var hits atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(sink.Close)
	replay := func(id string) *httptest.ResponseRecorder {
		return do(t, r, http.MethodPost, "/api/scenarios/"+id+"/replay-to", uid, oid, map[string]any{"url": sink.URL})
	}

	t.Run("request errors", func(t *testing.T) {
		id := mkScenario(t, r, uid, oid, "r-req")
		rec := httptest.NewRecorder()
		req := sessionReq(t, http.MethodPost, "/api/scenarios/"+id+"/replay-to", uid, oid, nil)
		req.Body = http.NoBody
		r.ServeHTTP(rec, req)
		wantErr(t, rec, http.StatusBadRequest, "invalid json")
		rec = do(t, r, http.MethodPost, "/api/scenarios/"+id+"/replay-to", uid, oid, map[string]any{"url": "ftp://nope"})
		if rec.Code != http.StatusBadRequest || !strings.HasPrefix(decodeMap(t, rec)["error"].(string), "invalid url: ") {
			t.Fatalf("bad url: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an empty scenario replays nothing", func(t *testing.T) {
		id := mkScenario(t, r, uid, oid, "r-empty")
		mustEq(t, "results", len(replayResults(t, replay(id))), 0)
		mustEq(t, "hits", hits.Load(), int32(0))
	})

	t.Run("steps wait their delay and run in order", func(t *testing.T) {
		id := mkScenario(t, r, uid, oid, "r-ok", stepIn(good1, 0), stepIn(good2, 40))
		start := time.Now()
		res := replayResults(t, replay(id))
		if time.Since(start) < 40*time.Millisecond {
			t.Fatalf("second step's 40ms delay was skipped (%v)", time.Since(start))
		}
		mustEq(t, "results", len(res), 2)
		for i, rr := range res {
			mustEq(t, "position", rr.Position, i)
			mustEq(t, "status", rr.Status, http.StatusAccepted)
			mustEq(t, "error", rr.Error, "")
		}
		mustEq(t, "hits", hits.Load(), int32(2))
	})

	t.Run("a step whose request left the org stops the run with 404", func(t *testing.T) {
		hits.Store(0)
		id := mkScenario(t, r, uid, oid, "r-stray", stepIn(good1, 0), stepIn(stray, 0), stepIn(good2, 0))
		res := replayResults(t, replay(id))
		mustEq(t, "results", len(res), 2)
		mustEq(t, "step 0", res[0].Status, http.StatusAccepted)
		mustEq(t, "step 1 status", res[1].Status, http.StatusNotFound)
		mustEq(t, "step 1 error", res[1].Error, "captured request no longer exists")
		mustEq(t, "hits (step 2 never ran)", hits.Load(), int32(1))
	})

	t.Run("database failures inside the loop are reported per step", func(t *testing.T) {
		hits.Store(0)
		id := mkScenario(t, r, uid, oid, "r-dbfail", stepIn(good1, 0), stepIn(good2, 0))
		route := func(marker string) http.Handler { return fullRouter(failingQueries(t, marker), pool, nil) }

		res := replayResults(t, do(t, route("from bookmarks where id = $1 and org_id = $2"), http.MethodPost, "/api/scenarios/"+id+"/replay-to", uid, oid, map[string]any{"url": sink.URL}))
		if len(res) != 1 || res[0].Position != 0 || res[0].Error != "bookmark unavailable" || res[0].Status != 0 {
			t.Fatalf("bookmark failure results = %+v", res)
		}
		res = replayResults(t, do(t, route("join sources s on s.id = r.source_id"), http.MethodPost, "/api/scenarios/"+id+"/replay-to", uid, oid, map[string]any{"url": sink.URL}))
		if len(res) != 1 || res[0].Status != http.StatusInternalServerError || res[0].Error != "load request" {
			t.Fatalf("request failure results = %+v", res)
		}
		mustEq(t, "nothing reached the sink", hits.Load(), int32(0))
	})

	t.Run("a dropped client stops the run during a step delay", func(t *testing.T) {
		hits.Store(0)
		id := mkScenario(t, r, uid, oid, "r-cancel", stepIn(good1, 30000))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h := Handlers{
			Log: discardLog(), Queries: cancelingQueries(t, "from scenario_steps s join bookmarks", cancel),
			BodyStore: ingest.NewPostgresBodyStore(q), Replayer: deliver.NewSafeHTTPClient(5*time.Second, true),
		}
		start := time.Now()
		rec := direct(ctx, h.ReplayScenarioTo, http.MethodPost, &auth.Principal{OrgID: oid, UserID: uid}, id, `{"url":"`+sink.URL+`"}`)
		if time.Since(start) > 10*time.Second {
			t.Fatal("replay slept out the 30s delay instead of stopping")
		}
		mustEq(t, "results", len(replayResults(t, rec)), 0)
		mustEq(t, "hits", hits.Load(), int32(0))
	})
}
