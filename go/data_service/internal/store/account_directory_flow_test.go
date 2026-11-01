package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	base "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/rolefence"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 建角、过户、读列表、判死的编排:每一步失败之后的结局和重放。
//
// 这些组合要真库跑得一个个摆故障,而结局只取决于编排。flowSteps 是 directorySteps 的内存替身,语义抄 SQL 实现
// (上限判定、nonce 比较并删、严格插入),真库上的对应用例见 account_directory_shard_tidb_test.go。

const flowStorage uint32 = 7

var errFlow = errors.New("flow step failed")

type flowSteps struct {
	calls []string
	// fail 给每一步排错误,按调用顺序消费。nil 表示这一次放行。
	fail map[string][]error
	// before 在这一步执行之前调一次,用来摆并发交错。
	before map[string]func()

	accounts  map[string]bool
	refs      map[string]map[uint64]roleRef
	owners    map[uint64]*dspb.AccountRoleOwnership
	versions  map[uint64]uint64
	routes    map[uint64]RouteRow
	allowed   map[uint32]bool
	initial   map[uint64]int
	listings  map[uint64]*dbpb.AccountRoleTrade
	transfers map[uint64]*dbpb.AccountRoleTransferReceipt
	nonce     uint64

	// reserveWrittenThenFail 让 reserveRef 写下 ref 之后才回这个错误:提交结局未知,其实写成了。
	reserveWrittenThenFail error

	storeOwners map[uint64]lockedPlayerStoreOwner
	plan        *deadHolderPlan
	drained     []*deadHolderPlan
}

func newFlowStore(maxRoles int) (*AccountDirectoryStore, *flowSteps) {
	steps := &flowSteps{
		fail:        map[string][]error{},
		before:      map[string]func(){},
		accounts:    map[string]bool{},
		refs:        map[string]map[uint64]roleRef{},
		owners:      map[uint64]*dspb.AccountRoleOwnership{},
		versions:    map[uint64]uint64{},
		routes:      map[uint64]RouteRow{},
		allowed:     map[uint32]bool{flowStorage: true},
		initial:     map[uint64]int{},
		listings:    map[uint64]*dbpb.AccountRoleTrade{},
		transfers:   map[uint64]*dbpb.AccountRoleTransferReceipt{},
		storeOwners: map[uint64]lockedPlayerStoreOwner{},
	}
	return &AccountDirectoryStore{steps: steps, maxRoles: maxRoles, staleRefs: make(chan staleRefKey, staleRefQueueSize), playerStoreReads: make(chan struct{}, 1)}, steps
}

func (f *flowSteps) step(name string) error {
	f.calls = append(f.calls, name)
	if hook := f.before[name]; hook != nil {
		delete(f.before, name)
		hook()
	}
	if queue := f.fail[name]; len(queue) > 0 {
		f.fail[name] = queue[1:]
		return queue[0]
	}
	return nil
}

// only 按顺序列出调用记录里属于 names 的步骤。
func (f *flowSteps) only(names ...string) []string {
	out := []string{}
	for _, call := range f.calls {
		for _, name := range names {
			if call == name {
				out = append(out, call)
			}
		}
	}
	return out
}

func (f *flowSteps) count(name string) int { return len(f.only(name)) }

// writes 是调用记录里所有写目录或存档的步骤。
func (f *flowSteps) writes() []string {
	return f.only("ensureAccount", "reserveRef", "activateRef", "promoteRef", "dropRef", "writeInitialRows", "commitRole", "tombstoneMissingRole", "commitTransfer", "raiseStoreEpoch", "markHoldersDrained")
}

// routed 给一个角色配好路由,回这个角色。
func (f *flowSteps) routed(player uint64) *base.AccountSimplePlayer {
	f.routes[player] = RouteRow{PlayerID: player, HomeZone: 1, StorageID: flowStorage, PlacementVersion: 1, Rev: 1}
	return &base.AccountSimplePlayer{PlayerId: player, ZoneId: 1, Name: "flow"}
}

func (f *flowSteps) setOwner(player uint64, account string, revision uint64, listing uint64) {
	f.owners[player] = &dspb.AccountRoleOwnership{Account: account, Role: &base.AccountSimplePlayer{PlayerId: player, ZoneId: 1, Name: "flow"}, OwnerRevision: revision, TradeListingId: listing}
	f.versions[player]++
}

// putRef 写一条 ref。Nonce 是 0 时换一个新的,用例摆状态时不用管它;reserveRef 用调用方给的。
func (f *flowSteps) putRef(account string, ref roleRef) {
	if f.refs[account] == nil {
		f.refs[account] = map[uint64]roleRef{}
	}
	if ref.Nonce == 0 {
		f.nonce++
		ref.Nonce = f.nonce
	}
	ref.Account = account
	f.refs[account][ref.PlayerID] = ref
	f.accounts[account] = true
}

func (f *flowSteps) refList(account string) []roleRef {
	refs := make([]roleRef, 0, len(f.refs[account]))
	for _, ref := range f.refs[account] {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].PlayerID < refs[j].PlayerID })
	return refs
}

func (f *flowSteps) readAccount(_ context.Context, account string) (bool, []roleRef, error) {
	if err := f.step("readAccount"); err != nil {
		return false, nil, err
	}
	if !f.accounts[account] {
		return false, nil, nil
	}
	return true, f.refList(account), nil
}

func (f *flowSteps) ensureAccount(_ context.Context, account string) error {
	if err := f.step("ensureAccount"); err != nil {
		return err
	}
	f.accounts[account] = true
	return nil
}

func (f *flowSteps) readRef(_ context.Context, account string, player uint64) (*roleRef, error) {
	if err := f.step("readRef"); err != nil {
		return nil, err
	}
	ref, ok := f.refs[account][player]
	if !ok {
		return nil, nil
	}
	return &ref, nil
}

func (f *flowSteps) reserveRef(_ context.Context, account string, want roleRef, maxRoles int) ([]roleRef, error) {
	if err := f.step("reserveRef"); err != nil {
		return nil, err
	}
	f.accounts[account] = true
	_, existing := f.refs[account][want.PlayerID]
	if existing && want.TxID == 0 {
		return nil, nil
	}
	if refs := f.refList(account); !existing && len(refs) >= maxRoles {
		return refs, nil
	}
	// 读完 ref 之后、写之前。写是 upsert,这时 ref 被删了也写得回来。
	if hook := f.before["reserveRef.write"]; hook != nil {
		delete(f.before, "reserveRef.write")
		hook()
	}
	want.State = refStatePending
	f.putRef(account, want)
	return nil, f.reserveWrittenThenFail
}

func (f *flowSteps) activateRef(_ context.Context, account string, player, txID uint64) error {
	if err := f.step("activateRef"); err != nil {
		return err
	}
	// 别的过户在途就不动。
	if current, ok := f.refs[account][player]; ok && current.TxID != 0 && current.TxID != txID {
		return nil
	}
	f.putRef(account, roleRef{PlayerID: player, State: refStateActive})
	return nil
}

func (f *flowSteps) promoteRef(_ context.Context, account string, ref roleRef) error {
	if err := f.step("promoteRef"); err != nil {
		return err
	}
	if current, ok := f.refs[account][ref.PlayerID]; ok && current.Nonce == ref.Nonce {
		f.putRef(account, roleRef{PlayerID: ref.PlayerID, State: refStateActive})
	}
	return nil
}

func (f *flowSteps) dropRef(_ context.Context, account string, ref roleRef) (bool, error) {
	if err := f.step("dropRef"); err != nil {
		return false, err
	}
	current, ok := f.refs[account][ref.PlayerID]
	if !ok || current.Nonce != ref.Nonce {
		return false, nil
	}
	delete(f.refs[account], ref.PlayerID)
	return true, nil
}

func (f *flowSteps) scanPendingRefs(_ context.Context, olderThan time.Duration, limit int) ([]roleRef, error) {
	if err := f.step("scanPendingRefs"); err != nil {
		return nil, err
	}
	accounts := make([]string, 0, len(f.refs))
	for account := range f.refs {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	var out []roleRef
	for _, account := range accounts {
		for _, ref := range f.refList(account) {
			if ref.State == refStatePending && ref.AgeMs >= uint64(olderThan.Milliseconds()) && len(out) < limit {
				out = append(out, ref)
			}
		}
	}
	return out, nil
}

func (f *flowSteps) readOwner(_ context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
	if err := f.step("readOwner"); err != nil {
		return nil, err
	}
	owned := f.owners[player]
	if owned == nil {
		return nil, nil
	}
	return proto.Clone(owned).(*dspb.AccountRoleOwnership), nil
}

func (f *flowSteps) readOwners(_ context.Context, players []uint64) (map[uint64]*dspb.AccountRoleOwnership, error) {
	if err := f.step("readOwners"); err != nil {
		return nil, err
	}
	out := map[uint64]*dspb.AccountRoleOwnership{}
	for _, player := range players {
		if owned := f.owners[player]; owned != nil {
			out[player] = proto.Clone(owned).(*dspb.AccountRoleOwnership)
		}
	}
	return out, nil
}

func (f *flowSteps) readRoute(_ context.Context, player uint64) (RouteRow, bool, error) {
	if err := f.step("readRoute"); err != nil {
		return RouteRow{}, false, err
	}
	route, ok := f.routes[player]
	return route, ok, nil
}

func (f *flowSteps) commitRole(_ context.Context, account string, role *base.AccountSimplePlayer, _ []byte, storage uint32) error {
	if err := f.step("commitRole"); err != nil {
		return err
	}
	route, ok := f.routes[role.PlayerId]
	if !ok || route.HomeZone != role.ZoneId || route.Rev == 0 || route.StorageID != storage {
		return accountdirectory.ErrConflict
	}
	if old := f.owners[role.PlayerId]; old != nil {
		return checkCreateReplay(old, account, role)
	}
	f.owners[role.PlayerId] = &dspb.AccountRoleOwnership{Account: account, Role: proto.Clone(role).(*base.AccountSimplePlayer), OwnerRevision: 1}
	f.versions[role.PlayerId] = 1
	return nil
}

func (f *flowSteps) tombstoneMissingRole(_ context.Context, account string, ref roleRef) (bool, error) {
	if err := f.step("tombstoneMissingRole"); err != nil {
		return false, err
	}
	if old := f.owners[ref.PlayerID]; old != nil {
		return old.Account == account, nil
	}
	f.setOwner(ref.PlayerID, "", 1, 0)
	return false, nil
}

func (f *flowSteps) readListingState(_ context.Context, _, listing uint64) (dspb.AccountRoleTradeState, bool, error) {
	if err := f.step("readListingState"); err != nil {
		return 0, false, err
	}
	r := f.listings[listing]
	if r == nil {
		return 0, false, nil
	}
	return dspb.AccountRoleTradeState(r.State), true, nil
}

func (f *flowSteps) readListing(_ context.Context, _, listing uint64) (*dbpb.AccountRoleTrade, error) {
	if err := f.step("readListing"); err != nil {
		return nil, err
	}
	r := f.listings[listing]
	if r == nil {
		return nil, accountdirectory.ErrMissing
	}
	return proto.Clone(r).(*dbpb.AccountRoleTrade), nil
}

func (f *flowSteps) readTransfer(_ context.Context, _, txID uint64) (*dbpb.AccountRoleTransferReceipt, error) {
	if err := f.step("readTransfer"); err != nil {
		return nil, err
	}
	r := f.transfers[txID]
	if r == nil {
		return nil, nil
	}
	return proto.Clone(r).(*dbpb.AccountRoleTransferReceipt), nil
}

func (f *flowSteps) commitTransfer(_ context.Context, in *dspb.TransferAccountRoleRequest, digest []byte) (*dspb.AccountRoleTransferReceipt, error) {
	if err := f.step("commitTransfer"); err != nil {
		return nil, err
	}
	if prior := f.transfers[in.TxId]; prior != nil {
		if !bytes.Equal(prior.RequestDigest, digest) {
			return nil, accountdirectory.ErrConflict
		}
		return transferReceipt(prior)
	}
	listing := f.listings[in.ListingId]
	if listing == nil {
		return nil, accountdirectory.ErrMissing
	}
	if !matchesRoleTrade(listing, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) || listing.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN) {
		return nil, accountdirectory.ErrConflict
	}
	owned := f.owners[in.PlayerId]
	if owned == nil {
		return nil, accountdirectory.ErrMissing
	}
	if owned.Account != in.SellerAccount || owned.OwnerRevision != in.ExpectedOwnerRevision || owned.TradeListingId != in.ListingId {
		return nil, accountdirectory.ErrConflict
	}
	raw, err := proto.Marshal(owned.Role)
	if err != nil {
		return nil, err
	}
	r := &dbpb.AccountRoleTransferReceipt{TxId: in.TxId, RequestDigest: digest, ListingId: in.ListingId, PlayerId: in.PlayerId, SellerAccount: in.SellerAccount, BuyerAccount: in.BuyerAccount, SellerOwnerRevision: in.ExpectedOwnerRevision, BuyerOwnerRevision: owned.OwnerRevision + 1, CreatedMs: 1, RoleProto: raw}
	f.transfers[in.TxId] = r
	owned.Account, owned.OwnerRevision, owned.TradeListingId = in.BuyerAccount, r.BuyerOwnerRevision, 0
	f.versions[in.PlayerId]++
	listing.State = uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED)
	return transferReceipt(r)
}

