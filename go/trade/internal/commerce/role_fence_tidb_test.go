package commerce

// 角色护栏接到 trade 之后的真库回归(docs/design/horizontal-scale-logical-db.md §3)。门控同 engine_tidb_test.go:
// COMMERCE_TEST_DSN + COMMERCE_REQUIRE_TIDB=1。目录是替身,副本表、合同表和挂起 SQL 都是真的。
//
// 挂起用例从 before-stage-c 的 data_service/internal/store/account_role_cross_domain_tidb_test.go 搬来,判据没变,
// 只是从「Begin 回 ErrConflict」改成「PrepareRoleFreeze 回 blocker,本库不冻结」。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	rfpb "proto/role_fence"
	tradepb "proto/trade"
	"shared/rolefence"
	"shared/roleidentity"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// fenceDirectory 是目录 GetRoleFenceState 的替身。
type fenceDirectory struct {
	mu     sync.Mutex
	states map[uint64]*rfpb.RoleFenceState
	reads  int
}

func (d *fenceDirectory) GetRoleFenceState(_ context.Context, player uint64) (*rfpb.RoleFenceState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	state, ok := d.states[player]
	if !ok {
		return nil, rolefence.ErrRoleNotFound
	}
	return state, nil
}

func (d *fenceDirectory) set(state *rfpb.RoleFenceState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.states[state.PlayerId] = state
}

func (d *fenceDirectory) readCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads
}

func fenceOf(player uint64, account string, revision, token, version uint64) *rfpb.RoleFenceState {
	return &rfpb.RoleFenceState{PlayerId: player, Account: account, OwnerRevision: revision, FreezeToken: token, Version: version, HomeZone: 1}
}

// finalFenceOf 是最终冻结的状态:目录转过 FREEZING 了。
func finalFenceOf(player uint64, account string, revision, token, version uint64) *rfpb.RoleFenceState {
	state := fenceOf(player, account, revision, token, version)
	state.FreezeFinal = true
	return state
}

func fenceEngine(t *testing.T) (*Engine, *fenceDirectory) {
	t.Helper()
	e := tiDBEngine(t)
	directory := &fenceDirectory{states: map[uint64]*rfpb.RoleFenceState{}}
	roles, err := NewRoleAuthority(e.Repo.DB, directory)
	require.NoError(t, err)
	e.Roles = roles
	return e, directory
}

func fenceActor(t *testing.T, player uint64, account string, revision uint64) context.Context {
	t.Helper()
	ctx, err := roleidentity.Capture(context.Background(), &dspb.AccountRoleOwnership{Account: account, OwnerRevision: revision, Role: &base.AccountSimplePlayer{PlayerId: player}})
	require.NoError(t, err)
	return ctx
}

func fenceConsignment(ctx context.Context, e *Engine, player uint64, requestID string) (*tradepb.TradeCommerceRecord, error) {
	return e.CreateConsignment(ctx, player, &tradepb.CreateConsignmentRequest{ClientRequestId: requestID, Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}}, CurrencyType: 1, Price: 50, DurationMs: 3_600_000})
}

func contractCount(t *testing.T, e *Engine) int {
	t.Helper()
	var n int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_commerce").Scan(&n))
	return n
}

func TestTiDBRoleFenceFrozenRoleRejectsNewContract(t *testing.T) {
	e, directory := fenceEngine(t)
	directory.set(fenceOf(11, "seller", 1, 88, 2))
	ctx := fenceActor(t, 11, "seller", 1)
	_, err := fenceConsignment(ctx, e, 11, "frozen-first")
	require.ErrorIs(t, err, ErrConflict)
	require.ErrorIs(t, err, errRoleConflict, "冻结是长期状态,后台按分钟级复查")
	require.Zero(t, contractCount(t, e))

	// 解冻推送丢了:副本还是冻结态,下一次写入读到权威状态之后通过。
	directory.set(fenceOf(11, "seller", 1, 0, 3))
	_, err = fenceConsignment(ctx, e, 11, "thawed-first")
	require.NoError(t, err)
	require.Equal(t, 1, contractCount(t, e))
}

