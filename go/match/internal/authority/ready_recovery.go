package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"match/internal/metrics"
	pb "proto/match"
)

// SQL只缩小待核候选；完整Create正文及当前角色P/工作租约仍由锁内入口复核。
// recoverableOutcomeShape 是同一规则的 Go 写法,两处必须一起改。
const recoverableOutcomeShapeSQL = "f.outcome_protocol_version=1 AND f.preflight_supported_version=1 AND f.operation_id<>'' AND f.target_instance_id<>'' AND f.preflight_instance_id=f.target_instance_id AND LENGTH(f.create_request)>0 AND ((f.adopted_outcome_observed=0 AND f.adopted_outcome_protocol_version=0) OR (f.adopted_outcome_observed=1 AND f.adopted_outcome_protocol_version=1))"

// recoverableOutcomeShape 是 recoverableOutcomeShapeSQL 的 Go 写法。只用来把「形状合格、却过不了持久 Create 原件核对」
// 与旧协议 / 能力证据不全(按设计保持 UNKNOWN,过截止后由巡检的 CountStrandedExpiredFormations 计数)分开。
func recoverableOutcomeShape(f *pb.MatchFormationRecord) bool {
	return f != nil && f.OutcomeProtocolVersion == 1 && f.PreflightSupportedVersion == 1 && f.OperationId != "" &&
		f.TargetInstanceId != "" && f.PreflightInstanceId == f.TargetInstanceId && len(f.CreateRequest) > 0 &&
		((!f.AdoptedOutcomeObserved && f.AdoptedOutcomeProtocolVersion == 0) || (f.AdoptedOutcomeObserved && f.AdoptedOutcomeProtocolVersion == 1))
}

// ErrUnrecoverableFormation:成局满足到期恢复的候选形状(recoverableOutcomeShape),持久 Create 原件却与成局字段对不上
// (supportedOutcomeOriginal 失败)。到期 ABORT 与 COMPLETE 都裁决不了它,票据永远停在 state 2/3/4,只能人工按
// cross-zone-matchmaking.md §13.3 处置。用 errors.Is 判断(2026-10-30,评审 B-4)。
var ErrUnrecoverableFormation = errors.New("成局形状合格但持久Create原件与成局字段不符，到期终局无法裁决")

// unrecoverableFormationOutcome 是这一类在 global_flow_total{stage="recover"} 下的 outcome 取值(低基数;battle_id 只进日志)。
const unrecoverableFormationOutcome = "original_mismatch"

// reportUnrecoverableFormation 记一次「到期恢复访问到不可恢复成局」:计数 + ERROR。每次访问都报,值班按速率判断还在不在。
func reportUnrecoverableFormation(battleID uint64, cause error) {
	metrics.ObserveGlobalFlow("recover", unrecoverableFormationOutcome)
	logx.Errorf("global formation can never reach a terminal outcome battle_id=%d: %v; its tickets stay occupied, "+
		"handle it per cross-zone-matchmaking.md 13.3 (never hand-edit match_formation / match_authority_ticket / match_battle_completion)",
		battleID, cause)
}

// 同一持久原操作与实例的合法终局才退出恢复；冲突/旧来源记录不能被当作新终局证明。
const matchingOutcomeTerminalSQL = "c.battle_id=f.battle_id AND c.outcome_protocol_version=1 AND c.placement_operation_id=f.operation_id AND c.target_instance_id=f.target_instance_id AND c.decision IN(1,2)"

