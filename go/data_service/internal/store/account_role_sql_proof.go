package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"sort"
	"strconv"

	"google.golang.org/protobuf/proto"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/generated/persistschema"
	"shared/placement"
	"shared/savebarrier"
)

// SQLRoleFreezeVerifier 只使用同一个 TiDB 权威池/事务。配置白名单不能改写权威落点，
// 仅限制允许跨 schema 强读的 storage_id；缺权限、缺行、旧围栏一律拒绝。
type SQLRoleFreezeVerifier struct{ allowed map[uint32]bool }

func NewSQLRoleFreezeVerifier(storageIDs []uint32) (*SQLRoleFreezeVerifier, error) {
	if len(storageIDs) == 0 || len(storageIDs) > 1024 {
		return nil, accountdirectory.ErrInvalid
	}
	v := &SQLRoleFreezeVerifier{allowed: make(map[uint32]bool, len(storageIDs))}
	for _, id := range storageIDs {
		if _, ok := placement.StoreDBName(id); !ok || v.allowed[id] {
			return nil, accountdirectory.ErrInvalid
		}
		v.allowed[id] = true
	}
	return v, nil
}

func (v *SQLRoleFreezeVerifier) VerifyFrozen(ctx context.Context, tx *sql.Tx, r *dbpb.AccountRoleTrade) (*dspb.AccountRoleFreezeEvidence, error) {
	if v == nil || tx == nil || r == nil || !accountdirectory.ValidSaveFloor(r.RequiredOwnerEpoch, r.RequiredSaveSeq) {
		return nil, accountdirectory.ErrUnavailable
	}
	holders, err := readRoleTradeHolders(ctx, tx, r.ListingId)
	if err != nil {
		return nil, err
	}
	for _, h := range holders {
		if !h.Drained || h.Seal.PlayerId != r.PlayerId || h.Seal.OwnerRevision != r.SellerOwnerRevision {
			return nil, accountdirectory.ErrConflict
		}
	}
	// 路由锁先于两张 zone 行，阻止读证据期间迁移切库。只认全局 SQL，不回落 Redis。
	var home, storage, version sql.NullInt64
	var frozen sql.NullString
	var rev uint64
	err = tx.QueryRowContext(ctx, "SELECT home_zone,storage_id,placement_version,frozen_run_id,rev FROM player_route WHERE route_shard=? AND player_id=? FOR UPDATE", placement.RouteRowShard(r.PlayerId), r.PlayerId).Scan(&home, &storage, &version, &frozen, &rev)
	if err != nil {
		return nil, fmt.Errorf("角色存盘证据缺权威落点: %w", err)
	}
	if !home.Valid || home.Int64 < 1 || home.Int64 > int64(placement.MaxZoneStorageID) || rev == 0 || frozen.Valid {
		return nil, accountdirectory.ErrConflict
	}
	storageID := uint32(home.Int64)
	if storage.Valid {
		if storage.Int64 < 1 || storage.Int64 > int64(^uint32(0)) || !version.Valid || version.Int64 < 1 {
			return nil, accountdirectory.ErrConflict
		}
		storageID = uint32(storage.Int64)
	} else if version.Valid {
		return nil, accountdirectory.ErrConflict
	}
	if !v.allowed[storageID] {
		return nil, accountdirectory.ErrUnavailable
	}
	schema, ok := placement.StoreDBName(storageID)
	if !ok {
		return nil, accountdirectory.ErrUnavailable
	}
	digest := sha256.New()
	writeProofPart(digest, []byte("account-role-sql-proof-v1"))
	writeProofPart(digest, []byte(schema))
	epoch, seq, gen, err := hashPlayerSQLRow(ctx, tx, schema, "player_database", r.PlayerId, digest)
	if err != nil {
		return nil, err
	}
	epoch1, seq1, gen1, err := hashPlayerSQLRow(ctx, tx, schema, "player_database_1", r.PlayerId, digest)
	if err != nil {
		return nil, err
	}
	if epoch != epoch1 || seq != seq1 || gen != gen1 || saveFloorAfter(r.RequiredOwnerEpoch, r.RequiredSaveSeq, epoch, seq) {
		return nil, accountdirectory.ErrConflict
	}
	kind := dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_SCENE
	// SCENE是非初始/非零种子的SQL存档分类，不证明逐holder收到Scene RPC回包。
	// 旧holder也可由本地维护永久收据退休；其recovery域ack_digest与原RPC Ack域不同，来源查恢复原件。
	if epoch == savebarrier.InitialOwnerEpoch && seq == savebarrier.InitialSaveSeq {
		if r.RequiredOwnerEpoch != 0 || r.RequiredSaveSeq != 0 || len(holders) != 0 {
			return nil, accountdirectory.ErrConflict
		}
		var marker, revision uint64
		if err := tx.QueryRowContext(ctx, "SELECT initial_player_commit,owner_revision FROM account_role WHERE player_id=? FOR UPDATE", r.PlayerId).Scan(&marker, &revision); err != nil {
			return nil, err
		}
		if marker != 1 || revision != 1 || r.SellerOwnerRevision != revision {
			return nil, accountdirectory.ErrConflict
		}
		first, err := initialSQLRowMatches(ctx, tx, schema, "player_database", r.PlayerId, (&dbpb.PlayerDatabase{}).ProtoReflect().Descriptor().Fields())
		if err != nil {
			return nil, err
		}
		second, err := initialSQLRowMatches(ctx, tx, schema, "player_database_1", r.PlayerId, (&dbpb.PlayerDatabase_1{}).ProtoReflect().Descriptor().Fields())
		if err != nil {
			return nil, err
		}
		if !first || !second {
			return nil, accountdirectory.ErrConflict
		}
		kind = dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_INITIAL_COMMIT
	} else if epoch == 0 && seq == 0 {
		// 只有实际未进场种子、没有历史存盘下限、没有本轮原生 holder 才能采用此分支。
		if r.RequiredOwnerEpoch != 0 || r.RequiredSaveSeq != 0 || len(holders) != 0 {
			return nil, accountdirectory.ErrConflict
		}
		kind = dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_NO_ADMISSION
	}
	return &dspb.AccountRoleFreezeEvidence{PlayerId: r.PlayerId, ListingId: r.ListingId, OwnerRevision: r.SellerOwnerRevision, OwnerEpoch: epoch, SaveSeq: seq, PersistedDigest: digest.Sum(nil), Kind: kind}, nil
}