func TestTiDBRoleFenceOldOwnerRejectedNewOwnerHealsAfterTransfer(t *testing.T) {
	e, directory := fenceEngine(t)
	seller, buyer := fenceActor(t, 11, "seller", 1), fenceActor(t, 11, "buyer", 2)
	directory.set(fenceOf(11, "seller", 1, 0, 1))
	old, err := fenceConsignment(seller, e, 11, "before-transfer")
	require.NoError(t, err)

	// 冻结写进了副本,过户提交,推送丢了。
	_, err = rolefence.Apply(context.Background(), e.Repo.DB, fenceOf(11, "seller", 1, 88, 2))
	require.NoError(t, err)
	directory.set(fenceOf(11, "buyer", 2, 0, 3))

	// 新主先到:副本修订号落后,自愈后通过,合同记下新主的身份。
	r, err := fenceConsignment(buyer, e, 11, "new-owner-first")
	require.NoError(t, err)
	_, w := state(t, e, r.CommerceId)
	require.Equal(t, "buyer", w.SellerIdentity.GetAccount())
	require.EqualValues(t, 2, w.SellerIdentity.GetOwnerRevision())

	// 旧主的会话:副本修订号更大,直接拒绝,不打 RPC。
	reads := directory.readCount()
	_, err = fenceConsignment(seller, e, 11, "old-owner-late")
	require.ErrorIs(t, err, ErrConflict)
	require.Equal(t, reads, directory.readCount(), "旧主被拒不用自愈")

	// 旧主名下的合同,后台推进同样拒绝,合同和托管保留。
	require.ErrorIs(t, e.advance(context.Background(), old), errRoleConflict)
	_, oldWorkflow := state(t, e, old.CommerceId)
	require.Nil(t, leg(oldWorkflow, "seller_debit"))
}

func TestTiDBRoleFenceOldOwnerOnStaleFrozenReplicaHealsThenRejected(t *testing.T) {
	e, directory := fenceEngine(t)
	_, err := rolefence.Apply(context.Background(), e.Repo.DB, fenceOf(11, "seller", 1, 88, 2))
	require.NoError(t, err)
	directory.set(fenceOf(11, "buyer", 2, 0, 3))
	_, err = fenceConsignment(fenceActor(t, 11, "seller", 1), e, 11, "old-owner-first")
	require.ErrorIs(t, err, ErrConflict)
	require.Equal(t, 1, directory.readCount())
	require.Zero(t, contractCount(t, e))
}

func TestTiDBRoleFenceAllowListingOnlyPassesItsOwnToken(t *testing.T) {
	e, directory := fenceEngine(t)
	ctx := context.Background()
	lock := func(allow uint64) error {
		return e.roleTx(ctx, func(tx *sql.Tx) error { return e.lockRoles(ctx, tx, allow, 11) })
	}
	push := func(state *rfpb.RoleFenceState) {
		directory.set(state)
		_, err := rolefence.Apply(ctx, e.Repo.DB, state)
		require.NoError(t, err)
	}
	push(fenceOf(11, "seller", 1, 77, 2))
	require.NoError(t, lock(77), "令牌等于自己的 listing 才放行")
	require.ErrorIs(t, lock(78), ErrConflict)
	require.ErrorIs(t, lock(0), ErrConflict)
	push(fenceOf(11, "seller", 1, rolefence.RemovalToken(11), 3))
	require.ErrorIs(t, lock(77), ErrConflict, "删角令牌不是任何 listing")
	push(fenceOf(11, "seller", 2, 0, 4))
	require.NoError(t, lock(0), "解冻之后没有令牌限制")
	push(fenceOf(11, "", 3, 0, 5))
	require.ErrorIs(t, lock(0), ErrConflict, "墓碑拒绝")
}

