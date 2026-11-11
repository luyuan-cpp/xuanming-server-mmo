package data

// asset_store_test.go —— 附件重投的到期扫描按玩家分片(缺口清单第 25 条第二批,
// docs/design/assetop-redelivery-sharding-20261110.md §10)在 mail outbox 上的 SQL 层回归。纯函数,不连库。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"shared/assetop"
	"shared/workshard"
)

// TestListDueQueryPutsShardFilterBeforeLimit:两段(新行 attempts < ?、老行 attempts >= ?)都带同一个分片条件,
// 位置在 ORDER BY / LIMIT 之前;占位符个数与 ListDueOwned 传的实参一致(now、now、FreshAttemptLimit、片号……、LIMIT);
// 零值条件是恒真 1=1;候选读是普通读。
func TestListDueQueryPutsShardFilterBeforeLimit(t *testing.T) {
	const kind = "mail-assetop"
	mine := map[string]bool{workshard.ShardKey(kind, 40): true, workshard.ShardKey(kind, 5): true}
	owned := workshard.SQLFilter(func(key string) bool { return mine[key] }, kind, "player_id")
	for _, comparison := range []string{"<", ">="} {
		query := listDueQuery(comparison, owned)
		want := " AND attempts " + comparison + " ? AND MOD(player_id,64) IN(?,?) ORDER BY next_attempt_ms,op_id LIMIT ?"
		if !strings.HasSuffix(query, want) {
			t.Fatalf("%s 段的分片条件必须在 ORDER BY / LIMIT 之前:\n  %s", comparison, query)
		}
		if got, wantArgs := strings.Count(query, "?"), 3+len(owned.Args())+1; got != wantArgs {
			t.Fatalf("%s 段占位符 %d 个,ListDueOwned 传 %d 个实参", comparison, got, wantArgs)
		}
		if strings.Contains(strings.ToUpper(query), "FOR UPDATE") {
			t.Fatalf("%s 段是候选读,必须是普通读: %s", comparison, query)
		}
		zero := listDueQuery(comparison, workshard.Filter{})
		if !strings.HasSuffix(zero, " ? AND 1=1 ORDER BY next_attempt_ms,op_id LIMIT ?") {
			t.Fatalf("%s 段:零值条件不过滤: %s", comparison, zero)
		}
	}
	if got := owned.Args(); len(got) != 2 || got[0] != 5 || got[1] != 40 {
		t.Fatalf("片号应按升序只走绑定参数: %v", got)
	}
}

// TestListDueOwnedRejectsLimitBeforeSQL:limit 越界在碰库之前拒绝。Repo 的 db 是 nil,走到 SQL 就会 panic,
// 所以这个用例同时钉住「ListDue 与 ListDueOwned 是同一个入口、校验在任何查询之前」。
func TestListDueOwnedRejectsLimitBeforeSQL(t *testing.T) {
	r := &Repo{}
	for _, limit := range []int{0, -1, 1001} {
		if _, err := r.ListDueOwned(context.Background(), 1, limit, workshard.Filter{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ListDueOwned limit=%d: err = %v,want ErrInvalid", limit, err)
		}
		if _, err := r.ListDue(context.Background(), 1, limit); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ListDue limit=%d: err = %v,want ErrInvalid", limit, err)
		}
	}
}

// TestStoreAdapterBindsWorkOwner:重投循环的 Store 是 StoreAdapter(值类型,内嵌 *Repo),绑分工时 assetop 按
// OwnedDueLister 做类型断言,ListDueOwned 必须经内嵌指针提升到 StoreAdapter 上,否则 mail 启动时 BindWorkOwner 回错误、拒启。
func TestStoreAdapterBindsWorkOwner(t *testing.T) {
	if _, ok := any(StoreAdapter{Repo: &Repo{}}).(assetop.OwnedDueLister); !ok {
		t.Fatal("StoreAdapter 必须实现 assetop.OwnedDueLister")
	}
}
