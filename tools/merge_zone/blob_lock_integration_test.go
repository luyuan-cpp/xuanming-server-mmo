//go:build merge_integration

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// cmdNameRecorder 按顺序记下 src 客户端发出的命令名,用来钉住「查数据锁(EXISTS)早于 SCAN」。
type cmdNameRecorder struct{ names []string }

func (r *cmdNameRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *cmdNameRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		r.names = append(r.names, cmd.Name())
		return next(ctx, cmd)
	}
}

func (r *cmdNameRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			r.names = append(r.names, cmd.Name())
		}
		return next(ctx, cmds)
	}
}

func (r *cmdNameRecorder) index(name string) int {
	for i, got := range r.names {
		if got == name {
			return i
		}
	}
	return -1
}

// TestIT_CopyPlayerStringKeys_RefusesWhileDataLockHeld:拷 blob 前查源端 data_service 数据锁(写门禁论证的另一半)。
//   - apply + 锁在:报错、目标端一个键都不写,且锁在时根本不 SCAN / MGET(检查早于 SCAN);
//   - dry-run + 锁在:只告警,照常计数;
//   - 锁不在:正常拷贝,目标端残留的字段索引被删,锁键不拷;EXISTS 先于 SCAN。
//
// 用 Redis DB 13 / 14,任一库非空就按依赖缺失处理(itMissing:默认 SKIP,MERGE_ZONE_IT_REQUIRE=1 时 FAIL;
// 不 flush 别人的数据),只清本用例写过的键。
func TestIT_CopyPlayerStringKeys_RefusesWhileDataLockHeld(t *testing.T) {
	requireIT(t)
	ctx := context.Background()
	src, dst := itRedis(t, 13), itRedis(t, 14)
	for _, c := range []*redis.Client{src, dst} {
		n, err := c.DBSize(ctx).Result()
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			itMissing(t, fmt.Sprintf("redis DB %d is not empty (%d keys); refusing to share it", c.Options().DB, n))
		}
	}
	const pid uint64 = 9201
	gold, ver := "player:{9201}:gold", "player:{9201}:__version"
	lock, idx := playerDataLockKey(pid), playerFieldIndexKey(pid)
	t.Cleanup(func() {
		src.Del(ctx, gold, ver, lock)
		dst.Del(ctx, gold, ver, lock, idx)
	})
	for key, val := range map[string]string{gold: "100", ver: "3"} {
		if err := src.Set(ctx, key, val, 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := src.Set(ctx, lock, "tok", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := dst.HSet(ctx, idx, "__version", "3").Err(); err != nil { // 目标端残留的字段索引
		t.Fatal(err)
	}
	rec := &cmdNameRecorder{}
	src.AddHook(rec)

	if _, err := copyPlayerStringKeys(ctx, src, dst, pid, false); err == nil || !strings.Contains(err.Error(), lock) {
		t.Fatalf("apply with the data lock held must fail naming %s, got %v", lock, err)
	}
	if n := dst.Exists(ctx, gold, ver).Val(); n != 0 {
		t.Fatalf("nothing may be copied while the lock is held, %d keys landed", n)
	}
	if rec.index("scan") >= 0 || rec.index("mget") >= 0 {
		t.Fatalf("the lock check must come before SCAN / MGET, got %v", rec.names)
	}

	if n, err := copyPlayerStringKeys(ctx, src, dst, pid, true); err != nil || n != 2 {
		t.Fatalf("dry-run only warns: n=%d err=%v (want 2, nil)", n, err)
	}

	if err := src.Del(ctx, lock).Err(); err != nil {
		t.Fatal(err)
	}
	rec.names = nil
	n, err := copyPlayerStringKeys(ctx, src, dst, pid, false)
	if err != nil || n != 2 {
		t.Fatalf("copy without the lock: n=%d err=%v (want 2, nil)", n, err)
	}
	if got := dst.Get(ctx, gold).Val(); got != "100" {
		t.Fatalf("gold = %q, want 100", got)
	}
	if got := dst.Get(ctx, ver).Val(); got != "3" {
		t.Fatalf("__version = %q, want 3", got)
	}
	if dst.Exists(ctx, idx).Val() != 0 {
		t.Fatal("the stale field index on the target must be deleted")
	}
	if dst.Exists(ctx, lock).Val() != 0 {
		t.Fatal("the data lock must never be copied")
	}
	if e, s := rec.index("exists"), rec.index("scan"); e < 0 || s < 0 || e > s {
		t.Fatalf("EXISTS must precede SCAN, got %v", rec.names)
	}
}
