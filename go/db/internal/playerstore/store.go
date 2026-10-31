package playerstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	storepb "proto/player_store"
)

const (
	ownerKind = int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER)

	// maxInstanceBytes 是实例标识的上限。scene_instance_id 是 36 字节的 uuid,这里只防传错。
	maxInstanceBytes = 64

	sqlSelectOwner = "SELECT owner_epoch,save_seq,schema_gen,holder_instance FROM player_store WHERE player_id=? AND kind=0 FOR UPDATE"
	sqlClaimOwner  = "UPDATE player_store SET owner_epoch=?,holder_instance=?,released=0 WHERE player_id=? AND kind=0"
	sqlSelectRows  = "SELECT kind,payload FROM player_store WHERE player_id=? AND kind>0"
	sqlSaveHead    = "INSERT INTO player_store (player_id,kind,owner_epoch,save_seq,schema_gen,holder_instance,released,payload) VALUES "
	sqlSaveRow     = "(?,?,?,?,?,?,?,?)"
	sqlSaveTail    = " ON DUPLICATE KEY UPDATE owner_epoch=VALUES(owner_epoch),save_seq=VALUES(save_seq),schema_gen=VALUES(schema_gen)," +
		"holder_instance=VALUES(holder_instance),released=VALUES(released),payload=VALUES(payload)"
	sqlSelectColumns = "SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='player_store'"
)

// requiredColumns 是 player_store 的全部列,要与 proto 的 message player_store 一致。
var requiredColumns = []string{"player_id", "kind", "owner_epoch", "save_seq", "schema_gen", "holder_instance", "released", "payload"}

const (
	// MaxClaimResponseBytes 是认领响应的上限,与 C++ 的 player_store_path::kMaxClaimResponseBytes 相同。
	// 响应是一个玩家的全部行。存盘只限单笔,行是多笔累计起来的,所以总量要靠「每行都到上限也装得下」来保证:
	// scene 收不下就按结果未知重试,这个角色再也进不来。
	MaxClaimResponseBytes = 8 << 20
	// claimResponseOverheadBytes 给响应里 payload 之外的内容留的余量。
	claimResponseOverheadBytes = 64 << 10
)

// Limits 是存盘的大小预算。
type Limits struct {
	// MaxRowBytes 是单行 payload 的上限。
	MaxRowBytes int
	// MaxRequestBytes 是一次存盘所有 payload 之和的上限,要小于 gRPC 单消息上限(4 MiB)。
	MaxRequestBytes int
}

// DefaultLimits 与今天的 BlobGuard 同一量级:整个玩家行 1 MiB。
func DefaultLimits() Limits {
	return Limits{MaxRowBytes: 1 << 20, MaxRequestBytes: 3 << 20}
}

// Store 是一个落点库上的玩家存档。并发安全;并发的同玩家请求靠写权行的行锁串行。
type Store struct {
	db       *sql.DB
	knownGen uint32
	limits   Limits
}

// New 构造 Store。knownGen 是本进程认识的最高世代(persistschema.Gen)。
func New(db *sql.DB, knownGen uint32, limits Limits) (*Store, error) {
	if db == nil {
		return nil, errors.New("playerstore: nil db")
	}
	if limits.MaxRowBytes <= 0 || limits.MaxRequestBytes < limits.MaxRowBytes {
		return nil, fmt.Errorf("playerstore: invalid limits %+v", limits)
	}
	// 写权行的 payload 恒空,不算。
	dataKinds := len(storepb.PlayerStoreKind_name) - 1
	if limits.MaxRowBytes*dataKinds+claimResponseOverheadBytes > MaxClaimResponseBytes {
		return nil, fmt.Errorf("playerstore: MaxRowBytes %d cannot fit %d rows into a claim response of %d bytes",
			limits.MaxRowBytes, dataKinds, MaxClaimResponseBytes)
	}
	return &Store{db: db, knownGen: knownGen, limits: limits}, nil
}

