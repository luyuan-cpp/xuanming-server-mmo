package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	base "proto/common/base"
	componentpb "proto/common/component"
	databasepb "proto/common/database"
	dspb "proto/data_service"
	storepb "proto/player_store"
	"shared/generated/persistschema"
)

// player_store 的真库用例:建角初始行、账本读、判死释放、交易冻结证据。

type storeRowForTest struct {
	epoch, seq      uint64
	gen, released   uint32
	holder, payload []byte
}

func readStoreRowForTest(t *testing.T, db *sql.DB, schema string, player uint64, kind storepb.PlayerStoreKind) (storeRowForTest, bool) {
	t.Helper()
	var row storeRowForTest
	err := db.QueryRow("SELECT owner_epoch,save_seq,schema_gen,released,holder_instance,payload FROM `"+schema+"`.`player_store` WHERE player_id=? AND kind=?", player, int32(kind)).
		Scan(&row.epoch, &row.seq, &row.gen, &row.released, &row.holder, &row.payload)
	if err == sql.ErrNoRows {
		return row, false
	}
	require.NoError(t, err)
	return row, true
}

func directRoleForTest(t *testing.T, s *AccountDirectoryStore, ctx context.Context, player uint64) *base.AccountSimplePlayer {
	t.Helper()
	role := &base.AccountSimplePlayer{PlayerId: player, ZoneId: 1, ClassId: 3, Gender: 2, Name: "新路径", AppearanceId: "look"}
	require.NoError(t, installInitialRouteForTest(s, ctx, role))
	_, err := s.CreateWithInitialCommit(ctx, "owner", role)
	require.NoError(t, err)
	return role
}

func TestTiDBCreateRoleWritesPlayerStoreRows(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{PlayerId: 3201, ZoneId: 1, ClassId: 3, Gender: 2, Name: "新路径", AppearanceId: "look"}
	require.NoError(t, installInitialRouteForTest(s, ctx, role))

	_, err := s.CreateWithInitialCommit(ctx, "owner", role)
	require.NoError(t, err)

	var marker uint64
	require.NoError(t, db.QueryRow("SELECT initial_player_commit FROM account_role WHERE player_id=?", role.PlayerId).Scan(&marker))
	require.EqualValues(t, 1, marker, "交易冻结靠它判「从没进过场」")

	owner, ok := readStoreRowForTest(t, db, schema, role.PlayerId, storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER)
	require.True(t, ok)
	require.EqualValues(t, 1, owner.epoch)
	require.EqualValues(t, 1, owner.seq)
	require.EqualValues(t, persistschema.Gen, owner.gen)
	require.EqualValues(t, 1, owner.released, "新角色没有持有者")
	require.Empty(t, owner.holder)

	hotRow, ok := readStoreRowForTest(t, db, schema, role.PlayerId, storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT)
	require.True(t, ok)
	var hot databasepb.PlayerDatabase
	require.NoError(t, proto.Unmarshal(hotRow.payload, &hot))
	require.EqualValues(t, 3, hot.GetUint32PbComponent().GetClass())

	profileRow, ok := readStoreRowForTest(t, db, schema, role.PlayerId, storepb.PlayerStoreKind_PLAYER_STORE_KIND_PROFILE)
	require.True(t, ok)
	var profile databasepb.PlayerDatabase
	require.NoError(t, proto.Unmarshal(profileRow.payload, &profile))
	require.Equal(t, "新路径", profile.GetProfileComponent().GetName())
	require.Equal(t, "look", profile.GetProfileComponent().GetAppearanceId())
	require.EqualValues(t, 2, profile.GetProfileComponent().GetGender())

	// 目录幂等重放不能重写初始行:那会盖掉已经存过的进度。
	_, err = db.Exec("UPDATE `"+schema+"`.`player_store` SET save_seq=99 WHERE player_id=? AND kind=0", role.PlayerId)
	require.NoError(t, err)
	_, err = s.CreateWithInitialCommit(ctx, "owner", role)
	require.NoError(t, err)
	owner, ok = readStoreRowForTest(t, db, schema, role.PlayerId, storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER)
	require.True(t, ok)
	require.EqualValues(t, 99, owner.seq)
}

