package commerce

// rmb_cancel_test.go —— 人民币寄售撤单的可撤阶段白名单(2026-11-12 审计发现的回归测试)。
//
// 缺陷:RmbConsignments.Cancel 以前按排除式判定(只排除已结算、rmb_delivery、rmb_refund、已取消),漏了 manual。合同转人工之后
// 卖家一撤,status 被改成 CancelPending 而 stage 仍是 manual:合同从人工列表(status=10)里消失,人工处置入口因 status 不是
// Manual 拒绝,引擎对 manual 不推进,deliver 也不再向商城回报 MANUAL,托管的资产或已付款的订单从此没有出口。
//
// 前三个用例不连库:白名单本身,以及 Cancel 在录制驱动上「只读一次合同就有结论」。TestTiDB* 走真库(门控同 engine_tidb_test.go:
// COMMERCE_TEST_DSN,验收带 COMMERCE_REQUIRE_TIDB=1),用引擎自己的写路径把合同推到转人工再撤单。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tradepb "proto/trade"
	"trade/internal/data"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	cancelProbeID      = uint64(7001)
	cancelProbeAccount = "account-11"
)

// rmbCancellableStages 是白名单里的阶段:买家付款之前的五个,加上已经在退回的 rmb_return。
var rmbCancellableStages = []string{"rmb_escrow", "rmb_freezing", "rmb_ready", "rmb_registering", "rmb_listed", "rmb_return"}

var errCancelProbeDriver = errors.New("撤单探针驱动:这条语句不该到达驱动")

// 人民币寄售的每个阶段都要有结论:白名单里的可撤,其余(含白名单不认识的)一律不可撤。
func TestRmbCancellableIsAStageWhitelist(t *testing.T) {
	for _, stage := range rmbCancellableStages {
		require.True(t, rmbCancellable(stage), "%s 在买家付款之前(或已在退回),卖家可撤", stage)
	}
	// rmb_delivery / rmb_refund:买家已付款;manual:转人工;rmb_delivered / rmb_returned:已结案;其余是白名单不认识的阶段。
	for _, stage := range []string{"rmb_delivery", "rmb_refund", "manual", "rmb_delivered", "rmb_returned", "", "rmb_some_future_stage", "open", "escrow"} {
		require.False(t, rmbCancellable(stage), "%q 不在可撤白名单里", stage)
	}
}

// cancelProbe 是只存一张人民币寄售合同的录制驱动:按主键读合同回这一行,其余语句(开事务、加锁、写)记下来并失败。
// 用来证明被拒的撤单没有碰库:到不了驱动的语句改不到合同行。做法同 logic 包的 missingContractStore。
type cancelProbe struct {
	mu    sync.Mutex
	stmts []string
	row   []driver.Value
}

func (p *cancelProbe) record(label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stmts = append(p.stmts, label)
}

func (p *cancelProbe) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.stmts...)
}

// consignments 造一张停在 stage / status 的人民币寄售合同(卖家账号 cancelProbeAccount),回只接了这张合同的 RmbConsignments。
func (p *cancelProbe) consignments(t *testing.T, stage string, status uint32) *RmbConsignments {
	t.Helper()
	raw, err := proto.Marshal(&tradepb.TradeCommerceWorkflow{Stage: stage, Rmb: &tradepb.TradeRmbConsignmentWorkflow{SellerAccount: cancelProbeAccount, SellerOwnerRevision: 1, AssetKind: "ITEMS", PriceFen: 101}})
	require.NoError(t, err)
	// 一行的列顺序同 data.CommerceColumns:kind=6,卖家角色 11,版本 3。
	p.row = []driver.Value{int64(cancelProbeID), "request-key", []byte{}, int64(6), int64(status), int64(11), int64(0), int64(0), int64(0), int64(0), int64(101), int64(0), int64(0), int64(0), int64(0), int64(1), int64(1), int64(3), raw, int64(0), int64(0), false}
	require.Len(t, p.row, len(strings.Split(data.CommerceColumns, ",")), "合同表加了列:把探针这一行补齐")
	// 用 sql.OpenDB + 自定义 Connector,不用 sql.Register:注册是全局的,名字撞了就 panic。
	db := sql.OpenDB(cancelProbeConnector{p: p})
	t.Cleanup(func() { _ = db.Close() })
	return &RmbConsignments{Engine: &Engine{Repo: &data.CommerceRepo{DB: db, Ops: &data.AssetOpRepo{}}, Now: time.Now, Roles: fixtureRoles{}}}
}

