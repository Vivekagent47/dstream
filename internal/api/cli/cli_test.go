package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) *redis.Client {
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
	return rdb
}

// TestReleaseSessionOnlyDeletesOwnToken covers audit #3: an older connection's
// teardown (releaseSession with its own token) must NOT delete the session key
// once a newer connection has overwritten it with a different token; only the
// current owner's teardown deletes.
func TestReleaseSessionOnlyDeletesOwnToken(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	key := SessionKey(uuid.New())
	defer rdb.Del(ctx, key)

	// B is the current owner.
	if err := rdb.Set(ctx, key, "tokenB", time.Minute).Err(); err != nil {
		t.Fatalf("set: %v", err)
	}
	// A's teardown with a stale token must be a no-op.
	if err := releaseSession.Run(ctx, rdb, []string{key}, "tokenA").Err(); err != nil {
		t.Fatalf("release A: %v", err)
	}
	if rdb.Exists(ctx, key).Val() != 1 {
		t.Fatal("stale teardown wiped the current owner's session key")
	}
	// The owner's teardown deletes it.
	if err := releaseSession.Run(ctx, rdb, []string{key}, "tokenB").Err(); err != nil {
		t.Fatalf("release B: %v", err)
	}
	if rdb.Exists(ctx, key).Val() != 0 {
		t.Fatal("owner teardown did not delete the session key")
	}
}