func TestTiDBReadPlayerStoreAsset(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := directRoleForTest(t, s, ctx, 3202)

	row, err := s.ReadPlayerStoreAsset(ctx, role.PlayerId)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.True(t, row.Released)
	require.EqualValues(t, role.PlayerId, row.Player.GetPlayerId())
	require.Nil(t, row.Player.GetAssetOpLedger(), "没有 kAsset 行:账本为空")

	// 没有路由 / 没有写权行:不是错误。
	row, err = s.ReadPlayerStoreAsset(ctx, 987654321)
	require.NoError(t, err)
	require.Nil(t, row)

	// 认领之后 released=0,账本行读得出来。
	asset, err := proto.Marshal(&databasepb.PlayerDatabase{AssetOpLedger: &componentpb.PlayerAssetOpLedgerComp{}})
	require.NoError(t, err)
	_, err = db.Exec("UPDATE `"+schema+"`.`player_store` SET owner_epoch=7,save_seq=9,released=0,holder_instance=? WHERE player_id=? AND kind=0", []byte("instance-a"), role.PlayerId)
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO `"+schema+"`.`player_store` (player_id,kind,owner_epoch,save_seq,schema_gen,holder_instance,released,payload) VALUES (?,?,7,9,?,?,0,?)",
		role.PlayerId, int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_ASSET), persistschema.Gen, []byte{}, asset)
	require.NoError(t, err)
	row, err = s.ReadPlayerStoreAsset(ctx, role.PlayerId)
	require.NoError(t, err)
	require.False(t, row.Released)
	require.EqualValues(t, 7, row.OwnerEpoch)
	require.EqualValues(t, 9, row.SaveSeq)
	require.NotNil(t, row.Player.GetAssetOpLedger())
	require.EqualValues(t, role.PlayerId, row.Player.GetPlayerId())
}

func TestTiDBReleaseDeadPlayerStoreHolder(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := directRoleForTest(t, s, ctx, 3203)
	_, err := db.Exec("UPDATE `"+schema+"`.`player_store` SET owner_epoch=7,save_seq=9,released=0,holder_instance=? WHERE player_id=? AND kind=0", []byte("instance-a"), role.PlayerId)
	require.NoError(t, err)

	require.NoError(t, s.ReleaseDeadPlayerStoreHolder(ctx, role.PlayerId, 7))
	owner, ok := readStoreRowForTest(t, db, schema, role.PlayerId, storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER)
	require.True(t, ok)
	require.EqualValues(t, 8, owner.epoch, "抬代次:旧持有者恢复之后的写因 E<Er 被挡住")
	require.EqualValues(t, 9, owner.seq)
	require.EqualValues(t, 1, owner.released)
	require.Empty(t, owner.holder)

	// 幂等:重试不再动。
	require.NoError(t, s.ReleaseDeadPlayerStoreHolder(ctx, role.PlayerId, 7))
	owner, _ = readStoreRowForTest(t, db, schema, role.PlayerId, storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER)
	require.EqualValues(t, 8, owner.epoch)

	// 新的持有者已经认领:不动它。
	_, err = db.Exec("UPDATE `"+schema+"`.`player_store` SET owner_epoch=20,released=0,holder_instance=? WHERE player_id=? AND kind=0", []byte("instance-b"), role.PlayerId)
	require.NoError(t, err)
	require.NoError(t, s.ReleaseDeadPlayerStoreHolder(ctx, role.PlayerId, 7))
	owner, _ = readStoreRowForTest(t, db, schema, role.PlayerId, storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER)
	require.EqualValues(t, 20, owner.epoch)
	require.EqualValues(t, 0, owner.released)
	require.Equal(t, []byte("instance-b"), owner.holder)

	// 没有这名玩家:没有东西要释放。
	require.NoError(t, s.ReleaseDeadPlayerStoreHolder(ctx, 987654321, 7))
}

