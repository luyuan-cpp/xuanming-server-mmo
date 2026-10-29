package debugutil

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// r2-ds#1 评审补充(2026-10-22):Redis 写目标靠本地标记键证明自己是本地;值不对、类型不对、读失败都不放行。
func TestHasLocalRedisMarker(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	if HasLocalRedisMarker(ctx, client) {
		t.Fatal("没有标记键时不能放行")
	}
	mr.Set(LocalRedisMarkerKey, "yes")
	if HasLocalRedisMarker(ctx, client) {
		t.Fatal("标记值不对时不能放行")
	}
	mr.Del(LocalRedisMarkerKey)
	mr.HSet(LocalRedisMarkerKey, "marker", LocalTargetMarkerValue)
	if HasLocalRedisMarker(ctx, client) {
		t.Fatal("标记键类型不对(WRONGTYPE)时不能放行")
	}
	mr.Del(LocalRedisMarkerKey)

	// 手册与拒绝信息给出的建标记命令,执行之后必须正好建出检查读的那把键;重复执行也成立。
	for round := 0; round < 2; round++ {
		args := []any{}
		for _, word := range strings.Fields(LocalRedisMarkerSetupCommand) {
			args = append(args, word)
		}
		if err := client.Do(ctx, args...).Err(); err != nil {
			t.Fatalf("第 %d 轮执行建标记命令 %q 失败: %v", round+1, LocalRedisMarkerSetupCommand, err)
		}
		if !HasLocalRedisMarker(ctx, client) {
			t.Fatalf("第 %d 轮执行建标记命令之后仍读不到标记", round+1)
		}
	}
	if ttl := mr.TTL(LocalRedisMarkerKey); ttl != 0 {
		t.Fatalf("标记不该带 TTL,得到 %v", ttl)
	}

	// 标记只认客户端选中的 DB:建在 DB 0 上,连 DB 1 的客户端读不到。
	other := redis.NewClient(&redis.Options{Addr: mr.Addr(), DB: 1})
	t.Cleanup(func() { _ = other.Close() })
	if HasLocalRedisMarker(ctx, other) {
		t.Fatal("别的 DB 上的标记不能放行这个 DB")
	}

	// 连接失败一律按没有标记处理。
	mr.Close()
	if HasLocalRedisMarker(ctx, client) {
		t.Fatal("连接失败时不能放行")
	}
}