// 退款不可逆:副本冻结或落后时不能直接判退款,先读权威状态。
func TestTiDBOfficialIntentHealsStaleFrozenReplicaBeforeRefunding(t *testing.T) {
	e, directory := fenceEngine(t)
	ctx := context.Background()
	_, err := rolefence.Apply(ctx, e.Repo.DB, fenceOf(22, "account-22", 1, 88, 2))
	require.NoError(t, err)
	directory.set(fenceOf(22, "account-22", 1, 0, 3))
	m := &Mall{Engine: e}
	r, err := m.accept(ctx, mallIntent{OrderID: "00112233-4455-6677-8899-aabbccddeeff", BuyerID: "22", BuyerAccount: "account-22", BuyerOwnerRevision: "1", SKU: 1, Quantity: 1, Config: 10, Count: 1, Amount: 101, Currency: "CNY"})
	require.NoError(t, err)
	require.Equal(t, Delivering, r.Status, "解冻推送丢了不能把已付款订单判成退款")

	// 真冻结:读完权威状态还是最终冻结,才持久退款结局。
	directory.set(finalFenceOf(23, "account-23", 1, 88, 2))
	r, err = m.accept(ctx, mallIntent{OrderID: "00112233-4455-6677-8899-aabbccddee00", BuyerID: "23", BuyerAccount: "account-23", BuyerOwnerRevision: "1", SKU: 1, Quantity: 1, Config: 10, Count: 1, Amount: 101, Currency: "CNY"})
	require.NoError(t, err)
	require.EqualValues(t, 8, r.Status)
}

// 规格 §3.5:还在投票的准备令牌不是最终结论。退款判定回可重试,不留合同;撤销之后照常发货,转最终之后才退款。
func TestTiDBOfficialIntentPreparingIsRetriedOnlyFinalRefunds(t *testing.T) {
	e, directory := fenceEngine(t)
	ctx := context.Background()
	m := &Mall{Engine: e}
	intent := func(order string, player uint64, account string) mallIntent {
		return mallIntent{OrderID: order, BuyerID: strconv.FormatUint(player, 10), BuyerAccount: account, BuyerOwnerRevision: "1", SKU: 1, Quantity: 1, Config: 10, Count: 1, Amount: 101, Currency: "CNY"}
	}

	// 投票之后撤销了:已付款的订单要发货。
	aborted := intent("00112233-4455-6677-8899-aabbccddee01", 24, "account-24")
	directory.set(fenceOf(24, "account-24", 1, 88, 2))
	_, err := m.accept(ctx, aborted)
	require.ErrorIs(t, err, errRolePreparing)
	require.True(t, OutcomeKnown(err), "事务已回滚,后台不能打 Hold")
	require.Zero(t, contractCount(t, e), "不能持久退款结局")
	directory.set(fenceOf(24, "account-24", 1, 0, 3))
	r, err := m.accept(ctx, aborted)
	require.NoError(t, err)
	require.Equal(t, Delivering, r.Status)

	// 投票通过转了最终:同一笔意图下一轮才退款。
	frozen := intent("00112233-4455-6677-8899-aabbccddee02", 25, "account-25")
	directory.set(fenceOf(25, "account-25", 1, 88, 2))
	_, err = m.accept(ctx, frozen)
	require.ErrorIs(t, err, errRolePreparing)
	directory.set(finalFenceOf(25, "account-25", 1, 88, 3))
	r, err = m.accept(ctx, frozen)
	require.NoError(t, err)
	require.EqualValues(t, 8, r.Status)

	// 投票中的新主:换主是最终结论,不等投票。
	moved := intent("00112233-4455-6677-8899-aabbccddee03", 26, "account-26")
	directory.set(fenceOf(26, "buyer", 2, 88, 4))
	r, err = m.accept(ctx, moved)
	require.NoError(t, err)
	require.EqualValues(t, 8, r.Status)
}

// 规格 §3.6:自愈读不到目录、目录里没有这名角色时事务已回滚,后台不能打 Hold;目录恢复之后同一条意图接着做。
func TestTiDBOfficialIntentHealFailureIsOutcomeKnown(t *testing.T) {
	e := tiDBEngine(t)
	directory := &unreadableDirectory{err: errors.Join(rolefence.ErrUnavailable, errors.New("data_service down"))}
	roles, err := NewRoleAuthority(e.Repo.DB, directory)
	require.NoError(t, err)
	e.Roles = roles
	m := &Mall{Engine: e}
	in := mallIntent{OrderID: "00112233-4455-6677-8899-aabbccddee09", BuyerID: "29", BuyerAccount: "account-29", BuyerOwnerRevision: "1", SKU: 1, Quantity: 1, Config: 10, Count: 1, Amount: 101, Currency: "CNY"}
	_, err = m.accept(context.Background(), in)
	require.ErrorIs(t, err, rolefence.ErrUnavailable)
	require.True(t, OutcomeKnown(fmt.Errorf("商城交付意图: %w", err)))
	require.Zero(t, contractCount(t, e))

	directory.err = rolefence.ErrRoleNotFound
	_, err = m.accept(context.Background(), in)
	require.ErrorIs(t, err, rolefence.ErrRoleNotFound)
	require.True(t, OutcomeKnown(err))
	require.Zero(t, contractCount(t, e))
}