func (f *flowSteps) readFence(_ context.Context, player uint64) (*rfpb.RoleFenceState, error) {
	if err := f.step("readFence"); err != nil {
		return nil, err
	}
	owned := f.owners[player]
	if owned == nil {
		return nil, accountdirectory.ErrMissing
	}
	return fenceStateOf(player, owned, f.versions[player]), nil
}

func (f *flowSteps) hasPendingHolders(context.Context, uint64, uint64) (bool, error) {
	if err := f.step("hasPendingHolders"); err != nil {
		return false, err
	}
	return f.plan != nil && len(f.plan.pending) != 0, nil
}

func (f *flowSteps) readDeadHolderPlan(_ context.Context, _, _ uint64) (*deadHolderPlan, error) {
	if err := f.step("readDeadHolderPlan"); err != nil {
		return nil, err
	}
	return f.plan, nil
}

func (f *flowSteps) markHoldersDrained(_ context.Context, plan *deadHolderPlan) error {
	if err := f.step("markHoldersDrained"); err != nil {
		return err
	}
	f.drained = append(f.drained, plan)
	return nil
}

func (f *flowSteps) storageAllowed(storage uint32) bool { return f.allowed[storage] }

func (f *flowSteps) writeInitialRows(_ context.Context, _ uint32, role *base.AccountSimplePlayer) error {
	if err := f.step("writeInitialRows"); err != nil {
		return err
	}
	f.initial[role.PlayerId]++
	return nil
}

func (f *flowSteps) readStoreOwner(_ context.Context, _ uint32, player uint64) (*lockedPlayerStoreOwner, error) {
	if err := f.step("readStoreOwner"); err != nil {
		return nil, err
	}
	owner, ok := f.storeOwners[player]
	if !ok {
		return nil, nil
	}
	return &owner, nil
}

func (f *flowSteps) raiseStoreEpoch(_ context.Context, _ uint32, player uint64, seen lockedPlayerStoreOwner, epoch uint64) (bool, error) {
	if err := f.step("raiseStoreEpoch"); err != nil {
		return false, err
	}
	current, ok := f.storeOwners[player]
	if !ok || current.ownerEpoch != seen.ownerEpoch || !bytes.Equal(current.holder, seen.holder) || current.claimSeq != seen.claimSeq {
		return false, nil
	}
	f.storeOwners[player] = lockedPlayerStoreOwner{ownerEpoch: epoch, claimSeq: current.claimSeq}
	return true, nil
}

// ── 令牌 ────────────────────────────────────────────────────────────────────

// freeze_token 是最终令牌,没有最终令牌时是准备令牌;freeze_final 只在最终令牌上成立。
func TestFenceStateOfSeparatesPreparingFromFinal(t *testing.T) {
	role := &base.AccountSimplePlayer{PlayerId: 11, ZoneId: 1}
	idle := fenceStateOf(11, &dspb.AccountRoleOwnership{Account: "seller", Role: role, OwnerRevision: 1}, 3)
	require.Zero(t, idle.FreezeToken)
	require.False(t, idle.FreezeFinal)
	preparing := fenceStateOf(11, &dspb.AccountRoleOwnership{Account: "seller", Role: role, OwnerRevision: 1, PrepareToken: 101}, 4)
	require.EqualValues(t, 101, preparing.FreezeToken)
	require.False(t, preparing.FreezeFinal)
	final := fenceStateOf(11, &dspb.AccountRoleOwnership{Account: "seller", Role: role, OwnerRevision: 1, TradeListingId: 101}, 5)
	require.EqualValues(t, 101, final.FreezeToken)
	require.True(t, final.FreezeFinal)
}

// 两个令牌不会同时非零,墓碑不带令牌:对不上就是库被动过,不能带着往下走。
func TestDecodeOwnershipRejectsImpossibleTokens(t *testing.T) {
	raw, err := proto.Marshal(&base.AccountSimplePlayer{PlayerId: 11, ZoneId: 1})
	require.NoError(t, err)
	_, err = decodeOwnership(11, "seller", raw, 1, 101, 0)
	require.NoError(t, err)
	_, err = decodeOwnership(11, "seller", raw, 1, 0, 101)
	require.NoError(t, err)
	_, err = decodeOwnership(11, "seller", raw, 1, 101, 102)
	require.Error(t, err)
	_, err = decodeOwnership(11, "", raw, 2, 0, 101)
	require.Error(t, err)
	_, err = decodeOwnership(11, "", raw, 2, 101, 0)
	require.Error(t, err)
}

// 规格 §3.5:Lookup 只看最终令牌,准备令牌不出目录,EnterGame 和 scene_manager 照常放行。
func TestLookupHidesPrepareToken(t *testing.T) {
	s, steps := newFlowStore(2)
	steps.routed(11)
	steps.setOwner(11, "seller", 1, 0)
	steps.owners[11].PrepareToken = 101
	owned, err := s.Lookup(context.Background(), 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
	require.Zero(t, owned.PrepareToken)
	steps.owners[11].PrepareToken = 0
	steps.owners[11].TradeListingId = 101
	owned, err = s.Lookup(context.Background(), 11)
	require.NoError(t, err)
	require.EqualValues(t, 101, owned.TradeListingId)
}

// 建角重放:准备令牌视同有令牌,不能把一名正在投票的角色当成空闲的回给 login。
func TestCreateRoleReplayRejectsPrepareToken(t *testing.T) {
	s, steps := newFlowStore(2)
	role := steps.routed(11)
	steps.setOwner(11, "owner", 1, 0)
	steps.owners[11].PrepareToken = 101
	_, err := s.Create(context.Background(), "owner", role)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.Empty(t, steps.writes())
}

// ── 建角 ────────────────────────────────────────────────────────────────────

func TestCreateRoleRunsFourStepsInOrder(t *testing.T) {
	s, steps := newFlowStore(2)
	role := steps.routed(11)
	out, err := s.Create(context.Background(), "owner", role)
	require.NoError(t, err)
	require.Len(t, out.Players, 1)
	require.Equal(t, []string{"reserveRef", "writeInitialRows", "commitRole", "activateRef"}, steps.writes())
	require.Equal(t, refStateActive, steps.refs["owner"][11].State)
	require.Equal(t, 1, steps.initial[11])
	require.Equal(t, "owner", steps.owners[11].Account)
}

// 每一步失败:角色行没有,读列表看不到,重放走到底。规格 §5.3:
//   - 第 3 步之前的失败回 ErrNotCommitted,这次写的 ref 当场删掉。login 据此释放名字。
//   - 第 3 步的错误(BEGIN 被拒之外)是结局未知,ref 留着,不能混:提交可能已经成了。
func TestCreateRoleFailureBeforeCommitThenReplay(t *testing.T) {
	for _, tc := range []struct {
		step string
		// unknown:结局未知,ref 留着。
		unknown bool
	}{
		{step: "readOwner"},
		{step: "readRoute"},
		{step: "reserveRef"},
		{step: "writeInitialRows"},
		{step: "commitRole", unknown: true},
	} {
		t.Run(tc.step, func(t *testing.T) {
			ctx := context.Background()
			s, steps := newFlowStore(2)
			role := steps.routed(11)
			steps.fail[tc.step] = []error{errFlow}

			_, err := s.Create(ctx, "owner", role)
			require.ErrorIs(t, err, errFlow)
			require.Nil(t, steps.owners[11], "提交点之前失败不能有角色行")
			if tc.unknown {
				require.NotErrorIs(t, err, accountdirectory.ErrNotCommitted, "提交结局未知不能说成确定没写")
				require.Equal(t, refStatePending, steps.refs["owner"][11].State)
				list, err := s.Read(ctx, "owner", false)
				require.NoError(t, err)
				require.Empty(t, list.Players, "PENDING ref 不等于有角色:列表只认真相")
			} else {
				require.ErrorIs(t, err, accountdirectory.ErrNotCommitted)
				require.Empty(t, steps.refs["owner"], "名额立刻退还")
			}

			out, err := s.Create(ctx, "owner", role)
			require.NoError(t, err)
			require.Len(t, out.Players, 1)
			require.Equal(t, refStateActive, steps.refs["owner"][11].State)
			require.Len(t, steps.refs["owner"], 1, "重放不多占名额")
			require.NotZero(t, steps.initial[11])
		})
	}
}

// 规格 §5.3:提交点之前失败,名额立刻退还,同账号满位也能接着建别的角色。
func TestCreateRoleNotCommittedReturnsTheSlot(t *testing.T) {
	for name, setup := range map[string]func(*flowSteps){
		"存档库故障": func(f *flowSteps) { f.fail["writeInitialRows"] = []error{errFlow} },
		"提交点 BEGIN 被闸门拒绝": func(f *flowSteps) {
			f.fail["commitRole"] = []error{notCommitted(authoritysql.ErrWriterFrozen)}
		},
		// 第 1 步提交结局未知,其实写成了:按这次的 nonce 删得掉。
		"第1步结局未知_ref其实写下了": func(f *flowSteps) { f.reserveWrittenThenFail = errFlow },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, steps := newFlowStore(1)
			role := steps.routed(11)
			setup(steps)

			_, err := s.Create(ctx, "owner", role)
			require.ErrorIs(t, err, accountdirectory.ErrNotCommitted)
			require.Nil(t, steps.owners[11])
			require.NotContains(t, steps.refs["owner"], uint64(11), "这次写的 ref 要删掉")

			steps.reserveWrittenThenFail = nil
			out, err := s.Create(ctx, "owner", steps.routed(12))
			require.NoError(t, err, "名额退还了")
			require.Len(t, out.Players, 1)
		})
	}
}

