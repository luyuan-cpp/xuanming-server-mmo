package server

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"testing"
	"time"

	"player_inbox/internal/constants"
	"player_inbox/internal/inbox"
	"player_inbox/internal/logic"

	componentpb "proto/common/component"
	inboxpb "proto/player_inbox"
	"shared/rolefence"

	"github.com/zeromicro/go-zero/core/logx/logtest"
	"google.golang.org/grpc/peer"
)

const (
	adminTestBudget = 2500 * time.Millisecond
	adminTestPeer   = "10.1.2.3:45678"
	gmDomain        = inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION
)

// fakeAdminStore 记录每个方法收到的入参与 ctx 是否带截止时间;err 非 nil 时一律返回它。
type fakeAdminStore struct {
	err          error
	sawDeadline  map[string]bool
	voidParams   inbox.VoidParams
	voidLedgers  inbox.LedgerReader
	summaryID    uint64
	listReq      *inboxpb.ListPlayerInboxItemsRequest
	lookupReq    *inboxpb.LookupPlayerInboxRequest
	reconcileReq *inboxpb.ReconcilePlayerInboxRequest
	ledgersSeen  inbox.LedgerReader
	blocksID     uint64
	blocksSeen   inbox.ItemGainBlockReader
}

func newFakeAdminStore() *fakeAdminStore { return &fakeAdminStore{sawDeadline: map[string]bool{}} }

func (f *fakeAdminStore) note(ctx context.Context, method string) {
	_, f.sawDeadline[method] = ctx.Deadline()
}

func (f *fakeAdminStore) Void(ctx context.Context, p inbox.VoidParams, ledgers inbox.LedgerReader) (*inboxpb.VoidPlayerInboxItemResponse, error) {
	f.note(ctx, "Void")
	f.voidParams, f.voidLedgers = p, ledgers
	if f.err != nil {
		return nil, f.err
	}
	if p.Mode == inboxpb.PlayerInboxVoidMode_PLAYER_INBOX_VOID_MODE_FORCE_MISMATCH {
		return &inboxpb.VoidPlayerInboxItemResponse{
			State:        inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_VOIDED,
			ForceVerdict: inboxpb.PlayerInboxForceVoidVerdict_PLAYER_INBOX_FORCE_VOID_PROVEN,
		}, nil
	}
	return &inboxpb.VoidPlayerInboxItemResponse{State: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING, VoidRequested: true}, nil
}

func (f *fakeAdminStore) Summary(ctx context.Context, playerID uint64) (*inboxpb.GetPlayerInboxSummaryResponse, error) {
	f.note(ctx, "Summary")
	f.summaryID = playerID
	if f.err != nil {
		return nil, f.err
	}
	return &inboxpb.GetPlayerInboxSummaryResponse{PlayerId: playerID, MaxFenceEpoch: 9}, nil
}

func (f *fakeAdminStore) List(ctx context.Context, req *inboxpb.ListPlayerInboxItemsRequest) (*inboxpb.ListPlayerInboxItemsResponse, error) {
	f.note(ctx, "List")
	f.listReq = req
	if f.err != nil {
		return nil, f.err
	}
	return &inboxpb.ListPlayerInboxItemsResponse{HasMore: true}, nil
}

func (f *fakeAdminStore) Lookup(ctx context.Context, req *inboxpb.LookupPlayerInboxRequest) (*inboxpb.LookupPlayerInboxResponse, error) {
	f.note(ctx, "Lookup")
	f.lookupReq = req
	if f.err != nil {
		return nil, f.err
	}
	return &inboxpb.LookupPlayerInboxResponse{Item: &inboxpb.PlayerInboxItemView{Seq: 5}}, nil
}

func (f *fakeAdminStore) ReconcilePlayer(ctx context.Context, req *inboxpb.ReconcilePlayerInboxRequest, ledgers inbox.LedgerReader) (*inboxpb.ReconcilePlayerInboxResponse, error) {
	f.note(ctx, "ReconcilePlayer")
	f.reconcileReq, f.ledgersSeen = req, ledgers
	if f.err != nil {
		return nil, f.err
	}
	return &inboxpb.ReconcilePlayerInboxResponse{LedgerFound: true}, nil
}