func writeProofPart(h hash.Hash, part []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(part)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(part)
}

// 两表均只读一行；摘要保留所有 SQL 列的原字节（包括未知 PB 字段所在 blob），
// 不用解码后重写的旧模型代替原件。按列名排序，避免新老建表列序差异影响摘要。
func hashPlayerSQLRow(ctx context.Context, tx *sql.Tx, schema, table string, player uint64, digest hash.Hash) (uint64, uint64, uint64, error) {
	return hashPlayerSQLRowWithBusiness(ctx, tx, schema, table, player, digest, nil)
}

func hashPlayerSQLRowWithBusiness(ctx context.Context, tx *sql.Tx, schema, table string, player uint64, digest, business hash.Hash) (uint64, uint64, uint64, error) {
	// schema 仅从 StoreDBName 派生，table 仅为上面两个常量，不接收 caller 标识符。
	rows, err := tx.QueryContext(ctx, "SELECT * FROM `"+schema+"`.`"+table+"` WHERE player_id=? FOR UPDATE", player)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil || len(columns) == 0 || len(columns) > 256 {
		return 0, 0, 0, errors.New("玩家 SQL 证据列形状异常")
	}
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return 0, 0, 0, err
		}
		return 0, 0, 0, accountdirectory.ErrMissing
	}
	values := make([]sql.RawBytes, len(columns))
	dest := make([]any, len(columns))
	for n := range values {
		dest[n] = &values[n]
	}
	if err = rows.Scan(dest...); err != nil {
		return 0, 0, 0, err
	}
	byName := make(map[string][]byte, len(columns))
	total := 0
	for n, name := range columns {
		if _, duplicate := byName[name]; duplicate {
			return 0, 0, 0, errors.New("玩家 SQL 证据重复列")
		}
		byName[name] = values[n]
		total += len(values[n])
	}
	// 证据读取不是批量导出；超过正常玩家行预算显式拒绝，避免持目录锁无限读 blob。
	if total > 16<<20 {
		return 0, 0, 0, errors.New("玩家 SQL 证据单行超过16MiB")
	}
	readUint := func(name string) (uint64, error) {
		raw, ok := byName[name]
		if !ok || len(raw) == 0 {
			return 0, errors.New("玩家 SQL 证据缺版本列")
		}
		n, err := strconv.ParseUint(string(raw), 10, 64)
		if err != nil || strconv.FormatUint(n, 10) != string(raw) {
			return 0, errors.New("玩家 SQL 证据版本不是规范uint")
		}
		return n, nil
	}
	id, err := readUint("player_id")
	if err != nil || id != player {
		return 0, 0, 0, errors.New("玩家 SQL 证据身份不符")
	}
	epoch, err := readUint("fence_owner_epoch")
	if err != nil {
		return 0, 0, 0, err
	}
	seq, err := readUint("fence_save_seq")
	if err != nil || !accountdirectory.ValidSaveFloor(epoch, seq) {
		return 0, 0, 0, errors.New("玩家 SQL 证据围栏非法")
	}
	gen, err := readUint("persist_schema_gen")
	if gen == 0 {
		gen = 1 // 与持久覆盖世代的正式协议一致，0 是未盖戳种子。
	}
	if err != nil || gen > uint64(persistschema.Gen) {
		return 0, 0, 0, errors.New("玩家 SQL 证据世代超前或非法")
	}
	writeProofPart(digest, []byte(table))
	// 与维护工具同一逐列协议；保SQL原列序及全部非围栏列，含NULL、未知blob、原世代戳。
	if business != nil {
		for _, name := range columns {
			if name == "fence_owner_epoch" || name == "fence_save_seq" {
				continue
			}
			writeProofPart(business, []byte(name))
			if byName[name] == nil {
				_, _ = business.Write([]byte{0})
			} else {
				_, _ = business.Write([]byte{1})
				writeProofPart(business, byName[name])
			}
		}
	}
	sort.Strings(columns)
	for _, name := range columns {
		writeProofPart(digest, []byte(name))
		if byName[name] == nil {
			_, _ = digest.Write([]byte{0})
		} else {
			_, _ = digest.Write([]byte{1})
			writeProofPart(digest, byName[name])
		}
	}
	if rows.Next() {
		return 0, 0, 0, errors.New("玩家 SQL 证据主键重复")
	}
	return epoch, seq, gen, rows.Err()
}

