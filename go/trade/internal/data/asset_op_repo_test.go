package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tradepb "proto/trade"

	"shared/assetop"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// 资产 outbox 的纯单测:不连库,只钉住"手写 SQL 与表 proto 是否还对得上"以及状态映射。
// 真库行为(seq 分配的锁序、Claim 的 CAS)见 shared/assetop 的集成测试与端到端冒烟。

// TestAssetOpColumnsCoverEveryProtoField 是本文件里最要紧的一条:
// assetOpColumns 是手写的列清单,InsertOp 的 VALUES 与 scanAssetOp 的扫描顺序都按它排。
// 往 TradeAssetOpRecord 里加字段却忘了改这三处,表现是"新列永远是零值"或者运行期
// "Scan 的目标数量对不上",两种都不会在编译期暴露。这里用描述符机械比对。
func TestAssetOpColumnsCoverEveryProtoField(t *testing.T) {
	fields := (&tradepb.TradeAssetOpRecord{}).ProtoReflect().Descriptor().Fields()
	want := make([]string, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		want = append(want, string(fields.Get(i).Name()))
	}

	got := splitColumns(assetOpColumns)
	if len(got) != len(want) {
		t.Fatalf("assetOpColumns 有 %d 列,TradeAssetOpRecord 有 %d 个字段:\n  列 = %v\n  字段 = %v",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 列 = %q,proto 第 %d 个字段 = %q(顺序必须一致:InsertOp 的 VALUES 与 scanAssetOp 都按它排)",
				i, got[i], i, want[i])
		}
	}
}

// splitColumns 把 "`a`, `b`" 拆成 ["a","b"]。
func splitColumns(cols string) []string {
	parts := strings.Split(cols, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.Trim(strings.TrimSpace(p), "`"))
	}
	return out
}

// TestAssetOpHasManualResolutionColumns:§4.38 的人工终结必须留痕。没有这两列的话,
// 一行卡死的资产指令可以被改成"已发放"而查不到是谁改的、依据是什么。
// 本工程未上线、没有老号,现在加列零迁移成本;上线后再加就是一次表变更。
func TestAssetOpHasManualResolutionColumns(t *testing.T) {
	fields := (&tradepb.TradeAssetOpRecord{}).ProtoReflect().Descriptor().Fields()
	for _, name := range []string{"resolved_by", "resolve_reason"} {
		if fields.ByName(protoreflect.Name(name)) == nil {
			t.Errorf("TradeAssetOpRecord 缺 %s:人工终结没有留痕列", name)
		}
		if !strings.Contains(assetOpColumns, "`"+name+"`") {
			t.Errorf("assetOpColumns 缺 %s", name)
		}
	}
}

// TestResolveManuallyRefusesNonFinalStatus:人工终结只能落成四个终结状态之一。
// 落成 PENDING 会被重投循环立刻再领走(等于没终结),落成 UNSPECIFIED 会让客服查不到结局。
// 这两种都必须在碰事务之前失败 —— 本用例的仓库没有连接池,越过这一步会当场 panic。
func TestResolveManuallyRefusesNonFinalStatus(t *testing.T) {
	repo, err := NewAssetOpRepo(nil, time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	for _, status := range []assetop.Status{assetop.StatusPending, assetop.Status(250)} {
		resolved, err := repo.ResolveManually(context.Background(),
			assetop.ManualResolution{OpID: 1, Final: status, Operator: "ops", Reason: "test"}, 1)
		if err == nil {
			t.Errorf("status=%v 应被拒绝", status)
		}
		if resolved {
			t.Errorf("status=%v 被拒时不能报告已终结", status)
		}
	}
}

// TestPendingStatusIsNotZero:重投循环按 status = PendingStatus() 领行。
// 零值行(写坏 / 半截插入)绝不能被当成待办领走,所以 PENDING 不能是 0。
func TestPendingStatusIsNotZero(t *testing.T) {
	if PendingStatus() == 0 {
		t.Fatal("PENDING 的库值不能是 0:零值行会被重投循环当成待办反复领取")
	}
	if got, want := PendingStatus(), uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING); got != want {
		t.Errorf("PendingStatus() = %d, want %d", got, want)
	}
}

