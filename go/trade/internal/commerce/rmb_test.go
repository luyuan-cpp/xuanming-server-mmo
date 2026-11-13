package commerce

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/accountdirectory"
	"shared/currencytype"
	"shared/roleidentity"
	"trade/internal/data"

	"github.com/stretchr/testify/require"
)

// 目录替身只隔离工作流I/O，SQL合同/outbox走真实专用TiDB；原生冻结证明由另外原生/目录用例验证。
type rmbDirectoryFixture struct {
	ConfirmReady            bool
	TransferUnknown         bool
	TransferCalls           []uint64
	BeginCalls, CancelCalls int
	BeginErr, CancelErr     error
	TransferDone            bool
	Roles                   fixtureRoles
	// TransferErr 按调用顺序回错误,空了就放行,放行就算过户提交了(Committed)。AccountFull、SlotErr 是名额检查的结论。
	TransferErr []error
	Committed   bool
	AccountFull bool
	SlotErr     error
	// CancelState 是 Cancel 回的收据状态,零值是 CANCELLED。
	CancelState dspb.AccountRoleTradeState
}

func (f *rmbDirectoryFixture) Lookup(ctx context.Context, p uint64) (*dspb.AccountRoleOwnership, error) {
	return f.Roles.Ownership(ctx, nil, p)
}
func (f *rmbDirectoryFixture) Begin(_ context.Context, in *dspb.BeginAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	f.BeginCalls++
	if f.BeginErr != nil {
		return nil, f.BeginErr
	}
	return &dspb.AccountRoleTradeReceipt{ListingId: in.ListingId, PlayerId: in.PlayerId, SellerAccount: in.SellerAccount, SellerOwnerRevision: in.ExpectedOwnerRevision, State: dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING}, nil
}
func (f *rmbDirectoryFixture) Confirm(_ context.Context, in *dspb.ConfirmAccountRoleTradeFreezeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	if !f.ConfirmReady {
		return nil, accountdirectory.ErrUnavailable
	}
	return &dspb.AccountRoleTradeReceipt{ListingId: in.ListingId, PlayerId: in.PlayerId, SellerAccount: in.SellerAccount, SellerOwnerRevision: in.ExpectedOwnerRevision, State: dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN, FreezeEvidence: &dspb.AccountRoleFreezeEvidence{ListingId: in.ListingId, PlayerId: in.PlayerId, OwnerRevision: in.ExpectedOwnerRevision, OwnerEpoch: 17, SaveSeq: 3, PersistedDigest: make([]byte, 32), Kind: dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_SCENE}}, nil
}
func (f *rmbDirectoryFixture) Cancel(_ context.Context, in *dspb.CancelAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	f.CancelCalls++
	if f.CancelErr != nil {
		return nil, f.CancelErr
	}
	state := f.CancelState
	if state == 0 {
		state = dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED
	}
	return &dspb.AccountRoleTradeReceipt{ListingId: in.ListingId, PlayerId: in.PlayerId, SellerAccount: in.SellerAccount, SellerOwnerRevision: in.ExpectedOwnerRevision, State: state}, nil
}
func (f *rmbDirectoryFixture) Transfer(_ context.Context, in *dspb.TransferAccountRoleRequest) (*dspb.AccountRoleTransferReceipt, error) {
	f.TransferCalls = append(f.TransferCalls, in.TxId)
	if f.TransferUnknown && !f.TransferDone {
		f.TransferDone = true
		return nil, context.DeadlineExceeded
	}
	if len(f.TransferErr) > 0 {
		err := f.TransferErr[0]
		f.TransferErr = f.TransferErr[1:]
		return nil, err
	}
	f.Committed = true
	return &dspb.AccountRoleTransferReceipt{TxId: in.TxId, ListingId: in.ListingId, PlayerId: in.PlayerId, SellerAccount: in.SellerAccount, BuyerAccount: in.BuyerAccount, SellerOwnerRevision: in.ExpectedOwnerRevision, BuyerOwnerRevision: in.ExpectedOwnerRevision + 1, Role: &base.AccountSimplePlayer{PlayerId: in.PlayerId}, CreatedMs: 1}, nil
}
func (f *rmbDirectoryFixture) RoleSlotFull(context.Context, string) (bool, error) {
	return f.AccountFull, f.SlotErr
}

