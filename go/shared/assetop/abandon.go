package assetop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	componentpb "proto/common/component"
)

// 自动放弃(ABANDONED):docs/design/landing-recovery-sync-digest.md §6.3。
//
// PENDING 超过 AbandonAfter(默认 180 天)、玩家一直不在线的行,在逐行拿到下面六条证据之后终结为
// StatusAbandoned,对侧账同 REJECTED / ABORTED(由业务方的 Abandoner.Abandon 在同一事务里做):
//
//  1. status = PENDING 且 created_ms 早于 now − AbandonAfter;
//  2. 以普通 Claim CAS 拿到租约(与重投循环、落地唤醒互斥);
//  3. 写权行 released=1:玩家没有持有者(与条件 5 的账本出自同一条语句);
//  4. 距该行最后一次**可能**的发送已超过 AbandonSafetyGap(看 Op.LastSendMs,见下文契约);
//  5. 已落盘账本**确定**未见这条 seq;已见则按已见结局走正常 Finalize;
//  6. 以 (op_id, status=PENDING, lease_token) 为条件在一个事务里终结并做对侧账。
//
// # 为什么这是不变量 I7 的窄例外,而不是破坏它
//
// I7(04-asset-channel.md §4.2)禁止 Go 自判中止,理由是:scene 没记账,就不能保证一个晚到的请求
// 将来不会把这个 seq 应用掉 —— Go 这边已经退了款,scene 那边又发了货,东西就复制了。
// 所以放弃要成立,只需证明两件事:scene 今后不可能再收到这条 seq,且它此前没有应用过。
//
//   - 对一条 seq 发请求的只有**持租约**的一方(Tick / WakePlayer / 业务同步投递),每次发送都落在
//     某个租约窗口之内;
//   - 放弃方以普通 Claim 拿到租约(条件 2),提交时以令牌为条件(条件 6):从领到租约到提交之间
//     没有别人能发,提交之后行已终结、再也领不出来;租约中途被人接管则令牌对不上,提交落空;
//   - 距此前任何一次可能的发送已超过安全间隔(条件 4)。gRPC 请求的生命期以秒计(单次 CallTimeout
//     800ms,单行 OpBudget 2.5s),所以既没有在途请求,也没有「scene 已在内存里应用、存盘还没落」的窗口;
//
// 以上三条保证 scene 不可能再收到这条 seq。再加上:
//
//   - 写权行已释放(条件 3):持有者已最终存盘,没有任何 scene 实体持有他,也就没有内存里尚未落盘的账本;
//   - 已落盘账本确定未见(条件 5)。「确定」= 有写权行,且这条流的账本是同纪元或更旧纪元(按空账本看),
//     seq 在窗内未见或超出窗口上沿。纪元比请求新(stale)、滑出窗口、跳号过远、账本损坏、根本没有写权行,
//     都给不出「确定未见」,一律不放弃。
//
// 于是「Go 退款、scene 从未发货」是唯一可能的终局,与 ABORTED 等价。任何一条拿不到证据都跳过、
// 还租约并计数,绝不猜(AGENTS §11.3,玩家资产路径 fail-closed)。
//
// seq 窗口的后果:放弃一条 scene 从未见过的 seq 与人工终结相同,会拉开 next_seq 与 scene max_seq 的
// 距离(04 §4.29:累计超过 511 时新 seq 判 kJumpTooFar,按 §4.31 抬纪元)。每个 (玩家, 流) 的未决上限
// 是 16(I5),一次放弃最多拉开 16,远低于 511。
//
// # LastSendMs 契约(条件 4 的依据)
//
// 设计稿原写「用 updated_ms 判断」,核实后不可用(2026-10-14,guild asset_store.go):
//   - Store.Claim 自己就写 updated_ms(sqlClaimAssetOp),放弃方领到行之后回读到的永远是「刚才」;
//   - 停放档每约 30 分钟领一次行,本地合成 NOT_HERE 之后的 Reschedule 也写 updated_ms ——
//     哪怕根本没有发出请求。于是 updated_ms 永远新于 1 小时,放弃永远不会发生。
//
// 所以条件 4 改看 Op.LastSendMs:「此前任何一次投递请求可能发出的最晚时刻」。它必须同时满足
// **安全**(不小于任何一次真实发送的时刻,包括发完请求就崩溃、没来得及 Reschedule 的那一次)与
// **活性**(本地 NOT_HERE、放弃方自己的领取与归还都不推进它)。Store 按下面四条维护 last_send_ms 列:
//
//  1. 插行时写 created_ms(插行即持租约,同步投递紧接着就可能发出)。存量行不做单独的回填迁移,
//     在第 2 条的 Claim CAS 里懒初始化:last_send_ms = 0 时先取 GREATEST(created_ms, updated_ms) 作初值,
//     再与 lease_until_ms 取 GREATEST。此前每次发送之后都有一次写 updated_ms 的重排或终结,没收尾的那次
//     由 lease_until_ms 盖住,所以初值不小于任何真实发送(设计 §6.3「last_send_ms 契约」第 5 条)。
//  2. Claim 的那条 CAS 里,**先于**改写 lease_until_ms 写
//     last_send_ms = GREATEST(last_send_ms, lease_until_ms)。
//     上一个租约若没有正常收尾(持有者发完请求就崩溃、Reschedule 落空或出错),lease_until_ms 仍是那个
//     已过期的非零值,而那次发送一定早于它(投递预算 < 租约)—— 折进来就盖住了它;正常收尾的租约
//     lease_until_ms 已清 0,折叠无效果。赋值写在 lease_until_ms 之前,「从左到右求值」(MySQL)与
//     「一律用旧值求值」两种实现读到的都是旧租约。回读时填 Op.LastSendMs 与 Op.CreatedMs。
//  3. Reschedule 写 last_send_ms = IF(?, GREATEST(last_send_ms, ?), last_send_ms),参数 (!res.Local, nowMs)。
//     res.Local 为真表示本次根本没发出请求(玩家无位置);传输失败、坏流号等不是 Local 的结果一律按
//     「发过」保守处理。nowMs 是 ProcessOne 的开始时刻,比真实发送早不到一个 OpBudget(< Lease),
//     所以 AbandonSafetyGap 的下限取 max(10 分钟, Lease)。
//  4. 毒行推迟与 ReleaseLease 只把 lease_until_ms 清 0,不动 last_send_ms;Finalize / Abandon 之后行已终结。
//
// 0 视为「Store 没维护」:放弃路径跳过并计 not_candidate,绝不拿 0 去算间隔(那等于条件 4 恒成立)。