// 维护撤销是永久SQL阶段收据，复核时只访问同一权威SQL事务，禁止Redis/Scene/Kafka I/O。
func (v *SQLRoleFreezeVerifier) verifyMaintenanceRecovery(ctx context.Context, tx *sql.Tx, schema string, in *dbpb.AccountRoleAdmissionRecovery_Request) (uint64, uint64, []byte, error) {
	var player uint64
	var phase dbpb.SaveBarrierMaintenanceReceipt_Phase
	var planRaw, requestDigest []byte
	if err := tx.QueryRowContext(ctx, "SELECT player_id,request_digest,plan_proto,phase FROM save_barrier_maintenance_receipt WHERE receipt_id=?", in.MaintenanceReceiptId).Scan(&player, &requestDigest, &planRaw, &phase); err != nil {
		return 0, 0, nil, err
	}
	p := &dbpb.SaveBarrierMaintenanceReceipt_PromotionPlan{}
	if len(planRaw) > 16384 || proto.Unmarshal(planRaw, p) != nil || len(p.ProtoReflect().GetUnknown()) != 0 || p.Format != "player-save-fence-promotion-v1" || p.PlayerId != in.Seal.PlayerId || player != p.PlayerId ||
		p.RouteSchema != schema || !bytes.Equal(p.ReceiptId, in.MaintenanceReceiptId) || !bytes.Equal(requestDigest, in.MaintenanceRequestDigest) || phase != dbpb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED || !p.WritersStopped || !p.QueuesDrained || len(p.AuditEvidenceSha256) != 32 ||
		p.TargetOwner == 0 || p.TargetOwner <= p.AuditedOwnerUpperBound || p.TargetOwner <= p.OriginalOwner || p.TargetSaveSeq != 1 || !v.allowed[p.StorageId] || p.AuditedOwnerUpperBound < in.Admission.OwnerEpoch {
		return 0, 0, nil, accountdirectory.ErrConflict
	}
	h := sha256.New()
	writeProofPart(h, []byte("player-save-fence-promotion-v1"))
	writeProofPart(h, planRaw)
	if !bytes.Equal(h.Sum(nil), requestDigest) {
		return 0, 0, nil, errors.New("永久维护计划摘要损坏")
	}
	rowSchema, ok := placement.StoreDBName(p.StorageId)
	if !ok || rowSchema != p.RowSchema {
		return 0, 0, nil, accountdirectory.ErrInvalid
	}
	var watermark uint64
	var generation string
	var writable uint64
	if err := tx.QueryRowContext(ctx, "SELECT watermark_ms FROM route_reconcile WHERE id=1 FOR UPDATE").Scan(&watermark); err != nil {
		return 0, 0, nil, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT generation,writable FROM route_meta WHERE id=1 FOR UPDATE").Scan(&generation, &writable); err != nil {
		return 0, 0, nil, err
	}
	var home, storage, version sql.NullInt64
	var frozen sql.NullString
	var revision uint64
	if err := tx.QueryRowContext(ctx, "SELECT home_zone,storage_id,placement_version,frozen_run_id,rev FROM player_route WHERE route_shard=? AND player_id=? FOR UPDATE", placement.RouteRowShard(p.PlayerId), p.PlayerId).Scan(&home, &storage, &version, &frozen, &revision); err != nil {
		return 0, 0, nil, err
	}
	if !home.Valid || home.Int64 != int64(p.HomeZone) || p.HomeZone != in.Seal.ZoneId || frozen.Valid && frozen.String != "" || storage.Valid != version.Valid || revision != p.RouteRevision || generation != p.RouteGeneration || watermark != p.RouteWatermark || writable != 0 {
		return 0, 0, nil, accountdirectory.ErrConflict
	}
	if storage.Valid {
		if storage.Int64 != int64(p.StorageId) || version.Int64 != int64(p.PlacementVersion) {
			return 0, 0, nil, accountdirectory.ErrConflict
		}
	} else if p.StorageId != p.HomeZone || p.PlacementVersion != 0 {
		return 0, 0, nil, accountdirectory.ErrConflict
	}
	full := sha256.New()
	writeProofPart(full, []byte("account-role-maintenance-sql-proof-v1"))
	writeProofPart(full, requestDigest)
	for n, table := range []string{"player_database", "player_database_1"} {
		business := sha256.New()
		e, q, gen, err := hashPlayerSQLRowWithBusiness(ctx, tx, rowSchema, table, p.PlayerId, full, business)
		wantGen := uint64(p.SchemaGen)
		if wantGen == 0 {
			wantGen = 1
		}
		want := p.FirstBusinessSha256
		if n == 1 {
			want = p.SecondBusinessSha256
		}
		if err != nil {
			return 0, 0, nil, err
		}
		if e != p.TargetOwner || q != p.TargetSaveSeq || gen != wantGen || !bytes.Equal(business.Sum(nil), want) {
			return 0, 0, nil, accountdirectory.ErrConflict
		}
	}
	// 锁序和promotion一致：route→两玩家行→永久维护收据。上面的普通读取只取不可变正文。
	var againDigest, againRaw []byte
	var againPlayer uint64
	var againPhase dbpb.SaveBarrierMaintenanceReceipt_Phase
	if err := tx.QueryRowContext(ctx, "SELECT player_id,request_digest,plan_proto,phase FROM save_barrier_maintenance_receipt WHERE receipt_id=? FOR UPDATE", p.ReceiptId).Scan(&againPlayer, &againDigest, &againRaw, &againPhase); err != nil {
		return 0, 0, nil, err
	}
	if againPlayer != player || againPhase != dbpb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED || !bytes.Equal(againRaw, planRaw) || !bytes.Equal(againDigest, requestDigest) {
		return 0, 0, nil, accountdirectory.ErrConflict
	}
	return p.TargetOwner, p.TargetSaveSeq, full.Sum(nil), nil
}
