package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// 审计表(player_snapshot、transaction_log)的过期清理。
//
// 2026-11-06 起按桶清理(缺口清单第 15 条,docs/design/audit-pipeline-horizontal-scale-20261106.md §2.D):
// 两张表都按 shard 分成 AuditShardCount 段(audit_shard.go),每个桶有一把会话级命名锁(GET_LOCK)。
// 每个副本照旧每轮都跑,但一个桶同一时刻只有拿到锁的那个实例在删,别的实例跳过它去删别的桶 ——
// 以前各副本删的是同一批最老的行,互相等锁、白干,删除吞吐不随副本数增长;现在几个副本的轮次重叠时各删各的桶。
// 一个实例同一时刻只用一条连接删(锁与 DELETE 在同一条钉住的连接上),连接占用与改动前相同。

const (
	// auditRetentionMaxBatchesPerShard 限制一轮里每个桶的工作量:积压多时分几轮删完,不长时间占着写入。
	// 16 个桶 × 32 批 ≈ 改动前一轮一张表 500 批的预算。
	auditRetentionMaxBatchesPerShard = 32
	// auditRetentionPause 是两批之间的间隔,把删除压力摊开。
	auditRetentionPause = 100 * time.Millisecond
	// auditRetentionStatementTimeout 是一条 DELETE(以及取锁)的上限。超时只影响这一个桶,下一轮接着删。
	auditRetentionStatementTimeout = 30 * time.Second
	// auditRetentionReleaseTimeout 是放锁那条语句的上限;超时也无妨,连接关闭时服务端会释放。
	auditRetentionReleaseTimeout = 5 * time.Second
	// auditRetentionLockPrefix 是清理锁名的前缀,完整名字见 auditRetentionLockName。
	auditRetentionLockPrefix = "ds_audit_retention."
)

// 指标和日志用的表名。
const (
	AuditTablePlayerSnapshot = "player_snapshot"
	AuditTableTransactionLog = "transaction_log"
)

// AuditRetentionOptions 是审计表清理循环的参数,全部要大于 0。
type AuditRetentionOptions struct {
	SnapshotMaxAge       time.Duration
	TransactionLogMaxAge time.Duration
	BatchSize            int
	Interval             time.Duration
	// Paused 回 true 时不再发新的 DELETE(维护排空期间),每个桶、每批之前都看。可以为 nil。
	Paused func() bool
	// Observe 每条 DELETE 之后调用一次,取桶锁出错时也调用一次(deleted=0),接指标和日志。可以为 nil。
	Observe func(table string, deleted int64, err error)
}

// retentionShard 是一个桶的清理会话:持有这个桶的清理锁,deleteOlder 删一批本桶里早于 olderThan(Unix 秒)的行,
// 回实际删除行数;release 放锁并归还连接,每个拿到的会话恰好调一次。
type retentionShard interface {
	deleteOlder(ctx context.Context, olderThan uint64, limit int) (int64, error)
	release()
}

// auditRetentionTable 是一张要清理的表。claim 不等待地取第 shard 个桶的清理锁:ok=false = 别的实例正在删这个桶。
type auditRetentionTable struct {
	name   string
	maxAge time.Duration
	claim  func(ctx context.Context, shard uint32) (retentionShard, bool, error)
}

type auditRetention struct {
	tables     []auditRetentionTable
	batchSize  int
	maxBatches int // 每个桶每轮最多删几批
	pause      time.Duration
	now        func() time.Time
	startShard func() uint32 // 每张表这一轮从哪个桶起步
	paused     func() bool
	observe    func(table string, deleted int64, err error)
}