// 自动放弃配置的硬边界,见 LoopConfig 对应字段的注释。
const (
	minAbandonAfter     = 30 * 24 * time.Hour
	minAbandonSafetyGap = 10 * time.Minute
	maxAbandonBatch     = 1000
)

// validateAbandon 校验自动放弃的配置。AbandonAfter == 0 表示关闭,其余三项随之不校验。
func (c LoopConfig) validateAbandon() error {
	if c.AbandonAfter == 0 {
		return nil
	}
	if c.AbandonAfter < minAbandonAfter {
		return fmt.Errorf("assetop: AbandonAfter(%v)必须为 0(关闭)或不小于 %v:放弃不可逆", c.AbandonAfter, minAbandonAfter)
	}
	if c.AbandonSafetyGap < minAbandonSafetyGap || c.AbandonSafetyGap < c.Lease {
		return fmt.Errorf("assetop: AbandonSafetyGap(%v)必须不小于 %v 且不小于 Lease(%v)",
			c.AbandonSafetyGap, minAbandonSafetyGap, c.Lease)
	}
	if c.AbandonBatch < 1 || c.AbandonBatch > maxAbandonBatch {
		return fmt.Errorf("assetop: AbandonBatch 必须在 [1, %d](当前 %d)", maxAbandonBatch, c.AbandonBatch)
	}
	if c.AbandonInterval <= 0 {
		return fmt.Errorf("assetop: 开启自动放弃时 AbandonInterval 必须为正(当前 %v)", c.AbandonInterval)
	}
	return nil
}