type unreadableDirectory struct{ err error }

func (d *unreadableDirectory) GetRoleFenceState(context.Context, uint64) (*rfpb.RoleFenceState, error) {
	return nil, d.err
}

// 准备令牌和最终令牌分开给调用方,其余判定不用改。
func TestTiDBRoleAuthorityOwnershipSeparatesTokens(t *testing.T) {
	e, directory := fenceEngine(t)
	ctx := context.Background()
	read := func(state *rfpb.RoleFenceState) *dspb.AccountRoleOwnership {
		t.Helper()
		directory.set(state)
		_, err := rolefence.Apply(ctx, e.Repo.DB, state)
		require.NoError(t, err)
		var owned *dspb.AccountRoleOwnership
		require.NoError(t, e.Repo.Tx(ctx, func(tx *sql.Tx) error {
			var err error
			owned, err = e.Roles.Ownership(ctx, tx, state.PlayerId)
			return err
		}))
		return owned
	}
	preparing := read(fenceOf(11, "seller", 1, 77, 2))
	require.Zero(t, preparing.TradeListingId)
	require.EqualValues(t, 77, preparing.PrepareToken)
	final := read(finalFenceOf(11, "seller", 1, 77, 3))
	require.EqualValues(t, 77, final.TradeListingId)
	require.Zero(t, final.PrepareToken)
}

// ---- 挂起全集 ----

const fenceCoordinatorSecret = "role-fence-trade-test-only-coordinator-secret"

// insertFenceFixture 按 proto 字段把一行写进表,写坏数据和未知状态用。
func insertFenceFixture(t *testing.T, db *sql.DB, table string, row proto.Message) {
	t.Helper()
	m := row.ProtoReflect()
	fields := m.Descriptor().Fields()
	columns, marks, values := []string{}, []string{}, []any{}
	for n := 0; n < fields.Len(); n++ {
		f := fields.Get(n)
		columns = append(columns, "`"+string(f.Name())+"`")
		marks = append(marks, "?")
		v := m.Get(f)
		switch f.Kind() {
		case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
			values = append(values, v.Uint())
		case protoreflect.Int32Kind, protoreflect.Int64Kind:
			values = append(values, v.Int())
		case protoreflect.BoolKind:
			values = append(values, v.Bool())
		case protoreflect.EnumKind:
			values = append(values, int32(v.Enum()))
		case protoreflect.StringKind:
			values = append(values, v.String())
		case protoreflect.BytesKind:
			values = append(values, append([]byte{}, v.Bytes()...))
		default:
			t.Fatalf("夹具遇到未支持字段: %s", f.FullName())
		}
	}
	_, err := db.Exec("INSERT INTO `"+table+"` ("+strings.Join(columns, ",")+") VALUES ("+strings.Join(marks, ",")+")", values...)
	require.NoError(t, err)
}

// prepareFreeze 走真的 PrepareRoleFreeze:鉴权、写副本、读挂起,有挂起就回滚。
func prepareFreeze(t *testing.T, db *sql.DB, state *rfpb.RoleFenceState) *rfpb.PrepareRoleFreezeResponse {
	t.Helper()
	server, err := rolefence.NewParticipantServer(db, RolePending, fenceCoordinatorSecret)
	require.NoError(t, err)
	in := &rfpb.PrepareRoleFreezeRequest{State: state}
	require.NoError(t, serviceauth.Sign(rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, in, rolefence.CoordinatorCaller, fenceCoordinatorSecret, uint64(time.Now().UnixMilli())))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := server.PrepareRoleFreeze(ctx, in)
	require.NoError(t, err)
	return out
}

// requireBlocked:过户和删角都被挡住,本库不冻结。
func requireBlocked(t *testing.T, db *sql.DB, blocker string) {
	t.Helper()
	for _, token := range []uint64{88, rolefence.RemovalToken(11)} {
		out := prepareFreeze(t, db, fenceOf(11, "seller", 1, token, 2))
		require.False(t, out.Clean)
		require.Equal(t, blocker, out.Blocker)
	}
	var frozen int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM role_fence WHERE player_id=11").Scan(&frozen))
	require.Zero(t, frozen, "有挂起时本库不留冻结")
}