// RunAuditRetention 定期删过期的 player_snapshot、transaction_log,阻塞到 ctx 取消。
//
// 每个副本都跑,各自错开一段随机时间起步;每张表从随机一个桶起轮一圈,拿不到锁的桶跳过(见文件头)。
// 失败只影响那一个桶的这一轮,过期行下一轮还在。
func RunAuditRetention(ctx context.Context, opts AuditRetentionOptions, snapshots *SnapshotStore, txLogs *TransactionLogStore) {
	r := &auditRetention{
		tables: []auditRetentionTable{
			{name: AuditTablePlayerSnapshot, maxAge: opts.SnapshotMaxAge, claim: snapshots.claimRetentionShard},
			{name: AuditTableTransactionLog, maxAge: opts.TransactionLogMaxAge, claim: txLogs.claimRetentionShard},
		},
		batchSize:  opts.BatchSize,
		maxBatches: auditRetentionMaxBatchesPerShard,
		pause:      auditRetentionPause,
		now:        time.Now,
		startShard: func() uint32 { return uint32(rand.N(AuditShardCount)) },
		paused:     opts.Paused,
		observe:    opts.Observe,
	}
	timer := time.NewTimer(rand.N(opts.Interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		r.runRound(ctx)
		timer.Reset(opts.Interval)
	}
}

func (r *auditRetention) runRound(ctx context.Context) {
	for _, table := range r.tables {
		r.sweepTable(ctx, table)
	}
}

func (r *auditRetention) isPaused() bool { return r.paused != nil && r.paused() }

func (r *auditRetention) observeDelete(table string, deleted int64, err error) {
	if r.observe != nil {
		r.observe(table, deleted, err)
	}
}

// sweepTable 按桶删一张表:从 startShard 起轮一圈(几个实例同时清理时各自从不同的桶起步,少撞锁),
// 每个桶先取清理锁,拿不到就跳过。截止时刻在一轮开始时定好,不随批次后移。
func (r *auditRetention) sweepTable(ctx context.Context, table auditRetentionTable) {
	olderThan := uint64(r.now().Add(-table.maxAge).Unix())
	start := r.startShard()
	swept, skipped := 0, 0
	var deleted int64
	for i := uint32(0); i < AuditShardCount; i++ {
		if ctx.Err() != nil || r.isPaused() {
			break
		}
		n, claimed := r.sweepShard(ctx, table, (start+i)%AuditShardCount, olderThan)
		deleted += n
		if claimed {
			swept++
		} else {
			skipped++
		}
	}
	logx.Infof("[AuditRetention] %s: shards swept=%d skipped=%d (held by another instance or lock failed) deleted=%d older_than=%d",
		table.name, swept, skipped, deleted, olderThan)
}

// sweepShard 取一个桶的清理锁,按批删,直到不满一批、出错、本桶预算用完、ctx 取消或进入排空,然后放锁。
// 返回删除行数,以及是否拿到了锁。
func (r *auditRetention) sweepShard(ctx context.Context, table auditRetentionTable, shard uint32, olderThan uint64) (int64, bool) {
	claimCtx, cancel := context.WithTimeout(ctx, auditRetentionStatementTimeout)
	s, ok, err := table.claim(claimCtx, shard)
	cancel()
	if err != nil {
		r.observeDelete(table.name, 0, err)
		return 0, false
	}
	if !ok {
		return 0, false
	}
	defer s.release()

	var total int64
	for batch := 0; batch < r.maxBatches; batch++ {
		if r.isPaused() {
			break
		}
		deleted, err := r.deleteBatch(ctx, s, olderThan)
		r.observeDelete(table.name, deleted, err)
		total += deleted
		if err != nil || deleted < int64(r.batchSize) {
			break
		}
		select {
		case <-ctx.Done():
			return total, true
		case <-time.After(r.pause):
		}
	}
	return total, true
}

func (r *auditRetention) deleteBatch(ctx context.Context, s retentionShard, olderThan uint64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, auditRetentionStatementTimeout)
	defer cancel()
	return s.deleteOlder(ctx, olderThan, r.batchSize)
}

// sqlRetentionShard 是 retentionShard 的真库实现:一条钉住的连接持有清理锁,DELETE 也在这条连接上跑。
type sqlRetentionShard struct {
	conn      *sql.Conn
	lockName  string
	deleteSQL string // 三个占位符:shard、截止时间、LIMIT
	shard     uint32
}

// claimSQLRetentionShard 在 db 上钉一条连接,不等待地取 (库, 表, 桶) 的清理锁(GET_LOCK(name, 0))。
// 拿到:返回在这条连接上删除的会话;别的会话持有:ok=false;出错:err(连接已归还)。
//
// 锁是会话级的:进程崩溃、连接断开、语句超时导致驱动关掉连接,服务端都会随之释放,不会卡死。
// 它只为不重复干活,不承担正确性:万一两个实例同删一个桶(例如连接被服务端回收、锁被另一个实例拿走),
// 删除本身仍是幂等的。
func claimSQLRetentionShard(ctx context.Context, db *sql.DB, dbName, tableCode, deleteSQL string, shard uint32) (retentionShard, bool, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("pin a connection for audit retention: %w", err)
	}
	name := auditRetentionLockName(dbName, tableCode, shard)
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("acquire audit retention lock %q: %w", name, err)
	}
	if !got.Valid {
		// NULL = 服务端出错(例如会话被 KILL),不是"别人持有"。
		_ = conn.Close()
		return nil, false, fmt.Errorf("acquire audit retention lock %q: GET_LOCK returned NULL (server error or session killed)", name)
	}
	if got.Int64 != 1 {
		_ = conn.Close()
		return nil, false, nil
	}
	return &sqlRetentionShard{conn: conn, lockName: name, deleteSQL: deleteSQL, shard: shard}, true, nil
}

func (s *sqlRetentionShard) deleteOlder(ctx context.Context, olderThan uint64, limit int) (int64, error) {
	res, err := s.conn.ExecContext(ctx, s.deleteSQL, s.shard, olderThan, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *sqlRetentionShard) release() {
	ctx, cancel := context.WithTimeout(context.Background(), auditRetentionReleaseTimeout)
	defer cancel()
	var released sql.NullInt64
	if err := s.conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", s.lockName).Scan(&released); err != nil {
		logx.Errorf("[AuditRetention] release lock %s failed (the server frees it when the connection closes): %v", s.lockName, err)
	}
	_ = s.conn.Close()
}

// auditRetentionLockName 是 (库, 表, 桶) 的清理锁名。GET_LOCK 的名字在整台服务器上共享、最长 64 字符
// (maxMigrateLockNameLen):库名放最后,超长截断,口径同 migrateLockName(生产库名远短于上限)。
func auditRetentionLockName(dbName, tableCode string, shard uint32) string {
	name := fmt.Sprintf("%s%s.%02d.%s", auditRetentionLockPrefix, tableCode, shard, dbName)
	if len(name) > maxMigrateLockNameLen {
		name = name[:maxMigrateLockNameLen]
	}
	return name
}
