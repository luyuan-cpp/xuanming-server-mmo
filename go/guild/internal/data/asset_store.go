package data

// 帮会资产指令的 assetop.Store 实现(设计 05-economy.md §5.19–§5.23;顶部"B4b 落地后的接口终稿"、
// D2 决策覆盖、90-consistency X-03 与 07-rollback-fail-closed.md §7.4.1 的订正优先于正文)。
//
// 它回答四件事:哪些行到期了(ListDue)、这一行归我处理(Claim)、这一轮没结论改天再来(Reschedule)、
// 结论定了一次性落库并做对侧账(Finalize / ResolveManually)。另有清理任务与 assetopfix 用的两个读。
//
// 与 go/trade/internal/data/asset_op_repo.go 同形,**有意不同的两处**:
//  1. Finalize 与 ResolveManually 都同写 next_attempt_ms = now(07 §7.4.1 硬要求):B5d 的回档检查按
//     "终态行的 next_attempt_ms = 终结时刻"查 `next_attempt_ms > since`,漏写 = 漏行 = 回档复制资产(fail-open),
//     清理判龄也会偏早。assetop.Store 的 Finalize 契约注释(reconcile.go:136-138)与 trade 都漏了它,别照抄。
//  2. 对侧账在同一事务里真的做了(资金 / 帮贡 / 次数 / 限购),trade 那边还只是 TODO。
//
// 锁序与业务写事务一致:guild → guild_member → guild_asset_op → guild_daily_counter。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	pb "proto/guild"

	"shared/assetop"
	"shared/gameday"
	"shared/safego"
)

// Store 各方法的子预算(90 part2 §2 第 8 条)。
//
// Finalize 的 2000ms 只在 assetopfix 路径上是真实上限:重投循环调 Finalize / Reschedule 时传进来的是
// settleContext(reconcile.go:552,700ms、不继承取消),子预算取两者较小值,实际只有 700ms。
// 这是 assetop 的取舍(落库必须在投递预算之外有独立时间),本文件不去抬高它。
const (
	storeReadBudget     = 1000 * time.Millisecond // ListDue / Reschedule / 各种读
	storeClaimBudget    = 1000 * time.Millisecond
	storeFinalizeBudget = 2000 * time.Millisecond // Finalize / ResolveManually(含事务外那一次不可变列读)
)

// backgroundTxAttempts:后台写(Finalize / ResolveManually / 清理)的总尝试次数(90 part2 §2 第 6 条)。
const backgroundTxAttempts = 3

// 清理节律(05 §5.22):单批 500 行、批间 100ms、每条语句每轮至多 20 批,RowsAffected < 500 即停。
// 批量删是为了不让一条 DELETE 长时间持有一大片行锁;批间 sleep 给业务写事务让路。
const (
	cleanupBatchSize   = 500
	cleanupBatchPause  = 100 * time.Millisecond
	cleanupMaxBatches  = 20
	cleanupBatchBudget = 2 * time.Second // 单批 DELETE 的上限;锁等待已被 DSN 封顶 1s,跑满 2s 说明库有状况

	// 计数行的周期键:日键 8 位(YYYYMMDD)、周键 6 位(YYYYWW),数值域不相交,
	// 两条 BETWEEN 各走 idx_guild_daily_counter_0 的一段,互不误删(gameday.PeriodKey 的注释)。
	dayKeyFloor  = 19700101
	weekKeyFloor = 100000
)

// 清理指标的 table label 与 orphan 指标的两个 label,全部取自固定集合(不带任何 id,AGENTS §9)。
const (
	cleanupTableAssetOp = "guild_asset_op"
	cleanupTableCounter = "guild_daily_counter"

	orphanKindDonate = "donate"
	orphanKindShop   = "shop"

	orphanWhatGuildGone        = "guild_gone"         // 捐献扣款成功,但绑定的帮会已解散:资金与帮贡都记不上
	orphanWhatMemberGone       = "member_gone"        // 捐献扣款成功,帮会还在但捐献者已离帮:只记资金、跳过帮贡
	orphanWhatRefundMemberGone = "refund_member_gone" // 兑换被永久拒绝,但兑换者已不是该帮成员:帮贡退不回去
)

var (
	guildAssetOrphanTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: guildMetricNamespace,
		Subsystem: "asset",
		Name:      "orphan_total",
		Help:      "资产指令终结时对侧账无处可记的次数(D2:捐献绑定的帮会已解散 / 捐献者已离帮 / 兑换退帮贡时已离帮)。不做补偿,只计数与打 INFO;持续上升说明离帮提前截止没有生效。",
		Labels:    []string{"kind", "what"},
	})

	guildAssetCleanupDeletedTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: guildMetricNamespace,
		Subsystem: "asset",
		Name:      "cleanup_deleted_total",
		Help:      "清理任务删除的行数(终态资产指令 / 过期计数行)。长期为 0 而表在长,说明清理没在跑。",
		Labels:    []string{"table"},
	})
)

// recordAssetOrphan / recordCleanupDeleted:生产实现计指标,单测替换为记录器(同 invalidateGaveUp 的惯例,
// 不去读 go-zero 的全局注册表)。
var (
	recordAssetOrphan    = func(kind, what string) { guildAssetOrphanTotal.Inc(kind, what) }
	recordCleanupDeleted = func(table string, n int64) { guildAssetCleanupDeletedTotal.Add(float64(n), table) }
)

