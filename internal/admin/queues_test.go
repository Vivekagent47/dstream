package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testDeps(t *testing.T) (Deps, *dqueue.Client) {
	t.Helper()
	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skip("no redis")
	}
	q := dqueue.NewClient(rdb).WithPrefix("admtest")
	if keys, _ := rdb.Keys(context.Background(), "admtest:*").Result(); len(keys) > 0 {
		rdb.Del(context.Background(), keys...)
	}
	d := Deps{Log: discardLogger(), Queue: q}
	// handleRequeueDead flips the event row to 'queued' via Queries; wire a real
	// pool when DSTREAM_TEST_DB_URL is set so that path runs. A missing row is a
	// 0-row no-op UPDATE, so no seeding is needed for these handler tests.
	if dsn := os.Getenv("DSTREAM_TEST_DB_URL"); dsn != "" {
		pool, err := store.NewPool(context.Background(), dsn, 2)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(pool.Close)
		d.Queries = store.New(pool)
	}
	return d, q
}

// router with ONLY the queue routes, no SuperAdminOnly (logic-under-test).
func queueRouter(d Deps) chi.Router {
	r := chi.NewRouter()
	r.Get("/admin/queues/items", d.handleQueueItems)
	r.Get("/admin/queues/orgs", d.handleQueueOrgs)
	r.Post("/admin/queues/dead/requeue", d.handleRequeueDead)
	r.Post("/admin/queues/scheduled/promote", d.handlePromoteScheduled)
	r.Post("/admin/queues/dead/drain", d.handleDrainDead)
	return r
}

func TestQueueItemsValidation(t *testing.T) {
	d, _ := testDeps(t)
	r := queueRouter(d)

	// pending without org -> 400
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/queues/items?lane=pending", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("pending no org: got %d want 400", rec.Code)
	}
	// unknown lane -> 400
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/queues/items?lane=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad lane: got %d want 400", rec.Code)
	}
}

func TestQueueDeadRequeueRoundTrip(t *testing.T) {
	d, q := testDeps(t)
	if d.Queries == nil {
		t.Skip("no DSTREAM_TEST_DB_URL")
	}
	ctx := context.Background()
	org := uuid.New()
	q.Enqueue(ctx, dqueue.Payload{EventID: uuid.New(), OrgID: org})
	raw, _, _, _ := q.FairPick(ctx, 60000)
	q.DeadLetter(ctx, raw)
	// FairPick's raw is a tokened lease handle; the dead lane holds the plain payload.
	dead, _, _ := q.Items(ctx, "dead", "", 100)
	if len(dead) != 1 {
		t.Fatalf("dead items: %d", len(dead))
	}

	r := queueRouter(d)
	body := strings.NewReader(`{"raw":` + jsonString(dead[0].Raw) + `}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/queues/dead/requeue", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("requeue: got %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["requeued"] != true {
		t.Fatalf("requeued=%v", got["requeued"])
	}
	// item is now in pending, not dead
	items, _, _ := q.Items(ctx, "pending", org.String(), 100)
	if len(items) != 1 {
		t.Fatalf("pending after requeue: %d", len(items))
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }
