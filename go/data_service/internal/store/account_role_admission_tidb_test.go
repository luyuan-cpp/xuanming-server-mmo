package store

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/generated/persistschema"
	"shared/placement"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const admissionTestInstance = "11111111-2222-3333-4444-555555555555"

// holdPlayerStoreForTest 让写权行显示 admissionTestInstance 是现任:现任不会被判死,要等真实的 ACK。
func holdPlayerStoreForTest(t *testing.T, ctx context.Context, d *AccountDirectoryStore, player uint64) {
	t.Helper()
	schema, err := d.playerStoreSchema(ctx, d.db, player)
	require.NoError(t, err)
	_, err = d.db.ExecContext(ctx, "UPDATE `"+schema+"`.`player_store` SET holder_instance=?,released=0 WHERE player_id=? AND kind=?", []byte(admissionTestInstance), player, playerStoreOwnerKind)
	require.NoError(t, err)
}

func roleImportTestID(n byte) []byte { out := make([]byte, 32); out[31] = n; return out }

func admissionTestRequest(player, generation, revision uint64) *dspb.RegisterRoleAdmissionRequest {
	return &dspb.RegisterRoleAdmissionRequest{Identity: &base.RoleAdmissionIdentity{PlayerId: player, SceneInstanceId: admissionTestInstance, Generation: generation, OwnerRevision: revision, OwnerEpoch: 17, SceneNodeId: "11", SceneId: 111, ZoneId: 1, SessionId: 7, LoginGeneration: 999}}
}
func TestTiDBRoleAdmissionFixedHolderCancelAndLateGenerations(t *testing.T) {
	d, db := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	a := NewAccountRoleAdmissionStore(d)
	in := admissionTestRequest(11, 1, 1)
	type result struct {
		out *dspb.RegisterRoleAdmissionResponse
		err error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := a.Register(ctx, proto.Clone(in).(*dspb.RegisterRoleAdmissionRequest))
			results <- result{out, err}
		}()
	}
	wg.Wait()
	close(results)
	count := 0
	for r := range results {
		require.NoError(t, r.err)
		require.True(t, proto.Equal(in.Identity, r.out.Identity))
		require.Zero(t, r.out.ClosedThrough)
		count++
	}
	require.Equal(t, 8, count)
	var rows int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_admission").Scan(&rows))
	require.Equal(t, 1, rows)
	changed := proto.Clone(in).(*dspb.RegisterRoleAdmissionRequest)
	changed.Identity.SessionId++
	_, err = a.Register(ctx, changed)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	in.Identity.Generation = 7
	_, err = a.Register(ctx, in)
	require.NoError(t, err)
	holdPlayerStoreForTest(t, ctx, d, 11)
	trades := newTestAccountRoleTradeStore(d, testRoleFreezeVerifier())
	begin := &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	first, err := trades.Begin(ctx, begin)
	require.NoError(t, err)
	require.Len(t, first.Holders, 1)
	require.Equal(t, uint64(7), first.Holders[0].Seal.ThroughGeneration)
	_, err = a.Register(ctx, admissionTestRequest(11, 8, 1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	closeReq := &dspb.CloseRoleAdmissionsRequest{PlayerId: 11, SceneInstanceId: admissionTestInstance, OwnerRevision: 1, ThroughGeneration: 7, RequiredOwnerEpoch: 17, RequiredSaveSeq: 3}
	_, err = a.CloseAdmissions(ctx, closeReq)
	require.NoError(t, err)
	stop := &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	waiting, err := trades.Cancel(ctx, stop)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING, waiting.State)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, uint64(101), owned.TradeListingId, "Close 不能代替 listing 墓碑/排空 ACK")
	ack := &dspb.AckRoleTradeDrainRequest{Seal: proto.Clone(first.Holders[0].Seal).(*base.RoleTradeSeal), RequiredOwnerEpoch: 17, RequiredSaveSeq: 3}
	bad := proto.Clone(ack).(*dspb.AckRoleTradeDrainRequest)
	bad.Seal.ThroughGeneration++
	require.ErrorIs(t, a.AckDrain(ctx, bad), accountdirectory.ErrConflict)
	require.NoError(t, a.AckDrain(ctx, ack))
	require.NoError(t, a.AckDrain(ctx, ack))
	bad = proto.Clone(ack).(*dspb.AckRoleTradeDrainRequest)
	bad.RequiredSaveSeq++
	require.ErrorIs(t, a.AckDrain(ctx, bad), accountdirectory.ErrConflict)
	finished, err := trades.Cancel(ctx, stop)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, finished.State)
	require.True(t, finished.Holders[0].Drained)
	_, err = a.Register(ctx, in)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "取消后旧 generation 不能再激活")
	_, err = a.Register(ctx, admissionTestRequest(11, 8, 1))
	require.NoError(t, err)
	require.NoError(t, a.AckDrain(ctx, ack), "旧 listing ACK 只重放原件，不关闭新 generation")
	var highest, closed, epoch, seq uint64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT highest_generation,closed_through,required_owner_epoch,required_save_seq FROM account_role_admission WHERE player_id=11").Scan(&highest, &closed, &epoch, &seq))
	require.Equal(t, []uint64{8, 7, 17, 3}, []uint64{highest, closed, epoch, seq})
	old, err := trades.Begin(ctx, begin)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, old.State)
	owned, err = d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
	_, err = a.Register(ctx, admissionTestRequest(11, 9, 2))
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
}

// 真实 TiDB、独立随机 storage schema、正式 proto2mysql 建的 player_store；只删本例已创建的库。
func roleProofStorage(t *testing.T, global *sql.DB, player uint64, fixture *directorySQLFixture) (uint32, string, *sql.DB) {
	t.Helper()
	require.NotNil(t, fixture)
	id := fixture.ProofStorageID
	name, ok := placement.StoreDBName(id)
	require.True(t, ok)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	handle, err := fixture.SQL.Open(ctx, name, 4)
	require.NoError(t, err)
	// 与建角写的初始行相同。
	initial, err := initialPlayerStoreRows(&base.AccountSimplePlayer{PlayerId: player, ZoneId: 1})
	require.NoError(t, err)
	for _, row := range initial {
		_, err = handle.ExecContext(ctx, "INSERT INTO player_store (player_id,kind,owner_epoch,save_seq,schema_gen,holder_instance,released,payload) VALUES (?,?,1,1,?,?,?,?)",
			player, int32(row.kind), persistschema.Gen, []byte{}, row.released, row.payload)
		require.NoError(t, err)
	}
	_, err = global.Exec("INSERT INTO player_route(route_shard,player_id,home_zone,storage_id,placement_version,rev,updated_by,created_at_ms,updated_at_ms) VALUES (?,?,1,?,1,1,'role-proof-test',1,1) ON DUPLICATE KEY UPDATE home_zone=VALUES(home_zone),storage_id=VALUES(storage_id),placement_version=VALUES(placement_version),rev=rev+1", placement.RouteRowShard(player), player, id)
	require.NoError(t, err)
	return id, name, handle
}

func setProofFence(t *testing.T, db *sql.DB, player, epoch, seq uint64) {
	t.Helper()
	_, err := db.Exec("UPDATE player_store SET owner_epoch=?,save_seq=? WHERE player_id=? AND kind=0", epoch, seq, player)
	require.NoError(t, err)
}

