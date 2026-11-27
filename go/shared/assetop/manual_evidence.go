package assetop

import (
	"context"
	"fmt"
	"time"

	assetpb "proto/common/asset"
)

// 人工终结的证据与结论:trade、guild 两个 cmd/assetopfix 共用这一份判据
// (docs/design/data-consistency-guarantee-map-20261020.md §6.2 第 2 条,手册 docs/ops/trade-commerce-quarantine-runbook.md §4.1)。
//
// 证据只认玩家**已提交**的资产账本(StoreLedgerReader 一次读出的写权行与账本),不认 scene 流水。流水描述的是存档改动:
// 2026-11-05 之前在内存改动时就发、早于存盘提交;之后改成提交后才发,但发送失败、提交成功而回包丢失时崩溃,
// 都会让已提交的改动没有流水。所以「有流水」最多说明已提交,「没流水」什么也证明不了,两者都不能单独定结局。
//
// 结论:
//   - applied:账本记这个 seq 已应用(全额),而且收据是这一条请求(CommittedOutcomeOf,与重投循环终结同一口径:
//     收据唯一、correlation 与请求摘要逐字节一致,回收要有明细,托管扣出要有可用原件);
//   - aborted:账本记已拒绝(含中止占位)且收据是这一条请求;或者写权行已释放、账本没见过这个 seq,而且这一行「安静」
//     (RowQuiet:此刻没人持租约,距最后一次可能的发送超过安全间隔)。读证据之后到终结之间,重投循环可能刚领走这一行、
//     发出一次投递,所以终结的 CAS 还要带上读证据时的租约、发送时刻与 attempts(QuietWitness),对不上就落空。
//     RowQuiet 拿本机的钟比副本写下的时刻,钟差超过安全间隔时正在投的行也像安静,所以两个 assetopfix 在按「从未应用」
//     终结之前,还要在本机单调时钟上等满观察窗口(ManualObserveWindow)再读一次行与账本,都没变才终结;
//   - 其余一律不终结:账本判不了(seq 滑出窗口、纪元比账本旧、跳号过远、账本损坏、没有写权行)、收据不是这一条请求、
//     部分发放(账本的部分标记,或行上粘性的 last_reason = ReasonPartialApplied)、还有 scene 持有这名玩家
//     (它内存里可能已经应用、只是还没提交)、行不安静。
//
// 账本判不了时 aborted 不许,因为没流水不等于没应用;applied 也不许,因为还没有能从提交后的流水还原整笔发放的证明。
// 这类行留在 PENDING,由卡死行告警进入逐件核对。哪天有一类行必须能判,单独给它开口子,带它自己的证据。

// ManualVerdict 是已提交账本对一行待定资产指令的结论。人工终结只认它。
type ManualVerdict uint8

const (
	// ManualVerdictNone 证据不足,不许人工终结。原因见 JudgeManual 返回的 detail。
	ManualVerdictNone ManualVerdict = iota + 1
	// ManualVerdictApplied 已提交:已应用(全额),收据是这一条请求。
	ManualVerdictApplied
	// ManualVerdictPartial 已提交:只发放了一部分。applied、aborted 都不对,要逐件核对,工具不终结。
	ManualVerdictPartial
	// ManualVerdictRejected 已提交:已拒绝(含中止占位),收据是这一条请求。scene 没应用,以后也不会。
	ManualVerdictRejected
	// ManualVerdictNeverApplied 写权行已释放、账本没见过这个 seq,而且这一行安静:读证据时没有任何实体能应用它,
	// 也没有在途投递(按本机的钟判,钟差由 ManualObserveWindow 兜住)。终结时还要带 QuietWitness,保证读完之后没人再领过它。
	ManualVerdictNeverApplied

	manualVerdictCount
)

var manualVerdictNames = [...]string{"invalid", "none", "applied", "partial", "rejected", "never_applied"}

var _ = [1]struct{}{}[len(manualVerdictNames)-int(manualVerdictCount)]

// String 进工具输出与审计行。
func (v ManualVerdict) String() string {
	if v >= manualVerdictCount {
		return manualVerdictNames[0]
	}
	return manualVerdictNames[v]
}

