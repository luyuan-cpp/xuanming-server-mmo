package data

// commerce_repo_test.go —— trade_commerce.on_book(材料订单是否挂在盘口上)的投影规则与写入口,不连库
// (录制驱动 recovScript 见 asset_op_recovery_test.go)。钉的是「只有 kind=4 的买卖单在 market_open 才算在盘口上」
// 「Insert / Save 按 workflow 现算、不信记录上带来的旧值」「列、占位符、扫描三处对齐」。
// 盘口选单真正跳过托管中的订单在真库用例 commerce.TestTiDBOrderBookSkipsOrdersStillInEscrow。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"

	tradepb "proto/trade"

	"google.golang.org/protobuf/proto"
)

func TestOnBookIsOnlyABookOrderInMarketOpen(t *testing.T) {
	order := func(kind, side uint32) *tradepb.TradeCommerceRecord {
		return &tradepb.TradeCommerceRecord{Kind: kind, Side: side}
	}
	at := func(stage string) *tradepb.TradeCommerceWorkflow { return &tradepb.TradeCommerceWorkflow{Stage: stage} }
	for _, c := range []struct {
		name string
		r    *tradepb.TradeCommerceRecord
		w    *tradepb.TradeCommerceWorkflow
		want bool
	}{
		{"买单托管完成、挂在盘口上", order(4, 1), at("market_open"), true},
		{"卖单托管完成、挂在盘口上", order(4, 2), at("market_open"), true},
		{"托管扣款还在途", order(4, 2), at("escrow"), false},
		{"撤单退回中", order(4, 1), at("return"), false},
		{"撤单后等成交子单收尾", order(4, 1), at("market_wait"), false},
		{"已结案", order(4, 1), at("done"), false},
		{"转人工", order(4, 2), at("manual"), false},
		{"成交子单不论 stage 都不在盘口上", order(4, 0), at("market_open"), false},
		{"在架寄售不是盘口订单", order(3, 0), at("open"), false},
		{"在拍拍品不是盘口订单", order(5, 0), at("lot_open"), false},
		{"别的合同即使 stage 写成 market_open 也不算", order(3, 1), at("market_open"), false},
		{"没有 workflow", order(4, 1), nil, false},
	} {
		if got := OnBook(c.r, c.w); got != c.want {
			t.Errorf("%s: OnBook = %v,want %v", c.name, got, c.want)
		}
	}
}

func commerceRecordingRepo(t *testing.T, s *recovScript) *CommerceRepo {
	t.Helper()
	db := sql.OpenDB(recovConnector{s: s})
	t.Cleanup(func() { _ = db.Close() })
	return &CommerceRepo{DB: db}
}

