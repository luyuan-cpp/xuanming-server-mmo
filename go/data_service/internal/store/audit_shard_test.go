package store

import (
	"math"
	"slices"
	"testing"

	dbpb "proto/common/database"

	"github.com/luyuancpp/proto2mysql"
	"google.golang.org/protobuf/proto"
)

// TestAuditShard 钉住分桶规则(audit_shard.go):16 段、按 id 取模,与两张表的 SHARD_ROW_ID_BITS=4 相同。
// 改分桶数会让已落库行的 shard 落到错的段里,按段清理与按段扫的查询都会漏掉它们。
func TestAuditShard(t *testing.T) {
	if AuditShardCount != 16 {
		t.Fatalf("AuditShardCount = %d,改它要重建流水库(见常量注释)", AuditShardCount)
	}
	for id, want := range map[uint64]uint32{0: 0, 1: 1, 15: 15, 16: 0, 99: 3, math.MaxUint64: 15} {
		if got := AuditShard(id); got != want {
			t.Errorf("AuditShard(%d) = %d, want %d", id, got, want)
		}
	}
	if want := "shard IN (0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15)"; auditShardInSQL != want {
		t.Fatalf("auditShardInSQL = %q, want %q", auditShardInSQL, want)
	}
}

// TestTxLogInsertColumnsEndWithShard:列清单最后一列是 shard,与 InsertBatchIgnore 每行最后一个实参 AuditShard(tx_id) 对应。
func TestTxLogInsertColumnsEndWithShard(t *testing.T) {
	if got := txLogInsertColumns[len(txLogInsertColumns)-1]; got != "shard" {
		t.Fatalf("txLogInsertColumns 最后一列应是 shard,got %q(%v)", got, txLogInsertColumns)
	}
	if n := len(slices.DeleteFunc(slices.Clone(txLogInsertColumns), func(c string) bool { return c != "shard" })); n != 1 {
		t.Fatalf("shard 只能出现一次,got %d", n)
	}
}

// TestAuditTimeIndexesAreSharded 钉住 proto 的 OptionIndex(不连库,解 proto2mysql 生成的建表语句):两张审计表里含时间列的
// 索引,要么以玩家打头(玩家号打散,不需要分桶),要么时间列紧跟在 shard 后面。再加一条不分桶的时间索引,
// 就把全服追加写重新压回索引末尾同一个 TiKV Region(docs/design/audit-pipeline-horizontal-scale-20261106.md §2.C)。
func TestAuditTimeIndexesAreSharded(t *testing.T) {
	for _, tc := range []struct {
		msg         proto.Message
		timeCol     string
		playerFirst map[string]bool // 允许不分桶的打头列
	}{
		{&dbpb.TransactionLog{}, "timestamp_sec", map[string]bool{"from_player": true, "to_player": true}},
		{&dbpb.PlayerSnapshot{}, "created_at", map[string]bool{"player_id": true}},
	} {
		model := proto2mysql.NewDB()
		model.RegisterTable(tc.msg)
		ddl := model.GetCreateTableSQL(tc.msg)
		indexes := parseWantedIndexes(ddl)
		if len(indexes) == 0 {
			t.Fatalf("没从建表语句里解出任何索引:\n%s", ddl)
		}
		timeIndexes := 0
		for _, idx := range indexes {
			pos := slices.Index(idx.cols, tc.timeCol)
			if pos < 0 || tc.playerFirst[idx.cols[0]] {
				continue
			}
			timeIndexes++
			if pos == 0 || idx.cols[pos-1] != "shard" {
				t.Errorf("索引 %s %v:时间列 %s 前面必须是 shard\n%s", idx.name, idx.cols, tc.timeCol, ddl)
			}
		}
		if timeIndexes == 0 {
			t.Errorf("%s 一条按时间的分桶索引都没有(按时间的清理与查询会整表扫):\n%s", tc.timeCol, ddl)
		}
	}
}