// Allows 是结论规则:applied 只认 ManualVerdictApplied;aborted 认 ManualVerdictRejected 与 ManualVerdictNeverApplied。
// 其余结局(拒绝是 scene 的判定、放弃只能由自动放弃产生)和其余结论一律不许。
func (v ManualVerdict) Allows(final Status) bool {
	switch final {
	case StatusApplied:
		return v == ManualVerdictApplied
	case StatusAborted:
		return v == ManualVerdictRejected || v == ManualVerdictNeverApplied
	default:
		return false
	}
}

// AllowedAs 是这份结论下唯一能选的 -as;没有可选的回 "none"。
func (v ManualVerdict) AllowedAs() string {
	switch v {
	case ManualVerdictApplied:
		return "applied"
	case ManualVerdictRejected, ManualVerdictNeverApplied:
		return "aborted"
	default:
		return "none"
	}
}

// ManualQuietFloor 是判「从未应用」时,距最后一次可能的发送至少要隔多久(实际取它与本服务租约的较大者,见 ManualQuietGap)。
// 一次投递请求的生命期以秒计,间隔远大于它,读证据时就没有在途请求;口径同自动放弃的 AbandonSafetyGap(abandon.go 条件 4)。
const ManualQuietFloor = 30 * time.Minute

// ManualQuietGap 是本服务判「安静」用的间隔:ManualQuietFloor 与重投租约取较大者。
func ManualQuietGap(lease time.Duration) time.Duration {
	return max(ManualQuietFloor, lease)
}

// ManualRow 是判定要用到的行上事实,各服务各自从本表的行转出来。
// Op 是重建出的请求,核收据身份要用它的 PlayerID、Stream、StreamEpoch、Seq、CorrelationID、TxType、Bundle;
// 另外四列单独给,不依赖 Op 里有没有填。
type ManualRow struct {
	Op Op
	// LastReason 是行上粘性的 last_reason。ReasonPartialApplied 表示 scene 答过部分发放,即使账本的部分标记已经不在。
	LastReason uint32
	// LeaseUntilMs、LastSendMs 是读行时的租约到期与最后一次可能发送的时刻(毫秒),判安静与终结 CAS 都用它们。
	LeaseUntilMs uint64
	LastSendMs   uint64
	// Attempts 是读行时的 attempts 列。只进终结 CAS 的见证,不参与判定。
	Attempts uint32
}

// QuietWitness 是读证据那一刻行上的租约、发送时刻与 attempts。人工终结的 CAS 要求三列都没变:任何领取都会改 lease_until_ms,
// 发过请求的重排会推进 last_send_ms,任何一次重排都给 attempts 加一。读完之后只要有人领过、排过,终结就落空,
// 不会和一次刚开始的投递抢结局。
//
// 为什么要比 attempts(2026-11-26):last_send_ms 按写入它的副本自己的钟推进(GREATEST(last_send_ms, 它的 now)),
// 钟慢的副本发了请求也推不动;结局不明的重排又把 lease_until_ms 清回 0,行上这两列与读证据时一模一样,只比它们就漏掉了
// 这一次发送。attempts 每次重排必加一,与时钟无关。
// 代价:只领没发的重排(玩家不在线,本地 NOT_HERE)也会加 attempts,条件写之前碰上一次就落空、要重跑。条件写紧跟着最后一次读,
// 这个窗口以毫秒计,碰上的概率可以忽略;跨观察窗口(分钟级)的两次读不比 attempts,见 LeaseAndSendUnchanged。
// 设计记录见 docs/design/data-consistency-guarantee-map-20261020.md §6.2 第 2 条。
type QuietWitness struct {
	LeaseUntilMs uint64
	LastSendMs   uint64
	Attempts     uint32
}

// Witness 给终结 CAS 用的见证值。
func (r ManualRow) Witness() QuietWitness {
	return QuietWitness{LeaseUntilMs: r.LeaseUntilMs, LastSendMs: r.LastSendMs, Attempts: r.Attempts}
}

