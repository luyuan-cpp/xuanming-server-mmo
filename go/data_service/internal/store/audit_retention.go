package store

import (
	"context"
	"math/rand/v2"
	"time"
)

const (
	// auditRetentionMaxBatchesPerRound 限制一轮的工作量:积压多时分几轮删完,不长时间占着写入。
	auditRetentionMaxBatchesPerRound = 500
	// auditRetentionPause 是两批之间的间隔,把删除压力摊开。
	auditRetentionPause = 100 * time.Millisecond
	// auditRetentionStatementTimeout 是一条 DELETE 的上限。超时只影响这一轮,下一轮接着删。
	auditRetentionStatementTimeout = 30 * time.Second
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
	// Paused 回 true 时不再发新的 DELETE(维护排空期间),每批之前都看。可以为 nil。
	Paused func() bool
	// Observe 每条 DELETE 之后调用一次,接指标和日志。可以为 nil。
	Observe func(table string, deleted int64, err error)
}

// auditRetentionTable 是一张要清理的表。deleteOlder 删一批早于 olderThan(Unix 秒)的行,回实际删除行数。
type auditRetentionTable struct {
	name        string
	maxAge      time.Duration
	deleteOlder func(ctx context.Context, olderThan uint64, limit int) (int64, error)
}

type auditRetention struct {
	tables     []auditRetentionTable
	batchSize  int
	maxBatches int
	pause      time.Duration
	now        func() time.Time
	paused     func() bool
	observe    func(table string, deleted int64, err error)
}

// RunAuditRetention 定期删过期的 player_snapshot、transaction_log,阻塞到 ctx 取消。
//
// 每个副本都跑:删除是幂等的,不值得为它选主。各副本错开一段随机时间起步,减少同时删同一批行的锁等待。
// 失败只影响这一轮,过期行下一轮还在。
func RunAuditRetention(ctx context.Context, opts AuditRetentionOptions, snapshots *SnapshotStore, txLogs *TransactionLogStore) {
	r := &auditRetention{
		tables: []auditRetentionTable{
			{name: AuditTablePlayerSnapshot, maxAge: opts.SnapshotMaxAge, deleteOlder: snapshots.DeleteOldSnapshots},
			{name: AuditTableTransactionLog, maxAge: opts.TransactionLogMaxAge, deleteOlder: txLogs.DeleteOldLogs},
		},
		batchSize:  opts.BatchSize,
		maxBatches: auditRetentionMaxBatchesPerRound,
		pause:      auditRetentionPause,
		now:        time.Now,
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
		r.sweep(ctx, table)
	}
}

// sweep 按批删一张表,直到不满一批、出错、预算用完或 ctx 取消。截止时刻在一轮开始时定好,不随批次后移。
func (r *auditRetention) sweep(ctx context.Context, table auditRetentionTable) {
	olderThan := uint64(r.now().Add(-table.maxAge).Unix())
	for batch := 0; batch < r.maxBatches; batch++ {
		if r.paused != nil && r.paused() {
			return
		}
		deleted, err := r.deleteBatch(ctx, table, olderThan)
		if r.observe != nil {
			r.observe(table.name, deleted, err)
		}
		if err != nil || deleted < int64(r.batchSize) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.pause):
		}
	}
}

func (r *auditRetention) deleteBatch(ctx context.Context, table auditRetentionTable, olderThan uint64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, auditRetentionStatementTimeout)
	defer cancel()
	return table.deleteOlder(ctx, olderThan, r.batchSize)
}
