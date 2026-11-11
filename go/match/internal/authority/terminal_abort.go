package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"match/internal/metrics"
	pb "proto/match"
)

// 终局 ABORT 在 global_flow_total{stage="terminal"} 下的 outcome 取值(2026-11-05,低基数;battle_id 只进日志)。
// 每计一次就是「这一局全员白打、结果作废、原票释放」,值班查询见 cross-zone-matchmaking.md §13.3。
// 只在 ABORT 事务提交成功之后计;回放既有 ABORT、提交结果未知都不计。
const (
	terminalAbortDeadline = "abort_deadline" // 到期恢复:过了权威截止加终局提交宽限,且原执行实例证明没有冻结待提交的终局(或原进程已不在)
	terminalAbortCancel   = "abort_cancel"   // 帮会取消入口:同一截止判据与同一份证据要求,由 CancelActivity 提交
	terminalAbortNoRoom   = "abort_no_room"  // 提前裁决:原执行实例证明这份 Create 没建、以后也不会再建
	terminalAbortManual   = "abort_manual"   // 人工处置:formationfix 对永久 UNKNOWN 的成局写的 ABORT(manual_abort.go)
	// 截止 ABORT(到期恢复或帮会取消)越过硬上限、没有证据写下的(2026-11-06):这一局可能已经打完而结果作废,另打 ERROR。
	terminalAbortDeadlineCap = "abort_deadline_cap"
)

// commitAbortTx 写这一局唯一的 ABORT:终局行(没有 Battle 原字节,format_version=0,source 标出由哪条规则裁决)、
// formation→5、原票 2/3→4,三者在同一事务里。调用方必须已持全部原 P 与 formation 行锁,并已在锁内确认
// match_battle_completion 没有这一局的行;提交由调用方做,提交成功后再 observeTerminalAbort。
// 所有 ABORT 入口(截止裁决、帮会取消、提前裁决、人工处置 ManualAbortFormation)都只走这里,不另写释放 SQL(2026-11-05)。
func commitAbortTx(ctx context.Context, tx *sql.Tx, f *pb.MatchFormationRecord, stamp uint64, source pb.MatchBattleCompletionSourceKind) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO match_battle_completion(battle_id,raw_payload,payload_digest,format_version,archived_ms,source_topic,source_partition,source_offset,decision,source_kind,placement_operation_id,target_instance_id,outcome_protocol_version,result_published,next_retry_ms) VALUES(?,?,?,0,?,'',0,0,?,?,?,?,1,1,0)", f.BattleId, []byte{}, []byte{}, stamp, uint32(pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT), uint32(source), f.OperationId, f.TargetInstanceId)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_formation SET state=5 WHERE battle_id=?", f.BattleId); err != nil {
		return err
	}
	for _, m := range f.Members {
		if _, err = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=? AND ticket=? AND formation_id=? AND state IN(2,3)", m.PlayerId, m.Ticket, f.BattleId); err != nil {
			return err
		}
	}
	return nil
}

// observeTerminalAbort 在 ABORT 事务提交成功之后调用:计一次低基数计数,打一行带 battle_id 的 INFO,
// 值班凭 battle_id 对照 battle 侧日志与 match_battle_completion。
func observeTerminalAbort(battleID uint64, outcome string) {
	metrics.ObserveGlobalFlow("terminal", outcome)
	logx.Infof("global formation aborted: battle_id=%d outcome=%s; the battle result is void for every member and their tickets are released", battleID, outcome)
}

