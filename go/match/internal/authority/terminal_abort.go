package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"
	"match/internal/metrics"
	pb "proto/match"
)

// 终局 ABORT 在 global_flow_total{stage="terminal"} 下的 outcome 取值(2026-11-05,低基数;battle_id 只进日志)。
// 每计一次就是「这一局全员白打、结果作废、原票释放」,值班查询见 cross-zone-matchmaking.md §13.3。
// 只在 ABORT 事务提交成功之后计;回放既有 ABORT、提交结果未知都不计。
const (
	terminalAbortDeadline = "abort_deadline" // 到期恢复:过了权威截止加终局提交宽限,仍没有 COMPLETE
	terminalAbortCancel   = "abort_cancel"   // 帮会取消入口:同一截止判据,由 CancelActivity 提交
	terminalAbortNoRoom   = "abort_no_room"  // 提前裁决:原执行实例证明这份 Create 没建、以后也不会再建
	terminalAbortManual   = "abort_manual"   // 人工处置:formationfix 对永久 UNKNOWN 的成局写的 ABORT(manual_abort.go)
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
	r, err := supportedOutcomeOriginal(f)
	if err != nil || (f.State != 2 && f.State != 3) {
		// 原件不可裁决留给截止路径报数;state 4 是 battle 确认过建房,不属于「不会建房」。
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