// Abandoner 是 Store 的**可选**能力:自动放弃。没实现它,AbandonOnce 不运行。
type Abandoner interface {
	// ListAbandonCandidates 非加锁读:status=PENDING 且 created_ms < createdBeforeMs 且 op_id > afterOpID,
	// 按 op_id 升序,只回主键,条数 <= limit。它只给候选,真正的判定在持租约之后逐行复核。
	ListAbandonCandidates(ctx context.Context, createdBeforeMs, afterOpID uint64, limit int) ([]uint64, error)

	// Abandon 在**一个事务**里(建议包 WithTxRetry)按业务锁序加锁 →
	//   UPDATE {op} SET status=<abandoned>, lease_until_ms=0, next_attempt_ms=?, updated_ms=?
	//    WHERE op_id=? AND status=<pending> AND lease_token=?
	// RowsAffected==1 才做对侧账:与 REJECTED / ABORTED 相同(退限购、退帮贡、退捐献次数);
	// 没有可退之物的发奖类只留审计行。RowsAffected==0 → **必须**回 ErrLeaseLost(可 %w 包一层):
	// 租约在判定期间被别人接管,别人可能已经发出请求,此时绝不能放弃。
	// 令牌取 op.LeaseToken,与 Reschedule 同一口径。不置 durable(不是 scene 确认的结局),
	// 不改 last_outcome(保留最后一次真实答复作证据);next_attempt_ms 同写终结时刻(与 Finalize 一致)。
	Abandon(ctx context.Context, op Op, nowMs uint64) error

	// ReleaseLease 在判定「这一轮不放弃」之后归还租约:
	//   UPDATE {op} SET lease_until_ms=0, updated_ms=? WHERE op_id=? AND status=<pending> AND lease_token=?
	// 只清租约,不动 attempts / next_attempt_ms / last_send_ms(LastSendMs 契约第 4 条)。
	// 不还会怎样:下一次 Claim 会把这个租约折进 last_send_ms,这一行在一个安全间隔内都过不了条件 4;
	// 玩家恰好上线时,唤醒的 Claim 也会撞上这个租约而落空。
	// RowsAffected==0 → 回 ErrLeaseLost(行已被别人接管或已终结,没有可还的)。
	ReleaseLease(ctx context.Context, op Op, nowMs uint64) error
}

// StoreLedger 是 StoreLedgerReader 的一次读。
type StoreLedger struct {
	// Found:写权行存在。
	Found bool
	// Released:写权行 released=1,持有者已最终存盘并释放,或者没有人认领过。
	Released bool
	Ledger   *componentpb.PlayerAssetOpLedgerComp
}

// StoreLedgerReader 是自动放弃需要的能力:按 player_store 读,写权行和账本出自同一条语句
// (docs/design/player-save-direct-commit.md §6)。DataServiceLedger 实现它;Loop.Ledger 没实现它,自动放弃不运行。读失败回 error。
type StoreLedgerReader interface {
	ReadStoreLedger(ctx context.Context, playerID uint64) (StoreLedger, error)
}

// abandonSkip 是 assetop_abandon_skipped_total 的 reason label。0 不是合法原因,
// judgeAbandon 用它表示「全部条件成立」。
type abandonSkip uint8

