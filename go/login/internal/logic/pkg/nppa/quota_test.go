package nppa

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// 两个 login 副本共用一份额度:同一时刻至多放 1 + Burst 个,之后每过一个间隔再放一个。
func TestQuotaIsSharedAcrossReplicasAndSpacesCalls(t *testing.T) {
	mr := miniredis.RunT(t)
	start := time.Unix(1_763_600_000, 0)
	mr.SetTime(start)
	replicaA := NewQuota(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	replicaB := NewQuota(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	rule := QuotaRule{Name: "test", PerSecond: 10, Burst: 2}
	ctx := context.Background()

	allow := func(q *Quota) bool {
		t.Helper()
		ok, err := q.Allow(ctx, rule)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for i, q := range []*Quota{replicaA, replicaB, replicaA} {
		if !allow(q) {
			t.Fatalf("call %d within the burst was refused", i+1)
		}
	}
	if allow(replicaB) {
		t.Fatal("the burst is shared: a fourth call at the same instant must be refused")
	}

	mr.SetTime(start.Add(100 * time.Millisecond)) // 一个间隔
	if !allow(replicaB) {
		t.Fatal("one interval later one more call is allowed")
	}
	if allow(replicaA) {
		t.Fatal("only one per interval after the burst is used")
	}
}

// 任何 1 秒内放行的个数不超过 PerSecond + Burst + 1:这是留在规范限流以下的依据。
func TestQuotaNeverExceedsTheRateInAnySecond(t *testing.T) {
	mr := miniredis.RunT(t)
	start := time.Unix(1_763_600_000, 0)
	q := NewQuota(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	rule := QuotaRule{Name: "window", PerSecond: 8}
	allowed := 0
	for ms := 0; ms < 1000; ms += 5 { // 每 5 毫秒来一个,远超额度
		mr.SetTime(start.Add(time.Duration(ms) * time.Millisecond))
		if ok, err := q.Allow(context.Background(), rule); err != nil {
			t.Fatal(err)
		} else if ok {
			allowed++
		}
	}
	if allowed > rule.PerSecond+rule.Burst+1 {
		t.Fatalf("allowed %d calls in one second, want at most %d", allowed, rule.PerSecond+rule.Burst+1)
	}
}
