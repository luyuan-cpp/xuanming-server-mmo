package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	base "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

type roleFreezeVerifierFunc func(context.Context, *sql.Tx, *dbpb.AccountRoleTrade) (*dspb.AccountRoleFreezeEvidence, error)

func newTestAccountRoleTradeStore(directory *AccountDirectoryStore, verifier RoleFreezeVerifier) *AccountRoleTradeStore {
	s := NewAccountRoleTradeStore(directory, verifier)
	// 原状态机用例不证明离帮；显式关业务策略，正式并发guard用例另装真实表。
	s.requireGuildExit = false
	return s
}

func (f roleFreezeVerifierFunc) VerifyFrozen(ctx context.Context, tx *sql.Tx, r *dbpb.AccountRoleTrade) (*dspb.AccountRoleFreezeEvidence, error) {
	return f(ctx, tx, r)
}

// 这是显式测试验证器，仅验证状态机装配与 SQL 原子范围；不冒充原生卸载／旧入场排空证据。
func testRoleFreezeVerifier() RoleFreezeVerifier {
	return roleFreezeVerifierFunc(func(ctx context.Context, tx *sql.Tx, r *dbpb.AccountRoleTrade) (*dspb.AccountRoleFreezeEvidence, error) {
		var listing uint64
		if err := tx.QueryRowContext(ctx, "SELECT trade_listing_id FROM account_role WHERE player_id=?", r.PlayerId).Scan(&listing); err != nil {
			return nil, err
		}
		if listing != r.ListingId {
			return nil, accountdirectory.ErrConflict
		}
		return &dspb.AccountRoleFreezeEvidence{PlayerId: r.PlayerId, ListingId: r.ListingId, OwnerRevision: r.SellerOwnerRevision, OwnerEpoch: 17, SaveSeq: 3, PersistedDigest: make([]byte, 32), Kind: dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_SCENE}, nil
	})
}
func beginAndConfirmRole(t *testing.T, s *AccountRoleTradeStore, ctx context.Context, listing, player uint64, seller string, rev uint64) {
	t.Helper()
	_, err := s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: listing, PlayerId: player, SellerAccount: seller, ExpectedOwnerRevision: rev})
	require.NoError(t, err)
	out, err := s.Confirm(ctx, &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: listing, PlayerId: player, SellerAccount: seller, ExpectedOwnerRevision: rev})
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN, out.State)
}