// ---- 截止作废先取证(2026-11-06,cross-zone-matchmaking.md §13.3「2026-11-06 修复」) ----
//
// 截止加宽限只说明「COMPLETE 本该到了」,不说明「不会再来」。match 或它的库不可用的时间跨过截止加宽限时,截止前已经正常
// 打完、battle 正在重放 CommitBattleOutcome 的局,恢复后通常是截止 ABORT 先写,整局作废、奖励丢失。所以截止 ABORT
// (到期恢复与帮会取消,abortTerminalTx)先向原执行实例要证据:证明它没有冻结待提交的终局,才作废;原实例仍持有房间就
// 等它在自己的重放窗口内提交;判不了时最多再等硬上限。时间判据只写在 judgeDeadlineAbort,要不要取证只看 deadlineAbortPending
// (到期恢复再加排期条件,deadlineProbeDue),取证只走 DeadlineAbortProbe(生产实现复用提前作废的那一次
// DestroyBattle + QueryBattleCreation,不另写探测)。
// 截止之后作废本身不破坏一致性(准入早已关闭,房间不会再建,作废对全员对称);证据只决定「什么时候作废才不丢一份已经打完的结果」。

// DeadlineAbortEvidence 是截止作废之前从原执行实例取到的证据。
type DeadlineAbortEvidence uint8

const (
	// DeadlineEvidenceUndetermined:判不了 —— 原实例连不上或超时、回包认不出,注册中心也说不清它还在不在;或没装配取证。
	DeadlineEvidenceUndetermined DeadlineAbortEvidence = iota
	// DeadlineEvidenceBattleStillHolds(battleStillHolds):原实例对这份 operation 回 PRESENT。battle 在截止时刻强制结束房间,
	// 截止之后房间还在,说明终局已冻结、正在重放 CommitBattleOutcome(或原件不合格、留在节点上等 match 裁决)。
	DeadlineEvidenceBattleStillHolds
	// DeadlineEvidenceNoRoom:原实例对这份 operation 立了拒收墓碑并回 ABORTED:没有房间,以后也建不了。房间只在拿到 match
	// 的签名决定之后或 30 小时本地清理之后才离开,所以 ABORTED 不会出现在「房间打完、COMPLETE 还在路上」的局上(NoRoomProof)。
	DeadlineEvidenceNoRoom
	// DeadlineEvidenceInstanceGone:原进程已不在 —— 原地址上应答的是另一个实例(battle 的实例 UUID 每个进程生成一次,
	// 对不符的 UUID 拒 DestroyBattle、查询回空包);注册中心里原 node_id 已登记成另一个实例;或原地址当场就连不上(不是等到
	// 期限才超时)且已同步的注册中心镜像里没有原实例。房间与待提交的终局只在 battle 进程内存里,进程不在就永远不会再提交。
	// 只凭「注册中心查不到」不算(etcd 抖动、租约短暂失效时也查不到,§13.3 2026-10-22 同一口径),要原地址也确认不了才算。
	DeadlineEvidenceInstanceGone
)

func (e DeadlineAbortEvidence) String() string {
	switch e {
	case DeadlineEvidenceUndetermined:
		return "undetermined"
	case DeadlineEvidenceBattleStillHolds:
		return "battle_still_holds"
	case DeadlineEvidenceNoRoom:
		return "no_room"
	case DeadlineEvidenceInstanceGone:
		return "instance_gone"
	}
	return "invalid"
}

// DeadlineAbortProbe 向这份成局的原执行实例取证。只发 RPC、只读注册中心镜像,不碰 SQL,不在任何事务或行锁里调用;
// ctx 带 DeadlineProbeTimeout 的期限;可能被多个 goroutine 同时调用。生产实现在 logic/global_deadline_evidence.go。
type DeadlineAbortProbe func(ctx context.Context, f *pb.MatchFormationRecord) DeadlineAbortEvidence

// DeadlineProbeTimeout 是一次取证(DestroyBattle 加 QueryBattleCreation)的总期限。原实例连不上时要等满它;
// 到期恢复一页的局并行取证(probeDeadlinePage),所以一页最多多花这么久。
const DeadlineProbeTimeout = 2 * time.Second

