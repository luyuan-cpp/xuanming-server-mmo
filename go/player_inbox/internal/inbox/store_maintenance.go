package inbox

import (
	"context"
	"fmt"
	"time"

	"player_inbox/internal/constants"
	"player_inbox/internal/metrics"

	inboxpb "proto/player_inbox"

	"github.com/zeromicro/go-zero/core/logx"
)

// 后台只读扫描仍不持锁；提醒CAS是纯簿记,只比 next_seq,不过角色护栏(见 ClearNotifyDue)。
// 条目行永久保存原唯一收据,没有清理:删了它,旧 GM request_id 就能换主后再次入箱,时间窗替代不了原 owner(设计 §9.1)。
// 原来空转的 SweepTerminal 与清理循环 2026-11-12 删除(数据覆写审计 r2-doc#2)。

// NotifyDue 是一条待补发的提醒:读出当时的 next_seq 作 CAS 令牌,流行的 updated_ms 作退避起算点。
type NotifyDue struct {
	PlayerID uint64
	Domain   inboxpb.PlayerInboxDomain
	NextSeq  uint64
	// PendingSinceMs 是流行最后一次写入的时刻(updated_ms),即这条提醒至少从何时起就没落定(见 notifyRetryDueMs)。
	PendingSinceMs uint64
}

// resendNotifyTimeout 是补发里单次提醒(读 location、写 Kafka、落定时的 CAS 清零)的预算。
// 没落定时的推迟 CAS 不吃它,用本轮的 ctx:提醒耗尽预算(典型是 Kafka 写超时)之后照样能推迟。
const resendNotifyTimeout = constants.NotifyTimeout * 5

// DueNotifications 读一个分片里到期的提醒:notify_due_ms ∈ [1, dueBeforeMs],最多 limit 条
// (走 (shard, notify_due_ms) 索引,设计 §6.3)。补发没落定的提醒已被推到未来(ResendNotify),到点之前不会再被读到。
func (s *Store) DueNotifications(ctx context.Context, shard uint32, dueBeforeMs uint64, limit int) ([]NotifyDue, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT `player_id`, `domain`, `next_seq`, `updated_ms` FROM "+streamTable+
		" WHERE `shard` = ? AND `notify_due_ms` BETWEEN 1 AND ? LIMIT ?", shard, dueBeforeMs, limit)
	if err != nil {
		return nil, fmt.Errorf("inbox: scan due notifications shard=%d: %w", shard, err)
	}
	defer rows.Close()
	var out []NotifyDue
	for rows.Next() {
		var d NotifyDue
		var domain uint32
		if err := rows.Scan(&d.PlayerID, &domain, &d.NextSeq, &d.PendingSinceMs); err != nil {
			return nil, err
		}
		if RowShard(d.PlayerID) != shard {
			return nil, fmt.Errorf("inbox: corrupt stream row shard=%d player_id=%d", shard, d.PlayerID)
		}
		d.Domain = inboxpb.PlayerInboxDomain(domain)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ResendNotify 给一条到期提醒补发一次(设计 §6.3、评审 L6)。ctx 是本轮补发的 ctx,单次提醒的预算由本函数加。
//   - 落定(送达 / 离线 / 等待落点 / 位置记录残缺):按读出时的 next_seq 做 CAS 置 0;
//   - 镜像已同步却查不到节点、且已悬满 notifyNodeMissingSettleAfter(notifyGivesUp,评审 R3):同样 CAS 置 0,打 ERROR;
//   - 其余没落定(Kafka / Redis 故障、镜像未同步、节点身份歧义、uuid 为空、刚开始查不到节点):在同一个 next_seq CAS 里
//     把 notify_due_ms 推到 notifyRetryDueMs 给出的时刻,到点之前不再被 DueNotifications 读到,不再每轮占着分片队头。
//
// CAS 失败都无害:这期间又有新的入箱,notify_due_ms 已被它写成 now,补发循环照常尽快处理。
func (s *Store) ResendNotify(ctx context.Context, d NotifyDue) NotifyResult {
	notifyCtx, cancel := context.WithTimeout(ctx, resendNotifyTimeout)
	result := s.notifyAndClear(notifyCtx, d.PlayerID, d.Domain, d.NextSeq)
	cancel()
	if result.Settled() {
		return result
	}
	nowMs := s.nowMs()
	if notifyGivesUp(result, nowMs, d.PendingSinceMs) {
		// 落定用本轮的 ctx:提醒可能已耗尽它自己的预算。
		logx.WithContext(ctx).Errorf("[player_inbox] Notify resend settled after the scene node stayed missing from the synced "+
			"node mirror for %v player=%d domain=%d (location points at a node that is gone; left to the landing fetch)",
			time.Duration(nowMs-d.PendingSinceMs)*time.Millisecond, d.PlayerID, d.Domain)
		if _, err := s.ClearNotifyDue(ctx, d.PlayerID, d.Domain, d.NextSeq); err != nil {
			logx.WithContext(ctx).Errorf("[player_inbox] Clearing notify_due after giving up failed player=%d domain=%d: %v "+
				"(resend loop retries it next round)", d.PlayerID, d.Domain, err)
		}
		return result
	}
	dueMs := notifyRetryDueMs(nowMs, d.PendingSinceMs)
	if _, err := s.DeferNotifyDue(ctx, d.PlayerID, d.Domain, d.NextSeq, dueMs); err != nil {
		// 推迟失败只是这一行下一轮还会被扫到(退避前的行为),不丢提醒。
		logx.WithContext(ctx).Errorf("[player_inbox] Notify resend not settled (result=%s) and deferring notify_due failed "+
			"player=%d domain=%d: %v (resend loop retries it next round)", result, d.PlayerID, d.Domain, err)
	}
	return result
}

// DeferNotifyDue 把一条没落定的提醒推迟到 dueMs 再补发,与 ClearNotifyDue 用同一个 next_seq CAS 令牌(评审 L6):
// 这期间只要又有新的入箱,next_seq 就变了,CAS 失败,notify_due_ms 保持入箱写下的 now。
// **不碰 updated_ms**:它是退避的起算点(notifyRetryDueMs),提醒的簿记不算流的业务写入(ClearNotifyDue 同样不碰)。
// 和 ClearNotifyDue 一样不过角色护栏。返回是否真的改了。
func (s *Store) DeferNotifyDue(ctx context.Context, playerID uint64, domain inboxpb.PlayerInboxDomain, expectNextSeq, dueMs uint64) (bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE "+streamTable+" SET `notify_due_ms` = ? WHERE "+streamPK+
		" AND `next_seq` = ? AND `notify_due_ms` <> 0", dueMs, RowShard(playerID), playerID, uint32(domain), expectNextSeq)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1 && err == nil, err
}