// 退还名额失败无害:结论还是确定没写,ref 留给后台。
func TestCreateRoleRefReleaseFailureKeepsTheVerdict(t *testing.T) {
	s, steps := newFlowStore(2)
	role := steps.routed(11)
	steps.fail["writeInitialRows"] = []error{errFlow}
	steps.fail["dropRef"] = []error{errors.New("account shard down")}
	_, err := s.Create(context.Background(), "owner", role)
	require.ErrorIs(t, err, accountdirectory.ErrNotCommitted)
	require.ErrorIs(t, err, errFlow)
	require.Equal(t, refStatePending, steps.refs["owner"][11].State)
}

// 只删这次尝试自己写的:ref 是上一次尝试留下的话,nonce 对不上,留给后台判陈旧。
func TestCreateRoleReleaseKeepsAnEarlierAttemptsRef(t *testing.T) {
	s, steps := newFlowStore(2)
	role := steps.routed(11)
	steps.putRef("owner", roleRef{PlayerID: 11, State: refStatePending, HomeZone: 1})
	earlier := steps.refs["owner"][11]
	steps.fail["writeInitialRows"] = []error{errFlow}
	_, err := s.Create(context.Background(), "owner", role)
	require.ErrorIs(t, err, accountdirectory.ErrNotCommitted)
	require.Equal(t, earlier, steps.refs["owner"][11])
}

// 确定的拒绝不包成 ErrNotCommitted:login 对它们有各自的处理(满位提示、冲突)。
func TestNotCommittedKeepsDefiniteRejections(t *testing.T) {
	for _, rejection := range []error{accountdirectory.ErrFull, accountdirectory.ErrConflict, accountdirectory.ErrInvalid} {
		require.NotErrorIs(t, notCommitted(rejection), accountdirectory.ErrNotCommitted, rejection.Error())
		require.ErrorIs(t, notCommitted(fmt.Errorf("包一层: %w", rejection)), rejection)
	}
	err := notCommitted(accountdirectory.ErrUnavailable)
	require.ErrorIs(t, err, accountdirectory.ErrNotCommitted)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable, "原因还在错误链上")
}

// 写过东西之后的失败不能带着哨兵:RPC 层会把它映射成确定性的码。
func TestNotRejectionDropsSentinels(t *testing.T) {
	require.NoError(t, notRejection(nil))
	require.ErrorIs(t, notRejection(errFlow), errFlow, "别的错误 RPC 层本来就回 Unavailable")
	for _, rejection := range []error{accountdirectory.ErrFull, accountdirectory.ErrConflict, accountdirectory.ErrInvalid, accountdirectory.ErrMissing, accountdirectory.ErrNotCommitted} {
		err := notRejection(fmt.Errorf("包一层: %w", rejection))
		require.ErrorIs(t, err, accountdirectory.ErrRetryLater, "这些哨兵都是回滚之后才回的,结局已知")
		require.NotErrorIs(t, err, rejection)
	}
	require.NotErrorIs(t, notRejection(errFlow), accountdirectory.ErrRetryLater, "SQL 失败可能是提交结局未知,不能说结局已知")
}

// 只读步骤失败:确定性的拒绝原样回,其余回 ErrRetryLater,原因留在错误链上。
func TestReadFailedKeepsDefiniteRejections(t *testing.T) {
	require.NoError(t, readFailed(nil))
	err := readFailed(errFlow)
	require.ErrorIs(t, err, accountdirectory.ErrRetryLater)
	require.ErrorIs(t, err, errFlow)
	for _, rejection := range []error{accountdirectory.ErrFull, accountdirectory.ErrConflict, accountdirectory.ErrInvalid, accountdirectory.ErrMissing} {
		err := readFailed(fmt.Errorf("包一层: %w", rejection))
		require.ErrorIs(t, err, rejection)
		require.NotErrorIs(t, err, accountdirectory.ErrRetryLater)
	}
}

// 第 4 步失败无害:角色已提交,列表照常有。重放只补 ACTIVE,不再碰存档:玩家可能已经存过盘。
func TestCreateRoleActivateFailureIsHarmless(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(2)
	role := steps.routed(11)
	steps.fail["activateRef"] = []error{errFlow}

	out, err := s.Create(ctx, "owner", role)
	require.NoError(t, err)
	require.Len(t, out.Players, 1)
	require.Equal(t, refStatePending, steps.refs["owner"][11].State)

	out, err = s.Create(ctx, "owner", role)
	require.NoError(t, err)
	require.Len(t, out.Players, 1)
	require.Equal(t, refStateActive, steps.refs["owner"][11].State)
	require.Equal(t, 1, steps.initial[11], "提交之后的重放不写存档")
	require.Equal(t, 1, steps.count("commitRole"), "提交之后的重放不再走提交点")
}

// 提交之后读列表失败:调用方当未知,重放拿到结果。
func TestCreateRoleReadBackFailureThenReplay(t *testing.T) {
	for _, step := range []string{"readAccount", "readOwners"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			s, steps := newFlowStore(2)
			role := steps.routed(11)
			steps.fail[step] = []error{errFlow}
			_, err := s.Create(ctx, "owner", role)
			require.ErrorIs(t, err, errFlow)
			require.Equal(t, "owner", steps.owners[11].Account, "已经过了提交点")
			out, err := s.Create(ctx, "owner", role)
			require.NoError(t, err)
			require.Len(t, out.Players, 1)
			require.Equal(t, 1, steps.initial[11])
		})
	}
}

// 第 3 步被别人抢先:这条 ref 现在就能证明陈旧,当场删掉,不占名额。
func TestCreateRoleCommitConflictDropsItsRef(t *testing.T) {
	s, steps := newFlowStore(2)
	role := steps.routed(11)
	steps.before["commitRole"] = func() { steps.setOwner(11, "other", 1, 0) }
	_, err := s.Create(context.Background(), "owner", role)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.NotContains(t, steps.refs["owner"], uint64(11))
	require.Equal(t, "other", steps.owners[11].Account)
}

// 路由在第 3 步对不上:证明不了这条 ref 陈旧(角色行还没有,也没超宽限),留给后台。
func TestCreateRoleRouteConflictKeepsItsRef(t *testing.T) {
	s, steps := newFlowStore(2)
	role := steps.routed(11)
	steps.before["commitRole"] = func() {
		route := steps.routes[11]
		route.StorageID = flowStorage + 1
		steps.routes[11] = route
	}
	_, err := s.Create(context.Background(), "owner", role)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.Equal(t, refStatePending, steps.refs["owner"][11].State)
	require.Nil(t, steps.owners[11])
}

// 已有角色行时不是重放:归属不同、已挂寄售、已删除、内容不同都冲突。删除后的角色不能复活。什么都不写。
func TestCreateRoleExistingRowRejected(t *testing.T) {
	for name, setup := range map[string]func(*flowSteps){
		"别的账号": func(f *flowSteps) { f.setOwner(11, "other", 1, 0) },
		"已删除":  func(f *flowSteps) { f.setOwner(11, "", 1, 0) },
		"已挂寄售": func(f *flowSteps) { f.setOwner(11, "owner", 1, 101) },
		"内容不同": func(f *flowSteps) {
			f.setOwner(11, "owner", 1, 0)
			f.owners[11].Role.Name = "changed"
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, steps := newFlowStore(2)
			role := steps.routed(11)
			setup(steps)
			_, err := s.Create(context.Background(), "owner", role)
			require.ErrorIs(t, err, accountdirectory.ErrConflict)
			require.Empty(t, steps.writes())
		})
	}
}

// 选存档库只认权威路由,对不上一律不写。
func TestCreateRoleInitialStorageChecks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*flowSteps)
		want  error
	}{
		{"没有路由", func(f *flowSteps) { delete(f.routes, 11) }, accountdirectory.ErrUnavailable},
		{"路由没有home", func(f *flowSteps) { f.routes[11] = RouteRow{PlayerID: 11, StorageID: flowStorage, Rev: 1} }, accountdirectory.ErrUnavailable},
		{"home不符", func(f *flowSteps) { f.routes[11] = RouteRow{PlayerID: 11, HomeZone: 2, StorageID: flowStorage, Rev: 1} }, accountdirectory.ErrConflict},
		{"没有落点", func(f *flowSteps) { f.routes[11] = RouteRow{PlayerID: 11, HomeZone: 1, Rev: 1} }, accountdirectory.ErrConflict},
		{"落点不在白名单", func(f *flowSteps) { delete(f.allowed, flowStorage) }, accountdirectory.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, steps := newFlowStore(2)
			role := steps.routed(11)
			tc.setup(steps)
			_, err := s.Create(context.Background(), "owner", role)
			require.ErrorIs(t, err, tc.want)
			require.Empty(t, steps.writes())
		})
	}
}

func TestCreateRoleRejectsInvalidInputWithoutTouchingAnything(t *testing.T) {
	s, steps := newFlowStore(2)
	_, err := s.Create(context.Background(), "", steps.routed(11))
	require.ErrorIs(t, err, accountdirectory.ErrInvalid)
	_, err = s.Create(context.Background(), "owner", &base.AccountSimplePlayer{ZoneId: 1})
	require.ErrorIs(t, err, accountdirectory.ErrInvalid)
	require.Empty(t, steps.calls)
}

