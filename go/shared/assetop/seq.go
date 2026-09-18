package assetop

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	assetpb "proto/common/asset"
)

// seq 分配(规格 §4.19 seq.go、§4.37 seq.go)。
//
// 本文件只提供**在调用方事务里**执行的两段 SQL,不持有连接、不建表、不决定表结构。
// 表名由调用方给,因此必须先过白名单正则:它会被拼进 SQL,是唯一的注入面。

// tableNameRe 是允许的表名形状:小写字母开头,后接小写字母 / 数字 / 下划线,总长 ≤63。
var tableNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// SeqTables 指明业务方的两张表,以及该表里「未决」在 status 列上的**数值**。
//
// PendingStatus 必须与业务 Store 写 outbox 行时用的数值一致;assetop 自己的
// StatusPending 只表达语义,不参与 SQL(见 types.go 的 Status 注释)。
type SeqTables struct {
	SeqTable      string
	OpTable       string
	PendingStatus uint32
}

// NewSeqTables 校验表名并返回配置。在装配层调用一次即可,失败就别启动服务。
func NewSeqTables(seq, op string, pendingStatus uint32) (SeqTables, error) {
	t := SeqTables{SeqTable: seq, OpTable: op, PendingStatus: pendingStatus}
	if err := t.validate(); err != nil {
		return SeqTables{}, err
	}
	return t, nil
}

func (t SeqTables) validate() error {
	if !tableNameRe.MatchString(t.SeqTable) {
		return fmt.Errorf("assetop: 非法的 seq 表名 %q", t.SeqTable)
	}
	if !tableNameRe.MatchString(t.OpTable) {
		return fmt.Errorf("assetop: 非法的 op 表名 %q", t.OpTable)
	}
	return nil
}

// Alloc 是一次分配的结果:纪元 + 流水号。两个一起才能唯一定位一笔业务。
type Alloc struct {
	Epoch uint64
	Seq   uint64
}

// EnsureSeqRow 保证 (player, stream) 的 seq 行存在,纪元取**建行时刻**的毫秒。
//
// 刻意在事务**外**用 autocommit 执行:两个并发请求同时对一张不存在的行做
// 「加锁读 → 没有 → 插入」时,共享锁升级会死锁。INSERT IGNORE 天然幂等,
// 已存在时不会改动纪元 —— 纪元一旦写下就不再变,库重建后重新建行自然得到更大的纪元
// (规格 §4.29;按库恢复后由运维手册显式抬高)。
func EnsureSeqRow(ctx context.Context, db *sql.DB, t SeqTables, playerID uint64, s assetpb.AssetOpStream, nowMs uint64) error {
	if err := t.validate(); err != nil {
		return err
	}
	if nowMs == 0 {
		return errors.New("assetop: 纪元必须为正(nowMs=0)")
	}
	q := fmt.Sprintf("INSERT IGNORE INTO %s (player_id, stream, next_seq, epoch, updated_ms) VALUES (?, ?, 1, ?, ?)", t.SeqTable)
	if _, err := db.ExecContext(ctx, q, playerID, uint32(s), nowMs, nowMs); err != nil {
		return fmt.Errorf("assetop: 建 seq 行失败(player=%d stream=%d): %w", playerID, s, err)
	}
	return nil
}

