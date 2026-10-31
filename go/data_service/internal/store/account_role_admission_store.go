package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"slices"

	base "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"

	"google.golang.org/protobuf/proto"
)

const AccountRoleAdmissionTableName = "account_role_admission"
const AccountRoleTradeHolderTableName = "account_role_trade_holder"

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
			// 这个实例被判过死,迟到的真实 Close 不算冲突:它活着也已经写不了存档了。
			if !bytes.Equal(r.CloseDigest, digest) && !bytes.Equal(r.CloseDigest, deadHolderAckDigest) {
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
			// 这个持有者被判过死,迟到的真实 ACK 不算冲突:它活着也已经写不了存档了。
			if !bytes.Equal(priorDigest, digest) && !bytes.Equal(priorDigest, deadHolderAckDigest) {
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