func TestTiDBRoleTradeFreezeCancelDoesNotRefreezeOrClearNewListing(t *testing.T) {
	directory, db := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(directory, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	s := newTestAccountRoleTradeStore(directory, nil)
	in := &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	type result struct {
		out *dspb.AccountRoleTradeReceipt
		err error
	}
	results := make(chan result, 8)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			out, err := s.Begin(ctx, proto.Clone(in).(*dspb.BeginAccountRoleTradeRequest))
			results <- result{out, err}
		}()
	}
	workers.Wait()
	close(results)
	count := 0
	for r := range results {
		require.NoError(t, r.err)
		require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING, r.out.State)
		count++
	}
	require.Equal(t, 8, count)
	var receipts int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_trade").Scan(&receipts))
	require.Equal(t, 1, receipts)
	confirmation := &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	_, err = s.Confirm(ctx, confirmation)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable, "生产未装配 verifier 不能生成 FROZEN")
	s.verifier = roleFreezeVerifierFunc(func(context.Context, *sql.Tx, *dbpb.AccountRoleTrade) (*dspb.AccountRoleFreezeEvidence, error) {
		return nil, errors.New("SQL proof unavailable")
	})
	_, err = s.Confirm(ctx, confirmation)
	require.ErrorContains(t, err, "proof unavailable")
	var state uint32
	require.NoError(t, db.QueryRowContext(ctx, "SELECT state FROM account_role_trade WHERE listing_id=101").Scan(&state))
	require.EqualValues(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING, state)
	s.verifier = testRoleFreezeVerifier()
	_, err = s.Confirm(ctx, confirmation)
	require.NoError(t, err)
	_, err = directory.Repair(ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11, ClassId: 3}, 1)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.ErrorIs(t, directory.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrConflict)
	for _, restore := range []bool{false, true} {
		_, err = directory.ImportRole(ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11}, restore, roleImportTestID(1))
		require.ErrorIs(t, err, accountdirectory.ErrConflict)
	}
	wrong := proto.Clone(in).(*dspb.BeginAccountRoleTradeRequest)
	wrong.ExpectedOwnerRevision = 2
	_, err = s.Begin(ctx, wrong)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	stop := &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	out, err := s.Cancel(ctx, stop)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, out.State)
	out, err = s.Begin(ctx, in)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, out.State)
	owned, err := directory.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
	second := proto.Clone(in).(*dspb.BeginAccountRoleTradeRequest)
	second.ListingId = 102
	_, err = s.Begin(ctx, second)
	require.NoError(t, err)
	_, err = s.Cancel(ctx, stop)
	require.NoError(t, err, "旧取消请求只重放原收据")
	owned, err = directory.Lookup(ctx, 11)
	require.NoError(t, err)
	require.EqualValues(t, 102, owned.TradeListingId, "旧取消不得清掉新 listing 锁")
	_, err = createInitialRoleForTest(directory, ctx, "other-seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 22})
	require.NoError(t, err)
	_, err = s.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 102, PlayerId: 22, SellerAccount: "other-seller", ExpectedOwnerRevision: 1})
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "不同账号命中同listing主键也只能拒绝重放")
	other, err := directory.Lookup(ctx, 22)
	require.NoError(t, err)
	require.Zero(t, other.TradeListingId)
}