// AllocateSeq 在**调用方的业务事务内**分配下一个 seq,并顺带回报当前纪元。
//
// 锁序(帮会):guild → guild_member → guild_player_op_seq → guild_asset_op。
// 本函数按这个顺序先锁 seq 行、再锁未决 op 行;调用方必须把本函数放在业务锁之后、
// 插入 op 行之前,否则两条路径的加锁顺序相反就会死锁。
//
// 未决行用**加锁读**而不是 COUNT(*):业务事务里通常已经做过普通读,MySQL 可重复读的
// 一致性快照看不到并发分配者刚插入的行;加锁读总读最新版本,MySQL 与 TiDB 都支持。
//
// 返回 ErrTooManyPending 表示守卫拒绝(不是故障):该玩家该流积压太多,再发就可能让
// 未决 seq 滑出 scene 的窗口,那时结局就查不回来了。
func AllocateSeq(ctx context.Context, tx *sql.Tx, t SeqTables, playerID uint64, s assetpb.AssetOpStream, lim Limits, nowMs uint64) (Alloc, error) {
	if err := t.validate(); err != nil {
		return Alloc{}, err
	}
	if err := lim.validate(); err != nil {
		return Alloc{}, err
	}

	var nextSeq, epoch uint64
	seqQuery := fmt.Sprintf("SELECT next_seq, epoch FROM %s WHERE player_id = ? AND stream = ? FOR UPDATE", t.SeqTable)
	if err := tx.QueryRowContext(ctx, seqQuery, playerID, uint32(s)).Scan(&nextSeq, &epoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Alloc{}, ErrSeqRowMissing
		}
		return Alloc{}, fmt.Errorf("assetop: 读 seq 行失败(player=%d stream=%d): %w", playerID, s, err)
	}
	if epoch == 0 {
		return Alloc{}, ErrSeqRowCorrupt
	}

	// 只数**本纪元**的未决行:旧纪元的未决行不该挡住新操作,它们会各自拿到真实旧结局
	// 或 UNKNOWN,走人工处置(规格 §4.29)。多取一行是为了区分「刚好到上限」和「超了」。
	pendingQuery := fmt.Sprintf(
		"SELECT seq FROM %s WHERE player_id = ? AND stream = ? AND stream_epoch = ? AND status = ? ORDER BY seq LIMIT ? FOR UPDATE",
		t.OpTable)
	rows, err := tx.QueryContext(ctx, pendingQuery, playerID, uint32(s), epoch, t.PendingStatus, int(lim.MaxPending)+1)
	if err != nil {
		return Alloc{}, fmt.Errorf("assetop: 读未决行失败(player=%d stream=%d): %w", playerID, s, err)
	}
	pending := make([]uint64, 0, int(lim.MaxPending)+1)
	for rows.Next() {
		var seq uint64
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return Alloc{}, fmt.Errorf("assetop: 解未决行失败(player=%d stream=%d): %w", playerID, s, err)
		}
		pending = append(pending, seq)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Alloc{}, fmt.Errorf("assetop: 遍历未决行失败(player=%d stream=%d): %w", playerID, s, err)
	}
	if err := rows.Close(); err != nil {
		return Alloc{}, fmt.Errorf("assetop: 关闭未决行游标失败(player=%d stream=%d): %w", playerID, s, err)
	}

	if uint32(len(pending)) >= lim.MaxPending {
		return Alloc{}, fmt.Errorf("%w (player=%d stream=%d pending=%d)", ErrTooManyPending, playerID, s, len(pending))
	}
	if len(pending) > 0 && nextSeq-pending[0] >= lim.MaxSpan {
		return Alloc{}, fmt.Errorf("%w (player=%d stream=%d span=%d)", ErrTooManyPending, playerID, s, nextSeq-pending[0])
	}

	bumpQuery := fmt.Sprintf("UPDATE %s SET next_seq = next_seq + 1, updated_ms = ? WHERE player_id = ? AND stream = ?", t.SeqTable)
	res, err := tx.ExecContext(ctx, bumpQuery, nowMs, playerID, uint32(s))
	if err != nil {
		return Alloc{}, fmt.Errorf("assetop: 推进 next_seq 失败(player=%d stream=%d): %w", playerID, s, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Alloc{}, fmt.Errorf("assetop: 读 next_seq 更新行数失败(player=%d stream=%d): %w", playerID, s, err)
	}
	// 行刚刚被 FOR UPDATE 锁住,更新不到只可能是有人在事务外删了它。
	// 这时继续下去会把同一个 seq 发第二次,必须失败(fail-closed)。
	if affected != 1 {
		return Alloc{}, fmt.Errorf("assetop: 推进 next_seq 影响 %d 行(player=%d stream=%d)", affected, playerID, s)
	}

	return Alloc{Epoch: epoch, Seq: nextSeq}, nil
}

// WithTxRetry 在死锁 / 锁等待超时 / TiDB 写冲突时把**整个业务事务**重跑一遍。
//
// shared 不引 MySQL 驱动(否则所有依赖 shared 的服务都被迫拖上它),所以错误分类由调用方注入:
// 帮会(B5)的实现是 errors.As 到 *mysql.MySQLError 且 Number ∈ {1213, 1205, 9007}。
// isRetryable 为 nil 时一次都不重试 —— 宁可让调用方看见错误,也不要在这里猜。
//
// fn 必须是**可重跑**的:它拿到的是一个全新事务,上一轮的任何内存副作用都要自己复位。
func WithTxRetry(ctx context.Context, db *sql.DB, attempts int, isRetryable func(error) bool, fn func(*sql.Tx) error) error {
	if db == nil || fn == nil {
		return errors.New("assetop: WithTxRetry 缺少 db 或 fn")
	}
	if attempts < 1 {
		return fmt.Errorf("assetop: WithTxRetry 的 attempts 必须 >= 1(当前 %d)", attempts)
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return errors.Join(lastErr, err)
			}
			return err
		}
		err := runTx(ctx, db, fn)
		if err == nil {
			return nil
		}
		lastErr = err
		if isRetryable == nil || !isRetryable(err) {
			return err
		}
	}
	return fmt.Errorf("assetop: 事务重试 %d 次仍失败: %w", attempts, lastErr)
}

// runTx 跑一次事务,保证无论哪条路径都不会漏掉 Rollback。
// 回滚失败不吞:它和业务错误一起返回,免得连接被留在事务里还没人知道。
func runTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("assetop: 开启事务失败: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("assetop: 回滚失败: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("assetop: 提交失败: %w", err)
	}
	return nil
}
