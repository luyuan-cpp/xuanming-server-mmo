package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"
	inboxpb "proto/player_inbox"
	"shared/inboxcontract"
)

// ---- 原件内容不合格的局:截止之后收到下一次重放就作废(2026-11-11,cross-zone-matchmaking.md §13.3 B5 残余) ----
//
// battle 打完一局就把终局冻结成一份原字节,之后只原样重放 CommitBattleOutcome(result_commit.h:冻结后 6 小时内每 5 秒、
// 之后 24 小时内每 60 秒),不会再算出另一份。这份原件若被内容规则拒收(个人结算超过 Inbox 上限、成员或队伍与创建原件不符、
// 逃跑 / 阵亡索引对不上、原字节解不开等),这一局就永远提交不了 COMPLETE。修复前 match 只回 FailedPrecondition:
// battle 照旧重放、房间一直在,截止作废取证看到 PRESENT,只能等到硬上限(截止后约 14 分钟)才无证据作废,全员一直被冻着。
// 现在过了权威截止加终局宽限之后,match 收到原执行实例的下一次重放时就在同一把锁里写 ABORT 并把它回给 battle,battle 收尾删房。
//
// 只认「原执行实例提交、内容本身不合格」这一类:
//   - 身份要与成局一致(placement_operation_id、target_instance_id):别的实例或过期操作提交的原件说明不了原实例冻结了什么;
//   - 版本错位类拒收不算(outcomeVersionSkew):带本版本不认识的字段、结算格式版本不是 1、本版本读不到 Inbox 上限。
//     滚动升级时新版 match 可能接受它,这类照旧等截止作废取证与硬上限,给升级留窗口;
//   - 成局必须自动路径可裁决(autoTerminalOriginal),与截止 ABORT 同一判据;
//   - 只在过了权威截止加终局宽限之后(terminalAbortDue),与截止 ABORT 同一分界:截止之前照旧回 FailedPrecondition,
//     battle 5 秒后重放,过分界后的第一次重放就裁决。
// 写 ABORT 走所有作废共用的 commitAbortTx(source = MATCH_REJECTED_ORIGINAL_ABORT),与 COMPLETE、截止 ABORT、帮会取消、
// 人工处置争同一把原 P→formation 锁和同一个终局主键,锁内先复读终局,只落一份决定。

// terminalAbortRejectedOriginal 是这类 ABORT 在 global_flow_total{stage="terminal"} 下的 outcome 取值(低基数,battle_id 只进日志)。
const terminalAbortRejectedOriginal = "abort_rejected_original"

// outcomeVersionSkew 判这份终局原件被拒是不是可能来自版本错位:请求或结算正文带本版本不认识的字段、结算格式版本不是 1,
// 或本版本读不到个人结算的 Inbox 上限(match 自己的配置 / 版本问题,不是原件的错)。为真时不提前作废:新版 match 可能接受它。
// 原字节解不开不算版本错位(protobuf 对新字段只会放进未知字段,解不开是原件坏了)。只看原件,不碰 SQL。纯函数。
func outcomeVersionSkew(req *pb.CommitBattleOutcomeRequest) bool {
	if completionHasUnknown(req.ProtoReflect()) {
		return true
	}
	if _, limited := inboxcontract.MaxPayloadBytes(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT); !limited {
		return true
	}
	event := &kafkapb.BattleResultEvent{}
	if err := proto.Unmarshal(req.RawResultPayload, event); err != nil {
		return false
	}
	return event.SettlementFormatVersion != 1 || completionHasUnknown(event.ProtoReflect())
}

// settleRejectedOriginal 是锁外校验就拒收的原件(decodeTerminalRequest 失败)的入口:加原 P→formation→原票锁,
// 锁内复读终局,再交给 abortRejectedOriginalLocked 判要不要作废。拿不到锁时有终局就回放它,否则回原拒收错误
// (battle 照旧重放,下次再判)。rejectErr 必须已包成 ErrBattleOutcomeOriginal。
func (s *Store) settleRejectedOriginal(ctx context.Context, req *pb.CommitBattleOutcomeRequest, rejectErr error) (pb.BattleOutcomeDecision, error) {
	if outcomeVersionSkew(req) {
		return 0, rejectErr // 不加锁:这类不会提前作废
	}
	tx, f, _, err := s.terminalFormation(ctx, req.BattleId)
	if err != nil {
		old, readErr := readTerminal(ctx, s.DB, req.BattleId)
		if readErr == nil && old != nil && old.Decision != 0 {
			return replayTerminal(old, req)
		}
		return 0, rejectErr
	}
	defer tx.Rollback()
	old, err := readTerminal(ctx, tx, f.BattleId)
	if err != nil {
		return 0, errors.Join(rejectErr, err)
	}
	if old != nil {
		return replayTerminal(old, req)
	}
	return s.abortRejectedOriginalLocked(ctx, tx, f, req, rejectErr)
}

// abortRejectedOriginalLocked 在原件被内容规则拒收之后判这一局要不要现在作废。调用方已持原 P→formation→原票锁,
// 并已在锁内确认 match_battle_completion 没有这一局的行。条件都满足时在 tx 里写 ABORT 并提交,回 ABORT;否则回 rejectErr
// (battle 照旧重放),不提交 tx。rejectErr 必须已包成 ErrBattleOutcomeOriginal。
func (s *Store) abortRejectedOriginalLocked(ctx context.Context, tx *sql.Tx, f *pb.MatchFormationRecord, req *pb.CommitBattleOutcomeRequest, rejectErr error) (pb.BattleOutcomeDecision, error) {
	if f.OperationId != req.PlacementOperationId || f.TargetInstanceId != req.TargetInstanceId || outcomeVersionSkew(req) {
		return 0, rejectErr
	}
	r, ok := autoTerminalOriginal(f)
	if !ok {
		return 0, rejectErr
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return 0, errors.Join(rejectErr, err)
	}
	if !terminalAbortDue(r.DeadlineMs, stamp) {
		return 0, rejectErr
	}
	if err = commitAbortTx(ctx, tx, f, stamp, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_REJECTED_ORIGINAL_ABORT); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("原件不合格的截止ABORT提交未知，必须重放原请求: %w", err)
	}
	observeTerminalAbort(f.BattleId, terminalAbortRejectedOriginal)
	logx.Errorf("global formation aborted after the deadline because its battle's frozen outcome can never commit: battle_id=%d reason=%v; "+
		"the battle result is void for every member, check the battle and match versions and the settlement limits for this battle_id", f.BattleId, rejectErr)
	return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, nil
}