// 新COMPLETE在提交事务中直接退休。旧版本少量成功局每轮最多八条只退休调度字段，
// 不动state4、deadline、原Create、票或双outbox；并发更新用原retry值与同终局原件CAS。
func (s *Store) retireTerminalOutcomeSchedules(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, "SELECT f.battle_id,f.retry_at_ms FROM match_formation f JOIN match_battle_completion c ON "+matchingOutcomeTerminalSQL+" WHERE f.state=4 AND f.retry_at_ms<? AND "+recoverableOutcomeShapeSQL+" ORDER BY f.retry_at_ms,f.battle_id LIMIT 8", terminalOutcomeNoRetryMs)
	if err != nil {
		return err
	}
	type schedule struct{ battle, retryMs uint64 }
	var schedules []schedule
	for rows.Next() {
		var item schedule
		if err := rows.Scan(&item.battle, &item.retryMs); err != nil {
			rows.Close()
			return err
		}
		schedules = append(schedules, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range schedules {
		if _, err := s.DB.ExecContext(ctx, "UPDATE match_formation f JOIN match_battle_completion c ON "+matchingOutcomeTerminalSQL+" SET f.retry_at_ms=? WHERE f.battle_id=? AND f.state=4 AND f.retry_at_ms=? AND "+recoverableOutcomeShapeSQL, terminalOutcomeNoRetryMs, item.battle, item.retryMs); err != nil {
			return err
		}
	}
	return nil
}

// RecoverExpiredOutcomes 是普通PVE/PVP与活动共用的正式有界恢复入口。
// 沿原state/retry_at_ms索引每批至多八局，只选尚无合法终局的v1资格形状。
// 游标只用于调度；故障项仍推进，下轮重访，旧0/明确采用0保持UNKNOWN且不占候选页。
func (s *Store) RecoverExpiredOutcomes(ctx context.Context, afterRetryMs, afterBattle uint64) (uint64, uint64, error) {
	if err := s.retireTerminalOutcomeSchedules(ctx); err != nil {
		return afterRetryMs, afterBattle, err
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return afterRetryMs, afterBattle, err
	}
	if stamp < terminalOutcomeGraceMs {
		return 0, 0, nil
	}
	// 先减宽限再绑定SQL参数，避免deadline+grace的uint64溢出；锁内仍重新读SQL时钟。
	rows, err := s.DB.QueryContext(ctx, "SELECT f.retry_at_ms,f.battle_id FROM match_formation f WHERE f.state IN(2,3,4) AND "+recoverableOutcomeShapeSQL+" AND f.retry_at_ms<=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED) AND f.deadline_ms>0 AND f.deadline_ms<=? AND (f.retry_at_ms>? OR (f.retry_at_ms=? AND f.battle_id>?)) AND NOT EXISTS(SELECT 1 FROM match_battle_completion c WHERE "+matchingOutcomeTerminalSQL+") ORDER BY f.retry_at_ms,f.battle_id LIMIT 8", stamp-terminalOutcomeGraceMs, afterRetryMs, afterRetryMs, afterBattle)
	if err != nil {
		return afterRetryMs, afterBattle, err
	}
	type candidate struct{ retryMs, battle uint64 }
	var formations []candidate
	for rows.Next() {
		var f candidate
		if err := rows.Scan(&f.retryMs, &f.battle); err != nil {
			rows.Close()
			return afterRetryMs, afterBattle, err
		}
		formations = append(formations, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return afterRetryMs, afterBattle, err
	}
	if len(formations) == 0 {
		return 0, 0, nil
	}
	var failures []error
	for _, f := range formations {
		if err := ctx.Err(); err != nil {
			return afterRetryMs, afterBattle, errors.Join(append(failures, err)...)
		}
		afterRetryMs, afterBattle = f.retryMs, f.battle
		// 先用独立标量游标推进，再解析完整原件；损坏的一条不能把整页卡在同一位置。
		original, err := s.Formation(ctx, f.battle)
		if err == nil && original.RetryAtMs != f.retryMs {
			continue // 并发worker换了租约，后续完整轮次重访，绝不沿旧观察写入。
		}
		if err == nil {
			_, err = s.RecoverExpiredOutcome(ctx, original)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("到期成局恢复未确认 battle=%d: %w", f.battle, err))
		}
	}
	return afterRetryMs, afterBattle, errors.Join(failures...)
}

// RecoverExpiredOutcome 不查询Battle、也不根据节点死亡猜终局。原发房前能力与完整
// v1 Create证明所有个人效果必须先过本SQL门；到期后在原P→formation锁内争唯一ABORT。
// 原工作租约仍有效时不抢占；多副本重放只读取同原操作/UUID的已提交终局。
//
// 不可恢复成局(ErrUnrecoverableFormation)在这里计数并打 ERROR,对外仍回 UNKNOWN、nil:现有调用方
// (RecoverExpiredOutcomes 的整轮失败、logic globalAbortPrepared 的 HoldRelay)会把非 nil error 当成本轮未确认的故障,
// 而这一类是永久状态,不是本轮故障。要区分它的新调用方用 recoverExpiredOutcome 拿哨兵。
func (s *Store) RecoverExpiredOutcome(ctx context.Context, observed *pb.MatchFormationRecord) (pb.BattleOutcomeDecision, error) {
	decision, err := s.recoverExpiredOutcome(ctx, observed)
	if errors.Is(err, ErrUnrecoverableFormation) {
		reportUnrecoverableFormation(observed.GetBattleId(), err)
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	return decision, err
}

// recoverExpiredOutcome 是 RecoverExpiredOutcome 的实现;形状合格却过不了原件核对时回 ErrUnrecoverableFormation(决定为 UNKNOWN)。
func (s *Store) recoverExpiredOutcome(ctx context.Context, observed *pb.MatchFormationRecord) (pb.BattleOutcomeDecision, error) {
	if observed == nil {
		return 0, errors.New("到期恢复缺持久创建原件")
	}
	if _, err := supportedOutcomeOriginal(observed); err != nil {
		if recoverableOutcomeShape(observed) {
			return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, fmt.Errorf("%w: %v", ErrUnrecoverableFormation, err)
		}
		// 旧协议 / 能力证据不全:按设计保持 UNKNOWN(过截止的由巡检 CountStrandedExpiredFormations 计数)。
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	readExisting := func() (pb.BattleOutcomeDecision, bool, error) {
		row, err := readTerminal(ctx, s.DB, observed.BattleId)
		if err != nil || row == nil {
			return 0, false, err
		}
		decision, err := replayTerminal(row, &pb.CommitBattleOutcomeRequest{BattleId: observed.BattleId, PlacementOperationId: observed.OperationId, TargetInstanceId: observed.TargetInstanceId, RawResultPayload: row.RawPayload})
		return decision, true, err
	}
	if decision, exists, err := readExisting(); err != nil || exists {
		return decision, err
	}
	// 兼容旧代码已经写出的state5：没有唯一终局就保留UNKNOWN。原票可能已换轮，
	// 不能补锁当前新票或以旧状态号倒造一份安全ABORT。
	if observed.State != 2 && observed.State != 3 && observed.State != 4 {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	tx, f, _, err := s.terminalFormation(ctx, observed.BattleId)
	if err != nil {
		// 另一副本可能刚持P提交ABORT并关闭原票；不能把同原件的成功重放当角色丢失。
		if decision, exists, readErr := readExisting(); readErr != nil || exists {
			return decision, readErr
		}
		return 0, err
	}
	defer tx.Rollback()
	if !proto.Equal(f, observed) {
		_ = tx.Rollback()
		if decision, exists, err := readExisting(); err != nil || exists {
			return decision, err
		}
		return 0, ErrWorkLease
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return 0, err
	}
	if f.RetryAtMs > stamp || (f.State != 2 && f.State != 3 && f.State != 4) {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	decision, err := s.abortTerminalTx(ctx, tx, f)
	if err != nil || decision != pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT {
		return decision, err
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("到期ABORT提交未知，必须复用原件重查: %w", err)
	}
	return decision, nil
}

// ReadyAfter 的游标仅调度巡检，不能授权释放。即使前面的房间一直 UNKNOWN，
// 下一页也照常访问；进程重启从头扫描不会改变任何票据的业务状态。
func (s *Store) ReadyAfter(ctx context.Context, afterMs, afterPlayer uint64) ([]*pb.MatchAuthorityTicketRecord, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE state=3 AND (enqueued_ms>? OR (enqueued_ms=? AND player_id>?)) ORDER BY enqueued_ms,player_id LIMIT 8", afterMs, afterMs, afterPlayer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchAuthorityTicketRecord
	for rows.Next() {
		ticket, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ticket)
	}
	return out, rows.Err()
}

// ReleaseCompletedReady 只认已归档且校验完整的本玩家结算原件，不使用客户端
// 状态、缓存或超时猜测房间结束。原角色P锁先于票锁；旧巡检不能释放新一轮票。
func (s *Store) ReleaseCompletedReady(ctx context.Context, observed *pb.MatchAuthorityTicketRecord) (bool, error) {
	if observed == nil || observed.State != 3 || observed.FormationId == 0 || observed.Ticket == "" {
		return false, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = s.lockOriginalPlayers(ctx, tx, []*pb.MatchAuthorityTicketRecord{observed}); err != nil {
		return false, err
	}
	var battle uint64
	// 同一原owner的签名永久APPLIED收据才是释放依据；归档/入箱/水位猜测都不能代替。
	err = tx.QueryRowContext(ctx, "SELECT s.battle_id FROM match_battle_settlement_source s JOIN match_battle_completion c ON c.battle_id=s.battle_id WHERE s.battle_id=? AND s.player_id=? AND s.owner_account=? AND s.owner_revision=? AND s.dispatch_state=2 AND s.inbox_state=2 AND s.inbox_seq>0 AND c.decision=1 AND c.outcome_protocol_version=1", observed.FormationId, observed.PlayerId, observed.OwnerAccount, observed.OwnerRevision).Scan(&battle)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=? AND ticket=? AND formation_id=? AND state=3 AND owner_account=? AND owner_revision=?", observed.PlayerId, observed.Ticket, observed.FormationId, observed.OwnerAccount, observed.OwnerRevision)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return changed != 0, nil
}