func TestTiDBRoleAdmissionStrongSQLProofRejectsUnsyncedRowsAndKeepsReceipt(t *testing.T) {
	d, global, fixture := accountDirectoryTiDBWithWriter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	// 目录按正式初始事务建立后，本例再把路由切到专门的旧版/追盘证据夹具。
	id, _, rows := roleProofStorage(t, global, 11, fixture)
	v, err := NewSQLRoleFreezeVerifier([]uint32{id})
	require.NoError(t, err)
	trades, admissions := newTestAccountRoleTradeStore(d, v), NewAccountRoleAdmissionStore(d)
	_, err = admissions.Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err)
	holdPlayerStoreForTest(t, ctx, d, 11)
	begin := &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	frozen, err := trades.Begin(ctx, begin)
	require.NoError(t, err)
	confirm := &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "未排空不能只凭SQL行放行")
	setProofFence(t, rows, 11, 17, 2)
	ack := &dspb.AckRoleTradeDrainRequest{Seal: frozen.Holders[0].Seal, RequiredOwnerEpoch: 17, RequiredSaveSeq: 3}
	require.NoError(t, admissions.AckDrain(ctx, ack))
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "scene ACK不替代真实SQL已追上")
	setProofFence(t, rows, 11, 17, 3)
	frozen, err = trades.Confirm(ctx, confirm)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_SCENE, frozen.FreezeEvidence.Kind)
	require.Equal(t, []uint64{17, 3}, []uint64{frozen.FreezeEvidence.OwnerEpoch, frozen.FreezeEvidence.SaveSeq})
	require.Len(t, frozen.FreezeEvidence.PersistedDigest, 32)
	transferred, err := trades.Transfer(ctx, &dspb.TransferAccountRoleRequest{TxId: 1001, ListingId: 101, PlayerId: 11, SellerAccount: "seller", BuyerAccount: "buyer", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	require.Equal(t, uint64(2), transferred.BuyerOwnerRevision)
	require.NoError(t, admissions.AckDrain(ctx, ack), "过户后旧ACK仍只返回原成功")
	_, err = admissions.Register(ctx, admissionTestRequest(11, 2, 1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = admissions.Register(ctx, admissionTestRequest(11, 2, 2))
	require.NoError(t, err)
	require.NoError(t, admissions.AckDrain(ctx, ack))
	var highest, closed uint64
	require.NoError(t, global.QueryRowContext(ctx, "SELECT highest_generation,closed_through FROM account_role_admission WHERE player_id=11").Scan(&highest, &closed))
	require.Equal(t, []uint64{2, 1}, []uint64{highest, closed})
}

func TestTiDBRoleAdmissionSQLProofNoAdmissionAndHistoricalFloor(t *testing.T) {
	d, global, fixture := accountDirectoryTiDBWithWriter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	// 目录按正式初始事务建立后，本例再把路由切到专门的旧版/追盘证据夹具。
	id, _, rows := roleProofStorage(t, global, 11, fixture)
	v, err := NewSQLRoleFreezeVerifier([]uint32{id})
	require.NoError(t, err)
	trades := newTestAccountRoleTradeStore(d, v)
	_, err = trades.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	confirm := &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	out, err := trades.Confirm(ctx, confirm)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_INITIAL_COMMIT, out.FreezeEvidence.Kind)
	require.EqualValues(t, 1, out.FreezeEvidence.OwnerEpoch)
	_, err = trades.Cancel(ctx, &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	a := NewAccountRoleAdmissionStore(d)
	_, err = a.Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err)
	_, err = a.CloseAdmissions(ctx, &dspb.CloseRoleAdmissionsRequest{PlayerId: 11, SceneInstanceId: admissionTestInstance, OwnerRevision: 1, ThroughGeneration: 1, RequiredOwnerEpoch: 17, RequiredSaveSeq: 9})
	require.NoError(t, err)
	_, err = a.Register(ctx, admissionTestRequest(11, 2, 1))
	require.NoError(t, err)
	_, err = a.CloseAdmissions(ctx, &dspb.CloseRoleAdmissionsRequest{PlayerId: 11, SceneInstanceId: admissionTestInstance, OwnerRevision: 1, ThroughGeneration: 2})
	require.NoError(t, err)
	_, err = trades.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 102, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	confirm.ListingId = 102
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "无实体不能把历史SQL下限归零")
	setProofFence(t, rows, 11, 17, 9)
	out, err = trades.Confirm(ctx, confirm)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_SCENE, out.FreezeEvidence.Kind)
	require.Empty(t, out.Holders, "已关闭登记不制造新holder，但历史floor仍必须检查")
}