const (
	// abandonSkipLocated 写权行没释放:有持有者。label 沿用 located。
	abandonSkipLocated abandonSkip = iota + 1
	// abandonSkipRecent 距最后一次可能的发送不足安全间隔。
	abandonSkipRecent
	// abandonSkipLedgerSeen 已落盘账本里已有结局:按已见结局正常终结,不放弃。
	abandonSkipLedgerSeen
	// abandonSkipLedgerError 读已落盘账本失败。
	abandonSkipLedgerError
	// abandonSkipLedgerUnknown 账本给不出「确定未见」:没有写权行、纪元过期、滑出窗口、跳号、损坏。
	abandonSkipLedgerUnknown
	// abandonSkipLeaseLost 没领到租约,或提交时令牌已对不上。
	abandonSkipLeaseLost
	// abandonSkipStoreError 领取或提交时存储出错(另计 assetop_store_errors_total)。
	abandonSkipStoreError
	// abandonSkipNotCandidate 持租约复核时不再是候选:创建时刻不够老、流号非法,或 Store 没填
	// CreatedMs / LastSendMs(契约违例,打 ERROR)。
	abandonSkipNotCandidate

	abandonSkipCount
)

var abandonSkipNames = [...]string{
	"invalid", "located", "recent", "ledger_seen", "ledger_error",
	"ledger_unknown", "lease_lost", "store_error", "not_candidate",
}

var _ = [1]struct{}{}[len(abandonSkipNames)-int(abandonSkipCount)]

// String 返回 label 值;越界回 "invalid"。
func (r abandonSkip) String() string {
	if r >= abandonSkipCount {
		return abandonSkipNames[0]
	}
	return abandonSkipNames[r]
}

// AbandonOnce 跑一轮自动放弃,返回本轮放弃的行数。
//
// nowMs 是本轮唯一的判定时刻:整轮用同一个值,偏旧只会让截止时刻与安全间隔的判定更保守。
// 一轮最多处理 AbandonBatch 行;候选按 op_id 翻页,下一轮从上一轮最后一个 op_id 之后接着翻,
// 翻到底再从头开始 —— 否则前 200 个候选若一直被跳过(比如玩家在线),后面的候选永远轮不到。
//
// 装配不齐(AbandonAfter 为 0、Store 没实现 Abandoner、Ledger 读不了 player_store)时整个功能不运行,
// 只打一次 INFO:缺任何一条证据来源都不该放弃。只有列候选失败才返回 error;逐行跳过只计数与打日志。
func (l *Loop) AbandonOnce(ctx context.Context, nowMs uint64) (int, error) {
	ab, ok := l.abandoner()
	if !ok {
		return 0, nil
	}
	cutoffMs := l.abandonCutoffMs(nowMs)
	if cutoffMs == 0 {
		return 0, nil
	}

	l.abandonMu.Lock()
	defer l.abandonMu.Unlock()

	ids, err := ab.ListAbandonCandidates(ctx, cutoffMs, l.abandonCursor, l.cfg.AbandonBatch)
	if err != nil {
		l.metrics.incStoreError("abandon_list")
		logx.Errorf("[AssetOp] Failed to list abandon candidates: %v", err)
		return 0, fmt.Errorf("assetop: 列出放弃候选失败: %w", err)
	}
	l.abandonCursor = nextAbandonCursor(ids, l.cfg.AbandonBatch)

	abandoned := 0
	for _, opID := range ids {
		if ctx.Err() != nil {
			break
		}
		if l.abandonOne(ctx, ab, opID, cutoffMs, nowMs) {
			abandoned++
		}
	}
	if len(ids) > 0 {
		logx.Infof("[AssetOp] Abandon round finished: candidates=%d abandoned=%d next_cursor=%d",
			len(ids), abandoned, l.abandonCursor)
	}
	return abandoned, nil
}

// nextAbandonCursor:本页不满说明已经翻到底,下一轮从头开始;否则从本页最大的 op_id 之后接着翻。
func nextAbandonCursor(ids []uint64, batch int) uint64 {
	if len(ids) < batch {
		return 0
	}
	var maxID uint64
	for _, id := range ids {
		if id > maxID {
			maxID = id
		}
	}
	return maxID
}

// abandoner 检查自动放弃是否装配齐全。配置关闭时静默;配置开了却缺零件时打一次 INFO。
func (l *Loop) abandoner() (Abandoner, bool) {
	if l.cfg.AbandonAfter <= 0 {
		return nil, false
	}
	ab, ok := l.store.(Abandoner)
	missing := ""
	switch {
	case !ok:
		missing = "store does not implement Abandoner"
	case l.storeLedger() == nil:
		missing = "Loop.Ledger cannot read player_store"
	}
	if missing != "" {
		l.abandonOffOnce.Do(func() {
			logx.Infof("[AssetOp] Automatic abandon not running: %s", missing)
		})
		return nil, false
	}
	return ab, true
}