const (
	sqlListDueSegmentFresh = "SELECT `op_id` FROM " + guildAssetOpTable +
		" WHERE `status` = ? AND `next_attempt_ms` <= ? AND `lease_until_ms` < ? AND `attempts` < ?" +
		" ORDER BY `next_attempt_ms` ASC, `op_id` ASC LIMIT ?"
	sqlListDueSegmentAged = "SELECT `op_id` FROM " + guildAssetOpTable +
		" WHERE `status` = ? AND `next_attempt_ms` <= ? AND `lease_until_ms` < ? AND `attempts` >= ?" +
		" ORDER BY `next_attempt_ms` ASC, `op_id` ASC LIMIT ?"

	sqlClaimAssetOp = "UPDATE " + guildAssetOpTable + " SET `lease_until_ms` = ?, `lease_token` = ?, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ? AND `lease_until_ms` < ?"
	// 毒行推迟:令牌之外还要带 `status = PENDING`。人工终结(sqlResolveAssetOp)的 CAS 只看 status、不换令牌,
	// 它可能恰好落在 Claim 的 CAS 与这里之间;只凭令牌会把已终结的行改回"一小时后再来"、抹掉 last_outcome,
	// 破坏"终态行此后没有任何路径再改"(07 §7.2)与"终态行 next_attempt_ms = 终结时刻"(07 §7.4.1)。
	sqlPoisonAssetOp = "UPDATE " + guildAssetOpTable +
		" SET `last_outcome` = ?, `next_attempt_ms` = ?, `lease_until_ms` = 0, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `lease_token` = ? AND `status` = ?"
	sqlRescheduleAssetOp = "UPDATE " + guildAssetOpTable +
		" SET `attempts` = `attempts` + 1, `next_attempt_ms` = ?, `lease_until_ms` = 0," +
		" `durable` = ?, `last_outcome` = ?, `last_reason` = ?, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ? AND `lease_token` = ?"

	// 终结 CAS:`status = PENDING` 是"一次且仅一次"的最后一道闸,人工与自动同时下手也只有一个赢家。
	sqlFinalizeAssetOp = "UPDATE " + guildAssetOpTable +
		" SET `status` = ?, `durable` = 1, `last_outcome` = ?, `last_reason` = ?, `reason_tip_id` = ?," +
		" `lease_until_ms` = 0, `next_attempt_ms` = ?, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ?"
	// 人工终结:不置 durable(不是 scene 确认的落盘结局,置了会让事后对账分不清"scene 答过"与"人写的")、
	// 不改 last_outcome(保留最后一次 scene 真实答复作证据),同样同写 next_attempt_ms。
	sqlResolveAssetOp = "UPDATE " + guildAssetOpTable +
		" SET `status` = ?, `resolved_by` = ?, `resolve_reason` = ?, `lease_until_ms` = 0," +
		" `next_attempt_ms` = ?, `updated_ms` = ?" +
		" WHERE `op_id` = ? AND `status` = ?"

	// 终结前在事务外读的**不可变列**:插入之后没有任何路径改它们,读一次即可,不必占事务时间。
	sqlSelectAssetOpImmutable = "SELECT `player_id`, `guild_id`, `kind`, `ref_id`, `ref_count`, `period_key`," +
		" `contribution_delta`, `funds_delta` FROM " + guildAssetOpTable + " WHERE `op_id` = ?"

	sqlSelectAssetOpByID = "SELECT " + assetOpColumns + " FROM " + guildAssetOpTable + " WHERE `op_id` = ?"
	sqlListStuckAssetOps = "SELECT " + assetOpColumns + " FROM " + guildAssetOpTable +
		" WHERE `status` = ? AND `created_ms` < ? ORDER BY `created_ms` ASC, `op_id` ASC LIMIT ?"
	sqlOldestPendingAssetOp = "SELECT MIN(`created_ms`) FROM " + guildAssetOpTable + " WHERE `status` = ? AND `stream` = ?"

	// 对侧账与计数行清理。
	sqlLockGuildFunds      = `SELECT funds FROM guild WHERE guild_id = ? FOR UPDATE`
	sqlCreditGuildFunds    = `UPDATE guild SET funds = funds + ? WHERE guild_id = ?`
	sqlCreditContribution  = `UPDATE guild_member SET contribution_total = contribution_total + ?, contribution_balance = contribution_balance + ? WHERE guild_id = ? AND player_id = ?`
	sqlRefundContribution  = `UPDATE guild_member SET contribution_balance = contribution_balance + ? WHERE guild_id = ? AND player_id = ?`
	sqlRefundCounter       = `UPDATE guild_daily_counter SET used_count = IF(used_count >= ?, used_count - ?, 0), updated_ms = ? WHERE player_id = ? AND counter_kind = ? AND ref_id = ? AND period_key = ?`
	sqlCleanupDailyCounter = `DELETE FROM guild_daily_counter WHERE period_key BETWEEN ? AND ? LIMIT ?`
)