// Matches 报告行是否还是取见证时的样子:租约、发送时刻、attempts 都没变。终结的条件写用它的口径:data_service 回收 settle
// 在行锁下比、trade 与 guild 的条件写按同样三列写 SQL。条件写紧跟着最后一次读。
func (w QuietWitness) Matches(r ManualRow) bool {
	return w == r.Witness()
}

// LeaseAndSendUnchanged 是两个 assetopfix 跨观察窗口的复核口径:两次读之间租约与发送时刻都没变,attempts 可以变。
//
// 为什么跨窗口不比 attempts:离线玩家的行会被定期领走、本地 NOT_HERE 重排(不发请求),每次 attempts 加一;停放档关掉时
// 大约每分钟一次,比 attempts 的话一个窗口几乎必然碰上,「从未应用」的行用工具永远终结不了。不比它也不放过发送:
//   - 第二次读时还在途的投递,它的领取必改 lease_until_ms(新租约与第一次读到的值不同),这里就拒绝;
//   - 窗口里已经结束的投递,哪怕是钟慢的副本发的(last_send_ms 推不动)、结局不明又重排(租约清回 0):scene 若已应用,
//     第二次读账本时要么还持有玩家(Released=false),要么已存盘(账本见过这个 seq),重判都不再是「从未应用」,照样拒绝;
//   - 第二次读之后的领取与重排,由条件写带第二次读到的整份见证(Matches,含 attempts)挡住。
//
// 所以两个工具复核通过之后,条件写用第二次读到的见证,不用第一次的。
func (w QuietWitness) LeaseAndSendUnchanged(r ManualRow) bool {
	return w.LeaseUntilMs == r.LeaseUntilMs && w.LastSendMs == r.LastSendMs
}

// ManualObserveFloor 是观察窗口的下限,见 ManualObserveWindow。
const ManualObserveFloor = time.Minute

// ManualObserveWindow 是两个 assetopfix 按「从未应用」终结之前,要在本机单调时钟上等满的时长:
// ManualObserveFloor 与两倍重投租约取较大者。
//
// 为什么要等:RowQuiet 用工具自己的墙钟比副本写下的 lease_until_ms / last_send_ms。工具可能在集群外的机器上跑,
// 没核过钟(clockcheck 只在集群里有参照);工具的钟快了、或者某个副本的钟慢了超过安全间隔,一次正在进行的投递就像「安静」。
//
// 为什么等这么久就够:读证据那一刻在途的投递,都落在一个已经领到的租约里,而处理预算短于租约,所以至迟在读证据之后一个租约内收尾。
// 成功 → 行被终结(status 变了);结局不明 → 重排(attempts 加一、租约清零);进程崩溃 → 行不变,但 scene 若已应用,
// 要么还持有玩家(账本 Released=false,判 none),要么已存盘(账本见过这个 seq,判 applied / rejected,-as aborted 不许)。
// 所以等满之后行仍是 PENDING、见证对得上(Matches)、账本重判仍是 never_applied,就说明读证据之后没有在途投递。
// 这个结论只用到本机的单调时钟,与任何一台机器的墙钟都无关。两倍租约和一分钟下限,是给收尾那次写库的锁等待与事务重试留的余量。
//
// 边界:前提与 ManualQuietFloor 相同,一次投递请求的生命期以秒计,不会在网络或 scene 的队列里滞留到窗口之外。
// applied / rejected 有账本里的收据作证,不用等。data_service 的回收人工终结不等(理由见 logic.resolveRecallOp)。
func ManualObserveWindow(lease time.Duration) time.Duration {
	return max(ManualObserveFloor, 2*lease)
}

// WaitManualObserveWindow 在本机单调时钟上等 d(time.Timer,不受墙钟调整影响),ctx 先结束就返回它的错误。
// 两个 assetopfix 的观察窗口用它;单测注入不真等的替身。
func WaitManualObserveWindow(ctx context.Context, d time.Duration) error {
	return sleepCtx(ctx, d)
}