// abandonCutoffMs 是「创建时刻早于它才算候选」的截止时刻;0 表示没有候选(功能关闭或时钟太早)。
// 日对账的 abandon_candidate 类也按它分,两边口径只此一处。
func (l *Loop) abandonCutoffMs(nowMs uint64) uint64 {
	if l.cfg.AbandonAfter <= 0 {
		return 0
	}
	after := uint64(l.cfg.AbandonAfter / time.Millisecond)
	if nowMs <= after {
		return 0
	}
	return nowMs - after
}

// abandonOne 处理一个候选,返回是否放弃了它。持租约之后的每一条出口要么终结这一行,
// 要么 ReleaseLease 把租约还回去。
func (l *Loop) abandonOne(ctx context.Context, ab Abandoner, opID, cutoffMs, nowMs uint64) bool {
	op, claimed := l.claim(ctx, opID)
	switch claimed {
	case claimOK:
	case claimFailed:
		l.metrics.incAbandonSkipped(abandonSkipStoreError)
		return false
	default:
		l.metrics.incAbandonSkipped(abandonSkipLeaseLost)
		return false
	}

	skip, seen := l.judgeAbandon(ctx, op, cutoffMs, nowMs)
	switch skip {
	case 0:
		return l.commitAbandon(ctx, ab, op, nowMs)
	case abandonSkipLedgerSeen:
		l.metrics.incAbandonSkipped(skip)
		l.finalizeSeen(ctx, ab, op, seen, nowMs)
		return false
	default:
		l.metrics.incAbandonSkipped(skip)
		l.releaseAbandonLease(ctx, ab, op, nowMs)
		return false
	}
}

// judgeAbandon 在持租约的前提下逐条复核条件 1、3、4、5。返回 0 表示全部成立、可以放弃;
// 返回 abandonSkipLedgerSeen 时第二个返回值是已落盘的结局(可直接交给 finalize,durable 已置)。
func (l *Loop) judgeAbandon(ctx context.Context, op Op, cutoffMs, nowMs uint64) (abandonSkip, Result) {
	if _, ok := ApplyRPCOf(op.Stream); !ok || op.CreatedMs == 0 || op.LastSendMs == 0 {
		// 坏流号不放弃:对侧账按流与种类做,坏行的对侧账谁也说不清。缺字段是 Store 没按契约回填。
		logx.Errorf("[AssetOp] Abandon candidate failed re-check (invalid stream or store did not fill created_ms / last_send_ms); skipped op_id=%d stream=%d created_ms=%d last_send_ms=%d",
			op.OpID, op.Stream, op.CreatedMs, op.LastSendMs)
		return abandonSkipNotCandidate, Result{}
	}
	if op.CreatedMs >= cutoffMs {
		return abandonSkipNotCandidate, Result{}
	}

	// 读账本共用投递那一份预算:持租约期间的总耗时要留在 OpBudget 之内(Lease >= OpBudget + 余量),
	// 余下的 settleBudget 留给提交或还租约。
	checkCtx, cancel := context.WithTimeout(ctx, l.cfg.OpBudget-settleBudget)
	defer cancel()

	reader := l.storeLedger()
	if reader == nil {
		// abandoner 已保证装配齐全;走到这里按读失败处理(fail-closed),绝不放弃。
		return abandonSkipLedgerError, Result{}
	}
	snapshot, err := reader.ReadStoreLedger(checkCtx, op.PlayerID)
	if err != nil {
		logx.Errorf("[AssetOp] Failed to read player store for abandon check; skipped op_id=%d: %v", op.OpID, err)
		return abandonSkipLedgerError, Result{}
	}
	return l.judgeWithPlayerStore(op, snapshot, nowMs)
}