// inTx 在录制库上开一个事务跑 body 并提交;录制驱动不碰真库,事务只为满足 Insert / Save 的签名。
func inTx(t *testing.T, repo *CommerceRepo, body func(*sql.Tx) error) {
	t.Helper()
	tx, err := repo.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := body(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("事务体失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// lastCall 返回最后一条以 prefix 打头的录音。
func lastCall(t *testing.T, s *recovScript, prefix string) recovCall {
	t.Helper()
	calls := s.callsWithPrefix(prefix)
	if len(calls) == 0 {
		t.Fatalf("没有录到以 %q 打头的语句: %+v", prefix, s.recordedCalls())
	}
	return calls[len(calls)-1]
}

// Save 写的 on_book 按要写的 workflow 现算,记录上带来的旧值不算数;写成功之后记录跟库一致。
func TestCommerceSaveWritesOnBookFromTheWorkflow(t *testing.T) {
	const wantSQL = "UPDATE trade_commerce SET status=?,buyer_player_id=?,filled_quantity=?,expires_ms=?,updated_ms=?,version=version+1,workflow_proto=?,on_book=? WHERE commerce_id=? AND version=?"
	for _, c := range []struct {
		name  string
		stage string
		stale bool
		want  bool
	}{
		{"托管完成进盘口", "market_open", false, true},
		{"撤单离开盘口", "return", true, false},
		{"转人工离开盘口", "manual", true, false},
	} {
		s := &recovScript{affected: 1}
		repo := commerceRecordingRepo(t, s)
		v := &tradepb.TradeCommerceRecord{CommerceId: 7, Kind: 4, Side: 2, Status: 3, Version: 3, OnBook: c.stale}
		inTx(t, repo, func(tx *sql.Tx) error {
			return repo.Save(context.Background(), tx, v, &tradepb.TradeCommerceWorkflow{Stage: c.stage}, 1000)
		})
		call := lastCall(t, s, "UPDATE trade_commerce SET")
		if call.sql != wantSQL {
			t.Fatalf("%s: 语句 =\n  %s\nwant\n  %s", c.name, call.sql, wantSQL)
		}
		if got := call.args[6]; got != c.want {
			t.Fatalf("%s: 写入的 on_book = %v,want %v(全部实参 %v)", c.name, got, c.want, call.args)
		}
		if v.OnBook != c.want {
			t.Fatalf("%s: 写成功之后记录上的 OnBook = %v,want %v", c.name, v.OnBook, c.want)
		}
	}
}

// Insert 按 v.WorkflowProto 现算 on_book;解不开的 workflow 不进盘口,插入照常。列数与占位符个数一致。
func TestCommerceInsertDerivesOnBookFromTheStoredWorkflow(t *testing.T) {
	marshal := func(stage string) []byte {
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&tradepb.TradeCommerceWorkflow{Stage: stage})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		return raw
	}
	columns := strings.Split(CommerceColumns, ",")
	if columns[len(columns)-1] != "on_book" {
		t.Fatalf("on_book 应是 CommerceColumns 的最后一列,实际最后一列是 %q", columns[len(columns)-1])
	}
	for _, c := range []struct {
		name  string
		side  uint32
		raw   []byte
		stale bool
		want  bool
	}{
		{"直接插入的在盘口订单", 1, marshal("market_open"), false, true},
		{"新下的订单还在托管", 2, marshal("escrow"), true, false},
		{"成交子单", 0, marshal("deliver"), true, false},
		{"workflow 解不开", 1, []byte{0xff, 0xff, 0xff}, true, false},
	} {
		s := &recovScript{affected: 1}
		repo := commerceRecordingRepo(t, s)
		v := &tradepb.TradeCommerceRecord{CommerceId: 9, RequestKey: "k", Kind: 4, Side: c.side, WorkflowProto: c.raw, OnBook: c.stale}
		inTx(t, repo, func(tx *sql.Tx) error { return repo.Insert(context.Background(), tx, v) })
		call := lastCall(t, s, "INSERT INTO trade_commerce")
		if !strings.HasPrefix(call.sql, "INSERT INTO trade_commerce ("+CommerceColumns+") VALUES") {
			t.Fatalf("%s: 列清单不是 CommerceColumns: %s", c.name, call.sql)
		}
		if placeholders := strings.Count(call.sql, "?"); placeholders != len(columns) || len(call.args) != len(columns) {
			t.Fatalf("%s: %d 列,占位符 %d 个,实参 %d 个", c.name, len(columns), placeholders, len(call.args))
		}
		if got := call.args[len(call.args)-1]; got != c.want {
			t.Fatalf("%s: 写入的 on_book = %v,want %v", c.name, got, c.want)
		}
		if v.OnBook != c.want {
			t.Fatalf("%s: 插入之后记录上的 OnBook = %v,want %v", c.name, v.OnBook, c.want)
		}
	}
}

// ScanCommerce 按 CommerceColumns 的顺序把 on_book 扫回记录(库里 TINYINT(1),驱动交上来的是整数)。
func TestScanCommerceReadsOnBook(t *testing.T) {
	columns := strings.Split(CommerceColumns, ",")
	for _, onBook := range []int64{1, 0} {
		s := &recovScript{rowsFor: func(string, []driver.Value) ([]string, [][]driver.Value) {
			row := make([]driver.Value, len(columns))
			for i, col := range columns {
				switch col {
				case "request_key":
					row[i] = "k"
				case "request_digest", "workflow_proto":
					row[i] = []byte{}
				case "on_book":
					row[i] = onBook
				default:
					row[i] = int64(0)
				}
			}
			return columns, [][]driver.Value{row}
		}}
		r, err := commerceRecordingRepo(t, s).Get(context.Background(), 9)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if r.OnBook != (onBook == 1) {
			t.Fatalf("库里 on_book=%d,扫回的 OnBook = %v", onBook, r.OnBook)
		}
	}
}
