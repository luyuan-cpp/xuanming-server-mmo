package logic

import (
	"context"
	"errors"
	"time"

	"match/internal/authority"

	battlepb "proto/battle"
	pb "proto/match"

	"google.golang.org/protobuf/proto"
)

// noRoomAbortBudget 是一次提前 ABORT 的总预算:读一次 SQL 时刻、确认本代租约下的原件、tombstoneAndConfirmAborted 的
// DestroyBattle 加 QueryBattleCreation(它自带 3 秒)、一个锁内事务。超时只是这一轮没判成,照旧等下一轮或截止,不释放任何东西。
const noRoomAbortBudget = 8 * time.Second

// noRoomAbortAuthority 是提前 ABORT 对成局权威的全部依赖,生产实现是 *authority.Store;收窄成接口便于替身测试。
type noRoomAbortAuthority interface {
	formationDestroyAuthority
	Now(ctx context.Context) (uint64, error)
	AbortWithoutRoom(ctx context.Context, work *pb.MatchFormationRecord, proof authority.NoRoomProof) (pb.BattleOutcomeDecision, error)
}

// abortProvablyNoRoom 处理「已持久 Create 原件、但原执行实例能证明不会建房」的成局(2026-11-05):不等权威截止
// (缺省成局后约 300 秒,长模板约 3720 秒,这段时间全员被占住),提前争这一局唯一的 SQL ABORT。
// 调用方必须持有 f 这一代工作租约(Store 在锁内复核)。证据按 authority.NoRoomProof 约定的顺序取得:
//  1. 先读 SQL 时刻,确认已过原件 AdmitBeforeMs 加时钟余量(authority.CreateAdmissionClosed)。battle 在逻辑线程里、
//     插房之前按本机时钟拒收过了准入期限的 Create,排队中、等分配许可的、网络迟到的都一样,所以此后它不会再接这份 Create;
//  2. 确认本代租约下原件确已持久(CreateIntentPersisted 为 true)。租约已被接手或读不到就不碰 battle,交给持租者;
//  3. tombstoneAndConfirmAborted:向原 TargetInstanceId 发 DestroyBattle(原 operation),battle 对尚无房间的原操作立拒收墓碑,
//     再查询到同一实例回 ABORTED 才算数。实例连不上、换了 UUID(回空包)、UNKNOWN、PRESENT 都不算,照旧等截止;
//     PRESENT 时按既有规则记采用版本并提交 state 4。
//
// 第 3 步不经 globalDestroy(2026-11-05 评审修正):globalDestroy 在「本代租约下无原件」时不问 battle 也回 true,
// 那个 true 不是「原实例立了墓碑」的证据,混用会在没有墓碑的情况下提前 ABORT。
//
// 返回 ABORT 表示这一局已有 ABORT(本次写下或此前已有);UNKNOWN 表示这一轮判不了。提前 ABORT 与迟到的 COMPLETE、
// 截止裁决、帮会取消抢同一终局主键,由原 P→formation 锁与主键保证只落一份。
func abortProvablyNoRoom(store noRoomAbortAuthority, f *pb.MatchFormationRecord) (pb.BattleOutcomeDecision, error) {
	request := &battlepb.CreateBattleRequest{}
	if len(f.CreateRequest) == 0 || proto.Unmarshal(f.CreateRequest, request) != nil {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), noRoomAbortBudget)
	defer cancel()
	observed, err := store.Now(ctx)
	if err != nil {
		return 0, err
	}
	if !authority.CreateAdmissionClosed(request.AdmitBeforeMs, observed) {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	persisted, err := store.CreateIntentPersisted(ctx, f)
	if errors.Is(err, authority.ErrWorkLease) {
		// 租约已被别的副本接手:交给接手者,本副本不碰 battle,也不算失败。
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	if err != nil {
		return 0, err
	}
	if !persisted {
		// 本代租约下没有原件不归这条路径(那是 globalDestroy → FinishFailed 的发房前收尾)。
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	if !tombstoneAndConfirmAborted(store, f) {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	decision, err := store.AbortWithoutRoom(ctx, f, authority.NoRoomProof{OperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId, ObservedAfterMs: observed})
	if errors.Is(err, authority.ErrWorkLease) {
		// 租约已被别的副本接手:交给接手者,本副本不算失败。
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	return decision, err
}