// judgeWithPlayerStore 是条件 3、4、5(docs/design/player-save-direct-commit.md §6):
// 写权行 released=1 代替「没有位置」,和账本出自同一条语句,判「不在线」和判「账本未见」看的是同一个时刻;
// 只有一个来源,不需要副本核验,也不会出现证据落后于父键。
// released=0 且持有者活着却没有实体时要等到进程退出或下次登录(规格 §9.8):不丢不复制。持有节点死了由 scene_manager 判死后释放。
func (l *Loop) judgeWithPlayerStore(op Op, snapshot StoreLedger, nowMs uint64) (abandonSkip, Result) {
	if !snapshot.Found {
		logx.Infof("[AssetOp] Abandon skipped: player has no owner row op_id=%d stream=%d seq=%d", op.OpID, op.Stream, op.Seq)
		return abandonSkipLedgerUnknown, Result{}
	}
	if !snapshot.Released {
		return abandonSkipLocated, Result{}
	}
	gapMs := uint64(l.cfg.AbandonSafetyGap / time.Millisecond)
	if op.LastSendMs >= nowMs || nowMs-op.LastSendMs < gapMs {
		return abandonSkipRecent, Result{}
	}
	switch verdict, state := abandonVerdict(snapshot.Ledger, op); verdict {
	case ledgerUnseen:
		return 0, Result{}
	case ledgerSeen:
		return seenForFinalize(snapshot.Ledger, op, true)
	default:
		logx.Infof("[AssetOp] Abandon skipped: persisted ledger cannot prove seq unseen op_id=%d stream=%d seq=%d epoch=%d state=%s",
			op.OpID, op.Stream, op.Seq, op.StreamEpoch, state)
		return abandonSkipLedgerUnknown, Result{}
	}
}

// ledgerVerdict 是一份已落盘账本对某个 seq 能给出的结论(条件 5)。
type ledgerVerdict uint8

const (
	// ledgerUnknown 给不出结论:没有账本、纪元比请求新、滑出窗口、跳号过远、账本损坏。
	ledgerUnknown ledgerVerdict = iota
	// ledgerUnseen 确定未见:同纪元或更旧纪元(按空账本看),seq 在窗内未见或超出窗口上沿。
	ledgerUnseen
	// ledgerSeen 已有结局(已应用或已拒绝)。
	ledgerSeen
)

func abandonVerdict(ledger *componentpb.PlayerAssetOpLedgerComp, op Op) (ledgerVerdict, SeqState) {
	if ledger == nil {
		return ledgerUnknown, SeqInvalid
	}
	state := ClassifySeq(FindStream(ledger, op.Stream), op.StreamEpoch, op.Seq)
	switch state {
	case SeqUnseen, SeqAheadOfWindow:
		return ledgerUnseen, state
	case SeqApplied, SeqRejected:
		return ledgerSeen, state
	default:
		return ledgerUnknown, state
	}
}

// seenForFinalize 把账本里已见的结局交给 finalizeSeen(durable 已置;sqlCommitted 表示这份账本是 SQL 证据)。
// 托管扣出缺可用原件时既不能终结(托管方拿不到完整实例),也不能放弃(scene 已应用),只能跳过并打 ERROR,
// 与 tryPersistedLedger 的同一判据(resultWithReceipt)一致。
func seenForFinalize(ledger *componentpb.PlayerAssetOpLedgerComp, op Op, sqlCommitted bool) (abandonSkip, Result) {
	outcome, partial, reason := ClassifyPersisted(ledger, op.Stream, op.StreamEpoch, op.Seq)
	res := resultWithReceipt(ledger, op, Result{Outcome: outcome, Reason: reason, Durable: true, SQLCommitted: sqlCommitted, Partial: partial})
	if !res.Durable {
		logx.Errorf("[AssetOp] Abandon skipped: persisted outcome lacks matching request identity or escrow instance; manual handling required op_id=%d stream=%d seq=%d corr=%d",
			op.OpID, op.Stream, op.Seq, op.CorrelationID)
		return abandonSkipLedgerUnknown, Result{}
	}
	return abandonSkipLedgerSeen, res
}