// deadlineAbortHardCapMs(hardCap):截止 + 终局宽限之后,拿不到「原实例没有冻结终局」的证据时最多再等多久;过了就不再
// 要证据,直接作废,计 abort_deadline_cap 并打 ERROR。取值与 scene 冻结到期之后的续冻一起定(player_battle.h:FIGHTING 冻结
// 到期 = 截止 + 60 秒终局宽限 + 60 秒投递余量,续冻上限 kFreezeHoldCapMs = 15 分钟,合计截止 + 17 分钟):match 可用时,
// 截止 ABORT 最晚在 截止 + 宽限 + 硬上限 + 持有宽限(截止 + 13 分钟)再加一圈恢复延迟之内落定,早于 scene 按上限解冻,
// scene 按 SQL 终局解冻;不等式由 TestDeadlineAbortBoundsFitSceneFreezeHold 钉住。match 自己不可用超过 scene 上限时,
// scene 已按上限解冻,之后到的 COMPLETE 照常应用(消耗按当时持有量夹紧并打 battle_inbox_shortfall_after_freeze_released),
// 之后到的 ABORT 不产生结算。battle 侧补提交的总跨度是 30 小时(result_commit.h:冻结后 6 小时内每 5 秒、之后 24 小时内每 60 秒),
// 远长于这里的等待:match 可用时 battle 在整个等待窗口里都在重放;battle 放弃重放、本地删房之后,查询回无房间,按证据作废。
// 宽限 + 硬上限还要不小于一个工作租约(WorkLeaseMs):租约最晚到 截止 + WorkLeaseMs,「retry_at_ms 越过硬上限」才只能来自
// 持有宽限(见 judgeDeadlineAbort)。
const deadlineAbortHardCapMs uint64 = 10 * 60 * 1000

// deadlineHeldPatienceMs:越过硬上限之后第一次看到原实例仍持有房间(battleStillHolds),再给它多久提交。硬上限从截止起算,
// match 或库不可用超过它时,恢复后第一眼就过了上限;battle 仍按自己的节奏重放(result_commit.h:冻结后 6 小时内每 5 秒、
// 之后 24 小时内每 60 秒,共 30 小时才放弃),给它一段比慢速间隔长的窗口,COMPLETE 就能先落库。窗口从 match 恢复、第一次
// 看到房间那一刻起算,起点记在 retry_at_ms 里,多副本、重启都认得。窗口内 match 可用而 battle 仍没提交,说明它的结果
// 被永久拒收或它连不上 match,再等只是把全员占住。
const deadlineHeldPatienceMs uint64 = 2 * 60 * 1000

// deadlineProbeBackoffMs:等证据期间到期恢复对同一局两次取证的最小间隔。经 retry_at_ms 排期、全集群共用:每局每 30 秒至多被
// 到期恢复取证一次(deadlineProbeDue),与副本数无关。帮会取消不按它(deadlineEvidenceOf)。
const deadlineProbeBackoffMs uint64 = 30 * 1000

// deadlineAbortVerdict 是 judgeDeadlineAbort 的结论。
type deadlineAbortVerdict struct {
	abort       bool   // 可以写截止 ABORT
	capped      bool   // abort 为真时:越过硬上限、没有证据(计 abort_deadline_cap 并打 ERROR)
	nextProbeMs uint64 // abort 为假时下次取证的时刻,到期恢复在同一把锁里写进 retry_at_ms;0 = 不改排期。帮会取消不用它
}

// deadlineAbortCapMs 是「没有证据也作废」的起点:截止 + 终局宽限 + 硬上限,上溢饱和。
func deadlineAbortCapMs(deadlineMs uint64) uint64 {
	const after = terminalOutcomeGraceMs + deadlineAbortHardCapMs
	if deadlineMs > math.MaxUint64-after {
		return math.MaxUint64
	}
	return deadlineMs + after
}