func (f *fakeAdminStore) ItemGainBlocks(ctx context.Context, playerID uint64, blocks inbox.ItemGainBlockReader) (*inboxpb.GetPlayerItemGainBlocksResponse, error) {
	f.note(ctx, "ItemGainBlocks")
	f.blocksID, f.blocksSeen = playerID, blocks
	if f.err != nil {
		return nil, f.err
	}
	return &inboxpb.GetPlayerItemGainBlocksResponse{PlayerId: playerID, Found: true, BlockedItemConfigIds: []uint32{30045}}, nil
}

// stubLedgers 用指针比较身份:核对存储层拿到的就是装配时传进来的那一个。
type stubLedgers struct{ name string }

func (*stubLedgers) ReadInboxLedger(context.Context, uint64) (*componentpb.PlayerInboxLedgerComp, bool, error) {
	return nil, false, nil
}

// stubItemGainBlocks 同上,用指针比较身份。
type stubItemGainBlocks struct{ name string }

func (*stubItemGainBlocks) ReadItemGainBlock(context.Context, uint64) (*componentpb.PlayerItemGainBlockComp, bool, error) {
	return nil, false, nil
}

// fakeSanctionStore 是 GM 处置的存储替身:记录入参与 ctx 截止时间,新建时调 inTx(nil, seq) 模拟同事务写审计。
type fakeSanctionStore struct {
	registry    *inbox.Registry
	params      []inbox.EnqueueParams
	sawDeadline bool
	result      inbox.EnqueueResult
	err         error
}

func (f *fakeSanctionStore) Registry() *inbox.Registry { return f.registry }

func (f *fakeSanctionStore) Enqueue(ctx context.Context, p inbox.EnqueueParams, inTx func(*sql.Tx, uint64) error) (inbox.EnqueueResult, error) {
	_, f.sawDeadline = ctx.Deadline()
	f.params = append(f.params, p)
	if f.err != nil {
		return inbox.EnqueueResult{}, f.err
	}
	if f.result.Created && inTx != nil {
		if err := inTx(nil, f.result.Seq); err != nil {
			return inbox.EnqueueResult{}, err
		}
	}
	return f.result, nil
}

type adminFixture struct {
	server   *AdminServer
	store    *fakeAdminStore
	sanction *fakeSanctionStore
	ledgers  inbox.LedgerReader
	blocks   inbox.ItemGainBlockReader
	audits   []*inboxpb.PlayerInboxAudit
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	logtest.Discard(t)
	reg, err := inbox.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	f := &adminFixture{store: newFakeAdminStore(), sanction: &fakeSanctionStore{registry: reg}, ledgers: &stubLedgers{name: "assembly"},
		blocks: &stubItemGainBlocks{name: "assembly"}}
	deps := logic.GmSanctionDeps{
		Store: f.sanction,
		Audit: func(_ context.Context, _ *sql.Tx, a *inboxpb.PlayerInboxAudit) error {
			f.audits = append(f.audits, a)
			return nil
		},
		Now: func() time.Time { return time.UnixMilli(1_800_000_000_000) },
	}
	f.server = NewAdminServer(f.store, deps, f.ledgers, f.blocks, adminTestBudget)
	return f
}

// peerCtx 是带对端地址的入站 ctx(审计里唯一不可伪造的部分,设计 §1.6)。
func peerCtx() context.Context {
	addr, _ := net.ResolveTCPAddr("tcp", adminTestPeer)
	return peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
}