type rmbSealerFixture struct{}

func (rmbSealerFixture) Seal(context.Context, *base.RoleTradeSeal) error { return nil }

type rmbMallProbe struct {
	Registers         []map[string]any
	Receipts          []map[string]string
	FailFirstRegister bool
}

func rmbRuntime(t *testing.T) (*RmbConsignments, *rmbDirectoryFixture, *rmbMallProbe) {
	t.Helper()
	e := tiDBEngine(t)
	directory := &rmbDirectoryFixture{Roles: fixtureRoles{}}
	probe := &rmbMallProbe{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "trade", r.Header.Get("X-Commerce-Subject"))
		require.Len(t, r.Header.Get("X-Commerce-Signature"), 64)
		switch r.URL.Path {
		case "/internal/consignments/register":
			var in map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
			probe.Registers = append(probe.Registers, in)
			if probe.FailFirstRegister && len(probe.Registers) == 1 {
				w.WriteHeader(503)
				return
			}
			require.NoError(t, json.NewEncoder(w).Encode(rmbListing{ID: in["listingId"].(string), Seller: in["sellerPlayerId"].(string), Zone: 1, Kind: in["assetKind"].(string), Title: in["title"].(string), Price: uint64(in["priceFen"].(float64)), Fee: 500, Cooling: 259200000, NoticeEnd: 1, SaleEnd: 100000, State: "LISTED", Phase: "SALE"}))
		case "/internal/consignments/cancel":
			var in map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
			require.NoError(t, json.NewEncoder(w).Encode(rmbListing{ID: in["listingId"], Seller: "11", Zone: 1, Kind: "CHARACTER", Title: "原托管", Price: 101, Fee: 500, NoticeEnd: 1, SaleEnd: 100000, State: "RETURNING", Phase: "ENDED"}))
		case "/internal/deliveries/receipt":
			var in map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
			probe.Receipts = append(probe.Receipts, in)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	mall, err := NewMall(server.URL, strings.Repeat("g", 32), strings.Repeat("d", 32), e)
	require.NoError(t, err)
	s := &RmbConsignments{Engine: e, Mall: mall, Directory: directory, Sealer: rmbSealerFixture{}, CurrencyTypes: []uint32{currencytype.Gold}}
	e.Rmb = s
	return s, directory, probe
}
func rmbStep(t *testing.T, s *RmbConsignments, id uint64) error {
	t.Helper()
	r, err := s.Engine.Repo.Get(context.Background(), id)
	require.NoError(t, err)
	return s.step(context.Background(), r)
}
func rmbCreate(t *testing.T, s *RmbConsignments, character bool) *tradepb.TradeCommerceRecord {
	t.Helper()
	in := &tradepb.CreateRmbConsignmentRequest{ClientRequestId: "rmb-request-original", Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 2}}, PriceFen: 101, Title: "原托管"}
	if character {
		in.Assets = nil
		in.CharacterPlayerId = 11
	}
	r, err := s.Create(context.Background(), "account-11", 11, 1, 1, in)
	require.NoError(t, err)
	return r
}

