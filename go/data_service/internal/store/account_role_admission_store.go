package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"

	base "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/placement"

	"google.golang.org/protobuf/proto"
)

const AccountRoleAdmissionTableName = "account_role_admission"
const AccountRoleTradeHolderTableName = "account_role_trade_holder"
const AccountRoleAdmissionRecoveryTableName = "account_role_admission_recovery"
const SaveBarrierMaintenanceTableName = "save_barrier_maintenance_receipt"

// 两类收据都永久保留原计划；摘要列固定宽度，正文不经过旧模型重写。
const saveBarrierMaintenanceDDL = "CREATE TABLE IF NOT EXISTS save_barrier_maintenance_receipt (receipt_id BINARY(32) NOT NULL COMMENT 'pb:1',player_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:2',request_digest BINARY(32) NOT NULL COMMENT 'pb:3',plan_proto MEDIUMBLOB NOT NULL COMMENT 'pb:4',phase INT UNSIGNED NOT NULL COMMENT 'pb:5',updated_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:6',PRIMARY KEY(receipt_id)) ENGINE=InnoDB"
const accountRoleAdmissionRecoveryDDL = "CREATE TABLE IF NOT EXISTS account_role_admission_recovery (request_id BINARY(32) NOT NULL COMMENT 'pb:1',listing_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:2',player_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:3',scene_instance_id VARBINARY(36) NOT NULL COMMENT 'pb:4',through_generation BIGINT UNSIGNED NOT NULL COMMENT 'pb:5',owner_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:6',request_digest BINARY(32) NOT NULL COMMENT 'pb:7',request_proto MEDIUMBLOB NOT NULL COMMENT 'pb:8',maintenance_receipt_id BINARY(32) NOT NULL COMMENT 'pb:9',maintenance_request_digest BINARY(32) NOT NULL COMMENT 'pb:10',required_owner_epoch BIGINT UNSIGNED NOT NULL COMMENT 'pb:11',required_save_seq BIGINT UNSIGNED NOT NULL COMMENT 'pb:12',sql_proof_digest BINARY(32) NOT NULL COMMENT 'pb:13',recovered_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:14',PRIMARY KEY(request_id),UNIQUE KEY recovery_holder(listing_id,scene_instance_id)) ENGINE=InnoDB"

// 主键字符串需要固定逐字节 UUID 列，不能由 proto2mysql 的 MEDIUMTEXT 代替。
const accountRoleAdmissionDDL = "CREATE TABLE IF NOT EXISTS account_role_admission (player_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:1',scene_instance_id VARBINARY(36) NOT NULL COMMENT 'pb:2',highest_generation BIGINT UNSIGNED NOT NULL COMMENT 'pb:3',closed_through BIGINT UNSIGNED NOT NULL COMMENT 'pb:4',identity_proto MEDIUMBLOB NOT NULL COMMENT 'pb:5',request_digest BINARY(32) NOT NULL COMMENT 'pb:6',required_owner_epoch BIGINT UNSIGNED NOT NULL COMMENT 'pb:7',required_save_seq BIGINT UNSIGNED NOT NULL COMMENT 'pb:8',close_digest VARBINARY(32) NOT NULL COMMENT 'pb:9',updated_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:10',PRIMARY KEY(player_id,scene_instance_id)) ENGINE=InnoDB"
const accountRoleTradeHolderDDL = "CREATE TABLE IF NOT EXISTS account_role_trade_holder (listing_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:1',scene_instance_id VARBINARY(36) NOT NULL COMMENT 'pb:2',seal_proto MEDIUMBLOB NOT NULL COMMENT 'pb:3',drained INT UNSIGNED NOT NULL COMMENT 'pb:4',ack_digest VARBINARY(32) NOT NULL COMMENT 'pb:5',updated_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:6',PRIMARY KEY(listing_id,scene_instance_id)) ENGINE=InnoDB"

func ensureRoleAdmissionSchema(ctx context.Context, db *sql.DB, name string) error {
	if err := assertColumnsPresent(ctx, db, name, &dbpb.AccountRoleImportReceipt{}, AccountRoleImportTableName); err != nil {
		return err
	}
	importKeys, err := primaryKeyColumns(ctx, db, name, AccountRoleImportTableName)
	if err != nil {
		return err
	}
	if !slices.Equal(importKeys, []string{"request_id"}) {
		return errors.New("维护导入收据主键不符")
	}
	for _, column := range []string{"request_id", "request_digest", "account"} {
		var typ string
		var width sql.NullInt64
		if err = db.QueryRowContext(ctx, "SELECT DATA_TYPE,CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME=?", name, AccountRoleImportTableName, column).Scan(&typ, &width); err != nil {
			return err
		}
		wantType, wantWidth := "binary", int64(32)
		if column == "account" {
			wantType, wantWidth = "varbinary", 191
		}
		if typ != wantType || !width.Valid || width.Int64 != wantWidth {
			return errors.New("维护导入收据字符串／摘要列不符")
		}
	}
	for _, model := range []proto.Message{&dbpb.AccountRoleAdmission{}, &dbpb.AccountRoleTradeHolder{}} {
		table, key := AccountRoleAdmissionTableName, "player_id"
		if _, ok := model.(*dbpb.AccountRoleTradeHolder); ok {
			table, key = AccountRoleTradeHolderTableName, "listing_id"
		}
		if err := assertColumnsPresent(ctx, db, name, model, table); err != nil {
			return err
		}
		keys, err := primaryKeyColumns(ctx, db, name, table)
		if err != nil {
			return err
		}
		if !slices.Equal(keys, []string{key, "scene_instance_id"}) {
			return errors.New("角色入场／排空表主键不符")
		}
		var typ string
		var width sql.NullInt64
		if err = db.QueryRowContext(ctx, "SELECT DATA_TYPE,CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME='scene_instance_id'", name, table).Scan(&typ, &width); err != nil {
			return err
		}
		if typ != "varbinary" || !width.Valid || width.Int64 != 36 {
			return errors.New("角色入场／排空实例列必须逐字节比较")
		}
	}
	return ensureAdmissionRecoverySchema(ctx, db, name)
}