// JudgeManual 拿这名玩家已提交的账本判一行待定的资产指令。detail 是给人看的一句话(英文,进工具输出与审计日志)。
// snapshot 必须来自 StoreLedgerReader 的同一次读:「没见过」和「已释放」要是同一时刻的事实。
// 位图分类与自动终结、自动放弃同一套(ClassifySeq);有结局时还要收据是这一条请求(CommittedOutcomeOf)。
func JudgeManual(snapshot StoreLedger, row ManualRow, nowMs uint64, quietGap time.Duration) (ManualVerdict, string) {
	if !snapshot.Found || snapshot.Ledger == nil {
		return ManualVerdictNone, "player_store has no owner row for this player; the row stays PENDING for item-by-item reconciliation"
	}
	op := row.Op
	state := ClassifySeq(FindStream(snapshot.Ledger, op.Stream), op.StreamEpoch, op.Seq)
	switch state {
	case SeqApplied, SeqRejected:
		outcome, partial, ok := CommittedOutcomeOf(snapshot.Ledger, op)
		switch {
		case !ok:
			return ManualVerdictNone, fmt.Sprintf("committed ledger has a %s outcome for this seq, but its receipt is not this request", state)
		case outcome == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED && (partial || row.LastReason == ReasonPartialApplied):
			return ManualVerdictPartial, "committed ledger: applied partially; reconcile item by item, this tool does not finalize it"
		case outcome == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED:
			return ManualVerdictApplied, "committed ledger: applied, receipt matches this request"
		default:
			return ManualVerdictRejected, "committed ledger: rejected (or abort placeholder), receipt matches this request"
		}
	case SeqUnseen, SeqAheadOfWindow:
		if !snapshot.Released {
			return ManualVerdictNone, "committed ledger has not seen this seq, but a scene still holds the player: it may have applied it in memory or may apply it later"
		}
		if quiet, why := RowQuiet(row.LastSendMs, row.LeaseUntilMs, nowMs, quietGap); !quiet {
			return ManualVerdictNone, "owner row released and committed ledger has not seen this seq, but " + why
		}
		return ManualVerdictNeverApplied, "owner row released, committed ledger has not seen this seq, and no delivery was possible within the quiet gap"
	default:
		// SeqBehindWindow / SeqStaleEpoch / SeqJumpTooFar / SeqInvalid:位图给不出这个 seq 的结局。跳号过远按理没应用过,
		// 但账本若被恢复到更早的时间点也会出现,不替人下结论。
		return ManualVerdictNone, fmt.Sprintf("committed ledger cannot tell (%s); the row stays PENDING for item-by-item reconciliation", state)
	}
}

// RowQuiet 判一行是不是安静:此刻没人持租约,距此前任何一次可能的发送超过 gap。依据是 last_send_ms 契约(abandon.go 文件头):
// 每次发送都落在某个租约里,没正常收尾的租约留着 lease_until_ms,所以最后一次可能的发送 = max(last_send_ms, lease_until_ms)。
// last_send_ms = 0 是 Store 没维护,算不出间隔,按不安静处理。
func RowQuiet(lastSendMs, leaseUntilMs, nowMs uint64, gap time.Duration) (bool, string) {
	if lastSendMs == 0 {
		return false, "last_send_ms is 0 (not maintained), so an in-flight delivery cannot be ruled out"
	}
	if leaseUntilMs > nowMs {
		return false, "a lease is held right now, so a delivery may be in flight"
	}
	last := max(lastSendMs, leaseUntilMs)
	gapMs := uint64(gap / time.Millisecond)
	if last >= nowMs || nowMs-last < gapMs {
		elapsedMin := uint64(0)
		if nowMs > last {
			elapsedMin = (nowMs - last) / uint64(time.Minute/time.Millisecond)
		}
		return false, fmt.Sprintf("the last possible delivery was %d minutes ago, less than %v", elapsedMin, gap)
	}
	return true, ""
}

// DescribeManualEvidence 是证据的一行说明,写明来源:已提交账本,不是流水。
func DescribeManualEvidence(v ManualVerdict, detail string) string {
	return fmt.Sprintf("evidence=%s allowed=-as %s (source: committed asset ledger in player_store via data_service.GetPlayerAssetOpLedger; "+
		"transaction_log is only a lead): %s", v, v.AllowedAs(), detail)
}