// sqlCleanupTerminalOps 只删三种终态:**不含** APPLIED_PARTIAL(要人工补偿,证据不能自动消失,X-15 / 90 part2 §3)
// 与 PENDING(永不删)。判龄按 next_attempt_ms —— 终态行上它等于终结时刻(Finalize / ResolveManually 同写)。
const sqlCleanupTerminalOps = "DELETE FROM " + guildAssetOpTable +
	" WHERE `status` IN (?, ?, ?) AND `next_attempt_ms` < ? LIMIT ?"

// PendingStatus 是 guild_asset_op.status 的"未决"库值,给 assetop 的 seq 分配与本文件的 SQL 用。
// 取生成枚举而不是写 1:assetop.Status 刻意不绑库值,库值只由本表的枚举定。
func PendingStatus() uint32 { return uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING) }

// StatusToRecord 把 assetop 的语义状态映射成本表的库值。映射只此一处。
// 未知语义值落 UNSPECIFIED:既不能落成 PENDING(会被循环反复领走),也不能落成 APPLIED(伪装成功)。
func StatusToRecord(st assetop.Status) pb.GuildAssetOpStatus {
	switch st {
	case assetop.StatusPending:
		return pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING
	case assetop.StatusApplied:
		return pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED
	case assetop.StatusRejected:
		return pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED
	case assetop.StatusAborted:
		return pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED
	case assetop.StatusAppliedPartial:
		return pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL
	default:
		return pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_UNSPECIFIED
	}
}

// isTerminalRecord:终结路径只接受四个终态。PENDING 写进去等于没终结,UNSPECIFIED 会让客服查不到结局。
func isTerminalRecord(st pb.GuildAssetOpStatus) bool {
	switch st {
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL:
		return true
	default:
		return false
	}
}

// isRetryableBackground:后台写的重试分类 = 1213 / 9007 / errRetryTx(isRetryableTxError)再加 1205。
// 与请求路径不同,后台写**重试** 1205:它没有玩家在等"稍后重试"的提示,锁等待超时之后再排一次队
// 比把一个已定的终局留在内存里、等下一轮重投再投一次 scene 划算得多。
func isRetryableBackground(err error) bool {
	return isRetryableTxError(err) || isLockWaitTimeout(err)
}

// FinalizedOp 是一次**本次**终结的摘要,供提交后的推送使用。
type FinalizedOp struct {
	OpID, PlayerID, GuildID uint64
	Kind                    pb.GuildAssetOpKind
	Status                  assetop.Status
}

// GuildAssetStore 是 guild_asset_op 的 assetop.Store 实现,另带人工终结与最老未决年龄两个可选能力。
//
// 线程模型:除 OnFinalized 外构造后只读;OnFinalized 必须在启动重投循环与 RPC 服务**之前**赋值,之后不再改
// (它没有锁,运行期改写与 worker 读取构成数据竞争)。
type GuildAssetStore struct {
	db     *sql.DB
	guilds *GuildRepo

	// OnFinalized 在**本次**终结(CAS 命中)且事务提交之后调用,guild.go 接到 logic 的推送;nil = 不推送(assetopfix)。
	// 它收到的 ctx 是调用 Finalize 时的 ctx(保留同步投递标记等值),不是本文件的子预算 ctx。
	OnFinalized func(context.Context, FinalizedOp)
}

var (
	_ assetop.Store            = (*GuildAssetStore)(nil)
	_ assetop.ManualResolver   = (*GuildAssetStore)(nil)
	_ assetop.PendingAgeReader = (*GuildAssetStore)(nil)
)

// NewGuildAssetStore。guilds 为 nil 时报错:对侧账要用它的缓存失效,连接池也取自它。
func NewGuildAssetStore(guilds *GuildRepo) (*GuildAssetStore, error) {
	if guilds == nil {
		return nil, errors.New("guild asset store: nil GuildRepo")
	}
	return &GuildAssetStore{db: guilds.db, guilds: guilds}, nil
}

// ── assetop.Store ────────────────────────────────────────────

// ListDue 非加锁一致性读,只回主键,走 idx_guild_asset_op_0 (status, next_attempt_ms)。
//
// **两段查询**(X-03 防饿死,契约见 assetop.Store.ListDue):第一段只取新行(attempts < FreshAttemptLimit)并占满
// limit;只有它不够时才发第二段取老行,且只补缺口。写成一条 SQL,退避封顶在 MaxBackoff 的老行永远"早就到期",
// 会霸占整批名额,新提交的捐献 / 兑换一次也轮不上。两段是两次独立的非锁读,期间 attempts 可能从 2 跳到 3,
// 同一行两段都出现,所以按 op_id 去重;第一段的 id 排前面,让 Claim 先抢新行。
func (s *GuildAssetStore) ListDue(ctx context.Context, nowMs uint64, limit int) ([]uint64, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()

	fresh, err := s.listDueSegment(ctx, sqlListDueSegmentFresh, nowMs, limit)
	if err != nil {
		return nil, err
	}
	if len(fresh) >= limit {
		return fresh, nil
	}
	aged, err := s.listDueSegment(ctx, sqlListDueSegmentAged, nowMs, limit-len(fresh))
	if err != nil {
		return nil, err
	}
	seen := make(map[uint64]struct{}, len(fresh))
	for _, id := range fresh {
		seen[id] = struct{}{}
	}
	for _, id := range aged {
		if _, dup := seen[id]; dup {
			continue
		}
		fresh = append(fresh, id)
	}
	return fresh, nil
}