func requireClean(t *testing.T, db *sql.DB, token uint64) {
	t.Helper()
	out := prepareFreeze(t, db, fenceOf(11, "seller", 1, token, 2))
	require.True(t, out.Clean, "blocker=%q", out.Blocker)
}

func TestTiDBRolePendingContractRejectsFreezeAndRemoval(t *testing.T) {
	e, install := tiDBEngineWithInstall(t)
	raw, err := proto.Marshal(&tradepb.TradeCommerceWorkflow{Stage: "rmb_listed", Rmb: &tradepb.TradeRmbConsignmentWorkflow{SellerAccount: "seller", SellerOwnerRevision: 1, AssetKind: "ITEMS"}})
	require.NoError(t, err)
	insertFenceFixture(t, install, "trade_commerce", &tradepb.TradeCommerceRecord{CommerceId: 77, RequestKey: "pending", RequestDigest: make([]byte, 32), Kind: 6, Status: 1, SellerPlayerId: 11, UnitPrice: 1, Quantity: 1, CreatedMs: 1, UpdatedMs: 1, Version: 1, WorkflowProto: raw})
	requireBlocked(t, e.Repo.DB, roleBlockerCommerce)
	_, err = install.Exec("UPDATE trade_commerce SET status=7 WHERE commerce_id=77")
	require.NoError(t, err)
	requireClean(t, e.Repo.DB, rolefence.RemovalToken(11))
}

func TestTiDBRolePendingAssetOpStatesBlock(t *testing.T) {
	for _, status := range []uint32{1, 5, 0, 99} { // pending、manual、未指定、未知
		e, install := tiDBEngineWithInstall(t)
		insertFenceFixture(t, install, "trade_asset_op", &tradepb.TradeAssetOpRecord{OpId: 51, PlayerId: 11, Status: tradepb.TradeAssetOpStatus(status)})
		requireBlocked(t, e.Repo.DB, roleBlockerAssetOp)
		_, err := install.Exec("DELETE FROM trade_asset_op WHERE player_id=11")
		require.NoError(t, err)
		requireClean(t, e.Repo.DB, 88)
	}
}

func TestTiDBRoleKnownAssetTerminalsAllowFreeze(t *testing.T) {
	for _, token := range []uint64{88, rolefence.RemovalToken(11)} {
		e, install := tiDBEngineWithInstall(t)
		for index, status := range []uint32{2, 3, 4, 6} {
			seq := uint64(index + 1)
			insertFenceFixture(t, install, "trade_asset_op", &tradepb.TradeAssetOpRecord{OpId: 100 + seq, PlayerId: 11, Seq: seq, Status: tradepb.TradeAssetOpStatus(status)})
		}
		requireClean(t, e.Repo.DB, token)
	}
}

// official_refund 例外只认 status=8:未知状态不能借 stage 字符串绕过;真实未终局 op 也不被例外豁免。
func TestTiDBRoleUnknownCommerceStatusCannotBorrowKnownRefundStage(t *testing.T) {
	e, install := tiDBEngineWithInstall(t)
	raw, err := proto.Marshal(&tradepb.TradeCommerceWorkflow{Stage: "official_refund"})
	require.NoError(t, err)
	insertFenceFixture(t, install, "trade_commerce", &tradepb.TradeCommerceRecord{CommerceId: 77, RequestKey: "fixture-refund", Kind: 1, SellerPlayerId: 11, Status: 99, WorkflowProto: raw})
	requireBlocked(t, e.Repo.DB, roleBlockerCommerce)
	_, err = install.Exec("UPDATE trade_commerce SET status=8 WHERE commerce_id=77")
	require.NoError(t, err)
	insertFenceFixture(t, install, "trade_asset_op", &tradepb.TradeAssetOpRecord{OpId: 51, PlayerId: 11, Status: 5})
	requireBlocked(t, e.Repo.DB, roleBlockerAssetOp)
	_, err = install.Exec("UPDATE trade_asset_op SET status=3 WHERE op_id=51")
	require.NoError(t, err)
	requireClean(t, e.Repo.DB, 88)
}

