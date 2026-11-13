package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	pb "proto/match"
)

// ---- 原执行进程确认已不在时提前作废(2026-11-11,docs/design/battle-instance-loss-early-abort.md §4) ----
//
// battle 进程崩溃时,它内存里的房间与冻结待提交的终局一起没了,这一局永远不会有 COMPLETE。以前要等到权威截止加宽限之后
// 才由截止裁决作废,全员一直被冻着、排不了队(长模板能到一个多小时)。现在锁外由 logic 层取证并复核
// (logic/global_instance_loss.go:两次相隔不少于 InstanceGoneSettleMs 的观察都说原进程不在,期间它没有亲口应答),
// 这里只做锁内复核与提交,截止之前之后都可以写。
//
// 不要求工作租约(与 AbortWithoutRoom 不同):建房目标在提交点写定进 formation,battle 按实例号精确比对、只接发给自己的 Create,
// 原进程已不在,任何副本的建房都不会再成功,也没有换节点的路径。与 COMPLETE、截止裁决、帮会取消、原件不合格作废、人工处置争
// 同一把原 P→formation 锁和同一个终局主键,锁内先复读终局,多副本、多入口只落一份决定。
// 作废结算由 commitAbortTx 在同一事务里写给每名成员(void_settlement.go)。

// terminalAbortInstanceGone 是这类 ABORT 在 global_flow_total{stage="terminal"} 下的 outcome 取值(低基数,battle_id 只进日志)。
const terminalAbortInstanceGone = "abort_instance_gone"

// InstanceGoneProof 是锁外复核过的「原执行进程已不在」证据所指的原操作与原实例。二者在提交点写定,锁内逐字比对。
type InstanceGoneProof struct {
	OperationId      string
	TargetInstanceId string
}

// AbortInstanceGone 在原执行进程确认已不在时写这一局唯一的 ABORT。
// 已有终局回它的决定;成局不归自动路径裁决(autoTerminalOriginal 为假)或身份与证据不符回 UNKNOWN,不写库。
// 提交结果未知时返回错误,调用方下一次复核会读到真实终局。
func (s *Store) AbortInstanceGone(ctx context.Context, work *pb.MatchFormationRecord, proof InstanceGoneProof) (pb.BattleOutcomeDecision, error) {
	if s == nil || s.DB == nil || work == nil || proof.OperationId == "" || proof.OperationId != work.OperationId ||
		proof.TargetInstanceId == "" || proof.TargetInstanceId != work.TargetInstanceId {
		return 0, errors.New("实例失效作废缺原成局或证据与原操作不符")
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
	if f.OperationId != proof.OperationId || f.TargetInstanceId != proof.TargetInstanceId {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	if _, ok := autoTerminalOriginal(f); !ok {
		// 不归自动路径的原件留给巡检报数与人工处置(manual_abort.go),与截止裁决同一判据。
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err = commitAbortTx(ctx, tx, f, stamp, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_INSTANCE_GONE_ABORT); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("实例失效作废提交结果未知,下一次复核重读终局: %w", err)
	}
	observeTerminalAbort(f.BattleId, terminalAbortInstanceGone)
	return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, nil
}

// InstanceLossCandidate 读一局成局,判它是否还归提前作废管:在途、原件可裁决(autoTerminalOriginal)、还没有终局行。
// pending 为假时 f 可能为 nil(成局不存在)。过了权威截止的局同样算:确认原进程已不在之后,截止前后都按同一条规则作废。
// 只读 SQL,不在事务里;结论只用来决定要不要继续取证,提交点在 AbortInstanceGone 的锁内复核。
func (s *Store) InstanceLossCandidate(ctx context.Context, battleID uint64) (f *pb.MatchFormationRecord, pending bool, err error) {
	if s == nil || s.DB == nil || battleID == 0 {
		return nil, false, nil
	}
	f, err = s.Formation(ctx, battleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if _, ok := autoTerminalOriginal(f); !ok {
		return f, false, nil
	}
	row, err := readTerminal(ctx, s.DB, battleID)
	if err != nil {
		return f, false, err
	}
	return f, row == nil, nil
}