func TestRmbSelectionRejectsMixedKindsBoundCurrencyAndUnconfiguredDiamond(t *testing.T) {
	s := &RmbConsignments{Engine: &Engine{Policy: Policy{Items: []uint32{10}, AllowInstances: true}}}
	for _, v := range []*tradepb.CreateRmbConsignmentRequest{{Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}, PetId: 7}}, {Assets: &tradepb.TradeAssetSelection{CurrencyAmount: 1, CurrencyType: currencytype.BindDiamond}}, {Assets: &tradepb.TradeAssetSelection{CurrencyAmount: 1, CurrencyType: currencytype.Diamond}}, {CharacterPlayerId: 11, Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}}}} {
		_, err := s.selection(v)
		require.Error(t, err)
	}
	// 没配 CurrencyTypes 时只收银两。
	kind, err := s.selection(&tradepb.CreateRmbConsignmentRequest{Assets: &tradepb.TradeAssetSelection{CurrencyAmount: 1, CurrencyType: currencytype.Gold}})
	require.NoError(t, err)
	require.Equal(t, "CURRENCY", kind)
	s.CurrencyTypes = []uint32{currencytype.Gold, currencytype.Diamond}
	_, err = s.selection(&tradepb.CreateRmbConsignmentRequest{Assets: &tradepb.TradeAssetSelection{CurrencyAmount: 1, CurrencyType: currencytype.Diamond}})
	require.NoError(t, err)
	// 绑定灵石被配进 CurrencyTypes(配置校验会拒绝,这里绕过它)也不可寄售。
	s.CurrencyTypes = []uint32{currencytype.Gold, currencytype.Diamond, currencytype.BindDiamond}
	_, err = s.selection(&tradepb.CreateRmbConsignmentRequest{Assets: &tradepb.TradeAssetSelection{CurrencyAmount: 1, CurrencyType: currencytype.BindDiamond}})
	require.ErrorIs(t, err, ErrNotTradable)
}
func TestTiDBRmbRegistersOnlyAfterEscrowAndDeliversOneOriginalCredit(t *testing.T) {
	s, _, probe := rmbRuntime(t)
	r := rmbCreate(t, s, false)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.Empty(t, probe.Registers)
	settleLegs(t, s.Engine, 2)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.Empty(t, probe.Registers)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.Len(t, probe.Registers, 1)
	current, w := state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_listed", w.Stage)
	in := rmbDelivery{Order: "00112233-4455-6677-8899-aabbccddeeff", Buyer: "22", Account: "account-22", Revision: "1", Listing: probe.Registers[0]["listingId"].(string), Reference: w.Rmb.EscrowReference, Proof: w.Rmb.EscrowProof, Amount: 101}
	require.NoError(t, s.deliver(context.Background(), in))
	require.NoError(t, rmbStep(t, s, current.CommerceId))
	settleLegs(t, s.Engine, 2)
	require.NoError(t, rmbStep(t, s, current.CommerceId))
	require.NoError(t, s.deliver(context.Background(), in))
	require.NoError(t, s.deliver(context.Background(), in))
	require.Len(t, probe.Receipts, 2)
	require.Equal(t, probe.Receipts[0], probe.Receipts[1])
	require.Equal(t, "DELIVERED", probe.Receipts[0]["state"])
	var credits int
	require.NoError(t, s.Engine.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE player_id=22 AND stream=4").Scan(&credits))
	require.Equal(t, 1, credits)
	changed := in
	changed.Account = "new-owner"
	require.ErrorIs(t, s.deliver(context.Background(), changed), ErrReplay)
}
func TestTiDBRmbCharacterDoesNotPublishWithoutSQLProofAndReusesTransferTxAfterUnknownResult(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	r := rmbCreate(t, s, true)
	require.Error(t, rmbStep(t, s, r.CommerceId))
	require.Empty(t, probe.Registers)
	dir.ConfirmReady = true
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	_, w := state(t, s.Engine, r.CommerceId)
	in := rmbDelivery{Order: "00112233-4455-6677-8899-aabbccddeeff", Buyer: "22", Account: "account-22", Revision: "1", Listing: probe.Registers[0]["listingId"].(string), Reference: w.Rmb.EscrowReference, Proof: w.Rmb.EscrowProof, Amount: 101}
	require.NoError(t, s.deliver(context.Background(), in))
	dir.TransferUnknown = true
	require.ErrorIs(t, rmbStep(t, s, r.CommerceId), context.DeadlineExceeded)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.Len(t, dir.TransferCalls, 2)
	require.NotZero(t, dir.TransferCalls[0])
	require.Equal(t, dir.TransferCalls[0], dir.TransferCalls[1])
	require.NoError(t, s.deliver(context.Background(), in))
	require.Equal(t, "DELIVERED", probe.Receipts[0]["state"])
	var ops int
	require.NoError(t, s.Engine.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op").Scan(&ops))
	require.Zero(t, ops, "角色过户不能复制玩家资产或伪造credit")
}
func TestTiDBRmbUnknownRegisterMustResolveBeforeCancelUnfreezesRole(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	dir.ConfirmReady = true
	probe.FailFirstRegister = true
	r := rmbCreate(t, s, true)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.Error(t, rmbStep(t, s, r.CommerceId))
	require.NoError(t, s.Cancel(context.Background(), "account-11", r.CommerceId))
	require.Zero(t, dir.CancelCalls)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.Len(t, probe.Registers, 2)
	require.Equal(t, probe.Registers[0], probe.Registers[1])
	require.Zero(t, dir.CancelCalls)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.Equal(t, 1, dir.CancelCalls)
	cur, _ := state(t, s.Engine, r.CommerceId)
	require.EqualValues(t, Cancelled, cur.Status)
}
func TestTiDBRmbCancelBeforeFreezeDoesNotNeedSuccessfulBegin(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	r := rmbCreate(t, s, true)
	require.NoError(t, s.Cancel(context.Background(), "account-11", r.CommerceId))
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.Zero(t, dir.BeginCalls)
	require.Equal(t, 1, dir.CancelCalls)
	require.Empty(t, probe.Registers)
	current, workflow := state(t, s.Engine, r.CommerceId)
	require.EqualValues(t, Cancelled, current.Status)
	require.Equal(t, "rmb_returned", workflow.Stage)
}

// 规格 §3.5:Begin 被拒是长期状态,按 role_conflict 分钟级复查;目录暂不可用才按一般失败重试。
func TestTiDBRmbBeginRejectionBacksOffByMinutes(t *testing.T) {
	s, dir, _ := rmbRuntime(t)
	r := rmbCreate(t, s, true)
	dir.BeginErr = accountdirectory.ErrConflict
	err := rmbStep(t, s, r.CommerceId)
	require.ErrorIs(t, err, errRoleConflict)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.Equal(t, failureRoleConflict, failureClass(err))
	dir.BeginErr = accountdirectory.ErrUnavailable
	err = rmbStep(t, s, r.CommerceId)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	require.Equal(t, failureRetry, failureClass(err))
	require.Zero(t, dir.CancelCalls, "没到期不取消")
	current, workflow := state(t, s.Engine, r.CommerceId)
	require.EqualValues(t, Escrowing, current.Status)
	require.Equal(t, "rmb_freezing", workflow.Stage)
}

// 挂单期限内没冻结下来就走取消,合同不能永远重试。没到取消结论之前合同不动,目录拒绝取消同样分钟级复查。
func TestTiDBRmbFreezingExpiryCancels(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	r := rmbCreate(t, s, true)
	dir.BeginErr = accountdirectory.ErrConflict
	s.Engine.Now = func() time.Time { return time.UnixMilli(int64(r.ExpiresMs)) }
	dir.CancelErr = accountdirectory.ErrConflict
	err := rmbStep(t, s, r.CommerceId)
	require.ErrorIs(t, err, errRoleConflict)
	require.Zero(t, dir.BeginCalls, "到期不再试冻结")
	require.Equal(t, 1, dir.CancelCalls)
	current, _ := state(t, s.Engine, r.CommerceId)
	require.EqualValues(t, Escrowing, current.Status)

	dir.CancelErr = nil
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.Zero(t, dir.BeginCalls)
	require.Equal(t, 2, dir.CancelCalls)
	require.Empty(t, probe.Registers, "还没进商城")
	current, workflow := state(t, s.Engine, r.CommerceId)
	require.EqualValues(t, Cancelled, current.Status)
	require.Equal(t, "rmb_returned", workflow.Stage)
}

func TestTiDBRmbBuyerOwnershipChangePersistsRefundDecisionWithoutCredit(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	r := rmbCreate(t, s, false)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	settleLegs(t, s.Engine, 2)
	for n := 0; n < 3; n++ {
		require.NoError(t, rmbStep(t, s, r.CommerceId))
	}
	_, w := state(t, s.Engine, r.CommerceId)
	dir.Roles.Values = map[uint64]*dspb.AccountRoleOwnership{22: {Account: "new-owner", OwnerRevision: 2, Role: &base.AccountSimplePlayer{PlayerId: 22}}}
	in := rmbDelivery{Order: "00112233-4455-6677-8899-aabbccddeeff", Buyer: "22", Account: "account-22", Revision: "1", Listing: probe.Registers[0]["listingId"].(string), Reference: w.Rmb.EscrowReference, Proof: w.Rmb.EscrowProof, Amount: 101}
	require.NoError(t, s.deliver(context.Background(), in))
	dir.Roles.Values = nil
	require.NoError(t, s.deliver(context.Background(), in))
	require.Len(t, probe.Receipts, 2)
	require.Equal(t, probe.Receipts[0], probe.Receipts[1])
	require.Equal(t, "REFUND_PENDING", probe.Receipts[0]["state"])
	cur, cw := state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_refund", cw.Stage)
	require.Zero(t, cur.BuyerPlayerId)
	var credits int
	require.NoError(t, s.Engine.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE stream=4").Scan(&credits))
	require.Zero(t, credits)
}

// 买卖同账号的已付款意图(商城下单时那道同账号判断被放开或绕过才会出现):不发货给卖家自己的另一个角色,
// 也不让已付的钱压在商城 —— 记下退款决定,向商城回 REFUND_PENDING。以前这里每轮回 ErrConflict,既不发货也不退款。
func TestTiDBRmbDeliverRefundsSelfPurchaseInsteadOfStalling(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	r := rmbCreate(t, s, false)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	settleLegs(t, s.Engine, 2)
	for n := 0; n < 3; n++ {
		require.NoError(t, rmbStep(t, s, r.CommerceId))
	}
	_, w := state(t, s.Engine, r.CommerceId)
	require.Equal(t, "account-11", w.Rmb.SellerAccount, "夹具前提:卖家账号是 account-11")
	// 22 号角色与卖家同属 account-11,角色归属、归属版本都对得上:只有「买卖同账号」这一条不满足。
	dir.Roles.Values = map[uint64]*dspb.AccountRoleOwnership{22: {Account: "account-11", OwnerRevision: 1, Role: &base.AccountSimplePlayer{PlayerId: 22}}}
	in := rmbDelivery{Order: "00112233-4455-6677-8899-aabbccddeeff", Buyer: "22", Account: "account-11", Revision: "1", Listing: probe.Registers[0]["listingId"].(string), Reference: w.Rmb.EscrowReference, Proof: w.Rmb.EscrowProof, Amount: 101}

	require.NoError(t, s.deliver(context.Background(), in))
	// 同一个意图重放:回同一张回执,不重复决定。
	require.NoError(t, s.deliver(context.Background(), in))
	require.Len(t, probe.Receipts, 2)
	require.Equal(t, probe.Receipts[0], probe.Receipts[1])
	require.Equal(t, "REFUND_PENDING", probe.Receipts[0]["state"])
	cur, cw := state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_refund", cw.Stage)
	require.EqualValues(t, 8, cur.Status)
	require.Zero(t, cur.BuyerPlayerId, "没有进交付")
	var credits int
	require.NoError(t, s.Engine.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op WHERE stream=4").Scan(&credits))
	require.Zero(t, credits, "不建任何发货原件")
}

// rmbCharacterInDelivery 把角色寄售推到买家已付款、进了 rmb_delivery。
func rmbCharacterInDelivery(t *testing.T, s *RmbConsignments, dir *rmbDirectoryFixture, probe *rmbMallProbe) (*tradepb.TradeCommerceRecord, rmbDelivery) {
	t.Helper()
	dir.ConfirmReady = true
	r := rmbCreate(t, s, true)
	for n := 0; n < 3; n++ {
		require.NoError(t, rmbStep(t, s, r.CommerceId))
	}
	_, w := state(t, s.Engine, r.CommerceId)
	in := rmbDelivery{Order: "00112233-4455-6677-8899-aabbccddeeff", Buyer: "22", Account: "account-22", Revision: "1", Listing: probe.Registers[0]["listingId"].(string), Reference: w.Rmb.EscrowReference, Proof: w.Rmb.EscrowProof, Amount: 101}
	require.NoError(t, s.deliver(context.Background(), in))
	_, w = state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_delivery", w.Stage)
	return r, in
}

// 规格 §5.4:买家账号满位没有别的出口。先 Cancel 和过户的提交点串行,Cancel 成了才退款:listing 到了 CANCELLED,过户再也提交不了。
func TestTiDBRmbBuyerFullRefundsOnlyAfterCancel(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	r, in := rmbCharacterInDelivery(t, s, dir, probe)

	// 目录这时取消不了,不能退款。
	dir.TransferErr = []error{accountdirectory.ErrFull}
	dir.CancelErr = accountdirectory.ErrUnavailable
	require.ErrorIs(t, rmbStep(t, s, r.CommerceId), accountdirectory.ErrUnavailable)
	_, w := state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_delivery", w.Stage, "Cancel 没成不能落退款")

	dir.TransferErr = []error{accountdirectory.ErrFull}
	dir.CancelErr = nil
	cancels := dir.CancelCalls
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.Equal(t, cancels+1, dir.CancelCalls)
	cur, w := state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_refund", w.Stage)
	require.EqualValues(t, 8, cur.Status)
	require.False(t, dir.Committed)

	// 商城下一轮拉到同一条意图,回报退款,不再发货。
	require.NoError(t, s.deliver(context.Background(), in))
	require.Len(t, probe.Receipts, 1)
	require.Equal(t, "REFUND_PENDING", probe.Receipts[0]["state"])

	// 卖家这时取消会把合同状态改走,回报就断了。
	require.ErrorIs(t, s.Cancel(context.Background(), "account-11", r.CommerceId), ErrConflict)
	require.NoError(t, rmbStep(t, s, r.CommerceId), "等商城退款,只推迟")
	calls := len(dir.TransferCalls)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.Len(t, dir.TransferCalls, calls, "退款决定之后不再过户")
}

// 上一轮 Cancel 提交了、崩溃在落退款之前:这一轮 Transfer 因为 listing 已取消被拒,Cancel 重放原收据,接着落。
func TestTiDBRmbCrashBetweenCancelAndRefundResumes(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	r, _ := rmbCharacterInDelivery(t, s, dir, probe)
	dir.TransferErr = []error{accountdirectory.ErrConflict}
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	cur, w := state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_refund", w.Stage)
	require.EqualValues(t, 8, cur.Status)
}

// Cancel 被拒说明过户可能已经提交了(超时的上一次调用):同一 tx_id 重放拿到收据就走完成路径,绝不退款。
func TestTiDBRmbCancelRejectedMeansTransferWon(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	r, _ := rmbCharacterInDelivery(t, s, dir, probe)
	dir.TransferErr = []error{accountdirectory.ErrFull}
	dir.CancelErr = accountdirectory.ErrConflict
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	cur, w := state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_delivered", w.Stage)
	require.EqualValues(t, Settled, cur.Status)
	require.Len(t, dir.TransferCalls, 2)
	require.Equal(t, dir.TransferCalls[0], dir.TransferCalls[1], "重放用同一 tx_id")
}

// Cancel 被拒、重放也拿不到收据:是别的冲突,不退款,分钟级复查。
func TestTiDBRmbCancelAndTransferBothRejectedDoesNotRefund(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	r, _ := rmbCharacterInDelivery(t, s, dir, probe)
	dir.TransferErr = []error{accountdirectory.ErrFull, accountdirectory.ErrFull}
	dir.CancelErr = accountdirectory.ErrConflict
	err := rmbStep(t, s, r.CommerceId)
	require.ErrorIs(t, err, errRoleConflict)
	require.Equal(t, failureRoleConflict, failureClass(err))
	_, w := state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_delivery", w.Stage)
}

// 过户结局未知、目录暂不可用不算被拒:重放,绝不去 Cancel。
func TestTiDBRmbTransferUnknownOutcomeNeverCancels(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	r, _ := rmbCharacterInDelivery(t, s, dir, probe)
	cancels := dir.CancelCalls
	for _, cause := range []error{accountdirectory.ErrUnavailable, accountdirectory.ErrRetryLater} {
		dir.TransferErr = []error{cause}
		require.ErrorIs(t, rmbStep(t, s, r.CommerceId), cause)
		require.Equal(t, cancels, dir.CancelCalls)
	}
}

// 进交付之前就知道买家满了:不进 rmb_delivery,没有过户原件,直接退款,也不用取消冻结。
func TestTiDBRmbDeliverRefundsWhenBuyerAccountFull(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	dir.ConfirmReady = true
	r := rmbCreate(t, s, true)
	for n := 0; n < 3; n++ {
		require.NoError(t, rmbStep(t, s, r.CommerceId))
	}
	_, w := state(t, s.Engine, r.CommerceId)
	in := rmbDelivery{Order: "00112233-4455-6677-8899-aabbccddeeff", Buyer: "22", Account: "account-22", Revision: "1", Listing: probe.Registers[0]["listingId"].(string), Reference: w.Rmb.EscrowReference, Proof: w.Rmb.EscrowProof, Amount: 101}

	// 读不到不能当成有名额,这个意图下一轮再判。
	dir.SlotErr = accountdirectory.ErrRetryLater
	err := s.deliver(context.Background(), in)
	require.ErrorIs(t, err, accountdirectory.ErrRetryLater)
	require.True(t, OutcomeKnown(err), "只读步骤失败确定没写")
	_, w = state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_listed", w.Stage)

	dir.SlotErr, dir.AccountFull = nil, true
	require.NoError(t, s.deliver(context.Background(), in))
	cur, w := state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_refund", w.Stage)
	require.EqualValues(t, 8, cur.Status)
	require.Zero(t, w.Rmb.TransferTxId, "没有过户原件")
	require.Zero(t, dir.CancelCalls)
	require.Empty(t, dir.TransferCalls)
	require.Equal(t, "REFUND_PENDING", probe.Receipts[0]["state"])
}

// 买的不是角色就不用名额。
func TestTiDBRmbItemDeliveryIgnoresRoleSlots(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	r := rmbCreate(t, s, false)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	settleLegs(t, s.Engine, 2)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	_, w := state(t, s.Engine, r.CommerceId)
	dir.AccountFull = true
	in := rmbDelivery{Order: "00112233-4455-6677-8899-aabbccddeeff", Buyer: "22", Account: "account-22", Revision: "1", Listing: probe.Registers[0]["listingId"].(string), Reference: w.Rmb.EscrowReference, Proof: w.Rmb.EscrowProof, Amount: 101}
	require.NoError(t, s.deliver(context.Background(), in))
	_, w = state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_delivery", w.Stage)
}

// 付款之前挡一下:只挡角色寄售,拿不准就放行,退款出口还在。
func TestTiDBRmbCheckBuyerSlot(t *testing.T) {
	s, dir, _ := rmbRuntime(t)
	character := rmbCreate(t, s, true)
	ctx := context.Background()
	require.NoError(t, s.CheckBuyerSlot(ctx, "account-22", character.CommerceId))
	dir.AccountFull = true
	require.ErrorIs(t, s.CheckBuyerSlot(ctx, "account-22", character.CommerceId), ErrBuyerFull)
	dir.SlotErr = accountdirectory.ErrRetryLater
	require.NoError(t, s.CheckBuyerSlot(ctx, "account-22", character.CommerceId), "读不到放行")
	dir.SlotErr = nil
	require.NoError(t, s.CheckBuyerSlot(ctx, "account-22", 999999), "商城才知道这个 listing")
}

// 规格 §3.6:投票期和角色冲突的拒绝事务都已回滚,后台不能打 Hold。
func TestTiDBDeliverRejectionsAreOutcomeKnown(t *testing.T) {
	s, dir, probe := rmbRuntime(t)
	r := rmbCreate(t, s, false)
	require.NoError(t, rmbStep(t, s, r.CommerceId))
	settleLegs(t, s.Engine, 2)
	for n := 0; n < 3; n++ {
		require.NoError(t, rmbStep(t, s, r.CommerceId))
	}
	_, w := state(t, s.Engine, r.CommerceId)
	in := rmbDelivery{Order: "00112233-4455-6677-8899-aabbccddeeff", Buyer: "22", Account: "account-22", Revision: "1", Listing: probe.Registers[0]["listingId"].(string), Reference: w.Rmb.EscrowReference, Proof: w.Rmb.EscrowProof, Amount: 101}
	// 买家还在投票:Lookup 不回准备令牌,事务里 lockRoles 拒绝。
	dir.Roles.Values = map[uint64]*dspb.AccountRoleOwnership{22: {Account: "account-22", OwnerRevision: 1, PrepareToken: 77, Role: &base.AccountSimplePlayer{PlayerId: 22}}}
	s.Engine.Roles = dir.Roles
	err := s.deliver(context.Background(), in)
	require.ErrorIs(t, err, errRoleConflict)
	require.True(t, OutcomeKnown(fmt.Errorf("人民币交付 %s: %w", in.Order, err)))
	_, w = state(t, s.Engine, r.CommerceId)
	require.Equal(t, "rmb_listed", w.Stage, "这个意图下一轮再判,不退款")
}

func TestTiDBOrdinaryBackgroundUsesPersistentOriginalIdentityAndRequestDigest(t *testing.T) {
	e := tiDBEngine(t)
	original := &dspb.AccountRoleOwnership{Account: "account-11", OwnerRevision: 1, Role: &base.AccountSimplePlayer{PlayerId: 11}}
	ctx, err := roleidentity.Capture(context.Background(), original)
	require.NoError(t, err)
	in := &tradepb.CreateConsignmentRequest{ClientRequestId: "original-contract", Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 1}}, CurrencyType: 1, Price: 10, DurationMs: 60000}
	r, err := e.CreateConsignment(ctx, 11, in)
	require.NoError(t, err)
	e.Roles = fixtureRoles{Values: map[uint64]*dspb.AccountRoleOwnership{11: {Account: "buyer", OwnerRevision: 2, Role: &base.AccountSimplePlayer{PlayerId: 11}}}}
	require.ErrorIs(t, e.advance(context.Background(), r), ErrConflict)
	var ops int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op").Scan(&ops))
	require.Zero(t, ops)
	other, err := roleidentity.Capture(context.Background(), &dspb.AccountRoleOwnership{Account: "buyer", OwnerRevision: 2, Role: &base.AccountSimplePlayer{PlayerId: 11}})
	require.NoError(t, err)
	_, err = e.CreateConsignment(other, 11, in)
	require.ErrorIs(t, err, ErrReplay)
}
func TestTiDBOfficialIntentForOldOwnerCreatesDurableRefundWithoutAssetOp(t *testing.T) {
	e := tiDBEngine(t)
	e.Roles = fixtureRoles{Values: map[uint64]*dspb.AccountRoleOwnership{22: {Account: "new-owner", OwnerRevision: 2, Role: &base.AccountSimplePlayer{PlayerId: 22}}}}
	m := &Mall{Engine: e}
	in := mallIntent{OrderID: "00112233-4455-6677-8899-aabbccddeeff", BuyerID: "22", BuyerAccount: "account-22", BuyerOwnerRevision: "1", SKU: 1, Quantity: 1, Config: 10, Count: 1, Amount: 101, Currency: "CNY"}
	r, err := m.accept(context.Background(), in)
	require.NoError(t, err)
	require.EqualValues(t, 8, r.Status)
	w, err := data.Workflow(r)
	require.NoError(t, err)
	require.Equal(t, "official_refund", w.Stage)
	require.Empty(t, w.Legs)
	var ops int
	require.NoError(t, e.Repo.DB.QueryRow("SELECT COUNT(*) FROM trade_asset_op").Scan(&ops))
	require.Zero(t, ops)
	again, err := m.accept(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, r.CommerceId, again.CommerceId)
}
