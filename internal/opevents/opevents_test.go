package opevents

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// Cursor "from newest" sentinel — same values the outbound handlers use
// (internal/api/outbound/reads.go): a far-future ts and the max uuid.
var (
	maxCursorTime = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	maxCursorID   = uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
)

func testQ(t *testing.T) *store.Queries {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	pool, err := store.NewPool(context.Background(), dsn, 2)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

func seedOrg(t *testing.T, q *store.Queries) uuid.UUID {
	t.Helper()
	o, err := q.CreateOrganization(context.Background(), store.CreateOrganizationParams{
		Name: "op " + uuid.NewString(), Slug: "op-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	return store.GoUUID(o.ID)
}

func TestSeedOperationalApp(t *testing.T) {
	q := testQ(t)
	org := seedOrg(t, q)
	id1, err := SeedOperationalApp(context.Background(), q, org)
	if err != nil {
		t.Fatalf("seed1: %v", err)
	}
	id2, err := SeedOperationalApp(context.Background(), q, org)
	if err != nil {
		t.Fatalf("seed2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("idempotent: got %s then %s", id1, id2)
	}
	// event types seeded
	for _, name := range []string{"endpoint.disabled", "message.attempt.exhausted"} {
		if _, err := q.GetEventTypeForOrg(context.Background(), store.GetEventTypeForOrgParams{
			OrgID: store.UUID(org), Name: name,
		}); err != nil {
			t.Errorf("event type %q not seeded: %v", name, err)
		}
	}
	// op app excluded from the normal list
	apps, err := q.ListApplicationsByOrg(context.Background(), store.ListApplicationsByOrgParams{
		OrgID:    store.UUID(org),
		CursorTs: pgtype.Timestamptz{Time: maxCursorTime, Valid: true},
		CursorID: store.UUID(maxCursorID),
		Lim:      100,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, a := range apps {
		if store.GoUUID(a.ID) == id1 {
			t.Error("operational app must be excluded from ListApplicationsByOrg")
		}
	}
}

// maxTs is the "from newest" cursor timestamp sentinel.
func maxTs() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: maxCursorTime, Valid: true}
}

// base64Std24 is a 24-byte StdEncoding base64 blob for a whsec_ endpoint secret.
func base64Std24() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// newTestQueue builds a real dqueue.Client against DSTREAM_REDIS_ADDR
// (default localhost:6379) and skips only when redis is unreachable. Mirrors
// the outbound + webhook test-queue harnesses.
func newTestQueue(t *testing.T) *dqueue.Client {
	t.Helper()
	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skip("no redis at " + addr)
	}
	prefix := "opevtest-" + uuid.NewString()[:8]
	c := dqueue.NewClient(rdb).WithPrefix(prefix)
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+":*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
		_ = rdb.Close()
	})
	return c
}

func TestPublishFansOutToOperationalEndpoints(t *testing.T) {
	q := testQ(t)
	org := seedOrg(t, q)
	appID, err := SeedOperationalApp(context.Background(), q, org)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// one endpoint on the op app
	ep, err := q.CreateEndpoint(context.Background(), store.CreateEndpointParams{
		AppID: store.UUID(appID), OrgID: store.UUID(org), Url: "https://example.test/ops",
		Secret: "whsec_" + base64Std24(), Headers: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	dq := newTestQueue(t) // reuse the outbound test queue helper pattern
	if err := Publish(context.Background(), q, dq, org, "endpoint.disabled",
		map[string]any{"type": "endpoint.disabled", "endpoint_id": store.GoUUID(ep.ID).String()}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// a message exists on the op app with a delivery to ep
	msgs, err := q.ListMessagesByApp(context.Background(), store.ListMessagesByAppParams{
		AppID: store.UUID(appID), CursorTs: maxTs(), CursorID: store.UUID(uuid.Max), Lim: 10,
	})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages: %d %v", len(msgs), err)
	}
	dels, err := q.ListDeliveriesForMessage(context.Background(), msgs[0].ID)
	if err != nil || len(dels) != 1 || store.GoUUID(dels[0].EndpointID) != store.GoUUID(ep.ID) {
		t.Fatalf("deliveries: %+v %v", dels, err)
	}
}