func TestTiDBRoleTransferCapConcurrentReplayAndOriginalReceipt(t *testing.T) {
	directory, db := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11, ClassId: 3, Name: "原角色"}
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 91, protowire.VarintType), 77)
	role.ProtoReflect().SetUnknown(unknown)
	_, err := createInitialRoleForTest(directory, ctx, "seller", role)
	require.NoError(t, err)
	for _, id := range []uint64{21, 22} {
		_, err = createInitialRoleForTest(directory, ctx, "buyer", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: id})
		require.NoError(t, err)
	}
	s := newTestAccountRoleTradeStore(directory, testRoleFreezeVerifier())
	beginAndConfirmRole(t, s, ctx, 101, 11, "seller", 1)
	in := &dspb.TransferAccountRoleRequest{TxId: 1001, ListingId: 101, PlayerId: 11, SellerAccount: "seller", BuyerAccount: "buyer", ExpectedOwnerRevision: 1}
	_, err = s.Transfer(ctx, in)
	require.ErrorIs(t, err, accountdirectory.ErrFull)
	var receipts int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_transfer_receipt").Scan(&receipts))
	require.Zero(t, receipts)
	owned, err := directory.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, "seller", owned.Account)
	require.EqualValues(t, 101, owned.TradeListingId)
	require.NoError(t, directory.Remove(ctx, "buyer", []*dspb.AccountRoleVersion{{PlayerId: 21, OwnerRevision: 1}}))
	type result struct {
		out *dspb.AccountRoleTransferReceipt
		err error
	}
	results := make(chan result, 8)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			out, err := s.Transfer(ctx, proto.Clone(in).(*dspb.TransferAccountRoleRequest))
			results <- result{out, err}
		}()
	}
	workers.Wait()
	close(results)
	var original *dspb.AccountRoleTransferReceipt
	count := 0
	for r := range results {
		require.NoError(t, r.err)
		if original == nil {
			original = r.out
		}
		require.True(t, proto.Equal(original, r.out))
		count++
	}
	require.Equal(t, 8, count)
	require.EqualValues(t, 2, original.BuyerOwnerRevision)
	require.Equal(t, unknown, original.Role.ProtoReflect().GetUnknown())
	for _, mutate := range []func(*dspb.TransferAccountRoleRequest){func(r *dspb.TransferAccountRoleRequest) { r.ListingId++ }, func(r *dspb.TransferAccountRoleRequest) { r.PlayerId++ }, func(r *dspb.TransferAccountRoleRequest) { r.SellerAccount = "other-seller" }, func(r *dspb.TransferAccountRoleRequest) { r.BuyerAccount = "other-buyer" }, func(r *dspb.TransferAccountRoleRequest) { r.ExpectedOwnerRevision++ }} {
		changed := proto.Clone(in).(*dspb.TransferAccountRoleRequest)
		mutate(changed)
		_, err = s.Transfer(ctx, changed)
		require.ErrorIs(t, err, accountdirectory.ErrConflict)
	}
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_transfer_receipt").Scan(&receipts))
	require.Equal(t, 1, receipts)
	newTx := proto.Clone(in).(*dspb.TransferAccountRoleRequest)
	newTx.TxId = 1003
	_, err = s.Transfer(ctx, newTx)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "换tx_id不能重复过户已成交listing")
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_transfer_receipt").Scan(&receipts))
	require.Equal(t, 1, receipts, "被拒新tx不能留下成功收据")
	owned, err = directory.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, "buyer", owned.Account)
	require.EqualValues(t, 2, owned.OwnerRevision)
	require.Zero(t, owned.TradeListingId)
	oldSeller, err := directory.Read(ctx, "seller", false)
	require.NoError(t, err)
	require.Empty(t, oldSeller.Players)
	_, err = directory.Repair(ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11, Gender: 1}, 1)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	require.ErrorIs(t, directory.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrConflict)
	beginAndConfirmRole(t, s, ctx, 102, 11, "buyer", 2)
	_, err = s.Transfer(ctx, &dspb.TransferAccountRoleRequest{TxId: 1002, ListingId: 102, PlayerId: 11, SellerAccount: "buyer", BuyerAccount: "third", ExpectedOwnerRevision: 2})
	require.NoError(t, err)
	replayed, err := s.Transfer(ctx, in)
	require.NoError(t, err)
	require.True(t, proto.Equal(original, replayed), "之后再次过户，旧 tx_id 仍返回原修订及原角色")
	owned, err = directory.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, "third", owned.Account)
	require.EqualValues(t, 3, owned.OwnerRevision)
}

func TestTiDBRoleTransferOppositeAccountsUseOneByteLockOrder(t *testing.T) {
	directory, _ := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(directory, ctx, "a", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	_, err = createInitialRoleForTest(directory, ctx, "B", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 22})
	require.NoError(t, err)
	s := newTestAccountRoleTradeStore(directory, testRoleFreezeVerifier())
	beginAndConfirmRole(t, s, ctx, 101, 11, "a", 1)
	beginAndConfirmRole(t, s, ctx, 102, 22, "B", 1)
	requests := []*dspb.TransferAccountRoleRequest{{TxId: 1001, ListingId: 101, PlayerId: 11, SellerAccount: "a", BuyerAccount: "B", ExpectedOwnerRevision: 1}, {TxId: 1002, ListingId: 102, PlayerId: 22, SellerAccount: "B", BuyerAccount: "a", ExpectedOwnerRevision: 1}}
	results := make(chan error, 2)
	for _, in := range requests {
		go func(r *dspb.TransferAccountRoleRequest) { _, err := s.Transfer(ctx, r); results <- err }(in)
	}
	for i := 0; i < 2; i++ {
		require.NoError(t, <-results)
	}
	first, err := directory.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, "B", first.Account)
	require.EqualValues(t, 2, first.OwnerRevision)
	second, err := directory.Lookup(ctx, 22)
	require.NoError(t, err)
	require.Equal(t, "a", second.Account)
	require.EqualValues(t, 2, second.OwnerRevision)
}