type cancelProbeConnector struct{ p *cancelProbe }

func (c cancelProbeConnector) Connect(context.Context) (driver.Conn, error) {
	return cancelProbeConn{p: c.p}, nil
}

func (c cancelProbeConnector) Driver() driver.Driver {
	return cancelProbeDriver{p: c.p}
}

type cancelProbeDriver struct{ p *cancelProbe }

func (d cancelProbeDriver) Open(string) (driver.Conn, error) {
	return cancelProbeConn{p: d.p}, nil
}

type cancelProbeConn struct{ p *cancelProbe }

func (c cancelProbeConn) Prepare(query string) (driver.Stmt, error) {
	c.p.record(query)
	return nil, errCancelProbeDriver
}

func (c cancelProbeConn) Close() error {
	return nil
}

func (c cancelProbeConn) Begin() (driver.Tx, error) {
	c.p.record("BEGIN")
	return nil, errCancelProbeDriver
}

func (c cancelProbeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.p.record("BEGIN")
	return nil, errCancelProbeDriver
}

func (c cancelProbeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.p.record(query)
	return nil, errCancelProbeDriver
}

func (c cancelProbeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(query, "FROM trade_commerce WHERE commerce_id=?") || strings.Contains(query, "FOR UPDATE") {
		c.p.record(query)
		return nil, errCancelProbeDriver
	}
	c.p.record("get")
	return &cancelProbeRows{columns: strings.Split(data.CommerceColumns, ","), row: c.p.row}, nil
}

// cancelProbeRows 是只有一行的结果集。
type cancelProbeRows struct {
	columns []string
	row     []driver.Value
	done    bool
}

func (r *cancelProbeRows) Columns() []string {
	return r.columns
}

func (r *cancelProbeRows) Close() error {
	return nil
}

func (r *cancelProbeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.row)
	return nil
}

// 不在白名单里的阶段在预检就有结论:只读一次合同,不开事务、不锁角色,所以合同行一个字段都不会变。
// 转人工的合同是这次修的缺陷;已取消的再撤算成功、别的账号查不到这张合同是原有口径,一起守着判定顺序。
func TestRmbCancelOutsideTheWhitelistReadsTheContractOnceAndWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stage   string
		status  uint32
		account string
		want    error
	}{
		{"转人工的合同只能走人工处置入口", "manual", Manual, cancelProbeAccount, ErrConflict},
		{"买家已付款正在交付", "rmb_delivery", Delivering, cancelProbeAccount, ErrConflict},
		{"买家已付款等商城退款", "rmb_refund", 8, cancelProbeAccount, ErrConflict},
		{"已成交", "rmb_delivered", Settled, cancelProbeAccount, ErrConflict},
		{"白名单不认识的阶段默认不可撤", "rmb_some_future_stage", Waiting, cancelProbeAccount, ErrConflict},
		{"已取消的再撤一次算成功", "rmb_returned", Cancelled, cancelProbeAccount, nil},
		{"别的账号查不到这张合同", "manual", Manual, "account-99", ErrMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &cancelProbe{}
			s := probe.consignments(t, tc.stage, tc.status)
			err := s.Cancel(context.Background(), tc.account, cancelProbeID)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
			require.Equal(t, []string{"get"}, probe.recorded(), "只读一次合同就有结论,没有开事务")
		})
	}
}