// VerifySchema 启动时核对 player_store 的列都在。基线已跑过的库上 cmd/migrate 建不出新表,
// 现有的启动核对只告警;而新路径缺表时每次存盘都报结果未知,scene 会一直重试,所以这里拒绝启动。
func (s *Store) VerifySchema(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, sqlSelectColumns)
	if err != nil {
		return fmt.Errorf("playerstore: read player_store columns: %w", err)
	}
	defer rows.Close()
	have := make(map[string]struct{}, len(requiredColumns))
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return fmt.Errorf("playerstore: scan player_store columns: %w", err)
		}
		have[strings.ToLower(column)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("playerstore: read player_store columns: %w", err)
	}
	var missing []string
	for _, column := range requiredColumns {
		if _, ok := have[column]; !ok {
			missing = append(missing, column)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("playerstore: player_store is missing columns %v; on a database whose baseline already ran, "+
			"cmd/migrate cannot create the table, rebuild the database (the project has not launched)", missing)
	}
	return nil
}

// validateSave 是进事务前的确定性拒绝,不碰库。返回 UNSPECIFIED 表示通过。
func (s *Store) validateSave(req *storepb.SavePlayerRequest) storepb.SavePlayerResult {
	if req.GetPlayerId() == 0 || req.GetOwnerEpoch() == 0 || req.GetSaveSeq() == 0 ||
		len(req.GetInstanceId()) == 0 || len(req.GetInstanceId()) > maxInstanceBytes {
		return storepb.SavePlayerResult_SAVE_PLAYER_INVALID
	}
	seen := make(map[int32]struct{}, len(req.GetRows()))
	total := 0
	for _, row := range req.GetRows() {
		kind := int32(row.GetKind())
		if _, known := storepb.PlayerStoreKind_name[kind]; !known || kind == ownerKind {
			return storepb.SavePlayerResult_SAVE_PLAYER_INVALID
		}
		if _, duplicate := seen[kind]; duplicate {
			return storepb.SavePlayerResult_SAVE_PLAYER_INVALID
		}
		seen[kind] = struct{}{}
		if len(row.GetPayload()) > s.limits.MaxRowBytes {
			return storepb.SavePlayerResult_SAVE_PLAYER_TOO_LARGE
		}
		total += len(row.GetPayload())
	}
	if total > s.limits.MaxRequestBytes {
		return storepb.SavePlayerResult_SAVE_PLAYER_TOO_LARGE
	}
	return storepb.SavePlayerResult_SAVE_PLAYER_RESULT_UNSPECIFIED
}

// Save 是一次存盘:写权判定和所有脏行在同一个事务里提交。
//
// 返回错误表示结果未知(authoritysql 对写路径上的错误一律不下结论),调用方用同样的内容、
// 同一个序号重试,已提交过的话会拿到 EQUAL。其余结局都在响应的 Result 里。
func (s *Store) Save(ctx context.Context, req *storepb.SavePlayerRequest) (*storepb.SavePlayerResponse, error) {
	if result := s.validateSave(req); result != storepb.SavePlayerResult_SAVE_PLAYER_RESULT_UNSPECIFIED {
		return &storepb.SavePlayerResponse{Result: result}, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("playerstore: begin save: %w", err)
	}
	defer tx.Rollback() // 提交之后是空操作
	row, err := readOwner(ctx, tx, req.GetPlayerId())
	if err != nil {
		return nil, err
	}
	result := decideSave(row, req.GetOwnerEpoch(), req.GetSaveSeq(), req.GetSchemaGen(), req.GetInstanceId(), s.knownGen)
	resp := &storepb.SavePlayerResponse{Result: result}
	if row != nil {
		resp.RowOwnerEpoch, resp.RowSaveSeq = row.ownerEpoch, row.saveSeq
	}
	if result != storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED {
		return resp, nil
	}
	query, args := buildSave(req)
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return nil, fmt.Errorf("playerstore: write player %d: %w", req.GetPlayerId(), err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("playerstore: commit player %d: %w", req.GetPlayerId(), err)
	}
	resp.RowOwnerEpoch, resp.RowSaveSeq = req.GetOwnerEpoch(), req.GetSaveSeq()
	return resp, nil
}