// GaugeCutoffs 是一次扫描型 gauge 用到的时间截止(Unix 毫秒,由后台循环按 now 算出,见 gaugeCutoffs)。
type GaugeCutoffs struct {
	// NotifyOverdueBeforeMs:notify_due_ms 非 0、且流行最后一次写入(updated_ms)不晚于它的流计入 notify_backlog
	// (提醒悬了 5 分钟以上)。按 updated_ms 而不按 notify_due_ms 算:补发没落定时 notify_due_ms 被推到了未来(评审 L6),
	// 按它算的话,Kafka 长时间写不进时积压反而一条都不剩。
	NotifyOverdueBeforeMs uint64
	// DeliveredStaleBeforeMs:已投递未确认、最后一次投递(last_delivered_ms)早于它的行算陈旧,只进
	// delivered_unacked_stale,不进告警口径的三项(评审 M4)。0 = 全部算近期。
	DeliveredStaleBeforeMs uint64
}

// ScanGauges 扫一遍全部分片,算出扫描型 gauge(设计 §9.4,只由领导者调用);截止见 GaugeCutoffs。
// 每个分片两条只读聚合(PENDING 行走 (shard, state, updated_ms) 索引);跨分片怎么折见 foldScanGauges。
// 任何一个分片读失败整轮作废(返回错误、不更新 gauge),不拿半套分片的数去刷新看板。
func (s *Store) ScanGauges(ctx context.Context, nowMs uint64, cut GaugeCutoffs) (metrics.ScanGauges, error) {
	stale := cut.DeliveredStaleBeforeMs
	shards := make([]shardGaugeScan, 0, ShardCount)
	for shard := uint32(0); shard < ShardCount; shard++ {
		if err := ctx.Err(); err != nil {
			return metrics.ScanGauges{}, err
		}
		var sc shardGaugeScan
		// 「近期已投递」= delivery_count > 0 且 last_delivered_ms ≥ 截止;陈旧 = delivery_count > 0 且早于截止。
		// max_delivery_count 只取近期行:没投递过的行 delivery_count 为 0,本来就不影响最大值。
		err := s.db.QueryRowContext(ctx, "SELECT COUNT(*),"+
			" COALESCE(SUM(CASE WHEN `delivery_count` > 0 AND `last_delivered_ms` >= ? THEN 1 ELSE 0 END), 0),"+
			" COALESCE(SUM(CASE WHEN `delivery_count` > 0 AND `last_delivered_ms` < ? THEN 1 ELSE 0 END), 0),"+
			" COALESCE(MAX(CASE WHEN `delivery_count` > 0 AND `last_delivered_ms` >= ? THEN `delivery_count` END), 0),"+
			" COALESCE(MIN(`created_ms`), 0),"+
			" COALESCE(MIN(CASE WHEN `delivery_count` > 0 AND `last_delivered_ms` >= ? THEN `last_delivered_ms` END), 0)"+
			" FROM "+itemTable+" WHERE `shard` = ? AND `state` = ?",
			stale, stale, stale, stale, shard, uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING)).
			Scan(&sc.openItems, &sc.deliveredUnacked, &sc.deliveredUnackedStale, &sc.maxDeliveryCount, &sc.minCreatedMs, &sc.minDeliveredMs)
		if err != nil {
			return metrics.ScanGauges{}, fmt.Errorf("inbox: gauge scan shard=%d: %w", shard, err)
		}
		// 走 (shard, notify_due_ms) 索引取出 notify_due_ms 非 0 的流(健康时几乎为空),再按 updated_ms 过滤。
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+streamTable+
			" WHERE `shard` = ? AND `notify_due_ms` >= 1 AND `updated_ms` <= ?", shard, cut.NotifyOverdueBeforeMs).Scan(&sc.notifyBacklog); err != nil {
			return metrics.ScanGauges{}, fmt.Errorf("inbox: notify backlog scan shard=%d: %w", shard, err)
		}
		shards = append(shards, sc)
	}
	return foldScanGauges(nowMs, shards), nil
}

