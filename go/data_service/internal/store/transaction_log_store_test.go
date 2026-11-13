package store

import (
	"context"
	"slices"
	"testing"

	dbpb "proto/common/database"

	"github.com/luyuancpp/proto2mysql"
)

// TestTransactionLogHasReceiverCorrelationIndex 钉住 proto 的 OptionIndex(不连库,解 proto2mysql 生成的建表语句):
// GM 回收按指令补查同指令流水(TransactionLogQuery.ToPlayerID + CorrelationIDs)要有 (to_player, correlation_id) 这条索引,
// 而且排在最后一条:proto2mysql 按位置起索引名,插在中间会让已有库的索引名对不上。
func TestTransactionLogHasReceiverCorrelationIndex(t *testing.T) {
	model := proto2mysql.NewDB()
	model.RegisterTable(&dbpb.TransactionLog{})
	ddl := model.GetCreateTableSQL(&dbpb.TransactionLog{})
	indexes := parseWantedIndexes(ddl)
	if len(indexes) == 0 {
		t.Fatalf("没从建表语句里解出任何索引:\n%s", ddl)
	}
	last := indexes[len(indexes)-1]
	if !slices.Equal(last.cols, []string{"to_player", "correlation_id"}) {
		t.Fatalf("最后一条索引是 %s %v,要的是 (to_player, correlation_id):\n%s", last.name, last.cols, ddl)
	}
}

// TestQueryLogRejectsBothPlayerConditions:双向玩家条件和只收到方的条件语义不同,同时给就是调用方拼错了,不发 SQL。
func TestQueryLogRejectsBothPlayerConditions(t *testing.T) {
	s := &TransactionLogStore{} // db 为 nil:走到发 SQL 就会 panic,能证明校验在发 SQL 之前
	if _, _, err := s.QueryLog(context.Background(), &TransactionLogQuery{PlayerID: 1, ToPlayerID: 1}); err == nil {
		t.Fatal("PlayerID 和 ToPlayerID 同时给必须拒绝")
	}
}

// TestTxLogRowsInLockOrder 钉住流水多行 INSERT IGNORE 的取锁顺序:按 tx_id 升序,且不改调用方的切片。
// 到达序拼批在 rebalance 重叠时会让两个实例在主键上反序互等(1213),见 InsertBatchIgnore 的注释;
// 真库并发对照(到达序 4/10 成环、升序 0/10)记在 docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md §9.6。
func TestTxLogRowsInLockOrder(t *testing.T) {
	in := []*TransactionLogRow{{TxID: 30}, {TxID: 10}, {TxID: 20}, {TxID: 10, Extra: "second"}}
	before := slices.Clone(in)

	got := txLogRowsInLockOrder(in)

	var ids []uint64
	for _, r := range got {
		ids = append(ids, r.TxID)
	}
	if !slices.Equal(ids, []uint64{10, 10, 20, 30}) {
		t.Fatalf("必须按 tx_id 升序拼 VALUES,got %v", ids)
	}
	if got[0].Extra != "" || got[1].Extra != "second" {
		t.Fatalf("同 tx_id 的行必须保持原相对次序(稳定排序),got %q, %q", got[0].Extra, got[1].Extra)
	}
	if !slices.Equal(in, before) {
		t.Fatal("不许就地排序:调用方的批还要按原样提交 offset")
	}
}