// commitAbandon 执行条件 6。Abandon 落空(令牌对不上)说明租约已被别人接管,什么都不做;
// 其它错误按事务已回滚处理,把租约还回去。提交结果不明(例如 COMMIT 时断线)时还租约的 CAS
// 会因为 status 已变而落空,同样安全。
func (l *Loop) commitAbandon(ctx context.Context, ab Abandoner, op Op, nowMs uint64) bool {
	settle, cancel := settleContext(ctx)
	defer cancel()
	if err := ab.Abandon(settle, op, nowMs); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			l.metrics.incAbandonSkipped(abandonSkipLeaseLost)
			logx.Infof("[AssetOp] Lease taken over during abandon check; not abandoned op_id=%d stream=%d seq=%d: %v",
				op.OpID, op.Stream, op.Seq, err)
			return false
		}
		l.metrics.incStoreError("abandon")
		l.metrics.incAbandonSkipped(abandonSkipStoreError)
		logx.Errorf("[AssetOp] Failed to abandon pending row op_id=%d stream=%d seq=%d: %v", op.OpID, op.Stream, op.Seq, err)
		l.releaseAbandonLease(ctx, ab, op, nowMs)
		return false
	}
	l.metrics.incAbandoned(op.Stream)
	// 每一行放弃都是事后人工补偿的依据:op_id / correlation_id 能在 scene 流水与业务表里定位到它。
	logx.Infof("[AssetOp] Pending row abandoned (player offline beyond threshold, persisted ledger unseen) op_id=%d stream=%d seq=%d epoch=%d corr=%d created_ms=%d last_send_ms=%d",
		op.OpID, op.Stream, op.Seq, op.StreamEpoch, op.CorrelationID, op.CreatedMs, op.LastSendMs)
	return true
}

// finalizeSeen 按已落盘的结局正常终结(条件 5 的「已见」分支),与 ProcessOne 离线读账本那条路径同口径:
// 过了截止时间的行按 Abort 判定状态(reason 0 的 REJECTED 落 Aborted)。终结失败时事务已回滚,还租约。
func (l *Loop) finalizeSeen(ctx context.Context, ab Abandoner, op Op, seen Result, nowMs uint64) {
	rpc, ok := l.rpcFor(op, nowMs)
	if !ok {
		// judgeAbandon 已经拦过坏流号,走到这里只可能是 bug。
		l.releaseAbandonLease(ctx, ab, op, nowMs)
		return
	}
	logx.Infof("[AssetOp] Abandon check found persisted outcome; finalizing normally op_id=%d stream=%d seq=%d outcome=%s partial=%t",
		op.OpID, op.Stream, op.Seq, outcomeLabel(seen.Outcome), seen.Partial)
	if _, err := l.finalize(ctx, op, rpc, seen, nowMs); err != nil {
		logx.Errorf("[AssetOp] Failed to finalize row seen in persisted ledger during abandon check op_id=%d: %v", op.OpID, err)
		l.releaseAbandonLease(ctx, ab, op, nowMs)
	}
}

// releaseAbandonLease 归还租约。落空(ErrLeaseLost)说明行已被接管或已终结,不是错误;
// 其它失败只计数:租约会自然过期,代价是这一行晚一个安全间隔才能再判(见 Abandoner.ReleaseLease)。
func (l *Loop) releaseAbandonLease(ctx context.Context, ab Abandoner, op Op, nowMs uint64) {
	settle, cancel := settleContext(ctx)
	defer cancel()
	if err := ab.ReleaseLease(settle, op, nowMs); err != nil && !errors.Is(err, ErrLeaseLost) {
		l.metrics.incStoreError("abandon_release")
		logx.Errorf("[AssetOp] Failed to release lease after abandon check; it will expire naturally op_id=%d: %v", op.OpID, err)
	}
}