// FinalizedRef 是对账要核对的一条终态行:对账谓词 finalizedLedgerGap 用到的列,外加翻页游标要的 updated_ms。
// 定时对账由 RecentlyFinalized 读出(只读消费方终结的行);ReconcilePlayer 由 finalizedRefOfRow 从整行取。
type FinalizedRef struct {
	PlayerID      uint64
	Domain        inboxpb.PlayerInboxDomain
	Seq           uint64
	State         inboxpb.PlayerInboxItemState
	RequestID     uint64
	MsgType       inboxpb.PlayerInboxMessageType
	OutcomeReason uint32
	FinalizedBy   inboxpb.PlayerInboxFinalizer
	UpdatedMs     uint64
}

const finalizedRefColumns = "`player_id`, `domain`, `seq`, `state`, `request_id`, `msg_type`, `outcome_reason`, `finalized_by`, `updated_ms`"

// RecentlyFinalized 读一个分片里 updated_ms ≥ sinceMs、由消费方终结的终态行的一页,最多 limit 条(设计 §9.3)。
//
// 按 (updated_ms, player_id, domain, seq) **降序**、键集翻页:before 为 nil 从最新的一行读起,否则只读严格排在
// before 之后(更旧)的行。后三项即主键,同一分片内全序唯一,翻页不重不漏;终态行的 updated_ms 终结之后不再变。
// 降序的理由:单片读满上限时被截掉的是最老的那部分 —— 它们在更早的轮次里已经作为「最新」被核对过,
// 而刚终结的行(新丢的数据)每一轮都最先被核对。
//
// 必须有 ORDER BY(评审 M5):没有它,LIMIT 截下的是索引 (shard, state, updated_ms) 上 state 最小的那一段
// (APPLIED),REJECTED / VOIDED 会被挤掉。ORDER BY 横跨 state IN (…) 的四个值,索引给不出这个序,
// 数据库要对窗口内该片的终态行排一次序;GM 域量很小(设计 §9.3),可以接受。
// 游标条件写成展开式而不是行比较 `(a, b, …) < (?, ?, …)`:MySQL 对行比较不做范围优化(同 guild activity_repo)。
func (s *Store) RecentlyFinalized(ctx context.Context, shard uint32, sinceMs uint64, before *FinalizedRef, limit int) ([]FinalizedRef, error) {
	query := "SELECT " + finalizedRefColumns + " FROM " + itemTable +
		" WHERE `shard` = ? AND `state` IN " + terminalStatesSQL + " AND `updated_ms` >= ? AND `finalized_by` = ?"
	args := []any{shard, sinceMs, uint32(inboxpb.PlayerInboxFinalizer_PLAYER_INBOX_FINALIZER_CONSUMER)}
	if before != nil {
		query += " AND (`updated_ms` < ? OR (`updated_ms` = ? AND (`player_id` < ? OR (`player_id` = ? AND" +
			" (`domain` < ? OR (`domain` = ? AND `seq` < ?))))))"
		args = append(args, before.UpdatedMs, before.UpdatedMs, before.PlayerID, before.PlayerID,
			uint32(before.Domain), uint32(before.Domain), before.Seq)
	}
	query += " ORDER BY `updated_ms` DESC, `player_id` DESC, `domain` DESC, `seq` DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("inbox: scan finalized shard=%d: %w", shard, err)
	}
	defer rows.Close()
	var out []FinalizedRef
	for rows.Next() {
		var f FinalizedRef
		var domain, state, msgType, finalizedBy uint32
		if err := rows.Scan(&f.PlayerID, &domain, &f.Seq, &state, &f.RequestID, &msgType, &f.OutcomeReason,
			&finalizedBy, &f.UpdatedMs); err != nil {
			return nil, err
		}
		f.Domain, f.State = inboxpb.PlayerInboxDomain(domain), inboxpb.PlayerInboxItemState(state)
		f.MsgType, f.FinalizedBy = inboxpb.PlayerInboxMessageType(msgType), inboxpb.PlayerInboxFinalizer(finalizedBy)
		out = append(out, f)
	}
	return out, rows.Err()
}