func TestTiDBRoleNullAssetStateRemainsBlocking(t *testing.T) {
	e, install := tiDBEngineWithInstall(t)
	insertFenceFixture(t, install, "trade_asset_op", &tradepb.TradeAssetOpRecord{OpId: 51, PlayerId: 11, Status: 2})
	// 只破坏本例随机库,模拟绕过 schema 装配后的坏值:不能把 NULL 当成安全终态。
	_, err := install.Exec("ALTER TABLE `trade_asset_op` MODIFY `status` INT UNSIGNED NULL")
	require.NoError(t, err)
	_, err = install.Exec("UPDATE `trade_asset_op` SET `status`=NULL WHERE player_id=11")
	require.NoError(t, err)
	requireBlocked(t, e.Repo.DB, roleBlockerAssetOp)
}

// 规格 §3.5:合同超过上限是确定性结论,要走撤销。回错误的话协调者按不可达处理,令牌留着等重放,而重放还是同样的结果。
func TestTiDBRolePendingTooManyContractsIsABlocker(t *testing.T) {
	e, install := tiDBEngineWithInstall(t)
	raw, err := proto.Marshal(&tradepb.TradeCommerceWorkflow{Stage: "official_refund"})
	require.NoError(t, err)
	// official_refund 不算挂起,但要计入这一轮读到的行数:这样正好卡在上限上。
	insert := func(id uint64) {
		insertFenceFixture(t, install, "trade_commerce", &tradepb.TradeCommerceRecord{CommerceId: id, RequestKey: "too-many-" + strconv.FormatUint(id, 10), Kind: 1, BuyerPlayerId: 11, Status: 8, WorkflowProto: raw})
	}
	for id := uint64(1); id <= maxRolePendingCommerce+1; id++ {
		insert(id)
	}
	requireBlocked(t, e.Repo.DB, roleBlockerTooManyDeals)
	_, err = install.Exec("DELETE FROM trade_commerce WHERE commerce_id=?", maxRolePendingCommerce+1)
	require.NoError(t, err)
	requireClean(t, e.Repo.DB, 88)
}

// rmb_freezing 例外逐项都要对上:listing、kind、status、角色、原账号、原修订、stage。
func TestTiDBRmbFreezingExceptionOnlyPassesThisListing(t *testing.T) {
	exact := func() (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) {
		return &tradepb.TradeCommerceRecord{CommerceId: 77, RequestKey: "rmb-freezing", Kind: 6, Status: 3, SellerPlayerId: 11},
			&tradepb.TradeCommerceWorkflow{Stage: "rmb_freezing", Rmb: &tradepb.TradeRmbConsignmentWorkflow{CharacterPlayerId: 11, SellerAccount: "seller", SellerOwnerRevision: 1, AssetKind: "CHARACTER"}}
	}
	insert := func(t *testing.T, install *sql.DB, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) {
		t.Helper()
		raw, err := proto.Marshal(w)
		require.NoError(t, err)
		r.WorkflowProto = raw
		insertFenceFixture(t, install, "trade_commerce", r)
	}
	t.Run("本 listing 放行", func(t *testing.T) {
		e, install := tiDBEngineWithInstall(t)
		r, w := exact()
		insert(t, install, r, w)
		requireClean(t, e.Repo.DB, 77)
	})
	t.Run("别的 listing 和删角不放行", func(t *testing.T) {
		e, install := tiDBEngineWithInstall(t)
		r, w := exact()
		insert(t, install, r, w)
		for _, token := range []uint64{78, rolefence.RemovalToken(11)} {
			out := prepareFreeze(t, e.Repo.DB, fenceOf(11, "seller", 1, token, 2))
			require.Equal(t, roleBlockerCommerce, out.Blocker)
		}
	})
	for name, mutate := range map[string]func(*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow){
		"不是人民币合同": func(r *tradepb.TradeCommerceRecord, _ *tradepb.TradeCommerceWorkflow) { r.Kind = 3 },
		"状态不是托管中": func(r *tradepb.TradeCommerceRecord, _ *tradepb.TradeCommerceWorkflow) { r.Status = 1 },
		"阶段不是冻结中": func(_ *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) { w.Stage = "rmb_ready" },
		"别的角色":    func(_ *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) { w.Rmb.CharacterPlayerId = 12 },
		"原账号不对":   func(_ *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) { w.Rmb.SellerAccount = "other" },
		"原修订不对":   func(_ *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) { w.Rmb.SellerOwnerRevision = 2 },
		"没有人民币条款": func(_ *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) { w.Rmb = nil },
	} {
		t.Run(name, func(t *testing.T) {
			e, install := tiDBEngineWithInstall(t)
			r, w := exact()
			mutate(r, w)
			insert(t, install, r, w)
			out := prepareFreeze(t, e.Repo.DB, fenceOf(11, "seller", 1, 77, 2))
			require.False(t, out.Clean)
			require.Equal(t, roleBlockerCommerce, out.Blocker)
		})
	}
}

