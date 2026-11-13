package leader

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// NewUniversalStore 的适配面:SET NX 带 TTL、属主校验脚本按整数返回、调用上界按客户端选项算。
// 锁语义本身由 leader_test.go 的内存替身覆盖,这里只钉适配器。

func newUniversalStoreForTest(t *testing.T) (*miniredis.Miniredis, Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	var c redis.UniversalClient = redis.NewClient(&redis.Options{Addr: mr.Addr(), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = c.Close() })
	return mr, NewUniversalStore(c)
}

func TestUniversalStoreSetNXHonorsTTLAndExclusion(t *testing.T) {
	mr, store := newUniversalStoreForTest(t)
	ctx := context.Background()

	ok, err := store.SetNX(ctx, "svc:leader:lock", "a", 1500*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("first SetNX must win, got ok=%v err=%v", ok, err)
	}
	ok, err = store.SetNX(ctx, "svc:leader:lock", "b", time.Second)
	if err != nil || ok {
		t.Fatalf("second SetNX must lose, got ok=%v err=%v", ok, err)
	}
	if ttl := mr.TTL("svc:leader:lock"); ttl != 1500*time.Millisecond {
		t.Fatalf("TTL must keep millisecond precision, got %v", ttl)
	}
}

func TestUniversalStoreEvalIntRunsTheOwnerScripts(t *testing.T) {
	mr, store := newUniversalStoreForTest(t)
	ctx := context.Background()
	if err := mr.Set("svc:leader:lock", "me"); err != nil {
		t.Fatal(err)
	}

	n, err := store.EvalInt(ctx, releaseScript, []string{"svc:leader:lock"}, "someone-else")
	if err != nil || n != 0 {
		t.Fatalf("release by a non-owner must be a no-op, got n=%d err=%v", n, err)
	}
	n, err = store.EvalInt(ctx, releaseScript, []string{"svc:leader:lock"}, "me")
	if err != nil || n != 1 {
		t.Fatalf("release by the owner must delete the lock, got n=%d err=%v", n, err)
	}
	if mr.Exists("svc:leader:lock") {
		t.Fatal("lock must be gone after the owner released it")
	}
}

func TestUniversalStoreEvalIntRejectsNonIntegerReplies(t *testing.T) {
	_, store := newUniversalStoreForTest(t)
	if _, err := store.EvalInt(context.Background(), `return "x"`, []string{"k"}); err == nil {
		t.Fatal("a non-integer script reply must be an error")
	}
}

func TestUniversalStoreMaxCallDurationFollowsClientOptions(t *testing.T) {
	_, store := newUniversalStoreForTest(t)
	bounder, ok := store.(CallBounder)
	if !ok {
		t.Fatal("universal store must report its call bound")
	}
	// ContextTimeoutEnabled:socket 读写认 ctx 截止时间,由调用方 ctx 封顶。
	if got := bounder.MaxCallDuration(); got != 0 {
		t.Fatalf("context-bounded client must report 0, got %v", got)
	}
}