// ── 上限和陈旧 ref ──────────────────────────────────────────────────────────

func TestCreateRoleAtCapWithLiveRolesIsFull(t *testing.T) {
	s, steps := newFlowStore(1)
	steps.setOwner(21, "owner", 1, 0)
	steps.putRef("owner", roleRef{PlayerID: 21, State: refStateActive})
	_, err := s.Create(context.Background(), "owner", steps.routed(11))
	require.ErrorIs(t, err, accountdirectory.ErrFull)
	require.NotErrorIs(t, err, accountdirectory.ErrNotCommitted, "满位 login 要提示玩家,不是稍后重试")
	require.Equal(t, []string{"reserveRef"}, steps.writes(), "满了不写存档,也不走提交点")
	require.NotContains(t, steps.refs["owner"], uint64(11))
	require.Zero(t, steps.initial[11])
}

// 满位不拒绝已提交的原角色重放:它已经有名额了。
func TestCreateRoleReplayAtCapStillSucceeds(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(1)
	role := steps.routed(11)
	_, err := s.Create(ctx, "owner", role)
	require.NoError(t, err)
	out, err := s.Create(ctx, "owner", role)
	require.NoError(t, err)
	require.Len(t, out.Players, 1)
}

// 提交结局未知之后的重放,ref 还在,不再占名额:满位也过得去。
func TestCreateRoleReplayOfPendingRefAtCapStillSucceeds(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(1)
	role := steps.routed(11)
	steps.fail["commitRole"] = []error{errFlow}
	_, err := s.Create(ctx, "owner", role)
	require.ErrorIs(t, err, errFlow)
	require.Contains(t, steps.refs["owner"], uint64(11))
	out, err := s.Create(ctx, "owner", role)
	require.NoError(t, err)
	require.Len(t, out.Players, 1)
}

func TestCapEvictsOnlyProvablyStaleRefs(t *testing.T) {
	old := uint64(refPendingGrace.Milliseconds())
	for _, tc := range []struct {
		name    string
		setup   func(*flowSteps)
		wantErr error
	}{
		{"ACTIVE_已卖给别人_腾位", func(f *flowSteps) {
			f.setOwner(21, "buyer", 2, 0)
			f.putRef("owner", roleRef{PlayerID: 21, State: refStateActive})
		}, nil},
		{"ACTIVE_已删除_腾位", func(f *flowSteps) {
			f.setOwner(21, "", 1, 0)
			f.putRef("owner", roleRef{PlayerID: 21, State: refStateActive})
		}, nil},
		{"PENDING建角_超宽限没有角色行_写墓碑腾位", func(f *flowSteps) {
			f.putRef("owner", roleRef{PlayerID: 21, State: refStatePending, HomeZone: 1, AgeMs: old})
		}, nil},
		{"PENDING建角_未超宽限_可能在途_不腾", func(f *flowSteps) {
			f.putRef("owner", roleRef{PlayerID: 21, State: refStatePending, HomeZone: 1, AgeMs: old - 1})
		}, accountdirectory.ErrFull},
		{"PENDING过户_还在FROZEN_可能在途_不腾", func(f *flowSteps) {
			f.listings[101] = &dbpb.AccountRoleTrade{ListingId: 101, PlayerId: 21, State: uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN)}
			f.setOwner(21, "seller", 1, 101)
			f.putRef("owner", roleRef{PlayerID: 21, State: refStatePending, TxID: 1001, ListingID: 101})
		}, accountdirectory.ErrFull},
		{"PENDING过户_已取消_腾位", func(f *flowSteps) {
			f.listings[101] = &dbpb.AccountRoleTrade{ListingId: 101, PlayerId: 21, State: uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED)}
			f.setOwner(21, "seller", 1, 0)
			f.putRef("owner", roleRef{PlayerID: 21, State: refStatePending, TxID: 1001, ListingID: 101})
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, steps := newFlowStore(1)
			tc.setup(steps)
			_, err := s.Create(context.Background(), "owner", steps.routed(11))
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Contains(t, steps.refs["owner"], uint64(21), "证明不了陈旧的 ref 不能删:它背后可能是一笔在途的提交")
				return
			}
			require.NoError(t, err)
			require.NotContains(t, steps.refs["owner"], uint64(21))
			require.Contains(t, steps.refs["owner"], uint64(11))
		})
	}
}

// 证明读失败时不能说是满了:玩家重试可能就过了。
func TestCapProofReadFailureIsNotFull(t *testing.T) {
	s, steps := newFlowStore(1)
	steps.putRef("owner", roleRef{PlayerID: 21, State: refStateActive})
	steps.setOwner(21, "buyer", 2, 0)
	// 第一次 readOwner 是建角的预读,第二次是判陈旧。
	steps.fail["readOwner"] = []error{nil, errFlow}
	_, err := s.Create(context.Background(), "owner", steps.routed(11))
	require.ErrorIs(t, err, errFlow)
	require.NotErrorIs(t, err, accountdirectory.ErrFull)
}

// ── 读列表 ──────────────────────────────────────────────────────────────────

// 列表只认真相:卖出去的、删掉的、建角没成的 ref 都不进列表,交给后台清。提交了但 ACTIVE 没写成的照样在列表里。
func TestReadRoleListFiltersRefsByTruth(t *testing.T) {
	s, steps := newFlowStore(5)
	steps.setOwner(1, "owner", 1, 0)
	steps.putRef("owner", roleRef{PlayerID: 1, State: refStateActive})
	steps.setOwner(2, "owner", 1, 0)
	steps.putRef("owner", roleRef{PlayerID: 2, State: refStatePending})
	steps.setOwner(3, "buyer", 2, 0)
	steps.putRef("owner", roleRef{PlayerID: 3, State: refStateActive})
	steps.setOwner(4, "", 1, 0)
	steps.putRef("owner", roleRef{PlayerID: 4, State: refStateActive})
	steps.putRef("owner", roleRef{PlayerID: 5, State: refStatePending})

	list, err := s.Read(context.Background(), "owner", false)
	require.NoError(t, err)
	ids := []uint64{}
	for _, role := range list.Players {
		ids = append(ids, role.PlayerId)
	}
	require.Equal(t, []uint64{1, 2}, ids)
	require.Empty(t, steps.writes(), "读路径不写")
	require.Equal(t, 3, len(s.staleRefs), "陈旧的交给后台")
}

func TestReadAccountDirectoryCreateFlag(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(2)
	_, err := s.Read(ctx, "owner", false)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	require.Empty(t, steps.writes(), "create=false 不建行")
	list, err := s.Read(ctx, "owner", true)
	require.NoError(t, err)
	require.Empty(t, list.Players)
	require.Equal(t, []string{"ensureAccount"}, steps.writes())
	steps.calls = nil
	_, err = s.Read(ctx, "owner", true)
	require.NoError(t, err)
	require.Empty(t, steps.writes(), "目录行已在就只读返回")
	_, err = s.Read(ctx, "", true)
	require.ErrorIs(t, err, accountdirectory.ErrInvalid)
}

// 读失败回错误,不能回空列表:空列表会被当成「这个账号没有角色」。
func TestReadRoleListFailuresAreErrors(t *testing.T) {
	for _, step := range []string{"readAccount", "readOwners"} {
		t.Run(step, func(t *testing.T) {
			s, steps := newFlowStore(2)
			steps.setOwner(1, "owner", 1, 0)
			steps.putRef("owner", roleRef{PlayerID: 1, State: refStateActive})
			steps.fail[step] = []error{errFlow}
			_, err := s.Read(context.Background(), "owner", false)
			require.ErrorIs(t, err, errFlow)
		})
	}
}

func TestLookupTreatsTombstoneAsMissing(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(2)
	_, err := s.Lookup(ctx, 1)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	steps.setOwner(1, "", 1, 0)
	_, err = s.Lookup(ctx, 1)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	steps.setOwner(2, "owner", 1, 0)
	owned, err := s.Lookup(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, "owner", owned.Account)
	steps.fail["readOwner"] = []error{errFlow}
	_, err = s.Lookup(ctx, 2)
	require.ErrorIs(t, err, errFlow)
	_, err = s.Lookup(ctx, 0)
	require.ErrorIs(t, err, accountdirectory.ErrInvalid)
}

// ── 判 ref 陈旧 ─────────────────────────────────────────────────────────────

func TestReapRefRules(t *testing.T) {
	const (
		frozen      = dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN
		transferred = dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED
		cancelled   = dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED
	)
	old := uint64(refPendingGrace.Milliseconds())
	create := func(age uint64) roleRef {
		return roleRef{PlayerID: 11, State: refStatePending, HomeZone: 1, AgeMs: age}
	}
	transfer := roleRef{PlayerID: 11, State: refStatePending, TxID: 1001, ListingID: 101}
	active := roleRef{PlayerID: 11, State: refStateActive}
	for _, tc := range []struct {
		name string
		ref  roleRef
		// owner 是角色行上的账号,noRow 表示没有角色行。
		owner string
		noRow bool
		// listing 是收据状态,0 表示没有收据。
		listing     dspb.AccountRoleTradeState
		wantDropped bool
		wantState   uint32
		wantTomb    bool
	}{
		{name: "ACTIVE_真相是本账号_不动", ref: active, owner: "owner", wantState: refStateActive},
		{name: "ACTIVE_真相是别人_删", ref: active, owner: "other", wantDropped: true},
		{name: "ACTIVE_墓碑_删", ref: active, owner: "", wantDropped: true},
		{name: "ACTIVE_没有角色行_库被人动过_不动", ref: active, noRow: true, wantState: refStateActive},
		{name: "PENDING建角_真相是本账号_补ACTIVE", ref: create(0), owner: "owner", wantState: refStateActive},
		{name: "PENDING建角_别人先建了_删", ref: create(0), owner: "other", wantDropped: true},
		{name: "PENDING建角_墓碑_删", ref: create(0), owner: "", wantDropped: true},
		{name: "PENDING建角_没有角色行_未超宽限_不动", ref: create(old - 1), noRow: true, wantState: refStatePending},
		{name: "PENDING建角_没有角色行_超宽限_写墓碑删", ref: create(old), noRow: true, wantDropped: true, wantTomb: true},
		{name: "PENDING过户_真相是本账号_补ACTIVE", ref: transfer, owner: "owner", listing: transferred, wantState: refStateActive},
		{name: "PENDING过户_还在FROZEN_不动", ref: transfer, owner: "seller", listing: frozen, wantState: refStatePending},
		{name: "PENDING过户_已转给别人_删", ref: transfer, owner: "third", listing: transferred, wantDropped: true},
		{name: "PENDING过户_已取消_删", ref: transfer, owner: "seller", listing: cancelled, wantDropped: true},
		{name: "PENDING过户_收据没了_库被人动过_不动", ref: transfer, owner: "seller", wantState: refStatePending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, steps := newFlowStore(5)
			if !tc.noRow {
				steps.setOwner(11, tc.owner, 1, 0)
			}
			if tc.listing != 0 {
				steps.listings[101] = &dbpb.AccountRoleTrade{ListingId: 101, PlayerId: 11, State: uint32(tc.listing)}
			}
			steps.putRef("owner", tc.ref)
			dropped, err := s.reapRef(context.Background(), "owner", steps.refs["owner"][11])
			require.NoError(t, err)
			require.Equal(t, tc.wantDropped, dropped)
			if tc.wantDropped {
				require.NotContains(t, steps.refs["owner"], uint64(11))
			} else {
				require.Equal(t, tc.wantState, steps.refs["owner"][11].State)
			}
			if tc.wantTomb {
				require.Equal(t, "", steps.owners[11].Account, "墓碑挡住迟到的建角")
			}
		})
	}
}