func (s *GuildAssetStore) listDueSegment(ctx context.Context, query string, nowMs uint64, limit int) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx, query, PendingStatus(), nowMs, nowMs, assetop.FreshAttemptLimit, limit)
	if err != nil {
		return nil, fmt.Errorf("list due %s: %w", guildAssetOpTable, err)
	}
	defer rows.Close()

	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan due %s: %w", guildAssetOpTable, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due %s: %w", guildAssetOpTable, err)
	}
	return ids, nil
}

// Claim 单行主键 CAS 领取(autocommit),紧挨着处理前调用,租约从"领到这一行"起算。
// RowsAffected != 1 → (Op{}, false, nil):已被别的副本领走或已终结。
//
// payload 为空或解不开都是毒行:把它推迟到 poisonUntilMs(循环按 LoopConfig.PoisonDelay 算好的绝对时刻,
// **本文件不另写毒行延迟常量**)并回 assetop.ErrPoisonRow,循环计 decode 后继续下一行。空 payload 也算毒行:
// 下发一个空包,scene 会回 kAssetInvalidBundle,行被 REJECTED 终结并触发退款 / 退次数,而那不是真实的业务结局。
func (s *GuildAssetStore) Claim(ctx context.Context, opID, nowMs, leaseUntilMs, poisonUntilMs, token uint64) (assetop.Op, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, storeClaimBudget)
	defer cancel()

	result, err := s.db.ExecContext(ctx, sqlClaimAssetOp, leaseUntilMs, token, nowMs, opID, PendingStatus(), nowMs)
	if err != nil {
		return assetop.Op{}, false, fmt.Errorf("claim %s %d: %w", guildAssetOpTable, opID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return assetop.Op{}, false, fmt.Errorf("claim %s %d: read rows affected: %w", guildAssetOpTable, opID, err)
	}
	if affected != 1 {
		return assetop.Op{}, false, nil
	}

	rec, found, err := s.getRecord(ctx, opID)
	if err != nil {
		return assetop.Op{}, false, err
	}
	if !found {
		// 刚 CAS 成功又查不到:只可能是被并发删了(清理只删终态行),按故障返回。
		return assetop.Op{}, false, fmt.Errorf("claim %s %d: row vanished after claim", guildAssetOpTable, opID)
	}
	bundle := &assetpb.AssetBundle{}
	decodeErr := errors.New("empty payload")
	if len(rec.GetPayload()) > 0 {
		decodeErr = proto.Unmarshal(rec.GetPayload(), bundle)
	}
	if decodeErr != nil {
		s.markPoison(ctx, opID, token, nowMs, poisonUntilMs)
		return assetop.Op{}, false, fmt.Errorf("%w: op_id=%d: %v", assetop.ErrPoisonRow, opID, decodeErr)
	}
	return assetop.Op{
		OpID:        rec.GetOpId(),
		PlayerID:    rec.GetPlayerId(),
		Stream:      assetpb.AssetOpStream(rec.GetStream()),
		Seq:         rec.GetSeq(),
		StreamEpoch: rec.GetStreamEpoch(),
		// correlation_id 取 op_id:人工对账按 scene 流水的 correlation_id 定位唯一一行指令;
		// 必须与同步首投时 logic 填的值一致,否则重投会换一个 correlation。
		CorrelationID: rec.GetOpId(),
		TxType:        rec.GetTxType(),
		Bundle:        bundle,
		Attempts:      rec.GetAttempts(),
		DeadlineMs:    rec.GetDeadlineMs(),
		LeaseToken:    token,
		LastReason:    rec.GetLastReason(),
	}, true, nil
}

// markPoison 把毒行推迟到 poisonUntilMs,last_outcome 记"结局未知"。用本次领取的 lease_token 且仍为 PENDING 做 CAS:
// 不会推迟别的副本刚领走的同一行,也不会改写领取之后才被人工终结的行。写失败只记日志:后果是下一轮再撞一次
// 同一行(仍然跳过),不丢数据。
func (s *GuildAssetStore) markPoison(ctx context.Context, opID, token, nowMs, poisonUntilMs uint64) {
	if _, err := s.db.ExecContext(ctx, sqlPoisonAssetOp,
		uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN), poisonUntilMs, nowMs, opID, token, PendingStatus()); err != nil {
		logx.Errorf("[GuildAsset] 推迟毒行失败 op_id=%d: %v", opID, err)
	}
}