// TestAdminServerFoldsStoreFaultsInBand:运维面每个查询 / 作废方法在存储故障时都 in-band 回
// kPlayerInboxStoreUnavailable(fault 码,serverbase 据此告警),gRPC 层永不报错;每次调用都套了整请求预算。
func TestAdminServerFoldsStoreFaultsInBand(t *testing.T) {
	f := newAdminFixture(t)
	f.store.err = errors.New("db down")
	ctx := peerCtx()

	sum, err := f.server.GetSummary(ctx, &inboxpb.GetPlayerInboxSummaryRequest{PlayerId: 42})
	if err != nil || sum.GetErrorMessage().GetId() != constants.ErrStoreUnavailable || sum.GetPlayerId() != 42 {
		t.Errorf("GetSummary = %+v, %v(故障时仍回显 player_id)", sum, err)
	}
	list, err := f.server.ListItems(ctx, &inboxpb.ListPlayerInboxItemsRequest{PlayerId: 42, Domain: gmDomain})
	if err != nil || list.GetErrorMessage().GetId() != constants.ErrStoreUnavailable {
		t.Errorf("ListItems = %+v, %v", list, err)
	}
	look, err := f.server.LookupByRequest(ctx, &inboxpb.LookupPlayerInboxRequest{PlayerId: 42, RequestId: 7})
	if err != nil || look.GetErrorMessage().GetId() != constants.ErrStoreUnavailable {
		t.Errorf("LookupByRequest = %+v, %v", look, err)
	}
	void, err := f.server.VoidItem(ctx, &inboxpb.VoidPlayerInboxItemRequest{PlayerId: 42, Domain: gmDomain, Seq: 3})
	if err != nil || void.GetErrorMessage().GetId() != constants.ErrStoreUnavailable {
		t.Errorf("VoidItem = %+v, %v", void, err)
	}
	// 强制模式读账本失败、库故障同样 in-band 回 StoreUnavailable(可重试),回包不带任何证明结论与状态。
	force, err := f.server.VoidItem(ctx, &inboxpb.VoidPlayerInboxItemRequest{PlayerId: 42, Domain: gmDomain, Seq: 3,
		Mode: inboxpb.PlayerInboxVoidMode_PLAYER_INBOX_VOID_MODE_FORCE_MISMATCH})
	if err != nil || force.GetErrorMessage().GetId() != constants.ErrStoreUnavailable ||
		force.GetForceVerdict() != inboxpb.PlayerInboxForceVoidVerdict_PLAYER_INBOX_FORCE_VOID_UNSPECIFIED ||
		force.GetState() != inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_UNSPECIFIED {
		t.Errorf("VoidItem(FORCE) = %+v, %v", force, err)
	}
	rec, err := f.server.ReconcilePlayer(ctx, &inboxpb.ReconcilePlayerInboxRequest{PlayerId: 42})
	if err != nil || rec.GetErrorMessage().GetId() != constants.ErrStoreUnavailable {
		t.Errorf("ReconcilePlayer = %+v, %v", rec, err)
	}
	// 读存档或收件箱库失败:回包只带故障 tip 与回显的 player_id,不带任何封禁表 —— 空表会被读成「没封」。
	blocks, err := f.server.GetItemGainBlocks(ctx, &inboxpb.GetPlayerItemGainBlocksRequest{PlayerId: 42})
	if err != nil || blocks.GetErrorMessage().GetId() != constants.ErrStoreUnavailable || blocks.GetPlayerId() != 42 ||
		blocks.GetFound() || len(blocks.GetBlockedItemConfigIds()) != 0 || len(blocks.GetPending()) != 0 {
		t.Errorf("GetItemGainBlocks = %+v, %v", blocks, err)
	}
	for _, m := range []string{"Summary", "List", "Lookup", "Void", "ReconcilePlayer", "ItemGainBlocks"} {
		if !f.store.sawDeadline[m] {
			t.Errorf("%s 没有套整请求预算的截止时间", m)
		}
	}
}

// 规格 §3.4:作废时角色冻结或目录读不到,事务已回滚,回非故障 tip,排空证明不能 Hold。
func TestAdminVoidRoleNotVerifiedIsNotAFault(t *testing.T) {
	for name, cause := range map[string]error{
		"冻结或归属变了": inbox.ErrOriginalRole,
		"目录读不到":   errors.Join(rolefence.ErrUnavailable, errors.New("data_service down")),
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdminFixture(t)
			f.store.err = cause
			void, err := f.server.VoidItem(peerCtx(), &inboxpb.VoidPlayerInboxItemRequest{PlayerId: 42, Domain: gmDomain, Seq: 3})
			if err != nil || void.GetErrorMessage().GetId() != constants.ErrStaleOwner {
				t.Fatalf("VoidItem = %+v, %v", void, err)
			}
		})
	}
}