// 写墓碑时迟到的建角先提交了:真相就是这个账号,补 ACTIVE,不删。
func TestReapRefTombstoneLosesToLateCreate(t *testing.T) {
	s, steps := newFlowStore(5)
	steps.putRef("owner", roleRef{PlayerID: 11, State: refStatePending, HomeZone: 1, AgeMs: uint64(refPendingGrace.Milliseconds())})
	steps.before["tombstoneMissingRole"] = func() { steps.setOwner(11, "owner", 1, 0) }
	dropped, err := s.reapRef(context.Background(), "owner", steps.refs["owner"][11])
	require.NoError(t, err)
	require.False(t, dropped)
	require.Equal(t, refStateActive, steps.refs["owner"][11].State)
}

// 过户的 PENDING 要先读 listing、后读真相。过户提交落在这两次读的前面或中间,这条 ref 都不能删:
// 删了之后买家拥有角色却没有 ref,finishTransfer 再补失败就没有任何路径能发现。
func TestReapRefPendingTransferSurvivesConcurrentCommit(t *testing.T) {
	for name, commitBefore := range map[string]string{
		"提交在读listing之前":     "readListingState",
		"提交在读listing和读真相之间": "readOwner",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			trades, steps, _, in := newFlowTransfer()
			digest, err := roleTradeDigest(in)
			require.NoError(t, err)
			steps.putRef("buyer", roleRef{PlayerID: 11, State: refStatePending, TxID: 1001, ListingID: 101})
			seen := steps.refs["buyer"][11]
			steps.before[commitBefore] = func() {
				_, err := steps.commitTransfer(ctx, in, digest)
				require.NoError(t, err)
			}
			dropped, err := trades.directory.reapRef(ctx, "buyer", seen)
			require.NoError(t, err)
			require.False(t, dropped, "真相就是买家")
			require.Equal(t, refStateActive, steps.refs["buyer"][11].State, "补上没做完的 ACTIVE")
			require.Equal(t, "buyer", steps.owners[11].Account)
			require.Equal(t, []string{"readListingState", "readOwner"}, steps.only("readListingState", "readOwner"), "先读 listing,后读真相")
		})
	}
}

// listing 已到终态、读完之后真相才变到这个账号:只可能是后面又一笔过户,它会改写 ref 换 nonce,删除比较 nonce 什么都不做。
func TestReapRefSettledListingDropIsComparedByNonce(t *testing.T) {
	s, steps := newFlowStore(5)
	steps.listings[101] = &dbpb.AccountRoleTrade{ListingId: 101, PlayerId: 11, State: uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED)}
	steps.setOwner(11, "third", 3, 0)
	steps.putRef("owner", roleRef{PlayerID: 11, State: refStatePending, TxID: 1001, ListingID: 101})
	seen := steps.refs["owner"][11]
	steps.before["dropRef"] = func() {
		steps.putRef("owner", roleRef{PlayerID: 11, State: refStatePending, TxID: 1002, ListingID: 102})
	}
	dropped, err := s.reapRef(context.Background(), "owner", seen)
	require.NoError(t, err)
	require.False(t, dropped)
	require.EqualValues(t, 1002, steps.refs["owner"][11].TxID)
}

// 迟到的建角被墓碑挡住之后是冲突,不会复活。
func TestLateCreateAfterTombstoneIsRejected(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(5)
	role := steps.routed(11)
	steps.putRef("owner", roleRef{PlayerID: 11, State: refStatePending, HomeZone: 1, AgeMs: uint64(refPendingGrace.Milliseconds())})
	dropped, err := s.reapRef(ctx, "owner", steps.refs["owner"][11])
	require.NoError(t, err)
	require.True(t, dropped)
	_, err = s.Create(ctx, "owner", role)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.Empty(t, s.mustListForTest(t, "owner").Players)
}

func (s *AccountDirectoryStore) mustListForTest(t *testing.T, account string) *base.AccountSimplePlayerList {
	t.Helper()
	list, err := s.Read(context.Background(), account, true)
	require.NoError(t, err)
	return list
}

// 判完陈旧之后又有新的写(过户第 1 步换了 nonce):删除比较 nonce,什么都不做。
func TestReapRefDropIsComparedByNonce(t *testing.T) {
	s, steps := newFlowStore(5)
	steps.setOwner(11, "other", 2, 0)
	steps.putRef("owner", roleRef{PlayerID: 11, State: refStateActive})
	seen := steps.refs["owner"][11]
	steps.before["dropRef"] = func() {
		steps.putRef("owner", roleRef{PlayerID: 11, State: refStatePending, TxID: 1001, ListingID: 101})
	}
	dropped, err := s.reapRef(context.Background(), "owner", seen)
	require.NoError(t, err)
	require.False(t, dropped)
	require.Contains(t, steps.refs["owner"], uint64(11))
}

func TestReapRefReadFailuresKeepTheRef(t *testing.T) {
	for _, step := range []string{"readOwner", "readListingState", "tombstoneMissingRole", "dropRef", "promoteRef"} {
		t.Run(step, func(t *testing.T) {
			s, steps := newFlowStore(5)
			var ref roleRef
			switch step {
			case "readListingState":
				steps.setOwner(11, "seller", 1, 0)
				ref = roleRef{PlayerID: 11, State: refStatePending, TxID: 1001, ListingID: 101}
			case "tombstoneMissingRole":
				ref = roleRef{PlayerID: 11, State: refStatePending, HomeZone: 1, AgeMs: uint64(refPendingGrace.Milliseconds())}
			case "dropRef":
				steps.setOwner(11, "other", 2, 0)
				ref = roleRef{PlayerID: 11, State: refStateActive}
			case "promoteRef":
				steps.setOwner(11, "owner", 1, 0)
				ref = roleRef{PlayerID: 11, State: refStatePending}
			default:
				ref = roleRef{PlayerID: 11, State: refStateActive}
			}
			steps.putRef("owner", ref)
			steps.fail[step] = []error{errFlow}
			dropped, err := s.reapRef(context.Background(), "owner", steps.refs["owner"][11])
			require.ErrorIs(t, err, errFlow)
			require.False(t, dropped)
			require.Contains(t, steps.refs["owner"], uint64(11))
		})
	}
}

func TestSweepStaleRefs(t *testing.T) {
	ctx := context.Background()
	old := uint64(refPendingGrace.Milliseconds())

	t.Run("读路径发现的和超时的PENDING都清", func(t *testing.T) {
		s, steps := newFlowStore(5)
		steps.setOwner(1, "other", 2, 0)
		steps.putRef("owner", roleRef{PlayerID: 1, State: refStateActive})
		s.noteStaleRef("owner", 1)
		steps.putRef("late", roleRef{PlayerID: 2, State: refStatePending, HomeZone: 1, AgeMs: old})
		steps.putRef("young", roleRef{PlayerID: 3, State: refStatePending, HomeZone: 1})
		result, err := s.SweepStaleRefs(ctx, refPendingGrace, 64)
		require.NoError(t, err)
		require.Equal(t, SweepResult{Scanned: 2, Dropped: 2}, result)
		require.NotContains(t, steps.refs["owner"], uint64(1))
		require.NotContains(t, steps.refs["late"], uint64(2))
		require.Contains(t, steps.refs["young"], uint64(3))
	})

	t.Run("名额用完就停", func(t *testing.T) {
		s, steps := newFlowStore(5)
		for player := uint64(1); player <= 3; player++ {
			steps.setOwner(player, "other", 2, 0)
			steps.putRef("owner", roleRef{PlayerID: player, State: refStateActive})
			s.noteStaleRef("owner", player)
		}
		result, err := s.SweepStaleRefs(ctx, refPendingGrace, 2)
		require.NoError(t, err)
		require.Equal(t, 2, result.Scanned)
		require.Zero(t, steps.count("scanPendingRefs"))
		require.Equal(t, 1, len(s.staleRefs), "没轮到的留给下一轮")
	})

	t.Run("一条失败不影响别的", func(t *testing.T) {
		s, steps := newFlowStore(5)
		steps.setOwner(1, "other", 2, 0)
		steps.setOwner(2, "other", 2, 0)
		steps.putRef("owner", roleRef{PlayerID: 1, State: refStateActive})
		steps.putRef("owner", roleRef{PlayerID: 2, State: refStateActive})
		s.noteStaleRef("owner", 1)
		s.noteStaleRef("owner", 2)
		steps.fail["dropRef"] = []error{errFlow}
		result, err := s.SweepStaleRefs(ctx, refPendingGrace, 64)
		require.ErrorIs(t, err, errFlow)
		require.Equal(t, SweepResult{Scanned: 2, Dropped: 1, Failed: 1}, result)
	})

	t.Run("扫描失败回错误", func(t *testing.T) {
		s, steps := newFlowStore(5)
		steps.fail["scanPendingRefs"] = []error{errFlow}
		_, err := s.SweepStaleRefs(ctx, refPendingGrace, 64)
		require.ErrorIs(t, err, errFlow)
	})
}

// 排队满了就丢,不能挡住登录的读路径。
func TestNoteStaleRefNeverBlocks(t *testing.T) {
	s, _ := newFlowStore(5)
	for i := 0; i < staleRefQueueSize+10; i++ {
		s.noteStaleRef("owner", uint64(i+1))
	}
	require.Equal(t, staleRefQueueSize, len(s.staleRefs))
}

// ── 过户 ────────────────────────────────────────────────────────────────────