func TestTiDBRoleAdmissionRegisterAndBeginShareCommitLock(t *testing.T) {
	d, global := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	a, trades := NewAccountRoleAdmissionStore(d), newTestAccountRoleTradeStore(d, nil)
	start := make(chan struct{})
	registered := make(chan error, 1)
	go func() { <-start; _, err := a.Register(ctx, admissionTestRequest(11, 1, 1)); registered <- err }()
	close(start)
	out, err := trades.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	registerErr := <-registered
	if registerErr == nil {
		require.Len(t, out.Holders, 1, "先提交的登记必须纳入本轮固定全集")
		require.Equal(t, uint64(1), out.Holders[0].Seal.ThroughGeneration)
	} else {
		require.ErrorIs(t, registerErr, accountdirectory.ErrConflict)
		require.Empty(t, out.Holders, "冻结先提交时不能出现未纳入holder的成功登记")
	}
	var listing uint64
	require.NoError(t, global.QueryRowContext(ctx, "SELECT trade_listing_id FROM account_role WHERE player_id=11").Scan(&listing))
	require.Equal(t, uint64(101), listing)
	_, err = a.Register(ctx, admissionTestRequest(11, 2, 1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
}

func TestTiDBRoleAdmissionSQLProofRejectsRouteAndSchemaFailures(t *testing.T) {
	d, global, fixture := accountDirectoryTiDBWithWriter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	// 目录按正式初始事务建立后，本例再把路由切到专门的旧版/追盘证据夹具。
	id, _, rows := roleProofStorage(t, global, 11, fixture)
	v, err := NewSQLRoleFreezeVerifier([]uint32{id})
	require.NoError(t, err)
	trades := newTestAccountRoleTradeStore(d, v)
	_, err = trades.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	confirm := &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	_, err = global.Exec("UPDATE player_route SET frozen_run_id='proof-maintenance' WHERE player_id=11")
	require.NoError(t, err)
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = global.Exec("UPDATE player_route SET frozen_run_id=NULL WHERE player_id=11")
	require.NoError(t, err)
	deny, err := NewSQLRoleFreezeVerifier([]uint32{id + 1})
	require.NoError(t, err)
	trades.verifier = deny
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	trades.verifier = v
	_, err = rows.Exec("UPDATE player_store SET schema_gen=4294967295 WHERE player_id=11 AND kind=0")
	require.NoError(t, err)
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorContains(t, err, "世代")
	_, err = rows.Exec("DELETE FROM player_store WHERE player_id=11")
	require.NoError(t, err)
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	var state uint32
	require.NoError(t, global.QueryRowContext(ctx, "SELECT state FROM account_role_trade WHERE listing_id=101").Scan(&state))
	require.EqualValues(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING, state, "失败不能留下FROZEN原件")
	var receipts int
	require.NoError(t, global.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_transfer_receipt").Scan(&receipts))
	require.Zero(t, receipts)
}

func TestTiDBRoleAdmissionOpenRejectsRemovalAndToolRestoreKeepsFloor(t *testing.T) {
	d, global := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11, AppearanceId: "original"}
	_, err := createInitialRoleForTest(d, ctx, "seller", role)
	require.NoError(t, err)
	a := NewAccountRoleAdmissionStore(d)
	_, err = a.Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err)
	require.ErrorIs(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrConflict)
	for _, restore := range []bool{false, true} {
		_, err = d.ImportRole(ctx, "seller", role, restore, roleImportTestID(1))
		require.ErrorIs(t, err, accountdirectory.ErrConflict)
	}
	_, err = createInitialRoleForTest(d, ctx, "seller", role)
	require.NoError(t, err, "普通建角原请求幂等不扩大成全部在线角色禁用")
	_, err = a.CloseAdmissions(ctx, &dspb.CloseRoleAdmissionsRequest{PlayerId: 11, SceneInstanceId: admissionTestInstance, OwnerRevision: 1, ThroughGeneration: 1, RequiredOwnerEpoch: 17, RequiredSaveSeq: 3})
	require.NoError(t, err)
	require.NoError(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}))
	_, err = d.ImportRole(ctx, "seller", role, true, roleImportTestID(1))
	require.NoError(t, err)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, uint64(3), owned.OwnerRevision)
	var floorEpoch, floorSeq, closed uint64
	require.NoError(t, global.QueryRowContext(ctx, "SELECT required_owner_epoch,required_save_seq,closed_through FROM account_role_admission WHERE player_id=11").Scan(&floorEpoch, &floorSeq, &closed))
	require.Equal(t, []uint64{17, 3, 1}, []uint64{floorEpoch, floorSeq, closed})
}