// judgeDeadlineAbort 是截止 ABORT 的全部时间判据,纯函数;到期恢复与帮会取消都在锁内只调它(经 abortTerminalTx)。
// deadlineMs:原件截止;stampMs:锁内读到的 SQL 时刻;retryAtMs:锁内成局行的 retry_at_ms;evidence:锁外这一次取到的证据。
//   - 没过截止 + 宽限:不作废(与 terminalAbortDue 同一分界);
//   - 有证据(NoRoom / InstanceGone):作废;
//   - 没到硬上限(battleStillHolds 或判不了):等,下次取证排在 min(stamp + 取证间隔, 硬上限);
//   - 过了硬上限、持有宽限还没过(retry_at_ms 在硬上限之后且还没到):等,不论这一次证据如何(宽限给的是「看到过房间」的局);
//   - 过了硬上限、原实例仍持有房间、持有宽限还没开始(retry_at_ms 不超过硬上限):排到 stamp + 持有宽限再看;
//   - 其余(过了硬上限且判不了,或持有宽限已过):作废,计 abort_deadline_cap。
//
// 只有「越过硬上限后看到 battleStillHolds」会把 retry_at_ms 排过硬上限:没到上限的排期都截在上限上,工作租约只在截止前发、
// 最晚到截止 + WorkLeaseMs(不超过宽限 + 硬上限),COMPLETE 改成 terminalOutcomeNoRetryMs 时已有终局、不再走到这里。所以
// 「retry_at_ms 超过硬上限」就是「持有宽限已经开始」的持久记号。帮会取消不改排期,只读这个记号。
func judgeDeadlineAbort(deadlineMs, stampMs, retryAtMs uint64, evidence DeadlineAbortEvidence) deadlineAbortVerdict {
	if !terminalAbortDue(deadlineMs, stampMs) {
		return deadlineAbortVerdict{}
	}
	if evidence == DeadlineEvidenceNoRoom || evidence == DeadlineEvidenceInstanceGone {
		return deadlineAbortVerdict{abort: true}
	}
	capAt := deadlineAbortCapMs(deadlineMs)
	switch {
	case stampMs < capAt:
		return deadlineAbortVerdict{nextProbeMs: min(stampMs+deadlineProbeBackoffMs, capAt)}
	case retryAtMs > capAt && stampMs < retryAtMs:
		return deadlineAbortVerdict{}
	case evidence == DeadlineEvidenceBattleStillHolds && retryAtMs <= capAt:
		return deadlineAbortVerdict{nextProbeMs: stampMs + deadlineHeldPatienceMs}
	}
	return deadlineAbortVerdict{abort: true, capped: true}
}

// BindDeadlineAbortProbe 接入截止作废的取证。启动时写一次,在 Store 交给任何后台循环与 RPC 之前;之后只读。
// 没绑(单测、装配遗漏)= 一律 DeadlineEvidenceUndetermined:截止 ABORT 只在越过硬上限之后才写(fail-closed)。
func (s *Store) BindDeadlineAbortProbe(probe DeadlineAbortProbe) { s.deadlineProbe = probe }

// probeDeadline 向原执行实例取一次证,期限 DeadlineProbeTimeout。
func (s *Store) probeDeadline(ctx context.Context, f *pb.MatchFormationRecord) DeadlineAbortEvidence {
	if s == nil || s.deadlineProbe == nil || f == nil {
		return DeadlineEvidenceUndetermined
	}
	probeCtx, cancel := context.WithTimeout(ctx, DeadlineProbeTimeout)
	defer cancel()
	return s.deadlineProbe(probeCtx, f)
}

// deadlineAbortPending:截止 ABORT 只差证据 —— 自动路径会裁决这份成局(autoTerminalOriginal),且已过截止 + 宽限。
// 帮会取消据它决定要不要取证(不看排期,见 deadlineEvidenceOf),到期恢复再加排期条件(deadlineProbeDue)。
// 只看成局行,不查终局行。纯函数。
func deadlineAbortPending(f *pb.MatchFormationRecord, stampMs uint64) bool {
	r, ok := autoTerminalOriginal(f)
	return ok && terminalAbortDue(r.DeadlineMs, stampMs)
}

// deadlineProbeDue:到期恢复此刻要不要为这份成局向原执行实例取证 —— deadlineAbortPending,且排期已到(retry_at_ms 不晚于
// stampMs:原工作租约已到期,上次取证排下的间隔或持有宽限已过)。与候选 SQL、锁内的租约复核同一条排期,排期没到时
// 锁内不会裁决,取了证也用不上。纯函数。
func deadlineProbeDue(f *pb.MatchFormationRecord, stampMs uint64) bool {
	return f != nil && f.RetryAtMs <= stampMs && deadlineAbortPending(f, stampMs)
}