func newFlowTransfer() (*AccountRoleTradeStore, *flowSteps, *fakeFences, *dspb.TransferAccountRoleRequest) {
	s, steps := newFlowStore(2)
	fences := &fakeFences{outcome: rolefence.PrepareClean}
	s.ConfigureFenceCoordinator(fences)
	steps.routed(11)
	steps.setOwner(11, "seller", 1, 101)
	steps.putRef("seller", roleRef{PlayerID: 11, State: refStateActive})
	steps.listings[101] = &dbpb.AccountRoleTrade{ListingId: 101, PlayerId: 11, SellerAccount: "seller", SellerOwnerRevision: 1, State: uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN)}
	in := &dspb.TransferAccountRoleRequest{TxId: 1001, ListingId: 101, PlayerId: 11, SellerAccount: "seller", BuyerAccount: "buyer", ExpectedOwnerRevision: 1}
	return NewAccountRoleTradeStore(s, nil), steps, fences, in
}

func requireTransferFinished(t *testing.T, steps *flowSteps) {
	t.Helper()
	require.Equal(t, "buyer", steps.owners[11].Account)
	require.EqualValues(t, 2, steps.owners[11].OwnerRevision)
	require.Zero(t, steps.owners[11].TradeListingId)
	require.Equal(t, refStateActive, steps.refs["buyer"][11].State)
	require.NotContains(t, steps.refs["seller"], uint64(11))
}

func TestTransferRunsThreeStepsInOrder(t *testing.T) {
	trades, steps, fences, in := newFlowTransfer()
	out, err := trades.Transfer(context.Background(), in)
	require.NoError(t, err)
	require.EqualValues(t, 2, out.BuyerOwnerRevision)
	require.Equal(t, []string{"reserveRef", "commitTransfer", "activateRef", "dropRef"}, steps.writes())
	requireTransferFinished(t, steps)
	require.Equal(t, 1, fences.pushedCount())
	require.Equal(t, "buyer", fences.lastPushed().Account)
	require.Zero(t, fences.lastPushed().FreezeToken)
	require.Equal(t, uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED), steps.listings[101].State)
}

// 提交点之前失败:归属没变,没有收据,同 tx_id 重放走到底。
func TestTransferFailureBeforeCommitThenReplay(t *testing.T) {
	for _, tc := range []struct {
		step string
		// wantPending:失败时买家的 PENDING ref 已经写下了。
		wantPending bool
		// readOnly:这一步只读,确定没写,不打排空 Hold。写的步骤失败可能是提交结局未知。
		readOnly bool
	}{
		{step: "readTransfer", readOnly: true},
		{step: "readListing", readOnly: true},
		{step: "reserveRef"},
		{step: "commitTransfer", wantPending: true},
	} {
		t.Run(tc.step, func(t *testing.T) {
			ctx := context.Background()
			trades, steps, fences, in := newFlowTransfer()
			steps.fail[tc.step] = []error{errFlow}
			_, err := trades.Transfer(ctx, in)
			require.ErrorIs(t, err, errFlow)
			require.Equal(t, tc.readOnly, errors.Is(err, accountdirectory.ErrRetryLater))
			require.Equal(t, "seller", steps.owners[11].Account)
			require.EqualValues(t, 101, steps.owners[11].TradeListingId)
			require.Empty(t, steps.transfers)
			require.Zero(t, fences.pushedCount())
			if tc.wantPending {
				ref := steps.refs["buyer"][11]
				require.Equal(t, refStatePending, ref.State)
				require.EqualValues(t, 1001, ref.TxID)
				require.EqualValues(t, 101, ref.ListingID)
			} else {
				require.Empty(t, steps.refs["buyer"])
			}

			out, err := trades.Transfer(ctx, in)
			require.NoError(t, err)
			require.EqualValues(t, 2, out.BuyerOwnerRevision)
			requireTransferFinished(t, steps)
		})
	}
}

// 规格 §5.4:提交之后读护栏状态或写 ACTIVE 失败回 ErrRetryLater,trade 用同一 tx_id 重放补上。不重放的话买家的 ref 可能一直缺。
// 过户已提交,结局已知,排空证明不能 Hold(§3.6)。
func TestTransferFinishFailureIsRetryLaterAndReplayCompletes(t *testing.T) {
	for _, step := range []string{"readFence", "activateRef"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			trades, steps, fences, in := newFlowTransfer()
			steps.fail[step] = []error{errFlow}
			_, err := trades.Transfer(ctx, in)
			require.ErrorIs(t, err, accountdirectory.ErrRetryLater)
			require.Equal(t, "buyer", steps.owners[11].Account, "已经过了提交点")
			require.Len(t, steps.transfers, 1)

			buyer, err := trades.directory.Read(ctx, "buyer", false)
			require.NoError(t, err)
			require.Len(t, buyer.Players, 1, "PENDING ref 也算:真相已经是买家")

			replayed, err := trades.Transfer(ctx, in)
			require.NoError(t, err)
			require.EqualValues(t, 2, replayed.BuyerOwnerRevision)
			requireTransferFinished(t, steps)
			require.Equal(t, 1, steps.count("commitTransfer"), "重放不再走提交点")
			require.NotZero(t, fences.pushedCount())
		})
	}
}

// 已经提交了,哨兵不能留在错误链上:ErrMissing 会被映射成确定性的 NotFound,trade 和排空证明都会当成没写。
func TestTransferFinishFailureDropsSentinels(t *testing.T) {
	trades, steps, _, in := newFlowTransfer()
	steps.fail["readFence"] = []error{accountdirectory.ErrMissing}
	_, err := trades.Transfer(context.Background(), in)
	require.ErrorIs(t, err, accountdirectory.ErrRetryLater)
	require.NotErrorIs(t, err, accountdirectory.ErrMissing)
}

// 写 ACTIVE 失败时删卖家 ref 和推送照做:过户已经提交,不用等重放。
func TestTransferActivateFailureStillPushes(t *testing.T) {
	trades, steps, fences, in := newFlowTransfer()
	steps.fail["activateRef"] = []error{errFlow}
	_, err := trades.Transfer(context.Background(), in)
	require.ErrorIs(t, err, accountdirectory.ErrRetryLater)
	require.Equal(t, 1, fences.pushedCount())
	require.NotContains(t, steps.refs["seller"], uint64(11))
}

// 写 ACTIVE 这条语句本身结局未知时照旧算未知,要 Hold。方向不能反。
func TestTransferActivateUnknownOutcomeStaysUnknown(t *testing.T) {
	trades, steps, _, in := newFlowTransfer()
	steps.fail["activateRef"] = []error{fmt.Errorf("commit: %w", authoritysql.ErrOutcomeUnknown)}
	_, err := trades.Transfer(context.Background(), in)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	require.NotErrorIs(t, err, accountdirectory.ErrRetryLater)
}

func newFlowBegin() (*AccountRoleTradeStore, *flowSteps, *fakeFences, *dspb.BeginAccountRoleTradeRequest) {
	s, steps := newFlowStore(2)
	fences := &fakeFences{outcome: rolefence.PrepareClean}
	s.ConfigureFenceCoordinator(fences)
	steps.setOwner(11, "seller", 1, 0)
	return NewAccountRoleTradeStore(s, nil), steps, fences, &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
}

// 规格 §3.6:试探问不齐时目录一个字都没写。回 ErrRetryLater,不能回 ErrUnavailable:那会把排空证明打成 Hold,Hold 不会自己清零。
func TestBeginDryRunUnreachableIsRetryLater(t *testing.T) {
	trades, steps, fences, in := newFlowBegin()
	fences.setDryRun(rolefence.PrepareRetry)
	_, err := trades.Begin(context.Background(), in)
	require.ErrorIs(t, err, accountdirectory.ErrRetryLater)
	require.Empty(t, steps.writes())
	require.Zero(t, fences.preparedCount())
}

// 试探之前的读失败同样确定没写。没有这名角色是确定的结论。
func TestBeginProbeReadFailure(t *testing.T) {
	for _, step := range []string{"readListing", "readFence"} {
		t.Run(step, func(t *testing.T) {
			trades, steps, fences, in := newFlowBegin()
			steps.fail[step] = []error{errFlow}
			_, err := trades.Begin(context.Background(), in)
			require.ErrorIs(t, err, accountdirectory.ErrRetryLater)
			require.ErrorIs(t, err, errFlow)
			require.Empty(t, steps.writes())
			require.Zero(t, fences.probedCount())
		})
	}
	trades, _, _, in := newFlowBegin()
	in.PlayerId = 999
	_, err := trades.Begin(context.Background(), in)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	require.NotErrorIs(t, err, accountdirectory.ErrRetryLater)
}

// 只数真相是这个账号的角色。陈旧的 ref 不算,读不到要回错误,不能当成有名额。
func TestRoleSlotFullCountsLiveRoles(t *testing.T) {
	ctx := context.Background()
	trades, steps, _, _ := newFlowTransfer()
	requireFull := func(want bool) {
		t.Helper()
		full, err := trades.RoleSlotFull(ctx, "buyer")
		require.NoError(t, err)
		require.Equal(t, want, full)
	}
	requireFull(false)
	steps.setOwner(21, "buyer", 1, 0)
	steps.putRef("buyer", roleRef{PlayerID: 21, State: refStateActive})
	requireFull(false)
	steps.setOwner(22, "someone-else", 1, 0)
	steps.putRef("buyer", roleRef{PlayerID: 22, State: refStateActive})
	requireFull(false)
	steps.setOwner(22, "buyer", 1, 0)
	requireFull(true)

	steps.fail["readAccount"] = []error{errFlow}
	_, err := trades.RoleSlotFull(ctx, "buyer")
	require.ErrorIs(t, err, errFlow)
	_, err = trades.RoleSlotFull(ctx, "")
	require.ErrorIs(t, err, accountdirectory.ErrInvalid)
}

// 提交点之后删卖家 ref 失败无害:调用方拿到成功,读列表按真相过滤。同 tx_id 重放读收据,补做第 3 步。
func TestTransferFailureAfterCommitThenReplay(t *testing.T) {
	for _, step := range []string{"readRef", "dropRef"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			trades, steps, fences, in := newFlowTransfer()
			steps.fail[step] = []error{errFlow}
			out, err := trades.Transfer(ctx, in)
			require.NoError(t, err, "提交点之后的失败不能让调用方以为没成")
			require.Equal(t, "buyer", steps.owners[11].Account)

			buyer, err := trades.directory.Read(ctx, "buyer", false)
			require.NoError(t, err)
			require.Len(t, buyer.Players, 1, "PENDING ref 也算:真相已经是买家")
			seller, err := trades.directory.Read(ctx, "seller", false)
			require.NoError(t, err)
			require.Empty(t, seller.Players, "卖家的 ref 没删掉也看不到")

			replayed, err := trades.Transfer(ctx, in)
			require.NoError(t, err)
			require.True(t, proto.Equal(out, replayed), "重放回原收据")
			requireTransferFinished(t, steps)
			require.Equal(t, 1, steps.count("commitTransfer"), "重放不再走提交点")
			require.NotZero(t, fences.pushedCount())
		})
	}
}