// Reschedule 退回待办并推迟下一次投递,带 lease_token 做 CAS。
// RowsAffected == 0 → assetop.ErrLeaseLost:租约在处理期间被另一个副本接管,本次结果一个字都没写进去;
// 回 nil 会把"我的结果被丢弃"伪装成成功,assetop_reschedule_lost_total 就恒为 0。
// last_reason 照写 res.Reason:部分发放码(assetop.ReasonPartialApplied)的粘性由 assetop 的 carryPartialReason 负责,
// 这里不自作主张。
func (s *GuildAssetStore) Reschedule(ctx context.Context, op assetop.Op, nextAttemptMs uint64, res assetop.Result, nowMs uint64) error {
	ctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()

	result, err := s.db.ExecContext(ctx, sqlRescheduleAssetOp,
		nextAttemptMs, durableFlag(res.Durable), uint32(res.Outcome), res.Reason, nowMs,
		op.OpID, PendingStatus(), op.LeaseToken)
	if err != nil {
		return fmt.Errorf("reschedule %s %d: %w", guildAssetOpTable, op.OpID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reschedule %s %d: read rows affected: %w", guildAssetOpTable, op.OpID, err)
	}
	if affected == 0 {
		return fmt.Errorf("guild asset op %d: %w", op.OpID, assetop.ErrLeaseLost)
	}
	return nil
}

func durableFlag(durable bool) uint32 {
	if durable {
		return 1
	}
	return 0
}

// Finalize 把行终结成 status,并在**同一事务**里做对侧账;返回是否**本次**终结。
//
// 事务外先读不可变列(无行 → ERROR + (false, nil));事务经 assetop.WithTxRetry(RC,1213 / 1205 / 9007 重跑 3 次):
// 按锁序加锁 → CAS(同写 next_attempt_ms = now,07 §7.4.1)→ RowsAffected == 1 才做对侧账。
// reason_tip_id 只在 REJECTED 时写 res.Reason(玩家看得到的拒绝原因),其余写 0。
// 提交后:动过 guild / 成员行 → 失效缓存;然后调 OnFinalized。
func (s *GuildAssetStore) Finalize(ctx context.Context, op assetop.Op, status assetop.Status, res assetop.Result, nowMs uint64) (bool, error) {
	final := StatusToRecord(status)
	if !isTerminalRecord(final) {
		return false, fmt.Errorf("finalize %s %d: status %s is not terminal", guildAssetOpTable, op.OpID, status)
	}
	var reasonTip uint32
	if status == assetop.StatusRejected {
		reasonTip = res.Reason
	}
	return s.terminate(ctx, op.OpID, status, nowMs, "finalize", sqlFinalizeAssetOp,
		int32(final), uint32(res.Outcome), res.Reason, reasonTip, nowMs, nowMs, op.OpID, PendingStatus())
}

// ResolveManually 是人工终结通道(assetopfix)。入参合法性(终态、操作人与理由长度)由 assetop.ResolveManually
// 先校验;这里仍兜住"映射不出终态库值"的情况,在碰库之前失败。
// 与 Finalize 共用同一把 `status = PENDING` 的 CAS 与同一份对侧账:人工与循环同时下手只有一个赢家;
// 对侧账若各写一份,迟早出现"只改了一边"的分叉。
func (s *GuildAssetStore) ResolveManually(ctx context.Context, r assetop.ManualResolution, nowMs uint64) (bool, error) {
	final := StatusToRecord(r.Final)
	if !isTerminalRecord(final) {
		return false, fmt.Errorf("manual resolve %s %d: status %s is not terminal", guildAssetOpTable, r.OpID, r.Final)
	}
	return s.terminate(ctx, r.OpID, r.Final, nowMs, "manual resolve", sqlResolveAssetOp,
		int32(final), r.Operator, r.Reason, nowMs, nowMs, r.OpID, PendingStatus())
}

// assetOpImmutable 是终结时要用的不可变列。
type assetOpImmutable struct {
	PlayerID, GuildID             uint64
	Kind                          pb.GuildAssetOpKind
	RefID, RefCount, PeriodKey    uint32
	ContributionDelta, FundsDelta uint64
}

// counterpartyLocks 是对侧账加锁的结果:行在不在,决定记账还是计 orphan。
type counterpartyLocks struct{ guildOK, memberOK bool }

// counterpartyOutcome 是对侧账做了什么。副作用(指标、日志)在提交**之后**才发出:
// 事务可能被重跑,在闭包里直接计数会把一次终结记成多次。
type counterpartyOutcome struct {
	touched     bool   // 改过 guild 或 guild_member 行 → 提交后失效缓存
	orphanKind  string // 非空 = 提交后计 guild_asset_orphan_total
	orphanWhat  string
	unknownKind bool // kind 不认识:只做了 CAS
}

// terminate 是 Finalize 与 ResolveManually 的共同骨架;两者只差 CAS 语句(casQuery / casArgs)。
func (s *GuildAssetStore) terminate(ctx context.Context, opID uint64, status assetop.Status, nowMs uint64,
	what, casQuery string, casArgs ...any) (bool, error) {
	dbCtx, cancel := context.WithTimeout(ctx, storeFinalizeBudget)
	defer cancel()

	row, found, err := s.readImmutable(dbCtx, opID)
	if err != nil {
		return false, fmt.Errorf("%s %s %d: %w", what, guildAssetOpTable, opID, err)
	}
	if !found {
		// 行不存在:清理只删终态行,所以这里是数据被人工删了或 op_id 传错。不终结、不报错,让调用方继续下一行。
		logx.Errorf("[GuildAsset] %s: op_id=%d 不存在,跳过", what, opID)
		return false, nil
	}

	var (
		finalized bool
		outcome   counterpartyOutcome
	)
	err = assetop.WithTxRetry(dbCtx, s.db, backgroundTxAttempts, isRetryableBackground, func(tx *sql.Tx) error {
		// 重试契约:每次尝试从零开始,结果只在成功返回前写到外层。
		finalized, outcome = false, counterpartyOutcome{}

		locks, err := lockCounterparty(dbCtx, tx, row, status)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(dbCtx, casQuery, casArgs...)
		if err != nil {
			return fmt.Errorf("terminal CAS: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("terminal CAS: read rows affected: %w", err)
		}
		if affected != 1 {
			return nil // 已被别的副本 / 人工终结:不是错误,也不做对侧账
		}
		done, err := applyCounterparty(dbCtx, tx, row, status, locks, nowMs)
		if err != nil {
			return err
		}
		finalized, outcome = true, done
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("%s %s %d: %w", what, guildAssetOpTable, opID, err)
	}
	if !finalized {
		return false, nil
	}

	switch {
	case outcome.orphanKind != "":
		recordAssetOrphan(outcome.orphanKind, outcome.orphanWhat)
		logx.Infof("[GuildAsset] 对侧账无处可记 kind=%s what=%s op_id=%d player_id=%d guild_id=%d funds=%d contribution=%d status=%s",
			outcome.orphanKind, outcome.orphanWhat, opID, row.PlayerID, row.GuildID, row.FundsDelta, row.ContributionDelta, status)
	case outcome.unknownKind:
		logx.Errorf("[GuildAsset] 未知 kind=%d,只终结不做对侧账 op_id=%d status=%s", int32(row.Kind), opID, status)
	}
	if outcome.touched {
		s.guilds.invalidateAfterCommit(ctx, opAssetFinalize, row.GuildID, row.PlayerID)
	}
	if s.OnFinalized != nil {
		s.OnFinalized(ctx, FinalizedOp{
			OpID:     opID,
			PlayerID: row.PlayerID,
			GuildID:  row.GuildID,
			Kind:     row.Kind,
			Status:   status,
		})
	}
	return true, nil
}

func (s *GuildAssetStore) readImmutable(ctx context.Context, opID uint64) (assetOpImmutable, bool, error) {
	var (
		row  assetOpImmutable
		kind int32
	)
	err := s.db.QueryRowContext(ctx, sqlSelectAssetOpImmutable, opID).Scan(
		&row.PlayerID, &row.GuildID, &kind, &row.RefID, &row.RefCount, &row.PeriodKey,
		&row.ContributionDelta, &row.FundsDelta)
	if errors.Is(err, sql.ErrNoRows) {
		return assetOpImmutable{}, false, nil
	}
	if err != nil {
		return assetOpImmutable{}, false, fmt.Errorf("read immutable columns: %w", err)
	}
	row.Kind = pb.GuildAssetOpKind(kind)
	return row, true, nil
}

// lockCounterparty 按锁序先锁对侧账要改的行(在 CAS 之前:op 表排在 guild / guild_member 之后)。
//
//   - DONATE + APPLIED:guild 行 FOR UPDATE;帮会在才锁成员行。成员行按 op.guild_id 找 —— D2:结算一律记给
//     发起时绑定的帮会,不看玩家此刻在哪个帮。
//   - SHOP + REJECTED / ABORTED:成员行 FOR UPDATE(要退帮贡)。
//   - 其余组合不加锁:APPLIED_PARTIAL 不做对侧账;退次数 / 退限购只动计数行(锁序最后一张表)。
func lockCounterparty(ctx context.Context, tx *sql.Tx, row assetOpImmutable, status assetop.Status) (counterpartyLocks, error) {
	var locks counterpartyLocks
	switch {
	case row.Kind == pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE && status == assetop.StatusApplied:
		ok, err := lockRowExists(ctx, tx, sqlLockGuildFunds, row.GuildID)
		if err != nil {
			return locks, fmt.Errorf("lock guild %d for donation: %w", row.GuildID, err)
		}
		locks.guildOK = ok
		if !ok {
			return locks, nil
		}
		if locks.memberOK, err = lockRowExists(ctx, tx, sqlLockMemberRole, row.GuildID, row.PlayerID); err != nil {
			return locks, fmt.Errorf("lock donor %d of guild %d: %w", row.PlayerID, row.GuildID, err)
		}
	case row.Kind == pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP &&
		(status == assetop.StatusRejected || status == assetop.StatusAborted):
		ok, err := lockRowExists(ctx, tx, sqlLockMemberRole, row.GuildID, row.PlayerID)
		if err != nil {
			return locks, fmt.Errorf("lock buyer %d of guild %d: %w", row.PlayerID, row.GuildID, err)
		}
		locks.memberOK = ok
	}
	return locks, nil
}

// lockRowExists 跑一条单列的加锁读,只关心"行在不在"。
func lockRowExists(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	var ignored uint64
	err := tx.QueryRowContext(ctx, query, args...).Scan(&ignored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// applyCounterparty 是 Finalize 与 ResolveManually 共用的对侧账(D2 覆盖后的 §5.19.3 第 4 步):
//
//	| kind            | 终态               | 动作                                                                 |
//	| 任意            | APPLIED_PARTIAL    | 无(只 CAS;部分发放转人工补偿,不退次数、不退帮贡)                    |
//	| DONATE          | APPLIED            | 帮会在:资金 += funds_delta;成员在:帮贡两列 += c;缺哪边计哪条 orphan |
//	| DONATE          | REJECTED / ABORTED | 退今日次数 1 次                                                      |
//	| SHOP            | REJECTED / ABORTED | 成员在:退 balance;否则 orphan{shop,refund_member_gone};有周期键则退限购 ref_count 份 |
//	| SHOP            | APPLIED            | 无                                                                   |
//	| ACTIVITY_REWARD | 任意               | 无(帮贡与资金在入队事务里已记完)                                     |
//	| 其它            | 任意               | 只 CAS,提交后 ERROR                                                  |
//
// 增量为 0 的那一列跳过:ClientFoundRows=false 下"加 0"的 UPDATE RowsAffected 为 0,会被恰好一行的自检误判。
func applyCounterparty(ctx context.Context, tx *sql.Tx, row assetOpImmutable, status assetop.Status,
	locks counterpartyLocks, nowMs uint64) (counterpartyOutcome, error) {
	var out counterpartyOutcome
	if status == assetop.StatusAppliedPartial {
		return out, nil
	}
	refunded := status == assetop.StatusRejected || status == assetop.StatusAborted

	switch row.Kind {
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE:
		switch {
		case status == assetop.StatusApplied:
			if !locks.guildOK {
				out.orphanKind, out.orphanWhat = orphanKindDonate, orphanWhatGuildGone
				return out, nil
			}
			if row.FundsDelta > 0 {
				if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("credit funds of guild %d", row.GuildID),
					sqlCreditGuildFunds, row.FundsDelta, row.GuildID); err != nil {
					return out, err
				}
				out.touched = true
			}
			if !locks.memberOK {
				out.orphanKind, out.orphanWhat = orphanKindDonate, orphanWhatMemberGone
				return out, nil
			}
			if row.ContributionDelta > 0 {
				if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("credit contribution of member %d in guild %d", row.PlayerID, row.GuildID),
					sqlCreditContribution, row.ContributionDelta, row.ContributionDelta, row.GuildID, row.PlayerID); err != nil {
					return out, err
				}
				out.touched = true
			}
		case refunded:
			if err := refundCounter(ctx, tx, row, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, nowMs); err != nil {
				return out, err
			}
		}
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP:
		if !refunded {
			return out, nil
		}
		switch {
		case !locks.memberOK:
			out.orphanKind, out.orphanWhat = orphanKindShop, orphanWhatRefundMemberGone
		case row.ContributionDelta > 0:
			if err := execExactlyOneRow(ctx, tx, fmt.Sprintf("refund contribution of member %d in guild %d", row.PlayerID, row.GuildID),
				sqlRefundContribution, row.ContributionDelta, row.GuildID, row.PlayerID); err != nil {
				return out, err
			}
			out.touched = true
		}
		if err := refundCounter(ctx, tx, row, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, row.RefCount, nowMs); err != nil {
			return out, err
		}
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD:
		// 只 CAS。
	default:
		out.unknownKind = true
	}
	return out, nil
}

// refundCounter 退回 n 次计数。period_key == 0 表示当初没占计数行(不限购的商品),跳过。
// 计数行可能已被清理(跨了保留期)或本来就被别的路径减到 0:影响 0 行无害,不做自检。
// IF 兜底到 0:计数是 unsigned 列,减穿会报 1690 而让整笔终结失败。
func refundCounter(ctx context.Context, tx *sql.Tx, row assetOpImmutable, kind pb.GuildDailyCounterKind, n uint32, nowMs uint64) error {
	if row.PeriodKey == 0 || n == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, sqlRefundCounter, n, n, nowMs,
		row.PlayerID, int32(kind), row.RefID, row.PeriodKey); err != nil {
		return fmt.Errorf("refund daily counter (player=%d ref=%d period=%d): %w", row.PlayerID, row.RefID, row.PeriodKey, err)
	}
	return nil
}

// OldestPendingCreatedMs 实现 assetop.PendingAgeReader,喂 assetop_pending_oldest_age_seconds{stream}。
// 每 30s 一次的聚合读,走 idx_guild_asset_op_0 的 status 前缀(只扫未决行)。
func (s *GuildAssetStore) OldestPendingCreatedMs(ctx context.Context, stream assetpb.AssetOpStream) (uint64, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, sqlOldestPendingAssetOp, PendingStatus(), uint32(stream)).Scan(&oldest); err != nil {
		return 0, false, fmt.Errorf("oldest pending %s: %w", guildAssetOpTable, err)
	}
	if !oldest.Valid || oldest.Int64 < 0 {
		return 0, false, nil
	}
	return uint64(oldest.Int64), true, nil
}

// ── assetopfix 用的读 ────────────────────────────────────────

// GetOp 按主键读一整行。found=false = 行不存在。
func (s *GuildAssetStore) GetOp(ctx context.Context, opID uint64) (AssetOpRow, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()
	rec, found, err := s.getRecord(ctx, opID)
	if err != nil || !found {
		return AssetOpRow{}, found, err
	}
	return assetOpRowFrom(rec), true, nil
}

// ListStuck 列出创建时刻早于 createdBeforeMs 的未决行,按创建时刻升序,至多 limit 条。limit <= 0 返回空。
func (s *GuildAssetStore) ListStuck(ctx context.Context, createdBeforeMs uint64, limit int) ([]AssetOpRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()
	return queryAssetOpRows(ctx, s.db, "list stuck asset ops", sqlListStuckAssetOps, PendingStatus(), createdBeforeMs, limit)
}

func (s *GuildAssetStore) getRecord(ctx context.Context, opID uint64) (*pb.GuildAssetOpRecord, bool, error) {
	rec, err := scanAssetOpRecord(s.db.QueryRowContext(ctx, sqlSelectAssetOpByID, opID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("select %s %d: %w", guildAssetOpTable, opID, err)
	}
	return rec, true, nil
}

// ── 清理(goroutine guild.asset_op_cleanup)─────────────────────

// CleanupConf 是清理任务的节律与保留期。
//
// TerminalRetention 同时是 B5d 回档检查"保留期可证明性"的下界(90 part2 §3 末行):
// 删掉的终态行回档检查就再也看不见,调小它之前先核对回档窗口。
type CleanupConf struct {
	Interval          time.Duration
	TerminalRetention time.Duration
	CounterRetention  time.Duration
}

// RunCleanup 阻塞运行清理循环,ctx 取消即返回;调用方用 safego.Go("guild.asset_op_cleanup", …) 启动。
//
// 每个副本都跑:删除幂等,多副本同时跑无害;随机初始延迟 [0, Interval) 只是把各副本错开,少抢同一批行锁。
// 每一轮各自 recover:单轮 panic 只丢那一轮,清理不会从此静默停摆。
func (s *GuildAssetStore) RunCleanup(ctx context.Context, c CleanupConf) {
	if c.Interval <= 0 {
		logx.Errorf("[GuildAsset] 清理间隔非法(%v),清理任务不启动", c.Interval)
		return
	}
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(c.Interval))))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		safego.Run("guild.asset_op_cleanup", func() {
			if err := s.CleanupOnce(ctx, time.Now(), c); err != nil && ctx.Err() == nil {
				logx.Errorf("[GuildAsset] 清理失败: %v", err)
			}
		})
		timer.Reset(c.Interval)
	}
}