func TestTiDBRoleImportReceiptRevokesOldRoutesAndDoesNotBumpOnResume(t *testing.T) {
	d, global := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11, AppearanceId: "original"}
	_, err := createInitialRoleForTest(d, ctx, "seller", role)
	require.NoError(t, err)
	type result struct{ err error }
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.ImportRole(ctx, "seller", role, false, roleImportTestID(1))
			results <- result{err}
		}()
	}
	wg.Wait()
	close(results)
	count := 0
	for r := range results {
		require.NoError(t, r.err)
		count++
	}
	require.Equal(t, 8, count)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, uint64(2), owned.OwnerRevision)
	_, err = d.ImportRole(ctx, "seller", role, true, roleImportTestID(1))
	require.NoError(t, err, "从false加replace恢复只能回原维护修订")
	owned, err = d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, uint64(2), owned.OwnerRevision)
	a := NewAccountRoleAdmissionStore(d)
	_, err = a.Register(ctx, admissionTestRequest(11, 1, 1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "旧路由不能在目录改修订后入场")
	_, err = d.ImportRole(ctx, "other", role, true, roleImportTestID(1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "同维护键换目标账号拒绝")
	changed := proto.Clone(role).(*base.AccountSimplePlayer)
	changed.AppearanceId = "changed"
	_, err = d.ImportRole(ctx, "seller", changed, true, roleImportTestID(1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = d.ImportRole(ctx, "seller", role, true, roleImportTestID(2))
	require.NoError(t, err)
	owned, err = d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, uint64(3), owned.OwnerRevision)
	_, err = d.ImportRole(ctx, "seller", role, true, roleImportTestID(1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "之后新导入使旧包不能覆盖新状态")
	var receipts int
	require.NoError(t, global.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_import_receipt").Scan(&receipts))
	require.Equal(t, 2, receipts)
}

// 持有者已经写不了存档(写权行不是它):不等 ACK,判为排空,代次抬到它登记的代次之上挡住在途的认领,关掉它的登记。
// 崩溃的实例永远不会发 ACK,不判掉的话这笔交易和这个角色之后的每一笔交易都会卡住。
func TestTiDBRoleTradeDeadHolderIsFencedWithoutAck(t *testing.T) {
	d, global, fixture := accountDirectoryTiDBWithWriter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	id, _, rows := roleProofStorage(t, global, 11, fixture)
	v, err := NewSQLRoleFreezeVerifier([]uint32{id})
	require.NoError(t, err)
	trades, admissions := newTestAccountRoleTradeStore(d, v), NewAccountRoleAdmissionStore(d)
	_, err = admissions.Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err)
	// 写权行是初始状态:没有现任。登记之后从未认领。
	begin := &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	first, err := trades.Begin(ctx, begin)
	require.NoError(t, err)
	require.Len(t, first.Holders, 1)
	require.True(t, first.Holders[0].Drained, "写不了存档的持有者不需要 ACK")
	var epoch uint64
	require.NoError(t, rows.QueryRowContext(ctx, "SELECT owner_epoch FROM player_store WHERE player_id=11 AND kind=0").Scan(&epoch))
	require.Equal(t, uint64(18), epoch, "抬到登记的代次(17)之上:在途的认领带着它,go/db 判 STALE;只抬一次的话 17 > 2 照样认领成功")
	var highest, closed uint64
	require.NoError(t, global.QueryRowContext(ctx, "SELECT highest_generation,closed_through FROM account_role_admission WHERE player_id=11").Scan(&highest, &closed))
	require.Equal(t, highest, closed, "登记要关,下一笔交易不能再把它拍进持有者")
	frozen, err := trades.Confirm(ctx, &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN, frozen.State)
	require.Equal(t, uint64(18), frozen.FreezeEvidence.OwnerEpoch)
	// 实例其实活着,迟到的真实 ACK 和 Close 都不算冲突。
	require.NoError(t, admissions.AckDrain(ctx, &dspb.AckRoleTradeDrainRequest{Seal: first.Holders[0].Seal, RequiredOwnerEpoch: 1, RequiredSaveSeq: 1}))
	_, err = admissions.CloseAdmissions(ctx, &dspb.CloseRoleAdmissionsRequest{PlayerId: 11, SceneInstanceId: admissionTestInstance, OwnerRevision: 1, ThroughGeneration: 1})
	require.NoError(t, err)
}

// 释放不是终态:断线后 30 秒内重连会复用实体接着存,released=1 的现任照样要等真实的 ACK。
// 它真死了(释放之后、关登记之前崩溃)由运维确认后摘掉。
func TestTiDBRoleTradeReleasedCurrentHolderStillWaitsForAck(t *testing.T) {
	d, global, fixture := accountDirectoryTiDBWithWriter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	id, _, rows := roleProofStorage(t, global, 11, fixture)
	v, err := NewSQLRoleFreezeVerifier([]uint32{id})
	require.NoError(t, err)
	trades, admissions := newTestAccountRoleTradeStore(d, v), NewAccountRoleAdmissionStore(d)
	_, err = admissions.Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err)
	holdPlayerStoreForTest(t, ctx, d, 11)
	_, err = rows.ExecContext(ctx, "UPDATE player_store SET released=1 WHERE player_id=11 AND kind=0")
	require.NoError(t, err)
	begin := &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	first, err := trades.Begin(ctx, begin)
	require.NoError(t, err)
	require.False(t, first.Holders[0].Drained, "已释放的现任可能还活着,不能判死")
	_, err = d.ReleaseHolderInstance(ctx, 11, admissionTestInstance, true)
	require.NoError(t, err)
	again, err := trades.Begin(ctx, begin)
	require.NoError(t, err)
	require.True(t, again.Holders[0].Drained, "摘掉之后,重放 Begin 判为排空")
}

// 现任可能活着、内存里还有没落盘的进度:判死之前不动。释放之后,取消的重试才能完成。
func TestTiDBRoleTradeCurrentHolderBlocksUntilReleased(t *testing.T) {
	d, global, fixture := accountDirectoryTiDBWithWriter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	id, _, rows := roleProofStorage(t, global, 11, fixture)
	v, err := NewSQLRoleFreezeVerifier([]uint32{id})
	require.NoError(t, err)
	trades, admissions := newTestAccountRoleTradeStore(d, v), NewAccountRoleAdmissionStore(d)
	_, err = admissions.Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err)
	holdPlayerStoreForTest(t, ctx, d, 11)
	first, err := trades.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	require.False(t, first.Holders[0].Drained, "现任要等真实的 ACK")
	stop := &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	waiting, err := trades.Cancel(ctx, stop)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING, waiting.State)
	// 实例号不对:不动。
	_, err = d.ReleaseHolderInstance(ctx, 11, "99999999-2222-3333-4444-555555555555", true)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	// plan 只核对,不写。
	epochBefore, err := d.ReleaseHolderInstance(ctx, 11, admissionTestInstance, false)
	require.NoError(t, err)
	var epoch uint64
	require.NoError(t, rows.QueryRowContext(ctx, "SELECT owner_epoch FROM player_store WHERE player_id=11 AND kind=0").Scan(&epoch))
	require.Equal(t, epochBefore, epoch)
	_, err = d.ReleaseHolderInstance(ctx, 11, admissionTestInstance, true)
	require.NoError(t, err)
	finished, err := trades.Cancel(ctx, stop)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, finished.State)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
}