// playerItemsPageSize 是 ReconcilePlayer 读一名玩家条目行时一页的行数。翻页读完这名玩家的全部行,不设总量上限:
// 条目行永久保留(设计 §9.1),老玩家的战斗结算行一直增长,设了上限就只能做部分核对。每页是一段主键范围扫描,
// 内存只占一页,总耗时随这名玩家的行数线性增长;读不完(超时、库错误)回错误,不回半份。
// 以前一次至多读 5000 行,超出时只打 ERROR、照样回结果,运维看到的「无差异」可能只是后面的行没读到(数据覆写审计 r2,2026-11-12 改)。
const playerItemsPageSize = 1000

// forEachPlayerItemsPage 按 (domain, seq) 升序键集翻页读一名玩家(可选一个域)的全部条目行,每读到一页交给 visit。
// 翻页条件落在主键 (shard, player_id, domain, seq) 上,每页是一段主键范围扫描,不随翻到第几页变慢。
// 读失败或 visit 返回错误时停下并返回错误:对账结果要么完整,要么是错误,不回半份。
// 各页是各自的读快照:翻页期间新入箱的行,排在游标之后的会读到;已读过的行之后再变,不回头重读。
func (s *Store) forEachPlayerItemsPage(ctx context.Context, playerID uint64, domain inboxpb.PlayerInboxDomain, pageSize int,
	visit func(rows []*inboxpb.PlayerInboxItemRow) error) error {
	if pageSize <= 0 {
		return fmt.Errorf("inbox: player items page size must be positive, got %d", pageSize)
	}
	var afterDomain uint32
	var afterSeq uint64
	first := true
	for {
		query := "SELECT " + itemColumns + " FROM " + itemTable + " WHERE `shard` = ? AND `player_id` = ?"
		args := []any{RowShard(playerID), playerID}
		if domain != inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_UNSPECIFIED {
			query += " AND `domain` = ?"
			args = append(args, uint32(domain))
		}
		if !first {
			query += " AND (`domain` > ? OR (`domain` = ? AND `seq` > ?))"
			args = append(args, afterDomain, afterDomain, afterSeq)
		}
		query += " ORDER BY `domain` ASC, `seq` ASC LIMIT ?"
		args = append(args, pageSize)
		rows, err := queryItems(ctx, s.db, query, args...)
		if err != nil {
			return fmt.Errorf("inbox: read player items player=%d after domain=%d seq=%d: %w", playerID, afterDomain, afterSeq, err)
		}
		if len(rows) > 0 {
			if err := visit(rows); err != nil {
				return err
			}
			last := rows[len(rows)-1]
			afterDomain, afterSeq, first = last.GetDomain(), last.GetSeq(), false
		}
		if len(rows) < pageSize {
			return nil
		}
	}
}