// TestStatusToRecordMapsEverySemanticStatus:assetop.Status 只表达语义、不绑库值(§4.43 #23),
// 映射只在这一处。漏一个分支的后果是把一个已终结的行落成 UNSPECIFIED(客服查不到结局)。
func TestStatusToRecordMapsEverySemanticStatus(t *testing.T) {
	cases := []struct {
		in   assetop.Status
		want tradepb.TradeAssetOpStatus
	}{
		{assetop.StatusPending, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING},
		{assetop.StatusApplied, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED},
		{assetop.StatusRejected, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_REJECTED},
		{assetop.StatusAborted, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED},
		{assetop.StatusAppliedPartial, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status_%d", tc.in), func(t *testing.T) {
			if got := StatusToRecord(tc.in); got != tc.want {
				t.Errorf("StatusToRecord(%d) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}

	// 未知语义值既不能落成 PENDING(会被循环反复领走),也不能落成 APPLIED(伪装成功)。
	unknown := StatusToRecord(assetop.Status(250))
	if unknown != tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_UNSPECIFIED {
		t.Errorf("未知语义值 → %v, want UNSPECIFIED", unknown)
	}
}

// TestIsRetryableTxError:只有死锁 / 锁等待超时 / TiDB 写冲突值得整体重试。
// 把别的错误(比如撞唯一键)判成可重试,会让一笔失败的上架被反复重放。
func TestIsRetryableTxError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"死锁 1213", &mysql.MySQLError{Number: 1213}, true},
		{"锁等待超时 1205", &mysql.MySQLError{Number: 1205}, true},
		{"TiDB 写冲突 9007", &mysql.MySQLError{Number: 9007}, true},
		{"唯一键冲突 1062", &mysql.MySQLError{Number: 1062}, false},
		{"包了一层的死锁", fmt.Errorf("insert trade_asset_op: %w", &mysql.MySQLError{Number: 1213}), true},
		{"普通错误", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryableTxError(tc.err); got != tc.want {
				t.Errorf("IsRetryableTxError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestNewAssetOpRepoUsesTheTwoTables:表名经 assetop 的正则校验(防拼接注入),
// 并且与 trade_table.proto 的 OptionTableName 逐字一致 —— 两边不一致的表现是
// "表建出来了但所有查询报 table doesn't exist"。
func TestNewAssetOpRepoUsesTheTwoTables(t *testing.T) {
	repo, err := NewAssetOpRepo(nil, time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	tables := repo.Tables()
	if tables.SeqTable != AssetOpSeqTableName || tables.OpTable != AssetOpTableName {
		t.Errorf("Tables() = %+v, want seq=%s op=%s", tables, AssetOpSeqTableName, AssetOpTableName)
	}
	if tables.PendingStatus != PendingStatus() {
		t.Errorf("Tables().PendingStatus = %d, want %d", tables.PendingStatus, PendingStatus())
	}
}

// TestTablesIncludeAssetOpTables:schemamigrate 只建 Tables() 列出的表。
// 漏登记的表现是启动不报错、上架时报 "table doesn't exist"。
func TestTablesIncludeAssetOpTables(t *testing.T) {
	var hasSeq, hasOp bool
	for _, m := range Tables() {
		switch m.(type) {
		case *tradepb.TradePlayerOpSeqRecord:
			hasSeq = true
		case *tradepb.TradeAssetOpRecord:
			hasOp = true
		}
	}
	if !hasSeq {
		t.Error("Tables() 缺 TradePlayerOpSeqRecord(trade_player_op_seq):seq 分配无表可用")
	}
	if !hasOp {
		t.Error("Tables() 缺 TradeAssetOpRecord(trade_asset_op):outbox 无表可用")
	}
}