func TestTransferRejectedBeforeWritingRef(t *testing.T) {
	for name, setup := range map[string]func(*flowSteps, *dspb.TransferAccountRoleRequest){
		"收据还没到FROZEN": func(f *flowSteps, _ *dspb.TransferAccountRoleRequest) {
			f.listings[101].State = uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING)
		},
		"已取消": func(f *flowSteps, _ *dspb.TransferAccountRoleRequest) {
			f.listings[101].State = uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED)
		},
		"已转走_换tx_id不能重复过户": func(f *flowSteps, _ *dspb.TransferAccountRoleRequest) {
			f.listings[101].State = uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED)
		},
		"卖家不符": func(f *flowSteps, in *dspb.TransferAccountRoleRequest) { in.SellerAccount = "other" },
		"修订不符": func(f *flowSteps, in *dspb.TransferAccountRoleRequest) { in.ExpectedOwnerRevision = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			trades, steps, _, in := newFlowTransfer()
			setup(steps, in)
			_, err := trades.Transfer(context.Background(), in)
			require.ErrorIs(t, err, accountdirectory.ErrConflict)
			require.Empty(t, steps.writes(), "注定被拒的请求不该占买家的名额")
		})
	}
}

// 买家满位回 ErrFull,不写收据,归属不动。trade 拿它去 Cancel 走退款;这里核目录一侧同一 tx_id 重放还是对的。
func TestTransferBuyerFullThenRetry(t *testing.T) {
	ctx := context.Background()
	trades, steps, _, in := newFlowTransfer()
	for _, player := range []uint64{21, 22} {
		steps.setOwner(player, "buyer", 1, 0)
		steps.putRef("buyer", roleRef{PlayerID: player, State: refStateActive})
	}
	_, err := trades.Transfer(ctx, in)
	require.ErrorIs(t, err, accountdirectory.ErrFull)
	require.Empty(t, steps.transfers)
	require.Equal(t, "seller", steps.owners[11].Account)
	require.NotContains(t, steps.refs["buyer"], uint64(11))

	// 买家腾出位子。
	steps.setOwner(21, "", 1, 0)
	_, err = trades.Transfer(ctx, in)
	require.NoError(t, err)
	requireTransferFinished(t, steps)
	require.NotContains(t, steps.refs["buyer"], uint64(21))
}

// 第 2 步被拒:已能证明这笔过户不会再提交时删掉第 1 步的 ref。
func TestTransferCommitRejectedDropsRefWhenProvable(t *testing.T) {
	trades, steps, _, in := newFlowTransfer()
	steps.before["commitTransfer"] = func() {
		steps.listings[101].State = uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED)
		steps.setOwner(11, "seller", 1, 0)
	}
	_, err := trades.Transfer(context.Background(), in)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.NotContains(t, steps.refs["buyer"], uint64(11))
	require.Empty(t, steps.transfers)
}

// listing 还开着的话证明不了:同 tx_id 的并发重放可能正要提交,留给后台。
func TestTransferCommitRejectedKeepsRefWhenNotProvable(t *testing.T) {
	for name, want := range map[string]error{"冲突": accountdirectory.ErrConflict, "缺行": accountdirectory.ErrMissing} {
		t.Run(name, func(t *testing.T) {
			trades, steps, _, in := newFlowTransfer()
			steps.fail["commitTransfer"] = []error{want}
			_, err := trades.Transfer(context.Background(), in)
			require.ErrorIs(t, err, want)
			require.Equal(t, refStatePending, steps.refs["buyer"][11].State)
		})
	}
}

// 第 1 步之后 ref 上写着这笔过户:ACTIVE 的旧 ref 也被改写并换 nonce,挡住在途的清理。
func TestTransferReservesOverExistingRef(t *testing.T) {
	trades, steps, _, in := newFlowTransfer()
	// 买家以前有过这名角色:ref 还没清掉。
	steps.putRef("buyer", roleRef{PlayerID: 11, State: refStateActive})
	before := steps.refs["buyer"][11].Nonce
	steps.fail["commitTransfer"] = []error{errFlow}
	_, err := trades.Transfer(context.Background(), in)
	require.ErrorIs(t, err, errFlow)
	ref := steps.refs["buyer"][11]
	require.Equal(t, refStatePending, ref.State)
	require.EqualValues(t, 1001, ref.TxID)
	require.NotEqual(t, before, ref.Nonce)
}

// 后台的 dropRef 不持目录行锁,可能抢在 reserveRef 读完 ref 之后删掉它。写是 upsert,ref 还在,过户提交之后买家有这条 ref。
// SQL 上的同一个交错见 TestTiDBWritePendingRefSurvivesConcurrentDrop。
func TestTransferReserveSurvivesConcurrentDrop(t *testing.T) {
	trades, steps, _, in := newFlowTransfer()
	steps.putRef("buyer", roleRef{PlayerID: 11, State: refStateActive})
	steps.before["reserveRef.write"] = func() { delete(steps.refs["buyer"], 11) }
	steps.fail["commitTransfer"] = []error{errFlow}
	_, err := trades.Transfer(context.Background(), in)
	require.ErrorIs(t, err, errFlow)
	ref := steps.refs["buyer"][11]
	require.Equal(t, refStatePending, ref.State)
	require.EqualValues(t, 1001, ref.TxID)
}

// 重放时真相是买家,但 ref 已被别的过户改写:不能覆盖,否则那条 ref 变成 ACTIVE,后台按「ACTIVE 且真相不是这个账号」把它删掉。
func TestTransferReplayDoesNotOverwriteAnotherTransfer(t *testing.T) {
	ctx := context.Background()
	trades, steps, _, in := newFlowTransfer()
	_, err := trades.Transfer(ctx, in)
	require.NoError(t, err)
	steps.putRef("buyer", roleRef{PlayerID: 11, State: refStatePending, TxID: 1002, ListingID: 102})
	other := steps.refs["buyer"][11]
	steps.calls = nil

	_, err = trades.Transfer(ctx, in)
	require.NoError(t, err)
	require.Equal(t, 1, steps.count("activateRef"), "重放走到了这一步,没改到不算错")
	require.Equal(t, other, steps.refs["buyer"][11], "ref 属于别的过户,不动")
}

// 同 tx_id、别的内容:tx_id 被用过了,拒绝,不碰任何东西。
func TestTransferReplayWithDifferentRequestRejected(t *testing.T) {
	ctx := context.Background()
	trades, steps, _, in := newFlowTransfer()
	_, err := trades.Transfer(ctx, in)
	require.NoError(t, err)
	steps.calls = nil
	changed := proto.Clone(in).(*dspb.TransferAccountRoleRequest)
	changed.BuyerAccount = "other-buyer"
	_, err = trades.Transfer(ctx, changed)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.Empty(t, steps.writes())
}

// 这笔过户之后角色又卖了:重放回原收据,但不能把买家的 ref 写回 ACTIVE。
func TestTransferReplayAfterResaleDoesNotRestoreRef(t *testing.T) {
	ctx := context.Background()
	trades, steps, _, in := newFlowTransfer()
	out, err := trades.Transfer(ctx, in)
	require.NoError(t, err)
	steps.setOwner(11, "third", 3, 0)
	delete(steps.refs["buyer"], 11)
	steps.calls = nil
	replayed, err := trades.Transfer(ctx, in)
	require.NoError(t, err)
	require.True(t, proto.Equal(out, replayed))
	require.Zero(t, steps.count("activateRef"))
	require.NotContains(t, steps.refs["buyer"], uint64(11))
}

// 并发的同一笔过户在两次读之间提交:listing 已是 TRANSFERRED,这不是冲突,回原收据。
func TestTransferConcurrentCommitBetweenReads(t *testing.T) {
	ctx := context.Background()
	trades, steps, _, in := newFlowTransfer()
	digest, err := roleTradeDigest(in)
	require.NoError(t, err)
	steps.before["readListing"] = func() {
		_, err := steps.commitTransfer(ctx, in, digest)
		require.NoError(t, err)
	}
	out, err := trades.Transfer(ctx, in)
	require.NoError(t, err)
	require.EqualValues(t, 2, out.BuyerOwnerRevision)
	requireTransferFinished(t, steps)
}

func TestTransferRequiresCoordinatorAndValidInput(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(2)
	in := &dspb.TransferAccountRoleRequest{TxId: 1001, ListingId: 101, PlayerId: 11, SellerAccount: "seller", BuyerAccount: "buyer", ExpectedOwnerRevision: 1}
	_, err := NewAccountRoleTradeStore(s, nil).Transfer(ctx, in)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable, "没有协调者不能跳过参与者")
	require.Empty(t, steps.calls)

	s.ConfigureFenceCoordinator(&fakeFences{})
	trades := NewAccountRoleTradeStore(s, nil)
	for name, mutate := range map[string]func(*dspb.TransferAccountRoleRequest){
		"买卖同账号":   func(r *dspb.TransferAccountRoleRequest) { r.BuyerAccount = r.SellerAccount },
		"没有tx":    func(r *dspb.TransferAccountRoleRequest) { r.TxId = 0 },
		"删角令牌当挂单": func(r *dspb.TransferAccountRoleRequest) { r.ListingId = rolefence.RemovalToken(r.PlayerId) },
		"修订号到头":   func(r *dspb.TransferAccountRoleRequest) { r.ExpectedOwnerRevision = math.MaxUint64 },
	} {
		changed := proto.Clone(in).(*dspb.TransferAccountRoleRequest)
		mutate(changed)
		_, err := trades.Transfer(ctx, changed)
		require.ErrorIs(t, err, accountdirectory.ErrInvalid, name)
	}
	require.Empty(t, steps.calls)
}

// ── 判死 ────────────────────────────────────────────────────────────────────