// 官方退款合同已确定零新增资产腿,不阻止后续出售。
func TestTiDBOfficialRefundContractDoesNotBlockFreeze(t *testing.T) {
	e, install := tiDBEngineWithInstall(t)
	raw, err := proto.Marshal(&tradepb.TradeCommerceWorkflow{Stage: "official_refund"})
	require.NoError(t, err)
	insertFenceFixture(t, install, "trade_commerce", &tradepb.TradeCommerceRecord{CommerceId: 77, RequestKey: "official-refund", Kind: 1, BuyerPlayerId: 11, Status: 8, WorkflowProto: raw})
	requireClean(t, e.Repo.DB, 88)
}

// ---- 不连库的判定 ----

type recordingRoles struct {
	fixtureRoles
	expects []rolefence.Expect
}

func (r *recordingRoles) Lock(_ context.Context, _ *sql.Tx, expects ...rolefence.Expect) error {
	r.expects = expects
	return nil
}

func TestLockRolesCarriesAllowTokenAndActorIdentity(t *testing.T) {
	roles := &recordingRoles{}
	e := &Engine{Roles: roles}
	require.NoError(t, e.lockRoles(fenceActor(t, 11, "seller", 5), nil, 77, 22, 0, 11))
	require.Len(t, roles.expects, 3, "player 0 跳过,actor 另算一条")
	for _, expect := range roles.expects {
		require.EqualValues(t, 77, expect.AllowToken)
		require.ErrorIs(t, expect.Reject, errRoleConflict, "拒绝标成 role_conflict,客户端 tip 不变")
		require.ErrorIs(t, expect.Reject, ErrConflict)
	}
	require.EqualValues(t, 22, roles.expects[0].PlayerID)
	require.Zero(t, roles.expects[0].OwnerRevision, "对端没有原身份,只判冻结和删除")
	actor := roles.expects[2]
	require.EqualValues(t, 11, actor.PlayerID)
	require.Equal(t, "seller", actor.Account)
	require.EqualValues(t, 5, actor.OwnerRevision)

	require.NoError(t, e.lockRoles(context.Background(), nil, 0, 22))
	require.Len(t, roles.expects, 1, "后台没有 actor")
	require.Zero(t, roles.expects[0].AllowToken)
}

func TestContractIdentityStaleReplicaHealsAndMismatchRejects(t *testing.T) {
	r := &tradepb.TradeCommerceRecord{SellerPlayerId: 11}
	w := &tradepb.TradeCommerceWorkflow{SellerIdentity: &tradepb.TradeRoleIdentity{Account: "seller", OwnerRevision: 5}}
	check := func(account string, revision uint64) error {
		e := &Engine{Roles: fixtureRoles{Values: map[uint64]*dspb.AccountRoleOwnership{11: {Account: account, OwnerRevision: revision, Role: &base.AccountSimplePlayer{PlayerId: 11}}}}}
		return e.checkContractIdentities(context.Background(), nil, r, w)
	}
	require.NoError(t, check("seller", 5))
	require.ErrorIs(t, check("buyer", 6), errRoleConflict, "修订号更大:已换主,确定拒绝")
	require.ErrorIs(t, check("buyer", 5), errRoleConflict)
	var stale *rolefence.StaleError
	require.ErrorAs(t, check("seller", 4), &stale, "副本修订号落后要读权威状态,不能按换主处理")
	require.Equal(t, []uint64{11}, stale.Players)
	require.ErrorIs(t, stale.Reject, errRoleConflict)
}

func TestRoleTxWithoutRoleLockerFailsClosed(t *testing.T) {
	require.Error(t, (&Engine{}).roleTx(context.Background(), func(*sql.Tx) error { return nil }))
}