// 白名单里的阶段过得了预检,走到开事务。录制驱动不给开事务,所以这里只证明预检放行;撤单写了什么由下面的真库用例看。
func TestRmbCancelLetsWhitelistedStagesThroughThePrecheck(t *testing.T) {
	for _, stage := range rmbCancellableStages {
		t.Run(stage, func(t *testing.T) {
			probe := &cancelProbe{}
			s := probe.consignments(t, stage, Escrowing)
			err := s.Cancel(context.Background(), cancelProbeAccount, cancelProbeID)
			require.ErrorIs(t, err, errCancelProbeDriver)
			require.Equal(t, []string{"get", "BEGIN"}, probe.recorded())
		})
	}
}

// rmbItemsAt 新建一张物品寄售并推到 stage(买家付款之前的阶段;rmb_escrow 是托管扣款已发出、还没有结局)。
func rmbItemsAt(t *testing.T, s *RmbConsignments, stage string) *tradepb.TradeCommerceRecord {
	t.Helper()
	r := rmbCreate(t, s, false)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	for n := 0; n < 4; n++ {
		if _, w := state(t, s.Engine, r.CommerceId); w.Stage == stage {
			return r
		}
		settleLegs(t, s.Engine, 2)
		require.NoError(t, rmbStep(t, s, r.CommerceId))
	}
	_, w := state(t, s.Engine, r.CommerceId)
	require.Equal(t, stage, w.Stage)
	return r
}

// requireRowUnchanged 断言合同行与 before 逐字段相同,含状态、版本、updated_ms 与 workflow 的原字节。
func requireRowUnchanged(t *testing.T, e *Engine, before *tradepb.TradeCommerceRecord) {
	t.Helper()
	after, err := e.Repo.Get(context.Background(), before.CommerceId)
	require.NoError(t, err)
	require.True(t, proto.Equal(before, after), "合同行被改了\n之前: %v\n之后: %v", before, after)
}

// requireOnlyManualContract 断言人工列表(status=10)里恰好是这一张合同。
func requireOnlyManualContract(t *testing.T, e *Engine, id uint64) {
	t.Helper()
	manual, err := e.Repo.ListManual(context.Background(), 0, 0, 10)
	require.NoError(t, err)
	require.Len(t, manual, 1)
	require.Equal(t, id, manual[0].CommerceId)
}

// 退回腿没到账转人工(原因 rmb_return_failed)之后卖家再撤:拒绝,合同行一个字段不变,仍在人工列表里,人工补发照常能做。
func TestTiDBRmbCancelRefusesManualContractAfterReturnFailed(t *testing.T) {
	s, _, _ := rmbRuntime(t)
	ctx := context.Background()
	id := rmbItemsAt(t, s, "rmb_ready").CommerceId
	require.NoError(t, s.Cancel(ctx, "account-11", id))
	require.NoError(t, rmbStep(t, s, id), "rmb_ready 认撤单请求,进 rmb_return")
	require.NoError(t, rmbStep(t, s, id), "发出退回腿")
	settleLegs(t, s.Engine, uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_REJECTED))
	require.NoError(t, rmbStep(t, s, id), "退回腿没到账,转人工")
	before, w := state(t, s.Engine, id)
	require.EqualValues(t, Manual, before.Status)
	require.Equal(t, "manual", w.Stage)
	require.Equal(t, manualReasonRmbReturn, w.GetQuarantine().GetReason())

	require.ErrorIs(t, s.Cancel(ctx, "account-11", id), ErrConflict)
	requireRowUnchanged(t, s.Engine, before)
	requireOnlyManualContract(t, s.Engine, id)

	// 合同还在人工状态:人工处置入口照常能算出补发计划(只算计划,不写)。以前被撤成 CancelPending 之后这里回「合同不在人工状态」。
	plan, err := ResolveManual(ctx, ManualDeps{Repo: s.Engine.Repo, Roles: s.Engine.Roles}, ManualRequest{CommerceID: id, ExpectVersion: before.Version, Action: ManualReissue, Operator: "ops", Reason: "return leg never arrived"}, s.Engine.now(), false)
	require.NoError(t, err)
	require.Equal(t, "rmb_return", plan.Stage)
	require.Equal(t, []string{"seller_return#2"}, plan.NewLegs)
	requireRowUnchanged(t, s.Engine, before)
}