// probeDeadlinePage 为到期恢复的一页成局并行取证(不持锁、不在事务里),不需要取证的位置回 Undetermined。原实例连不上时
// 一次要等满 DeadlineProbeTimeout,逐局串行会让 battle 整批失联时一页拖上十几秒、作废吞吐跌到每副本每两秒一局。
// 并发数不超过页大小(ExpiredOutcomePageSize)。取证函数 panic 时等全部结束后在调用方 goroutine 重新 panic,不吞掉。
func (s *Store) probeDeadlinePage(ctx context.Context, stampMs uint64, formations []*pb.MatchFormationRecord) []DeadlineAbortEvidence {
	out := make([]DeadlineAbortEvidence, len(formations))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		panicked any
	)
	for i, f := range formations {
		if !deadlineProbeDue(f, stampMs) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					mu.Lock()
					if panicked == nil {
						panicked = recovered
					}
					mu.Unlock()
				}
			}()
			out[i] = s.probeDeadline(ctx, f)
		}()
	}
	wg.Wait()
	if panicked != nil {
		panic(panicked)
	}
	return out
}

// deadlineEvidence 在事务与行锁之外为一份成局取一次截止作废证据,给到期恢复的单局入口(RecoverExpiredOutcome)用;
// 到期恢复的一页走 probeDeadlinePage,两者同一个判据 deadlineProbeDue。不该取证时回 Undetermined,锁内的 judgeDeadlineAbort
// 不会据它在硬上限之前作废。没装配取证时不读 SQL,直接回 Undetermined。只读 SQL 时刻与发 RPC。
func (s *Store) deadlineEvidence(ctx context.Context, f *pb.MatchFormationRecord) (DeadlineAbortEvidence, error) {
	if s == nil || s.deadlineProbe == nil || !formationInFlight(f) {
		return DeadlineEvidenceUndetermined, nil
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return DeadlineEvidenceUndetermined, err
	}
	return s.probeDeadlinePage(ctx, stamp, []*pb.MatchFormationRecord{f})[0], nil
}

// deadlineEvidenceOf 给帮会取消在事务之前取证(事务里不调 RPC):读这一局的成局行,已有终局行就不问 battle;截止 ABORT 只差
// 证据(deadlineAbortPending)才问原执行实例,否则回 Undetermined。与到期恢复不同,帮会取消不看、也不改排期:原租约还在时照旧
// 可以凭证据作废(与 2026-11-06 之前帮会取消不看租约同口径;截止之后持租者能做的只剩同样经锁与主键串行的提前作废),
// 代价是等证据期间每次重放取消都问一次 battle(帮会恢复器每 10 秒一轮)。持有宽限由锁内的 judgeDeadlineAbort 读 retry_at_ms 认出。
// 读失败原样返回,调用方按本次取消失败处理(帮会侧会重放)。operation 与目标实例在成局行上写定之后不再改,
// 锁内读到的同一 battle_id 仍是这份证据所指的原实例。
func (s *Store) deadlineEvidenceOf(ctx context.Context, battleID uint64) (DeadlineAbortEvidence, error) {
	if s == nil || s.deadlineProbe == nil || battleID == 0 {
		return DeadlineEvidenceUndetermined, nil
	}
	f, err := s.Formation(ctx, battleID)
	if err != nil {
		return DeadlineEvidenceUndetermined, err
	}
	if row, err := readTerminal(ctx, s.DB, battleID); err != nil || row != nil {
		return DeadlineEvidenceUndetermined, err
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return DeadlineEvidenceUndetermined, err
	}
	if !deadlineAbortPending(f, stamp) {
		return DeadlineEvidenceUndetermined, nil
	}
	return s.probeDeadline(ctx, f), nil
}