func TestTiDBVerifyFrozenOnPlayerStore(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	storageID, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := directRoleForTest(t, s, ctx, 3204)

	verifier, err := NewSQLRoleFreezeVerifier([]uint32{storageID})
	require.NoError(t, err)
	verify := func(required uint64, holders int) (*dspb.AccountRoleFreezeEvidence, error) {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		require.NoError(t, err)
		defer tx.Rollback()
		trade := &databasepb.AccountRoleTrade{PlayerId: role.PlayerId, ListingId: 1, SellerOwnerRevision: 1, RequiredOwnerEpoch: required, RequiredSaveSeq: required}
		return verifier.verifyFrozenPlayerStore(ctx, tx, trade, holders, schema)
	}

	// 从没进过场:初始提交。
	evidence, err := verify(0, 0)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_INITIAL_COMMIT, evidence.GetKind())
	initialDigest := evidence.GetPersistedDigest()

	// 初始存档上不该有持有者。
	_, err = verify(0, 1)
	require.Error(t, err)

	// 存过盘:版本取写权行,摘要跟着变。
	_, err = db.Exec("UPDATE `"+schema+"`.`player_store` SET owner_epoch=7,save_seq=9 WHERE player_id=? AND kind=0", role.PlayerId)
	require.NoError(t, err)
	evidence, err = verify(7, 1)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_SCENE, evidence.GetKind())
	require.EqualValues(t, 7, evidence.GetOwnerEpoch())
	require.EqualValues(t, 9, evidence.GetSaveSeq())
	require.NotEqual(t, initialDigest, evidence.GetPersistedDigest())

	// 库里的版本低于下限:最后那笔存盘还没提交,不能冻结。
	_, err = verify(8, 1)
	require.Error(t, err)
}

func TestTiDBReadPlayerDisplays(t *testing.T) {
	s, db := accountDirectoryTiDB(t)
	_, schema := initialPlayerSchemaForTest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := directRoleForTest(t, s, ctx, 3205)

	// 重复、0、不存在的 id 都不是错误。
	displays, err := s.ReadPlayerDisplays(ctx, []uint64{role.PlayerId, role.PlayerId, 0, 987654321})
	require.NoError(t, err)
	require.Len(t, displays, 1)
	display := displays[role.PlayerId]
	require.Equal(t, "新路径", display.GetName())
	require.Equal(t, "look", display.GetAppearanceId())
	require.EqualValues(t, 2, display.GetGender())
	require.EqualValues(t, 3, display.GetClassId())
	require.Zero(t, display.GetLevel(), "还没存过盘")

	// 存过盘之后等级跟着 kHot 行走。
	hot, err := proto.Marshal(&databasepb.PlayerDatabase{
		LevelComponent:    &componentpb.LevelComp{Level: 42},
		Uint32PbComponent: &componentpb.PlayerUint32Comp{Class: 3},
	})
	require.NoError(t, err)
	_, err = db.Exec("UPDATE `"+schema+"`.`player_store` SET payload=? WHERE player_id=? AND kind=?", hot, role.PlayerId, int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT))
	require.NoError(t, err)
	displays, err = s.ReadPlayerDisplays(ctx, []uint64{role.PlayerId})
	require.NoError(t, err)
	require.EqualValues(t, 42, displays[role.PlayerId].GetLevel())

	// 一行坏了只跳过那一行,不拖垮整批。
	_, err = db.Exec("UPDATE `"+schema+"`.`player_store` SET payload=? WHERE player_id=? AND kind=?", []byte{0xff, 0xff}, role.PlayerId, int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT))
	require.NoError(t, err)
	displays, err = s.ReadPlayerDisplays(ctx, []uint64{role.PlayerId})
	require.NoError(t, err)
	require.Equal(t, "新路径", displays[role.PlayerId].GetName())
	require.Zero(t, displays[role.PlayerId].GetLevel())

	_, err = s.ReadPlayerDisplays(ctx, make([]uint64, PlayerDisplayBatchLimit+1))
	require.Error(t, err)
}