// CleanupOnce 跑一轮清理(可测:时刻由调用方给)。三条语句各自分批,每批一个 RC 事务(WithTxRetry,
// 不用连接默认的 RR 裸跑 —— RR 的范围 DELETE 会在 idx_0 上留下间隙锁,挡住业务事务插新行)。
// 一条语句失败不影响另外两条,错误合并返回;已删的行数照常计指标。
func (s *GuildAssetStore) CleanupOnce(ctx context.Context, now time.Time, c CleanupConf) error {
	if c.TerminalRetention <= 0 || c.CounterRetention <= 0 {
		return fmt.Errorf("guild asset cleanup: retention must be positive (terminal=%v counter=%v)",
			c.TerminalRetention, c.CounterRetention)
	}
	nowMs := uint64(now.UnixMilli())
	terminalMs := uint64(c.TerminalRetention / time.Millisecond)
	var opCutoffMs uint64
	if nowMs > terminalMs {
		opCutoffMs = nowMs - terminalMs
	}
	counterCutoff := now.Add(-c.CounterRetention)
	dayCutoff := gameday.DayKey(counterCutoff)
	weekCutoff := gameday.WeekKey(counterCutoff)

	var errs []error
	if opCutoffMs > 0 {
		errs = append(errs, s.deleteInBatches(ctx, cleanupTableAssetOp, sqlCleanupTerminalOps,
			int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED),
			int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED),
			int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED),
			opCutoffMs))
	}
	errs = append(errs,
		s.deleteInBatches(ctx, cleanupTableCounter, sqlCleanupDailyCounter, uint32(dayKeyFloor), dayCutoff),
		s.deleteInBatches(ctx, cleanupTableCounter, sqlCleanupDailyCounter, uint32(weekKeyFloor), weekCutoff))
	return errors.Join(errs...)
}

// deleteInBatches 反复执行一条带 LIMIT 的 DELETE(最后一个占位符是批大小,由本函数追加),
// 直到某批不足 cleanupBatchSize 行、达到 cleanupMaxBatches 批或 ctx 结束。
func (s *GuildAssetStore) deleteInBatches(ctx context.Context, table, query string, args ...any) error {
	batchArgs := append(append(make([]any, 0, len(args)+1), args...), cleanupBatchSize)
	for batch := 0; batch < cleanupMaxBatches; batch++ {
		deleted, err := s.deleteOneBatch(ctx, query, batchArgs)
		if deleted > 0 {
			recordCleanupDeleted(table, deleted)
		}
		if err != nil {
			return fmt.Errorf("cleanup %s: %w", table, err)
		}
		if deleted < cleanupBatchSize {
			return nil
		}
		timer := time.NewTimer(cleanupBatchPause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func (s *GuildAssetStore) deleteOneBatch(ctx context.Context, query string, args []any) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, cleanupBatchBudget)
	defer cancel()
	var deleted int64
	err := assetop.WithTxRetry(ctx, s.db, backgroundTxAttempts, isRetryableBackground, func(tx *sql.Tx) error {
		deleted = 0
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		deleted = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