// observeDeadlineWait 在到期恢复因证据不足而改排期之后调用(提交成功之后):计 global_flow_total{stage="terminal",
// outcome="abort_wait_<证据>"},值班据此看出有多少局在等 battle 提交(battle_still_holds)或在等证据(undetermined)。
// 不带 battle_id,不打日志:每局每个取证间隔至多一次。
func observeDeadlineWait(evidence DeadlineAbortEvidence) {
	metrics.ObserveGlobalFlow("terminal", "abort_wait_"+evidence.String())
}

// observeDeadlineAbort 在截止 ABORT 提交成功之后调用:有证据的计 evidenced(abort_deadline / abort_cancel);越过硬上限、
// 没有证据的计 abort_deadline_cap 并打 ERROR —— 这一局可能已经打完、结果被作废,值班凭 battle_id 去 battle 侧查。
func observeDeadlineAbort(battleID uint64, verdict deadlineAbortVerdict, evidence DeadlineAbortEvidence, evidenced string) {
	if !verdict.capped {
		observeTerminalAbort(battleID, evidenced)
		return
	}
	observeTerminalAbort(battleID, terminalAbortDeadlineCap)
	logx.Errorf("global formation aborted at the deadline hard cap without proof that its battle holds no frozen outcome: battle_id=%d evidence=%s; "+
		"a result the battle could not commit in time is void, check the battle node for this battle_id", battleID, evidence)
}

// createAdmissionMarginMs 是原件准入期限(CreateBattleRequest.admit_before_ms)之后再留的时钟余量。
// battle 用本机墙钟判准入:HandleCreateBattle 在逻辑线程里、插房之前两次复核 now < admit_before_ms,排队、等分配许可、
// 网络迟到的 Create 都按处理那一刻的本机时钟判。admit_before_ms 却是成局行的 TiDB 时刻推出来的,两台主机的时钟
// 不完全一致。部署要求偏差小于 1 秒(cross-zone-matchmaking.md §13.3),这里取 30 秒,覆盖校时抖动、虚机迁移这类异常;更大的时钟回拨还有
// DestroyBattle 立下的拒收墓碑兜着(battle 侧至少保留 360 秒)。
const createAdmissionMarginMs uint64 = 30 * 1000

// CreateAdmissionClosed 判 SQL 时刻 stampMs 是否已过原件准入期限加时钟余量:过了,原执行实例按本机时钟也一定拒收
// 这份 Create。admit_before_ms 为 0 的原件 battle 一律拒收,但它不是 gather 会产出的形状,这里按「判不了」处理,
// 交给截止裁决。先比较再相减,不会 uint64 溢出。
func CreateAdmissionClosed(admitBeforeMs, stampMs uint64) bool {
	return admitBeforeMs != 0 && stampMs >= admitBeforeMs && stampMs-admitBeforeMs >= createAdmissionMarginMs
}

// NoRoomProof 是原执行实例给出的「这份 Create 现在没建、以后也不会再建」的证据(2026-11-05)。
// 只有按下面的顺序取得才成立,调用方(logic.abortProvablyNoRoom)负责照做:
//  1. 先读 SQL 时刻 ObservedAfterMs,且 CreateAdmissionClosed(原件 AdmitBeforeMs, ObservedAfterMs) 为真。
//     这样后两步一定发生在原实例的准入关闭之后,此后它不会再接这份 Create;
//  2. 向原 TargetInstanceId 发 DestroyBattle(原 operation)。battle 在逻辑线程里对尚无房间的原操作立拒收墓碑,
//     有房间则忽略;
//  3. 再 QueryBattleCreation,得到 ABORTED 且回显的实例就是原 TargetInstanceId。
//
// 进程重启后实例 UUID 会变,battle 按 target_instance_id 精确比对,旧 Create 一律拒收,旧 UUID 的查询回空包(UNKNOWN),
// 拿不到这份证据。房间只在拿到 match 的签名决定之后(这时终局行已在)或 30 小时本地清理之后才会离开,
// 所以 ABORTED 也不会出现在「房间打完、COMPLETE 还在路上」的局上。
type NoRoomProof struct {
	OperationId      string
	TargetInstanceId string
	ObservedAfterMs  uint64 // 发 DestroyBattle 之前读到的 SQL 时刻
}