// TestAdminServerPassesThroughAndWires:成功路径原样透传存储回包;请求字段、对端地址与账本读取器按原样交给存储层。
func TestAdminServerPassesThroughAndWires(t *testing.T) {
	f := newAdminFixture(t)
	ctx := peerCtx()

	if sum, err := f.server.GetSummary(ctx, &inboxpb.GetPlayerInboxSummaryRequest{PlayerId: 42}); err != nil ||
		sum.GetErrorMessage() != nil || sum.GetMaxFenceEpoch() != 9 || f.store.summaryID != 42 {
		t.Errorf("GetSummary = %+v, %v (store saw %d)", sum, err, f.store.summaryID)
	}

	listReq := &inboxpb.ListPlayerInboxItemsRequest{PlayerId: 42, Domain: gmDomain, AfterSeq: 10, Limit: 5,
		State: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING}
	if list, err := f.server.ListItems(ctx, listReq); err != nil || !list.GetHasMore() || f.store.listReq != listReq {
		t.Errorf("ListItems = %+v, %v", list, err)
	}

	lookReq := &inboxpb.LookupPlayerInboxRequest{PlayerId: 42, ProducerKind: inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_GM, RequestId: 7}
	if look, err := f.server.LookupByRequest(ctx, lookReq); err != nil || look.GetItem().GetSeq() != 5 || f.store.lookupReq != lookReq {
		t.Errorf("LookupByRequest = %+v, %v", look, err)
	}

	void, err := f.server.VoidItem(ctx, &inboxpb.VoidPlayerInboxItemRequest{
		PlayerId: 42, Domain: gmDomain, Seq: 3, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_QUARANTINED,
		Operator: "ops_bob", Note: "poison",
	})
	if err != nil || !void.GetVoidRequested() {
		t.Errorf("VoidItem = %+v, %v", void, err)
	}
	want := inbox.VoidParams{
		PlayerID: 42, Domain: gmDomain, Seq: 3, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_QUARANTINED,
		Operator: "ops_bob", Note: "poison", PeerAddr: adminTestPeer,
	}
	if f.store.voidParams != want {
		t.Errorf("VoidParams = %+v, want %+v(对端地址取自 gRPC peer,进审计;缺省 mode = NORMAL)", f.store.voidParams, want)
	}

	// 强制模式:mode 原样交给存储层,账本读取器是装配层那一个(与对账共用);证明结论与终态原样透传。
	force, err := f.server.VoidItem(ctx, &inboxpb.VoidPlayerInboxItemRequest{
		PlayerId: 42, Domain: gmDomain, Seq: 4, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_CANCELLED,
		Operator: "ops_carol", Note: "工单 #11:恢复后旧行", Mode: inboxpb.PlayerInboxVoidMode_PLAYER_INBOX_VOID_MODE_FORCE_MISMATCH,
	})
	if err != nil || force.GetErrorMessage() != nil || force.GetVoidRequested() ||
		force.GetState() != inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_VOIDED ||
		force.GetForceVerdict() != inboxpb.PlayerInboxForceVoidVerdict_PLAYER_INBOX_FORCE_VOID_PROVEN {
		t.Errorf("VoidItem(FORCE) = %+v, %v", force, err)
	}
	wantForce := inbox.VoidParams{
		PlayerID: 42, Domain: gmDomain, Seq: 4, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_CANCELLED,
		Operator: "ops_carol", Note: "工单 #11:恢复后旧行", PeerAddr: adminTestPeer,
		Mode: inboxpb.PlayerInboxVoidMode_PLAYER_INBOX_VOID_MODE_FORCE_MISMATCH,
	}
	if f.store.voidParams != wantForce {
		t.Errorf("VoidParams(FORCE) = %+v, want %+v", f.store.voidParams, wantForce)
	}
	if f.store.voidLedgers != f.ledgers {
		t.Error("VoidItem 强制模式必须拿装配层的账本读取器去做证明(与对账共用同一个)")
	}

	recReq := &inboxpb.ReconcilePlayerInboxRequest{PlayerId: 42, Domain: gmDomain}
	if rec, err := f.server.ReconcilePlayer(ctx, recReq); err != nil || !rec.GetLedgerFound() || f.store.reconcileReq != recReq {
		t.Errorf("ReconcilePlayer = %+v, %v", rec, err)
	}
	if f.store.ledgersSeen != f.ledgers {
		t.Error("ReconcilePlayer 必须把装配层的账本读取器交给存储层(运维对账与定时对账共用同一个)")
	}

	blocks, err := f.server.GetItemGainBlocks(ctx, &inboxpb.GetPlayerItemGainBlocksRequest{PlayerId: 42})
	if err != nil || blocks.GetErrorMessage() != nil || !blocks.GetFound() || len(blocks.GetBlockedItemConfigIds()) != 1 ||
		f.store.blocksID != 42 {
		t.Errorf("GetItemGainBlocks = %+v, %v (store saw %d)", blocks, err, f.store.blocksID)
	}
	if f.store.blocksSeen != f.blocks {
		t.Error("GetItemGainBlocks 必须把装配层的封禁表读取器交给存储层")
	}
}

