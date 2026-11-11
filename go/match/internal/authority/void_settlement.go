package authority

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	pb "proto/match"
)

// ---- 作废结算:每个 ABORT 在同一事务里给参战者写个人来源(2026-11-11,docs/design/battle-instance-loss-early-abort.md §5) ----
//
// 以前 ABORT 只写终局行、formation→5、原票→4,不给场景服投任何东西:参战者的冻结要等场景服冻结到期(截止 + 2 分钟)
// 之后自己来问 SQL 终局,这段时间排不了队,客户端也收不到任何结局。现在 ABORT 与 COMPLETE 走同一条个人结算链:
// 每名成员一行 match_battle_settlement_source,载荷是 outcome = BATTLE_OUTCOME_VOID、没有任何效果的 BattleSettlementData,
// 由现有个人发件箱首投 Inbox、等 APPLIED(outcome_outbox.go 只按行状态取件,不看终局种类)。场景服应用时不碰血量与资产,
// 只释放道具预留、记一条作废结果;存档确认后解冻、删锁、推 BattleEndS2C{VOID}。
//
// 约束:
//   - 归属取同一事务里的原票(owner_account / owner_revision),与 COMPLETE 写个人来源同一口径;
//   - 原票缺失只会来自坏数据:这名成员不写来源、打 ERROR,他的冻结仍由场景服到期查询兜底;
//   - 原票照旧在 ABORT 里直接放回(state 4),不等作废 APPLIED:作废没有要扣的东西,排队闸由场景服的战斗锁与冻结负责;
//   - archived_ms / issued_at_ms / next_retry_ms 都取 ABORT 落库的 SQL 时刻,发件箱积压年龄按它算。

// voidSettlementFor 返回一名成员的作废结算正文。纯函数:只有身份、作废结局与展示字段,效果字段全部留空。
// finished_at_ms 记 ABORT 落库的 SQL 时刻,供近期战斗结果展示;模式与配置号同样只供展示,不参与任何判定。
func voidSettlementFor(f *pb.MatchFormationRecord, m *pb.MatchFormationMember, stamp uint64) *battlepb.BattleSettlementData {
	return &battlepb.BattleSettlementData{
		BattleId:        f.BattleId,
		PlayerId:        m.PlayerId,
		Outcome:         battlepb.EBattleOutcome_BATTLE_OUTCOME_VOID,
		PlayerTeamIndex: m.TeamIndex,
		FinishedAtMs:    stamp,
		MatchMode:       f.Mode,
		BattleConfigId:  f.ConfigId,
	}
}

// writeVoidSettlementSourcesTx 在 ABORT 的事务里给 f 的每名成员写一行作废结算来源。只由 commitAbortTx 调用:
// 调用方已持全部原 P 与 formation 行锁,并已在锁内确认这一局没有终局行,所以这里不会与 COMPLETE 的个人来源撞主键。
func writeVoidSettlementSourcesTx(ctx context.Context, tx *sql.Tx, f *pb.MatchFormationRecord, stamp uint64) error {
	for _, m := range f.Members {
		var account string
		var revision uint64
		err := tx.QueryRowContext(ctx, "SELECT owner_account,owner_revision FROM match_authority_ticket WHERE player_id=? AND ticket=? AND formation_id=?", m.PlayerId, m.Ticket, f.BattleId).Scan(&account, &revision)
		if errors.Is(err, sql.ErrNoRows) {
			logx.Errorf("void settlement skipped: battle_id=%d player_id=%d has no original ticket in this formation; the scene freeze expiry releases this player instead", f.BattleId, m.PlayerId)
			continue
		}
		if err != nil {
			return err
		}
		body, err := proto.MarshalOptions{Deterministic: true}.Marshal(voidSettlementFor(f, m, stamp))
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		if _, err = tx.ExecContext(ctx, "INSERT INTO match_battle_settlement_source(battle_id,player_id,settlement_proto,payload_digest,archived_ms,owner_account,owner_revision,issued_at_ms,inbox_seq,inbox_state,dispatch_state,next_retry_ms) VALUES(?,?,?,?,?,?,?,?,0,0,1,?)",
			f.BattleId, m.PlayerId, body, digest[:], stamp, account, revision, stamp, stamp); err != nil {
			return err
		}
	}
	return nil
}