func ensureAdmissionRecoverySchema(ctx context.Context, db *sql.DB, name string) error {
	for table, model := range map[string]proto.Message{SaveBarrierMaintenanceTableName: &dbpb.SaveBarrierMaintenanceReceipt{}, AccountRoleAdmissionRecoveryTableName: &dbpb.AccountRoleAdmissionRecovery{}} {
		if err := assertColumnsPresent(ctx, db, name, model, table); err != nil {
			return err
		}
		key := "request_id"
		if table == SaveBarrierMaintenanceTableName {
			key = "receipt_id"
		}
		keys, err := primaryKeyColumns(ctx, db, name, table)
		if err != nil {
			return err
		}
		if !slices.Equal(keys, []string{key}) {
			return errors.New("旧holder恢复收据主键不符")
		}
		columns := []string{key, "request_digest"}
		if table == AccountRoleAdmissionRecoveryTableName {
			columns = append(columns, "maintenance_receipt_id", "maintenance_request_digest", "sql_proof_digest", "scene_instance_id")
		}
		for _, column := range columns {
			var typ string
			var width sql.NullInt64
			if err := db.QueryRowContext(ctx, "SELECT DATA_TYPE,CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME=?", name, table, column).Scan(&typ, &width); err != nil {
				return err
			}
			want, n := "binary", int64(32)
			if column == "scene_instance_id" {
				want, n = "varbinary", 36
			}
			if typ != want || !width.Valid || width.Int64 != n {
				return errors.New("旧holder恢复收据摘要或实例列不符")
			}
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT COLUMN_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND INDEX_NAME='recovery_holder' AND NON_UNIQUE=0 ORDER BY SEQ_IN_INDEX", name, AccountRoleAdmissionRecoveryTableName)
	if err != nil {
		return err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !slices.Equal(keys, []string{"listing_id", "scene_instance_id"}) {
		return errors.New("旧holder恢复缺永久业务唯一键")
	}
	return nil
}

type AccountRoleAdmissionStore struct{ directory *AccountDirectoryStore }

func NewAccountRoleAdmissionStore(d *AccountDirectoryStore) *AccountRoleAdmissionStore {
	return &AccountRoleAdmissionStore{directory: d}
}

var _ accountdirectory.Admissions = (*AccountRoleAdmissionStore)(nil)

func (s *AccountRoleAdmissionStore) ready() bool {
	return s != nil && s.directory != nil && s.directory.db != nil
}

// CheckRecoverySchema只读核恢复收据表，不在维护恢复时隐式DDL。
func (s *AccountRoleAdmissionStore) CheckRecoverySchema(ctx context.Context) error {
	if !s.ready() {
		return accountdirectory.ErrUnavailable
	}
	return ensureRoleAdmissionSchema(ctx, s.directory.db, s.directory.schema)
}
func lockAdmissionRole(ctx context.Context, tx *sql.Tx, player, revision uint64) (*dspb.AccountRoleOwnership, error) {
	r, err := scanOwnership(tx.QueryRowContext(ctx, "SELECT account,role_proto,owner_revision,trade_listing_id FROM account_role WHERE player_id=? FOR UPDATE", player), player)
	if err != nil {
		return nil, err
	}
	if r.OwnerRevision != revision {
		return nil, accountdirectory.ErrConflict
	}
	return r, nil
}
func readAdmission(ctx context.Context, tx *sql.Tx, player uint64, instance string) (*dbpb.AccountRoleAdmission, error) {
	r := &dbpb.AccountRoleAdmission{PlayerId: player, SceneInstanceId: instance}
	err := tx.QueryRowContext(ctx, "SELECT highest_generation,closed_through,identity_proto,request_digest,required_owner_epoch,required_save_seq,close_digest,updated_ms FROM account_role_admission WHERE player_id=? AND scene_instance_id=? FOR UPDATE", player, []byte(instance)).Scan(&r.HighestGeneration, &r.ClosedThrough, &r.IdentityProto, &r.RequestDigest, &r.RequiredOwnerEpoch, &r.RequiredSaveSeq, &r.CloseDigest, &r.UpdatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, accountdirectory.ErrMissing
	}
	if err != nil {
		return nil, err
	}
	if _, err = admissionIdentity(r); err != nil {
		return nil, err
	}
	return r, nil
}
func admissionIdentity(r *dbpb.AccountRoleAdmission) (*base.RoleAdmissionIdentity, error) {
	i := &base.RoleAdmissionIdentity{}
	if len(r.IdentityProto) > 1024 || proto.Unmarshal(r.IdentityProto, i) != nil || !accountdirectory.ValidAdmission(i) || i.PlayerId != r.PlayerId || i.SceneInstanceId != r.SceneInstanceId || i.Generation != r.HighestGeneration || r.ClosedThrough > r.HighestGeneration || len(r.RequestDigest) != 32 || (len(r.CloseDigest) != 0 && len(r.CloseDigest) != 32) || !accountdirectory.ValidSaveFloor(r.RequiredOwnerEpoch, r.RequiredSaveSeq) {
		return nil, errors.New("角色入场登记原件损坏")
	}
	return i, nil
}
func saveFloorAfter(epoch, seq, oldEpoch, oldSeq uint64) bool {
	return epoch > oldEpoch || (epoch == oldEpoch && seq > oldSeq)
}

// 调用者已锁 role；所有 Register/Close/Ack 也先持同一行锁，所以当前强读不会漏掉并发入场。
// 关闭的历史行和其 SQL 下限永久保留，工具不能通过删除登记绕开未来冻结验证。
func requireClosedAdmissions(ctx context.Context, tx *sql.Tx, player uint64) error {
	var open bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM account_role_admission WHERE player_id=? AND closed_through<highest_generation)", player).Scan(&open); err != nil {
		return err
	}
	if open {
		return accountdirectory.ErrConflict
	}
	return nil
}

func (s *AccountRoleAdmissionStore) Register(ctx context.Context, in *dspb.RegisterRoleAdmissionRequest) (*dspb.RegisterRoleAdmissionResponse, error) {
	if !s.ready() {
		return nil, accountdirectory.ErrUnavailable
	}
	if in == nil || !accountdirectory.ValidAdmission(in.Identity) || proto.Size(in.Identity) > 1024 {
		return nil, accountdirectory.ErrInvalid
	}
	digest, err := roleTradeDigest(in)
	if err != nil {
		return nil, err
	}
	i := in.Identity
	raw, err := proto.Marshal(i)
	if err != nil {
		return nil, err
	}
	var out *dspb.RegisterRoleAdmissionResponse
	err = s.directory.transaction(ctx, func(tx *sql.Tx) error {
		owned, err := lockAdmissionRole(ctx, tx, i.PlayerId, i.OwnerRevision)
		if err != nil {
			return err
		}
		if owned.TradeListingId != 0 {
			return accountdirectory.ErrConflict
		}
		r, err := readAdmission(ctx, tx, i.PlayerId, i.SceneInstanceId)
		if err != nil && !errors.Is(err, accountdirectory.ErrMissing) {
			return err
		}
		if r != nil {
			if i.Generation <= r.ClosedThrough || i.Generation < r.HighestGeneration {
				return accountdirectory.ErrConflict
			}
			if i.Generation == r.HighestGeneration {
				if !bytes.Equal(r.RequestDigest, digest) {
					return accountdirectory.ErrConflict
				}
				out = &dspb.RegisterRoleAdmissionResponse{Identity: proto.Clone(i).(*base.RoleAdmissionIdentity), ClosedThrough: r.ClosedThrough}
				return nil
			}
			// 同一个进程 UUID 的物理节点身份不能在后续登录里改派。
			old, _ := admissionIdentity(r)
			if old.SceneNodeId != i.SceneNodeId || old.ZoneId != i.ZoneId {
				return accountdirectory.ErrConflict
			}
			_, err = tx.ExecContext(ctx, "UPDATE account_role_admission SET highest_generation=?,identity_proto=?,request_digest=?,updated_ms="+dbNowMsSQL+" WHERE player_id=? AND scene_instance_id=?", i.Generation, raw, digest, i.PlayerId, []byte(i.SceneInstanceId))
		} else {
			_, err = tx.ExecContext(ctx, "INSERT INTO account_role_admission(player_id,scene_instance_id,highest_generation,closed_through,identity_proto,request_digest,required_owner_epoch,required_save_seq,close_digest,updated_ms) VALUES (?,?,?,0,?,?,0,0,'',"+dbNowMsSQL+")", i.PlayerId, []byte(i.SceneInstanceId), i.Generation, raw, digest)
		}
		if err != nil {
			return err
		}
		closed := uint64(0)
		if r != nil {
			closed = r.ClosedThrough
		}
		out = &dspb.RegisterRoleAdmissionResponse{Identity: proto.Clone(i).(*base.RoleAdmissionIdentity), ClosedThrough: closed}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *AccountRoleAdmissionStore) CloseAdmissions(ctx context.Context, in *dspb.CloseRoleAdmissionsRequest) (*dspb.CloseRoleAdmissionsResponse, error) {
	if !s.ready() {
		return nil, accountdirectory.ErrUnavailable
	}
	if in == nil || in.PlayerId == 0 || !accountdirectory.ValidSceneInstance(in.SceneInstanceId) || in.OwnerRevision == 0 || in.ThroughGeneration == 0 || !accountdirectory.ValidSaveFloor(in.RequiredOwnerEpoch, in.RequiredSaveSeq) {
		return nil, accountdirectory.ErrInvalid
	}
	digest, err := roleTradeDigest(in)
	if err != nil {
		return nil, err
	}
	var out *dspb.CloseRoleAdmissionsResponse
	err = s.directory.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := lockAdmissionRole(ctx, tx, in.PlayerId, in.OwnerRevision); err != nil {
			return err
		}
		r, err := readAdmission(ctx, tx, in.PlayerId, in.SceneInstanceId)
		if err != nil {
			return err
		}
		i, _ := admissionIdentity(r)
		if i.OwnerRevision != in.OwnerRevision || in.ThroughGeneration > r.HighestGeneration || in.ThroughGeneration < r.ClosedThrough {
			return accountdirectory.ErrConflict
		}
		if in.ThroughGeneration == r.ClosedThrough {
			if !bytes.Equal(r.CloseDigest, digest) {
				return accountdirectory.ErrConflict
			}
		} else {
			epoch, seq := r.RequiredOwnerEpoch, r.RequiredSaveSeq
			if saveFloorAfter(in.RequiredOwnerEpoch, in.RequiredSaveSeq, epoch, seq) {
				epoch, seq = in.RequiredOwnerEpoch, in.RequiredSaveSeq
			}
			if _, err = tx.ExecContext(ctx, "UPDATE account_role_admission SET closed_through=?,required_owner_epoch=?,required_save_seq=?,close_digest=?,updated_ms="+dbNowMsSQL+" WHERE player_id=? AND scene_instance_id=?", in.ThroughGeneration, epoch, seq, digest, in.PlayerId, []byte(in.SceneInstanceId)); err != nil {
				return err
			}
		}
		out = &dspb.CloseRoleAdmissionsResponse{ClosedThrough: in.ThroughGeneration}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// 快照与 Register/Close 都在同一 role 行锁内，固定本轮全集；不靠在线索引或 TTL 猜排空。
func snapshotRoleTradeHolders(ctx context.Context, tx *sql.Tx, r *dbpb.AccountRoleTrade) error {
	rows, err := tx.QueryContext(ctx, "SELECT scene_instance_id,highest_generation,closed_through,identity_proto,request_digest,required_owner_epoch,required_save_seq,close_digest,updated_ms FROM account_role_admission WHERE player_id=? ORDER BY scene_instance_id FOR UPDATE", r.PlayerId)
	if err != nil {
		return err
	}
	var holders []*base.RoleTradeSeal
	for rows.Next() {
		a := &dbpb.AccountRoleAdmission{PlayerId: r.PlayerId}
		if err = rows.Scan(&a.SceneInstanceId, &a.HighestGeneration, &a.ClosedThrough, &a.IdentityProto, &a.RequestDigest, &a.RequiredOwnerEpoch, &a.RequiredSaveSeq, &a.CloseDigest, &a.UpdatedMs); err != nil {
			break
		}
		i, e := admissionIdentity(a)
		if e != nil {
			err = e
			break
		}
		if saveFloorAfter(a.RequiredOwnerEpoch, a.RequiredSaveSeq, r.RequiredOwnerEpoch, r.RequiredSaveSeq) {
			r.RequiredOwnerEpoch, r.RequiredSaveSeq = a.RequiredOwnerEpoch, a.RequiredSaveSeq
		}
		if a.ClosedThrough == a.HighestGeneration {
			continue
		}
		if i.OwnerRevision != r.SellerOwnerRevision || len(holders) >= accountdirectory.MaxTradeHolders {
			err = accountdirectory.ErrConflict
			break
		}
		holders = append(holders, &base.RoleTradeSeal{ListingId: r.ListingId, PlayerId: r.PlayerId, OwnerRevision: r.SellerOwnerRevision, SceneInstanceId: a.SceneInstanceId, ThroughGeneration: a.HighestGeneration, SceneNodeId: i.SceneNodeId, ZoneId: i.ZoneId})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, seal := range holders {
		raw, err := proto.Marshal(seal)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO account_role_trade_holder(listing_id,scene_instance_id,seal_proto,drained,ack_digest,updated_ms) VALUES (?,?,?,0,'',"+dbNowMsSQL+")", r.ListingId, []byte(seal.SceneInstanceId), raw); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE account_role_trade SET required_owner_epoch=?,required_save_seq=? WHERE listing_id=?", r.RequiredOwnerEpoch, r.RequiredSaveSeq, r.ListingId)
	return err
}

func readRoleTradeHolders(ctx context.Context, tx *sql.Tx, listing uint64) ([]*dspb.AccountRoleTradeHolder, error) {
	rows, err := tx.QueryContext(ctx, "SELECT scene_instance_id,seal_proto,drained FROM account_role_trade_holder WHERE listing_id=? ORDER BY scene_instance_id LIMIT 257 FOR UPDATE", listing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dspb.AccountRoleTradeHolder
	for rows.Next() {
		var instance string
		var raw []byte
		var drained uint32
		if err = rows.Scan(&instance, &raw, &drained); err != nil {
			return nil, err
		}
		seal := &base.RoleTradeSeal{}
		if len(raw) > 512 || proto.Unmarshal(raw, seal) != nil || !accountdirectory.ValidTradeSeal(seal) || seal.SceneInstanceId != instance || seal.ListingId != listing || drained > 1 || len(out) >= accountdirectory.MaxTradeHolders {
			return nil, errors.New("角色冻结 holder 原件损坏或超过硬上限")
		}
		out = append(out, &dspb.AccountRoleTradeHolder{Seal: seal, Drained: drained == 1})
	}
	return out, rows.Err()
}

func (s *AccountRoleAdmissionStore) AckDrain(ctx context.Context, in *dspb.AckRoleTradeDrainRequest) error {
	if !s.ready() {
		return accountdirectory.ErrUnavailable
	}
	if in == nil || !accountdirectory.ValidTradeSeal(in.Seal) || !accountdirectory.ValidSaveFloor(in.RequiredOwnerEpoch, in.RequiredSaveSeq) {
		return accountdirectory.ErrInvalid
	}
	digest, err := roleTradeDigest(in)
	if err != nil {
		return err
	}
	seal := in.Seal
	// 先普通读不可变卖家，再遵守账号→listing→role 的既有锁序，不能从 role 回头锁账号。
	var seller string
	if err = s.directory.db.QueryRowContext(ctx, "SELECT seller_account FROM account_role_trade WHERE listing_id=?", seal.ListingId).Scan(&seller); errors.Is(err, sql.ErrNoRows) {
		return accountdirectory.ErrMissing
	} else if err != nil {
		return err
	}
	return s.directory.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockAccounts(ctx, tx, seller); err != nil {
			return err
		}
		r, err := readRoleTrade(ctx, tx, seal.ListingId)
		if err != nil {
			return err
		}
		if r.SellerAccount != seller || r.PlayerId != seal.PlayerId || r.SellerOwnerRevision != seal.OwnerRevision {
			return accountdirectory.ErrConflict
		}
		// 终局重放可能已经发生下一次过户；先锁当前行但不拿旧修订阻断原 ACK 回放。
		owned, err := scanOwnership(tx.QueryRowContext(ctx, "SELECT account,role_proto,owner_revision,trade_listing_id FROM account_role WHERE player_id=? FOR UPDATE", seal.PlayerId), seal.PlayerId)
		if err != nil {
			return err
		}
		a, err := readAdmission(ctx, tx, seal.PlayerId, seal.SceneInstanceId)
		if err != nil {
			return err
		}
		// 终局 ACK 也必须先比原持久摘要；旧请求不得推进新轮水位。
		var raw, priorDigest []byte
		var drained uint32
		if err = tx.QueryRowContext(ctx, "SELECT seal_proto,drained,ack_digest FROM account_role_trade_holder WHERE listing_id=? AND scene_instance_id=? FOR UPDATE", seal.ListingId, []byte(seal.SceneInstanceId)).Scan(&raw, &drained, &priorDigest); errors.Is(err, sql.ErrNoRows) {
			return accountdirectory.ErrMissing
		} else if err != nil {
			return err
		}
		priorSeal := &base.RoleTradeSeal{}
		if len(raw) > 512 || proto.Unmarshal(raw, priorSeal) != nil || !proto.Equal(priorSeal, seal) || drained > 1 {
			return accountdirectory.ErrConflict
		}
		if drained == 1 {
			if !bytes.Equal(priorDigest, digest) {
				return accountdirectory.ErrConflict
			}
			return nil
		}
		if r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING) && r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING) {
			return accountdirectory.ErrConflict
		}
		if owned.OwnerRevision != seal.OwnerRevision || owned.TradeListingId != seal.ListingId {
			return accountdirectory.ErrConflict
		}
		i, _ := admissionIdentity(a)
		if a.HighestGeneration != seal.ThroughGeneration || i.OwnerRevision != seal.OwnerRevision || i.SceneNodeId != seal.SceneNodeId || i.ZoneId != seal.ZoneId {
			return accountdirectory.ErrConflict
		}
		epoch, seq := a.RequiredOwnerEpoch, a.RequiredSaveSeq
		if saveFloorAfter(in.RequiredOwnerEpoch, in.RequiredSaveSeq, epoch, seq) {
			epoch, seq = in.RequiredOwnerEpoch, in.RequiredSaveSeq
		}
		if _, err = tx.ExecContext(ctx, "UPDATE account_role_admission SET closed_through=highest_generation,required_owner_epoch=?,required_save_seq=?,updated_ms="+dbNowMsSQL+" WHERE player_id=? AND scene_instance_id=?", epoch, seq, seal.PlayerId, []byte(seal.SceneInstanceId)); err != nil {
			return err
		}
		if saveFloorAfter(epoch, seq, r.RequiredOwnerEpoch, r.RequiredSaveSeq) {
			if _, err = tx.ExecContext(ctx, "UPDATE account_role_trade SET required_owner_epoch=?,required_save_seq=?,updated_ms="+dbNowMsSQL+" WHERE listing_id=?", epoch, seq, seal.ListingId); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, "UPDATE account_role_trade_holder SET drained=1,ack_digest=?,updated_ms="+dbNowMsSQL+" WHERE listing_id=? AND scene_instance_id=?", digest, seal.ListingId, []byte(seal.SceneInstanceId))
		return err
	})
}

// PlanCrashedTradeHolder与apply共用完整SQL校验，只读回滚，不创建收据/推进水位。
// 唯一关闭依据是同SQL源内永久维护收据及完整双行E>U；不是Scene的跨实例关闭RPC。
func (s *AccountRoleAdmissionStore) PlanCrashedTradeHolder(ctx context.Context, in *dbpb.AccountRoleAdmissionRecovery_Request, verifier *SQLRoleFreezeVerifier, target MaintenanceRecoveryTarget) (*dbpb.AccountRoleAdmissionRecovery, error) {
	if !target.Valid() {
		return nil, accountdirectory.ErrInvalid
	}
	return s.recoverCrashedTradeHolder(ctx, in, verifier, false, target, nil)
}

// ApplyCrashedTradeHolder仅接受审定plan的原SQL摘要，并在相同事务重核独立维护目标marker。
// 提交错误含结局未知，绝不伪成功；同原请求重入读永久原件，不推进新入场/归属。
func (s *AccountRoleAdmissionStore) ApplyCrashedTradeHolder(ctx context.Context, in *dbpb.AccountRoleAdmissionRecovery_Request, verifier *SQLRoleFreezeVerifier, target MaintenanceRecoveryTarget, expectedProof []byte) (*dbpb.AccountRoleAdmissionRecovery, error) {
	if !target.Valid() || len(expectedProof) != 32 {
		return nil, accountdirectory.ErrInvalid
	}
	return s.recoverCrashedTradeHolder(ctx, in, verifier, true, target, expectedProof)
}

func (s *AccountRoleAdmissionStore) recoverCrashedTradeHolder(ctx context.Context, in *dbpb.AccountRoleAdmissionRecovery_Request, verifier *SQLRoleFreezeVerifier, apply bool, target MaintenanceRecoveryTarget, expectedProof []byte) (*dbpb.AccountRoleAdmissionRecovery, error) {
	if !s.ready() || verifier == nil {
		return nil, accountdirectory.ErrUnavailable
	}
	if in == nil || len(in.ProtoReflect().GetUnknown()) != 0 || len(in.RequestId) != 32 || !accountdirectory.ValidAccount(in.SellerAccount) ||
		!accountdirectory.ValidTradeSeal(in.Seal) || !accountdirectory.ValidAdmission(in.Admission) || len(in.AdmissionRequestDigest) != 32 ||
		len(in.MaintenanceReceiptId) != 32 || len(in.MaintenanceRequestDigest) != 32 ||
		!accountdirectory.ValidSaveFloor(in.ExpectedRequiredOwnerEpoch, in.ExpectedRequiredSaveSeq) {
		return nil, accountdirectory.ErrInvalid
	}
	seal, identity := in.Seal, in.Admission
	if len(seal.ProtoReflect().GetUnknown()) != 0 || len(identity.ProtoReflect().GetUnknown()) != 0 || identity.PlayerId != seal.PlayerId || identity.SceneInstanceId != seal.SceneInstanceId ||
		identity.Generation != seal.ThroughGeneration || identity.OwnerRevision != seal.OwnerRevision || identity.SceneNodeId != seal.SceneNodeId || identity.ZoneId != seal.ZoneId || in.ExpectedClosedThrough >= seal.ThroughGeneration {
		return nil, accountdirectory.ErrInvalid
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(in)
	if err != nil || len(raw) > 4096 {
		return nil, accountdirectory.ErrInvalid
	}
	hash := sha256.New()
	writeProofPart(hash, []byte("account-role-admission-recovery-v1"))
	writeProofPart(hash, raw)
	digest := hash.Sum(nil)
	// 原件只读快速重放；损坏或换正文绝不补写原件/修改当前业务行。
	if prior, err := readAdmissionRecovery(ctx, s.directory.db, in.RequestId); err == nil {
		if !bytes.Equal(prior.RequestProto, raw) || !bytes.Equal(prior.RequestDigest, digest) || len(expectedProof) > 0 && !bytes.Equal(expectedProof, prior.SqlProofDigest) {
			return nil, accountdirectory.ErrConflict
		}
		{
			tx, err := s.directory.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			if err != nil {
				return nil, err
			}
			defer tx.Rollback()
			if err := checkRecoveryMaintenanceTarget(ctx, tx, target, in.Seal.ZoneId, s.directory.schema, verifier); err != nil {
				return nil, err
			}
		}
		return prior, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var out *dbpb.AccountRoleAdmissionRecovery
	err = s.directory.transaction(ctx, func(tx *sql.Tx) error {
		{
			if err := checkRecoveryMaintenanceTarget(ctx, tx, target, in.Seal.ZoneId, s.directory.schema, verifier); err != nil {
				return err
			}
		}
		if err := lockAccounts(ctx, tx, in.SellerAccount); err != nil {
			return err
		}
		r, err := readRoleTrade(ctx, tx, seal.ListingId)
		if err != nil {
			return err
		}
		owned, err := lockAdmissionRole(ctx, tx, seal.PlayerId, seal.OwnerRevision)
		if err != nil {
			return err
		}
		a, err := readAdmission(ctx, tx, seal.PlayerId, seal.SceneInstanceId)
		if err != nil {
			return err
		}
		var holderRaw, holderDigest []byte
		var drained uint32
		if err := tx.QueryRowContext(ctx, "SELECT seal_proto,drained,ack_digest FROM account_role_trade_holder WHERE listing_id=? AND scene_instance_id=? FOR UPDATE", seal.ListingId, []byte(seal.SceneInstanceId)).Scan(&holderRaw, &drained, &holderDigest); err != nil {
			return err
		}
		// 同账号并发重入在原锁序之后核永久原件；不会把另一次常规Ack当本工具成功。
		if prior, err := readAdmissionRecovery(ctx, tx, in.RequestId); err == nil {
			if !bytes.Equal(prior.RequestProto, raw) || !bytes.Equal(prior.RequestDigest, digest) || len(expectedProof) > 0 && !bytes.Equal(expectedProof, prior.SqlProofDigest) {
				return accountdirectory.ErrConflict
			}
			out = prior
			if !apply {
				return errRecoveryPlanReadOnly
			}
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		priorSeal := &base.RoleTradeSeal{}
		actualIdentity, err := admissionIdentity(a)
		if err != nil || proto.Unmarshal(holderRaw, priorSeal) != nil || !proto.Equal(priorSeal, seal) || drained != 0 || len(holderDigest) != 0 ||
			r.SellerAccount != in.SellerAccount || r.PlayerId != seal.PlayerId || r.SellerOwnerRevision != seal.OwnerRevision ||
			r.State != uint32(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING) ||
			owned.Account != in.SellerAccount || owned.TradeListingId != seal.ListingId || !proto.Equal(actualIdentity, identity) || !bytes.Equal(a.RequestDigest, in.AdmissionRequestDigest) ||
			a.ClosedThrough != in.ExpectedClosedThrough || a.RequiredOwnerEpoch != in.ExpectedRequiredOwnerEpoch || a.RequiredSaveSeq != in.ExpectedRequiredSaveSeq {
			return accountdirectory.ErrConflict
		}
		floorE, floorQ, sqlDigest, err := verifier.verifyMaintenanceRecovery(ctx, tx, s.directory.schema, in)
		if err != nil {
			return err
		}
		if len(expectedProof) > 0 && !bytes.Equal(expectedProof, sqlDigest) {
			return accountdirectory.ErrConflict
		}
		if saveFloorAfter(a.RequiredOwnerEpoch, a.RequiredSaveSeq, floorE, floorQ) || saveFloorAfter(r.RequiredOwnerEpoch, r.RequiredSaveSeq, floorE, floorQ) {
			return accountdirectory.ErrConflict
		}
		out = &dbpb.AccountRoleAdmissionRecovery{RequestId: bytes.Clone(in.RequestId), ListingId: seal.ListingId, PlayerId: seal.PlayerId, SceneInstanceId: seal.SceneInstanceId, ThroughGeneration: seal.ThroughGeneration, OwnerRevision: seal.OwnerRevision, RequestDigest: digest, RequestProto: raw, MaintenanceReceiptId: bytes.Clone(in.MaintenanceReceiptId), MaintenanceRequestDigest: bytes.Clone(in.MaintenanceRequestDigest), RequiredOwnerEpoch: floorE, RequiredSaveSeq: floorQ, SqlProofDigest: sqlDigest}
		if err := tx.QueryRowContext(ctx, "SELECT "+dbNowMsSQL).Scan(&out.RecoveredMs); err != nil {
			return err
		}
		if !apply {
			return errRecoveryPlanReadOnly
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO account_role_admission_recovery(request_id,listing_id,player_id,scene_instance_id,through_generation,owner_revision,request_digest,request_proto,maintenance_receipt_id,maintenance_request_digest,required_owner_epoch,required_save_seq,sql_proof_digest,recovered_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)", out.RequestId, out.ListingId, out.PlayerId, []byte(out.SceneInstanceId), out.ThroughGeneration, out.OwnerRevision, out.RequestDigest, out.RequestProto, out.MaintenanceReceiptId, out.MaintenanceRequestDigest, floorE, floorQ, sqlDigest, out.RecoveredMs); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE account_role_admission SET closed_through=?,required_owner_epoch=?,required_save_seq=?,close_digest=?,updated_ms=? WHERE player_id=? AND scene_instance_id=?", seal.ThroughGeneration, floorE, floorQ, digest, out.RecoveredMs, seal.PlayerId, []byte(seal.SceneInstanceId)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE account_role_trade SET required_owner_epoch=?,required_save_seq=?,updated_ms=? WHERE listing_id=?", floorE, floorQ, out.RecoveredMs, seal.ListingId); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE account_role_trade_holder SET drained=1,ack_digest=?,updated_ms=? WHERE listing_id=? AND scene_instance_id=?", digest, out.RecoveredMs, seal.ListingId, []byte(seal.SceneInstanceId))
		return err
	})
	if errors.Is(err, errRecoveryPlanReadOnly) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

var errRecoveryPlanReadOnly = errors.New("旧holder维护plan完成只读回滚")

// MaintenanceRecoveryTarget只限定运维预置的物理目标，不代表操作人权限。
// 权限来自维护命令和受限凭据；此记录绝不让debug_import取得写权。
type MaintenanceRecoveryTarget struct {
	ID      string `json:"target_nonce"`
	SQLAddr string `json:"physical_addr"`
}

func (t MaintenanceRecoveryTarget) Valid() bool {
	host, port, err := net.SplitHostPort(t.SQLAddr)
	n, portErr := strconv.ParseUint(port, 10, 16)
	return len(t.ID) == 32 && strings.Trim(t.ID, "0123456789abcdef") == "" && t.ID != strings.Repeat("0", 32) &&
		err == nil && host != "" && !strings.ContainsAny(host, " /\\@|\r\n\t") && portErr == nil && n != 0 && strconv.FormatUint(n, 10) == port
}

// 标记由有权运维预置；工具只核验并持行锁，不建表、不续期、不覆盖。
// 固定全局库和显式storage白名单对应的行库，按库名锁序核完整集合。
func checkRecoveryMaintenanceTarget(ctx context.Context, tx *sql.Tx, target MaintenanceRecoveryTarget, zone uint32, global string, verifier *SQLRoleFreezeVerifier) error {
	if !target.Valid() || zone == 0 || verifier == nil || len(verifier.allowed) == 0 {
		return accountdirectory.ErrInvalid
	}
	schemas := []string{global}
	for storage := range verifier.allowed {
		name, ok := placement.StoreDBName(storage)
		if !ok {
			return accountdirectory.ErrInvalid
		}
		schemas = append(schemas, name)
	}
	slices.Sort(schemas)
	schemas = slices.Compact(schemas)
	for _, schema := range schemas {
		var nonce, address string
		err := tx.QueryRowContext(ctx, "SELECT target_nonce,physical_addr FROM `"+schema+"`.maintenance_recovery_target WHERE purpose=? AND zone_id=? AND schema_name=? FOR UPDATE", "admission", zone, []byte(schema)).Scan(&nonce, &address)
		if errors.Is(err, sql.ErrNoRows) {
			return accountdirectory.ErrConflict
		}
		if err != nil {
			return err
		}
		if nonce != target.ID || address != target.SQLAddr {
			return accountdirectory.ErrConflict
		}
	}
	return nil
}

type recoveryRowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readAdmissionRecovery(ctx context.Context, q recoveryRowReader, id []byte) (*dbpb.AccountRoleAdmissionRecovery, error) {
	r := &dbpb.AccountRoleAdmissionRecovery{RequestId: bytes.Clone(id)}
	err := q.QueryRowContext(ctx, "SELECT listing_id,player_id,scene_instance_id,through_generation,owner_revision,request_digest,request_proto,maintenance_receipt_id,maintenance_request_digest,required_owner_epoch,required_save_seq,sql_proof_digest,recovered_ms FROM account_role_admission_recovery WHERE request_id=?", id).Scan(&r.ListingId, &r.PlayerId, &r.SceneInstanceId, &r.ThroughGeneration, &r.OwnerRevision, &r.RequestDigest, &r.RequestProto, &r.MaintenanceReceiptId, &r.MaintenanceRequestDigest, &r.RequiredOwnerEpoch, &r.RequiredSaveSeq, &r.SqlProofDigest, &r.RecoveredMs)
	if err != nil {
		return nil, err
	}
	stored := &dbpb.AccountRoleAdmissionRecovery_Request{}
	if len(r.RequestProto) > 4096 || proto.Unmarshal(r.RequestProto, stored) != nil || len(r.RequestDigest) != 32 || len(r.SqlProofDigest) != 32 ||
		!bytes.Equal(stored.RequestId, id) || stored.GetSeal().GetListingId() != r.ListingId || stored.GetSeal().GetPlayerId() != r.PlayerId || stored.GetSeal().GetSceneInstanceId() != r.SceneInstanceId || stored.GetSeal().GetThroughGeneration() != r.ThroughGeneration || stored.GetSeal().GetOwnerRevision() != r.OwnerRevision ||
		!bytes.Equal(stored.MaintenanceReceiptId, r.MaintenanceReceiptId) || !bytes.Equal(stored.MaintenanceRequestDigest, r.MaintenanceRequestDigest) || !accountdirectory.ValidSaveFloor(r.RequiredOwnerEpoch, r.RequiredSaveSeq) || r.RequiredOwnerEpoch == 0 {
		return nil, errors.New("永久旧holder恢复收据损坏")
	}
	h := sha256.New()
	writeProofPart(h, []byte("account-role-admission-recovery-v1"))
	writeProofPart(h, r.RequestProto)
	if !bytes.Equal(h.Sum(nil), r.RequestDigest) {
		return nil, errors.New("永久旧holder恢复摘要不符")
	}
	return r, nil
}