func blockRequest() *inboxpb.GmSanctionPlayerRequest {
	return &inboxpb.GmSanctionPlayerRequest{
		PlayerId: 42, RequestId: 9001, IssuedAtMs: 1_799_999_999_000,
		Action:   &inboxpb.GmSanctionPlayerRequest_BlockGain{BlockGain: &inboxpb.GmCurrencyGainPayload{CurrencyType: 2}},
		Reason:   "工单 #7",
		Operator: "ops_alice",
	}
}

// TestAdminServerGmSanctionPlayer:GM 处置经逻辑层入箱;审计带上对端地址;整请求预算生效;
// 存储故障 in-band 回 kPlayerInboxStoreUnavailable,业务拒绝透传 tip,gRPC 层永不报错。
func TestAdminServerGmSanctionPlayer(t *testing.T) {
	f := newAdminFixture(t)
	f.sanction.result = inbox.EnqueueResult{Seq: 123, Created: true, State: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING}

	resp, err := f.server.GmSanctionPlayer(peerCtx(), blockRequest())
	if err != nil || resp.GetErrorMessage() != nil || resp.GetSeq() != 123 || !resp.GetCreated() ||
		resp.GetState() != inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING {
		t.Fatalf("GmSanctionPlayer = %+v, %v", resp, err)
	}
	if !f.sanction.sawDeadline {
		t.Error("GmSanctionPlayer 没有套整请求预算的截止时间")
	}
	if len(f.sanction.params) != 1 || f.sanction.params[0].Type != inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN ||
		f.sanction.params[0].Producer != inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_GM {
		t.Fatalf("入箱参数 = %+v", f.sanction.params)
	}
	if len(f.audits) != 1 || f.audits[0].GetPeerAddr() != adminTestPeer || f.audits[0].GetSeq() != 123 ||
		f.audits[0].GetOperator() != "ops_alice" || f.audits[0].GetDomain() != uint32(gmDomain) {
		t.Fatalf("审计行 = %+v(对端地址必须取自 gRPC peer)", f.audits)
	}

	// 没有 peer 的调用(不该发生,防御):审计记 unknown,不 panic。
	f.audits = nil
	if _, err := f.server.GmSanctionPlayer(context.Background(), blockRequest()); err != nil ||
		len(f.audits) != 1 || f.audits[0].GetPeerAddr() != "unknown" {
		t.Fatalf("无 peer 时审计 = %+v, %v", f.audits, err)
	}

	f.sanction.result = inbox.EnqueueResult{Code: constants.ErrFull}
	if resp, err := f.server.GmSanctionPlayer(peerCtx(), blockRequest()); err != nil || resp.GetErrorMessage().GetId() != constants.ErrFull {
		t.Fatalf("业务拒绝应透传 tip: %+v, %v", resp, err)
	}

	f.sanction.err = errors.New("db down")
	if resp, err := f.server.GmSanctionPlayer(peerCtx(), blockRequest()); err != nil || resp.GetErrorMessage().GetId() != constants.ErrStoreUnavailable {
		t.Fatalf("存储故障应 in-band 回 StoreUnavailable: %+v, %v", resp, err)
	}
}
