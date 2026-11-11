package authority

import (
	"context"
	"database/sql"
	"errors"

	pb "proto/match"
)

// BattleTerminalState 是 scene 冻结到期查询(MatchInternal.GetBattleTerminalDecision,2026-11-06;
// docs/design/turn-battle-gap-closure.md §11)读的全部 SQL 事实。只读,不加锁、不写任何行。
//
//   - decision:match_battle_completion 的终局决定(唯一权威,readTerminal 同一条 SQL);没有行,或只有 Kafka 归档、
//     尚无决定时为 UNKNOWN。
//   - terminalExpected:decision 为 UNKNOWN 时,这一局终局会不会落定 —— 没有终局行、成局行在、且自动路径会裁决它
//     (TerminalOutcomeExpected)。会 = UNKNOWN 只说明还没落定:match 不可用,或截止 ABORT 还在等「原实例没有冻结终局」的
//     证据(最多到截止 + 宽限 + 硬上限 + 持有宽限,terminal_abort.go),COMPLETE 仍可能先落库,所以不是终局证据。
//     不会 = 没有成局行;成局不在途;原件不可裁决(终局协议 0、旧局、坏形状、operation 为空);或已有终局行却没有决定
//     (只有归档):COMPLETE 与截止 ABORT 都插不进同一主键、也都要求原件可裁决,这一局永远不会有 COMPLETE,也就不会有任何结算。
//     已有决定时恒为 true。
//
// 先读终局再读成局。两次读之间若刚好落定终局,回的是保守的「还没落定」,scene 下一拍再问即可;反过来不可能:
// 自动路径不裁决的局永远写不进 COMPLETE。读失败原样返回,调用方按 retry 带内回。
func (s *Store) BattleTerminalState(ctx context.Context, battle uint64) (decision pb.BattleOutcomeDecision, terminalExpected bool, err error) {
	terminal, err := readTerminal(ctx, s.DB, battle)
	if err != nil {
		return 0, false, err
	}
	if terminal != nil && terminal.Decision != pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN {
		return terminal.Decision, true, nil
	}
	if terminal != nil {
		// 只有归档、没有决定的终局行:CommitBattleOutcome 与 abortTerminalTx 读到它都只回放 UNKNOWN,人工处置也拒绝(has_terminal_row)。
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, false, nil
	}
	f, err := s.Formation(ctx, battle)
	if errors.Is(err, sql.ErrNoRows) {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, TerminalOutcomeExpected(f), nil
}

// TerminalOutcomeExpected 判这份成局(尚无终局行时)的终局会不会落定:与截止 ABORT、到期恢复调的是同一个函数
// autoTerminalOriginal(2026-11-06 起两边共用,原先只核 supportedOutcomeOriginal,漏了「operation 为空、到期恢复不选」与
// 「不在途」两类:回包说会有终局、恢复循环却永远不写)。成局在途(state 2/3/4)且原件可裁决(终局协议 1、预检能力证据完整、
// 采用回执合法、原件与成局身份一致、在到期恢复的候选形状里)才为真。COMPLETE 的判据(completableOriginal)比它更严,
// 所以它不过时 COMPLETE 也一定过不了。纯函数。
func TerminalOutcomeExpected(f *pb.MatchFormationRecord) bool {
	_, ok := autoTerminalOriginal(f)
	return ok
}
