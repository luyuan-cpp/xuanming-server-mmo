package savebarrier

import (
	"math"
	"testing"
)

func TestClassifyBarrier(t *testing.T) {
	v1, v2 := Version(3, 10), Version(3, 11)
	for _, tc := range []struct {
		name                string
		pending, ack1, ack2 string
		want                BarrierState
	}{
		{"无历史", "", "", "", BarrierCommitted},
		{"已提交", v2, v2, v2, BarrierCommitted},
		{"第一张表在途", v2, v1, v2, BarrierInflight},
		{"首存未回 ACK", v2, "", "", BarrierInflight},
		{"ACK 高于 pending", v1, v2, v1, BarrierAckAhead},
		{"pending 缺失而 ACK 存在", "", v1, "", BarrierAckAhead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyBarrier(tc.pending, tc.ack1, tc.ack2)
			if err != nil || got != tc.want {
				t.Fatalf("ClassifyBarrier = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	if _, err := ClassifyBarrier("3:10", "", ""); err == nil {
		t.Fatal("不规范的版本串必须报错")
	}
}

func TestReconcileVerdictTable(t *testing.T) {
	old, cur, newer := Version(3, 10), Version(3, 11), Version(4, 1)
	for _, tc := range []struct {
		name string
		obs  Observation
		want Verdict
	}{
		{"V1 全部相等", Observation{Pending: cur, AckFirst: cur, AckSecond: cur, SQLFirst: cur, SQLSecond: cur}, VerdictConsistent},
		{"V6 双方都没有历史", Observation{}, VerdictConsistent},
		{"V2 主结局:ACK 被推到 SQL 版本", Observation{Pending: old, AckFirst: cur, AckSecond: cur, SQLFirst: cur, SQLSecond: cur}, VerdictAdoptSQL},
		{"V2 次结局:ACK 也回退了", Observation{Pending: old, AckFirst: old, AckSecond: old, SQLFirst: cur, SQLSecond: cur}, VerdictAdoptSQL},
		{"V2 SQL 已提交 pending、只丢了 ACK", Observation{Pending: cur, AckFirst: old, AckSecond: cur, SQLFirst: cur, SQLSecond: cur}, VerdictAdoptSQL},
		{"V2 新 epoch 的 SQL 行", Observation{Pending: cur, AckFirst: cur, AckSecond: cur, SQLFirst: newer, SQLSecond: newer}, VerdictAdoptSQL},
		{"V7 屏障缺失而 SQL 有历史", Observation{SQLFirst: cur, SQLSecond: cur}, VerdictAdoptSQL},
		{"V3 pending 新于 SQL", Observation{Pending: cur, AckFirst: old, AckSecond: old, SQLFirst: old, SQLSecond: old}, VerdictInflight},
		{"V3 首存尚未落 SQL", Observation{Pending: cur}, VerdictInflight},
		{"V3 两行不同但都不新于 pending", Observation{Pending: cur, AckFirst: cur, AckSecond: old, SQLFirst: cur, SQLSecond: old}, VerdictInflight},
		{"V4 两行不同且一行新于 pending", Observation{Pending: old, AckFirst: old, AckSecond: old, SQLFirst: cur, SQLSecond: old}, VerdictReconciling},
		{"V5 ACK 高于 SQL", Observation{Pending: cur, AckFirst: cur, AckSecond: cur, SQLFirst: old, SQLSecond: old}, VerdictSQLBehind},
		{"V5 有 ACK 而 SQL 行缺失", Observation{Pending: cur, AckFirst: cur, AckSecond: cur}, VerdictSQLBehind},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Reconcile(tc.obs)
			if err != nil || got != tc.want {
				t.Fatalf("Reconcile = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// 定宽串的字符串比较必须等于数值比较,包括 2^53 相邻值与 uint64 最大值。
func TestReconcileComparesVersionsWithoutFloatLoss(t *testing.T) {
	const twoPow53 = uint64(1) << 53
	lo, hi := Version(twoPow53, 1), Version(twoPow53+1, 1)
	if got, _ := Reconcile(Observation{Pending: lo, AckFirst: lo, AckSecond: lo, SQLFirst: hi, SQLSecond: hi}); got != VerdictAdoptSQL {
		t.Fatalf("2^53 相邻值比较错误: %v", got)
	}
	maxV := Version(math.MaxUint64, math.MaxUint64)
	if got, _ := Reconcile(Observation{Pending: maxV, AckFirst: maxV, AckSecond: maxV, SQLFirst: maxV, SQLSecond: maxV}); got != VerdictConsistent {
		t.Fatalf("uint64 最大值比较错误: %v", got)
	}
}

func TestReconcileRejectsMalformedVersions(t *testing.T) {
	if _, err := Reconcile(Observation{SQLFirst: "00000000000000000003:x0000000000000000001"}); err == nil {
		t.Fatal("畸形 SQL 版本必须报错")
	}
}

func TestRowVersion(t *testing.T) {
	if RowVersion(0, 0) != "" {
		t.Fatal("(0,0) 是未经围栏写入的行,必须记为空串")
	}
	if RowVersion(5, 0) != Version(5, 0) || RowVersion(5, 7) != Version(5, 7) {
		t.Fatal("非零围栏必须原样换成版本串")
	}
}