func TestDecideDeadHolders(t *testing.T) {
	plan := &deadHolderPlan{pending: []string{"dead-a", "dead-b"}, maxEpoch: 17}
	for _, tc := range []struct {
		name    string
		seen    lockedPlayerStoreOwner
		plan    *deadHolderPlan
		want    deadHolderVerdict
		wantErr error
	}{
		{name: "待判持有者还是现任_等ACK", seen: lockedPlayerStoreOwner{ownerEpoch: 30, holder: []byte("dead-b")}, plan: plan, want: deadHolderVerdict{alive: true}},
		{name: "从没认领过_抬到登记的代次之上", seen: lockedPlayerStoreOwner{ownerEpoch: 1}, plan: plan, want: deadHolderVerdict{raise: true, epoch: 18}},
		{name: "代次相等也要抬_认领带这个代次还过得去", seen: lockedPlayerStoreOwner{ownerEpoch: 17}, plan: plan, want: deadHolderVerdict{raise: true, epoch: 18}},
		{name: "上一轮抬过之后崩了_已经挡住_不再抬", seen: lockedPlayerStoreOwner{ownerEpoch: 18}, plan: plan, want: deadHolderVerdict{}},
		{name: "写权行有别的持有者_代次更高_在它之上抬", seen: lockedPlayerStoreOwner{ownerEpoch: 30, holder: []byte("drained")}, plan: plan, want: deadHolderVerdict{raise: true, epoch: 31}},
		{name: "代次到头_拒绝", seen: lockedPlayerStoreOwner{ownerEpoch: math.MaxUint64, holder: []byte("drained")}, plan: plan, wantErr: accountdirectory.ErrConflict},
		{name: "登记的代次到头_拒绝", seen: lockedPlayerStoreOwner{ownerEpoch: 1}, plan: &deadHolderPlan{pending: []string{"dead-a"}, maxEpoch: math.MaxUint64}, wantErr: accountdirectory.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decideDeadHolders(tc.seen, tc.plan)
			require.ErrorIs(t, err, tc.wantErr)
			require.Equal(t, tc.want, got)
		})
	}
}

func newFlowDeadHolder() (*AccountDirectoryStore, *flowSteps) {
	s, steps := newFlowStore(2)
	steps.routed(11)
	steps.storeOwners[11] = lockedPlayerStoreOwner{ownerEpoch: 1}
	steps.plan = &deadHolderPlan{listing: 101, player: 11, version: 3, pending: []string{"dead"}, maxEpoch: 17}
	return s, steps
}

var deadHolderSteps = []string{"readStoreOwner", "readDeadHolderPlan", "raiseStoreEpoch", "markHoldersDrained"}

// 顺序不能反:先读写权行再读目录,比较并写的依据才成立;先抬代次再记 holder,持有者才真的写不了存档。
func TestFenceDeadHoldersOrder(t *testing.T) {
	s, steps := newFlowDeadHolder()
	require.NoError(t, s.fenceDeadTradeHolders(context.Background(), 101, 11))
	require.Equal(t, deadHolderSteps, steps.only(deadHolderSteps...))
	require.Equal(t, lockedPlayerStoreOwner{ownerEpoch: 18}, steps.storeOwners[11])
	require.Len(t, steps.drained, 1)
}

// 抬完代次之后崩溃:重跑不再抬,接着记 holder。代次不会一轮一轮往上涨。
func TestFenceDeadHoldersCrashAfterRaiseThenRerun(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowDeadHolder()
	steps.fail["markHoldersDrained"] = []error{errFlow}
	require.ErrorIs(t, s.fenceDeadTradeHolders(ctx, 101, 11), errFlow)
	require.EqualValues(t, 18, steps.storeOwners[11].ownerEpoch)
	require.Empty(t, steps.drained, "没记成排空")

	steps.calls = nil
	require.NoError(t, s.fenceDeadTradeHolders(ctx, 101, 11))
	require.Zero(t, steps.count("raiseStoreEpoch"), "已经挡住就不再抬")
	require.EqualValues(t, 18, steps.storeOwners[11].ownerEpoch)
	require.Len(t, steps.drained, 1)
}

// 抬代次之前失败:什么都没记。不存在「记了排空但没抬代次」的状态。
func TestFenceDeadHoldersFailureBeforeRaiseRecordsNothing(t *testing.T) {
	for _, step := range []string{"readStoreOwner", "readDeadHolderPlan", "raiseStoreEpoch"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			s, steps := newFlowDeadHolder()
			steps.fail[step] = []error{errFlow}
			require.ErrorIs(t, s.fenceDeadTradeHolders(ctx, 101, 11), errFlow)
			require.Empty(t, steps.drained)
			require.EqualValues(t, 1, steps.storeOwners[11].ownerEpoch)

			require.NoError(t, s.fenceDeadTradeHolders(ctx, 101, 11))
			require.Len(t, steps.drained, 1)
			require.EqualValues(t, 18, steps.storeOwners[11].ownerEpoch)
		})
	}
}

// 写权行变了(解锁之后有新的持有者认领了):这一轮的结论不算数。挂住的判死不能踢掉新持有者。
func TestFenceDeadHoldersLostCompareDoesNothing(t *testing.T) {
	s, steps := newFlowDeadHolder()
	claimed := lockedPlayerStoreOwner{ownerEpoch: 40, holder: []byte("new-holder")}
	steps.before["raiseStoreEpoch"] = func() { steps.storeOwners[11] = claimed }
	require.NoError(t, s.fenceDeadTradeHolders(context.Background(), 101, 11))
	require.Equal(t, claimed, steps.storeOwners[11])
	require.Empty(t, steps.drained)
}

// 判死读到 (E, X) 之后挂住,令牌清除,玩家同节点重登:(E, X) 写回原值,只有 claim_seq 变。
// 三列一起比才比得出来,否则抬代次踢掉在线玩家,进度丢失。真库上的同一个比较见 TestTiDBRaiseStoreEpochComparesClaimSeq。
func TestFenceDeadHoldersSameHolderReclaimDoesNothing(t *testing.T) {
	s, steps := newFlowDeadHolder()
	seen := lockedPlayerStoreOwner{ownerEpoch: 30, holder: []byte("online"), claimSeq: 4}
	steps.storeOwners[11] = seen
	reclaimed := lockedPlayerStoreOwner{ownerEpoch: 30, holder: []byte("online"), claimSeq: 5}
	steps.before["raiseStoreEpoch"] = func() { steps.storeOwners[11] = reclaimed }
	require.NoError(t, s.fenceDeadTradeHolders(context.Background(), 101, 11))
	require.Equal(t, reclaimed, steps.storeOwners[11], "写权行不动")
	require.Empty(t, steps.drained, "这一轮的结论不算数")
}

// 没有人重新认领时照常抬,claim_seq 不变:判死不是认领。
func TestFenceDeadHoldersRaiseKeepsClaimSeq(t *testing.T) {
	s, steps := newFlowDeadHolder()
	steps.storeOwners[11] = lockedPlayerStoreOwner{ownerEpoch: 30, holder: []byte("drained"), claimSeq: 4}
	require.NoError(t, s.fenceDeadTradeHolders(context.Background(), 101, 11))
	require.Equal(t, lockedPlayerStoreOwner{ownerEpoch: 31, claimSeq: 4}, steps.storeOwners[11])
	require.Len(t, steps.drained, 1)
}

// 抬过代次之后记 holder 失败:写过东西了,不能回确定性的拒绝。
func TestFenceDeadHoldersMarkFailureAfterRaiseDropsSentinels(t *testing.T) {
	s, steps := newFlowDeadHolder()
	steps.fail["markHoldersDrained"] = []error{accountdirectory.ErrMissing}
	err := s.fenceDeadTradeHolders(context.Background(), 101, 11)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	require.NotErrorIs(t, err, accountdirectory.ErrMissing)
	require.EqualValues(t, 18, steps.storeOwners[11].ownerEpoch)
}

func TestFenceDeadHoldersDoesNothing(t *testing.T) {
	for name, setup := range map[string]func(*flowSteps){
		"现任可能活着":  func(f *flowSteps) { f.storeOwners[11] = lockedPlayerStoreOwner{ownerEpoch: 40, holder: []byte("dead")} },
		"没有写权行":   func(f *flowSteps) { delete(f.storeOwners, 11) },
		"收据不是开着的": func(f *flowSteps) { f.plan = nil },
		"没有待判持有者": func(f *flowSteps) { f.plan.pending = nil },
		"路由查不到":   func(f *flowSteps) { delete(f.routes, 11) },
		"路由没有落点":  func(f *flowSteps) { f.routes[11] = RouteRow{PlayerID: 11, HomeZone: 1, Rev: 1} },
		"落点不在白名单": func(f *flowSteps) { delete(f.allowed, flowStorage) },
	} {
		t.Run(name, func(t *testing.T) {
			s, steps := newFlowDeadHolder()
			setup(steps)
			require.NoError(t, s.fenceDeadTradeHolders(context.Background(), 101, 11))
			require.Empty(t, steps.writes())
		})
	}
}

// 规格 §3.5:没有未排空的持有者就不碰路由和存档库。PREPARING 没有 holder,存档库故障时取消照样能完成。
func TestFenceDeadHoldersSkipsPlayerStoreWithoutPendingHolders(t *testing.T) {
	s, steps := newFlowDeadHolder()
	steps.plan = nil
	steps.fail["readRoute"] = []error{errFlow}
	steps.fail["readStoreOwner"] = []error{errFlow}
	require.NoError(t, s.fenceDeadTradeHolders(context.Background(), 101, 11))
	require.Empty(t, steps.only("readRoute", "readStoreOwner", "readDeadHolderPlan", "raiseStoreEpoch"))
}

// 普通读 holder 失败不能当成没有持有者:照常回错误,调用方重试。
func TestFenceDeadHoldersPendingReadFailureIsAnError(t *testing.T) {
	s, steps := newFlowDeadHolder()
	steps.fail["hasPendingHolders"] = []error{errFlow}
	require.ErrorIs(t, s.fenceDeadTradeHolders(context.Background(), 101, 11), errFlow)
	require.Empty(t, steps.writes())
}

func TestFenceDeadHoldersRouteReadFailureIsAnError(t *testing.T) {
	s, steps := newFlowDeadHolder()
	steps.fail["readRoute"] = []error{errFlow}
	require.ErrorIs(t, s.fenceDeadTradeHolders(context.Background(), 101, 11), errFlow)
	require.Empty(t, steps.writes())
}

// ── 选存档库 ────────────────────────────────────────────────────────────────

func TestPlayerStorage(t *testing.T) {
	ctx := context.Background()
	s, steps := newFlowStore(2)
	steps.routed(11)
	got, err := s.playerStorage(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, flowStorage, got)

	_, err = s.playerStorage(ctx, 12)
	require.ErrorIs(t, err, accountdirectory.ErrMissing, "没有路由")
	steps.routes[12] = RouteRow{PlayerID: 12, HomeZone: 1, Rev: 1}
	_, err = s.playerStorage(ctx, 12)
	require.ErrorIs(t, err, accountdirectory.ErrMissing, "没有落点不回落 home_zone")
	steps.routes[13] = RouteRow{PlayerID: 13, HomeZone: 1, StorageID: flowStorage + 1, PlacementVersion: 1, Rev: 1}
	_, err = s.playerStorage(ctx, 13)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable, "不在白名单不读")
	steps.fail["readRoute"] = []error{errFlow}
	_, err = s.playerStorage(ctx, 11)
	require.ErrorIs(t, err, errFlow, "读失败不能当成没有")
}