// 买家已付款、交付腿部分发放转人工之后卖家再撤:拒绝,合同行一个字段不变;商城下一轮拉到同一条意图,仍收到 MANUAL。
func TestTiDBRmbCancelRefusesManualContractAfterPaidDeliveryWentPartial(t *testing.T) {
	s, _, probe := rmbRuntime(t)
	ctx := context.Background()
	id := rmbItemsAt(t, s, "rmb_listed").CommerceId
	_, w := state(t, s.Engine, id)
	in := rmbDelivery{Order: "00112233-4455-6677-8899-aabbccddeeff", Buyer: "22", Account: "account-22", Revision: "1", Listing: probe.Registers[0]["listingId"].(string), Reference: w.Rmb.EscrowReference, Proof: w.Rmb.EscrowProof, Amount: 101}
	require.NoError(t, s.deliver(ctx, in), "买家已付款,进 rmb_delivery")
	require.NoError(t, rmbStep(t, s, id), "发出交付腿")
	settleLegs(t, s.Engine, uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL))
	require.NoError(t, rmbStep(t, s, id), "交付腿部分发放,转人工")
	before, w := state(t, s.Engine, id)
	require.EqualValues(t, Manual, before.Status)
	require.Equal(t, "manual", w.Stage)
	require.Equal(t, manualReasonAppliedPartial, w.GetQuarantine().GetReason())
	require.Empty(t, probe.Receipts)

	require.ErrorIs(t, s.Cancel(ctx, "account-11", id), ErrConflict)
	requireRowUnchanged(t, s.Engine, before)
	requireOnlyManualContract(t, s.Engine, id)

	// 以前被撤成 CancelPending 之后 deliver 什么都不回,商城的订单一直停在待交付。
	require.NoError(t, s.deliver(ctx, in))
	require.Len(t, probe.Receipts, 1)
	require.Equal(t, "MANUAL", probe.Receipts[0]["state"])
	requireRowUnchanged(t, s.Engine, before)
}

// 白名单里的阶段照常可撤:撤单只记请求(CancelRequested、CancelPending),阶段由引擎推进;托管退回之后按取消结案。
func TestTiDBRmbCancelStillWorksInEveryWhitelistedStage(t *testing.T) {
	ctx := context.Background()
	for _, stage := range rmbCancellableStages {
		t.Run(stage, func(t *testing.T) {
			s, _, _ := rmbRuntime(t)
			var id uint64
			switch stage {
			case "rmb_freezing":
				// 角色寄售:建出来就停在冻结。
				id = rmbCreate(t, s, true).CommerceId
			case "rmb_return":
				// 已经在退回:在 rmb_ready 撤一次、推进一步就到;下面再撤一次是重复请求。
				id = rmbItemsAt(t, s, "rmb_ready").CommerceId
				require.NoError(t, s.Cancel(ctx, "account-11", id))
				require.NoError(t, rmbStep(t, s, id))
			default:
				id = rmbItemsAt(t, s, stage).CommerceId
			}
			_, w := state(t, s.Engine, id)
			require.Equal(t, stage, w.Stage)

			require.NoError(t, s.Cancel(ctx, "account-11", id))
			cur, w := state(t, s.Engine, id)
			require.Equal(t, stage, w.Stage)
			require.True(t, w.CancelRequested)
			require.EqualValues(t, CancelPending, cur.Status)

			// 每一步之前把在途的腿按到账终结:托管扣到了的原样退回,角色寄售直接解冻。
			for n := 0; n < 8 && cur.Status != Cancelled; n++ {
				settleLegs(t, s.Engine, 2)
				require.NoError(t, rmbStep(t, s, id))
				cur, w = state(t, s.Engine, id)
			}
			require.EqualValues(t, Cancelled, cur.Status)
			require.Equal(t, "rmb_returned", w.Stage)
		})
	}
}