// AbortWithoutRoom 在 NoRoomProof 成立时提前判这一局 ABORT,不等权威截止(2026-11-05)。
// 只接已持久 v1 Create、还没建成(state 2/3)的成局,只由当前工作租约的持有者提交(checkWork),与截止裁决、帮会取消、
// COMPLETE 争同一把原 P→formation 锁和同一个终局主键,锁内先复读终局,多副本、多入口只落一份决定。
// 返回 UNKNOWN 表示证据不足(准入还没关、原件不可裁决、已建成),调用方照旧等截止;已有终局时回它的决定。
func (s *Store) AbortWithoutRoom(ctx context.Context, work *pb.MatchFormationRecord, proof NoRoomProof) (pb.BattleOutcomeDecision, error) {
	if s == nil || s.DB == nil || work == nil || proof.OperationId == "" || proof.OperationId != work.OperationId || proof.TargetInstanceId == "" || proof.TargetInstanceId != work.TargetInstanceId {
		return 0, errors.New("提前ABORT缺原成局或执行端证据与原操作不符")
	}
	tx, f, _, err := s.terminalFormation(ctx, work.BattleId)
	if err != nil {
		// 另一入口可能刚提交了终局并在同一事务里关闭原票:有终局就照它回答,不当成失败。
		if decision, exists, readErr := s.committedTerminal(ctx, work); readErr != nil || exists {
			return decision, readErr
		}
		return 0, err
	}
	defer tx.Rollback()
	old, err := readTerminal(ctx, tx, f.BattleId)
	if err != nil {
		return 0, err
	}
	if old != nil {
		return replayFormationTerminal(old, f)
	}
	r, err := outcomeAdjudicable(f)
	if err != nil || (f.State != 2 && f.State != 3) {
		// 不归自动路径的原件(outcomeAdjudicable 不过)留给巡检报数与人工处置;state 4 是 battle 确认过建房,不属于「不会建房」。
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	if err = checkWork(ctx, tx, f, work); err != nil {
		return 0, err
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return 0, err
	}
	// 证据时刻与锁内时刻都要过准入期限加余量:前者保证 DestroyBattle 与查询发生在准入关闭之后,后者兜住调用方传错时刻。
	if !CreateAdmissionClosed(r.AdmitBeforeMs, proof.ObservedAfterMs) || !CreateAdmissionClosed(r.AdmitBeforeMs, stamp) {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	if err = commitAbortTx(ctx, tx, f, stamp, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_NO_ROOM_ABORT); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("提前ABORT提交未知，必须复用原件重查: %w", err)
	}
	observeTerminalAbort(f.BattleId, terminalAbortNoRoom)
	return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, nil
}

// committedTerminal 在锁外复读同一原操作与实例已提交的终局(另一入口刚提交、原票已关闭时用)。
// exists=false 表示还没有终局行;身份不符按原件冲突返回错误。
func (s *Store) committedTerminal(ctx context.Context, f *pb.MatchFormationRecord) (pb.BattleOutcomeDecision, bool, error) {
	row, err := readTerminal(ctx, s.DB, f.GetBattleId())
	if err != nil || row == nil {
		return 0, false, err
	}
	decision, err := replayFormationTerminal(row, f)
	return decision, true, err
}

// replayFormationTerminal 按成局自己的原操作与实例回放已有终局行:身份一致回它的决定,不一致按原件冲突。
// 没有 Battle 原字节可比,COMPLETE 只核身份(拿行里的原字节与自身比)。
func replayFormationTerminal(row *pb.MatchBattleCompletionRecord, f *pb.MatchFormationRecord) (pb.BattleOutcomeDecision, error) {
	return replayTerminal(row, &pb.CommitBattleOutcomeRequest{BattleId: f.GetBattleId(), PlacementOperationId: f.GetOperationId(), TargetInstanceId: f.GetTargetInstanceId(), RawResultPayload: row.GetRawPayload()})
}