// LoadAndClaim 认领写权并读出完整存档,在同一个事务里。认领之后旧持有者的下一笔存盘立刻被拒,
// 所以新持有者读到的就是最后提交的状态。
func (s *Store) LoadAndClaim(ctx context.Context, req *storepb.LoadAndClaimRequest) (*storepb.LoadAndClaimResponse, error) {
	if req.GetPlayerId() == 0 || req.GetOwnerEpoch() == 0 ||
		len(req.GetInstanceId()) == 0 || len(req.GetInstanceId()) > maxInstanceBytes {
		return &storepb.LoadAndClaimResponse{Result: storepb.LoadAndClaimResult_LOAD_AND_CLAIM_INVALID}, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("playerstore: begin claim: %w", err)
	}
	defer tx.Rollback() // 提交之后是空操作
	row, err := readOwner(ctx, tx, req.GetPlayerId())
	if err != nil {
		return nil, err
	}
	result := decideClaim(row, req.GetOwnerEpoch(), req.GetInstanceId(), req.GetBinarySchemaGen())
	resp := &storepb.LoadAndClaimResponse{Result: result}
	if row != nil {
		resp.OwnerEpoch, resp.SaveSeq, resp.SchemaGen = row.ownerEpoch, row.saveSeq, row.schemaGen
	}
	if result != storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED {
		return resp, nil
	}
	if _, err := tx.ExecContext(ctx, sqlClaimOwner, req.GetOwnerEpoch(), req.GetInstanceId(), req.GetPlayerId()); err != nil {
		return nil, fmt.Errorf("playerstore: claim player %d: %w", req.GetPlayerId(), err)
	}
	if resp.Rows, err = readRows(ctx, tx, req.GetPlayerId()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("playerstore: commit claim of player %d: %w", req.GetPlayerId(), err)
	}
	resp.OwnerEpoch = req.GetOwnerEpoch()
	return resp, nil
}

// readOwner 锁住写权行并读出来。读已提交下,拿到锁之后读到的是最新提交的值。
func readOwner(ctx context.Context, tx *sql.Tx, playerID uint64) (*ownerRow, error) {
	row := &ownerRow{}
	err := tx.QueryRowContext(ctx, sqlSelectOwner, playerID).Scan(&row.ownerEpoch, &row.saveSeq, &row.schemaGen, &row.holder)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("playerstore: read owner of player %d: %w", playerID, err)
	}
	return row, nil
}

func readRows(ctx context.Context, tx *sql.Tx, playerID uint64) ([]*storepb.PlayerStoreRow, error) {
	rows, err := tx.QueryContext(ctx, sqlSelectRows, playerID)
	if err != nil {
		return nil, fmt.Errorf("playerstore: read rows of player %d: %w", playerID, err)
	}
	defer rows.Close()
	var out []*storepb.PlayerStoreRow
	for rows.Next() {
		var kind int32
		var payload []byte
		if err := rows.Scan(&kind, &payload); err != nil {
			return nil, fmt.Errorf("playerstore: scan rows of player %d: %w", playerID, err)
		}
		out = append(out, &storepb.PlayerStoreRow{Kind: storepb.PlayerStoreKind(kind), Payload: payload})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("playerstore: read rows of player %d: %w", playerID, err)
	}
	slices.SortFunc(out, func(a, b *storepb.PlayerStoreRow) int { return int(a.GetKind()) - int(b.GetKind()) })
	return out, nil
}

// buildSave 拼出写权行加所有脏行的一条语句。写权行的 payload 恒为空,数据行不用 holder_instance 和 released。
// 空字节一律绑定成非 nil 的空切片:nil 会被驱动写成 NULL,撞 NOT NULL。
func buildSave(req *storepb.SavePlayerRequest) (string, []any) {
	released := 0
	if req.GetRelease() {
		released = 1
	}
	empty := []byte{}
	var query strings.Builder
	query.WriteString(sqlSaveHead)
	query.WriteString(sqlSaveRow)
	args := make([]any, 0, 8*(1+len(req.GetRows())))
	args = append(args, req.GetPlayerId(), ownerKind, req.GetOwnerEpoch(), req.GetSaveSeq(), req.GetSchemaGen(),
		req.GetInstanceId(), released, empty)
	for _, row := range req.GetRows() {
		payload := row.GetPayload()
		if payload == nil {
			payload = empty
		}
		query.WriteByte(',')
		query.WriteString(sqlSaveRow)
		args = append(args, req.GetPlayerId(), int32(row.GetKind()), req.GetOwnerEpoch(), req.GetSaveSeq(), req.GetSchemaGen(),
			empty, 0, payload)
	}
	query.WriteString(sqlSaveTail)
	return query.String(), args
}
