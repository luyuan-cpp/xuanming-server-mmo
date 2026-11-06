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
//   - terminalExpected:decision 为 UNKNOWN 时,这一局终局会不会落定 —— 成局行在、且 Create 原件可裁决
//     (TerminalOutcomeExpected)。会 = UNKNOWN 只说明还没落定:match 不可用,或截止 + 宽限之后的 ABORT 补写滞后,
//     COMPLETE 仍可能先落库,所以不是终局证据。不会 = 没有成局行,或原件不可裁决(终局协议 0、旧局、坏形状):
//     COMPLETE 与截止 ABORT 两条写终局的路径都要求原件可裁决,这一局永远不会有 COMPLETE,也就不会有任何结算。
//     已有决定时恒为 true。
//
// 先读终局再读成局。两次读之间若刚好落定终局,回的是保守的「还没落定」,scene 下一拍再问即可;反过来不可能:
// 不可裁决的原件永远写不进 COMPLETE。读失败原样返回,调用方按 retry 带内回。
func (s *Store) BattleTerminalState(ctx context.Context, battle uint64) (decision pb.BattleOutcomeDecision, terminalExpected bool, err error) {
	terminal, err := readTerminal(ctx, s.DB, battle)
	if err != nil {
		return 0, false, err
	}
	if terminal != nil && terminal.Decision != pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN {
		return terminal.Decision, true, nil
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

// TerminalOutcomeExpected 判这份成局的终局会不会落定:Create 原件可裁决,与截止 ABORT 读的是同一个判据
// (supportedOutcomeOriginal:终局协议 1、预检能力证据完整、采用回执合法、原件与成局身份一致)。
// COMPLETE 的判据(completableOriginal)比它更严,所以它不过时 COMPLETE 也一定过不了。纯函数。
func TerminalOutcomeExpected(f *pb.MatchFormationRecord) bool {
	_, err := supportedOutcomeOriginal(f)
	return err == nil
}
