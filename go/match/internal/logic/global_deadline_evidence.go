package logic

import (
	"context"

	"match/internal/authority"
	"match/internal/discovery"
	"match/internal/svc"

	battlepb "proto/battle"
	pb "proto/match"
)

// 截止作废的取证(2026-11-06,cross-zone-matchmaking.md §13.3「2026-11-06 修复」;时间判据与证据取值在
// authority/terminal_abort.go)。到期恢复与帮会取消写截止 ABORT 之前,先向成局的原执行实例确认它没有冻结待提交的终局:
// 截止前已经打完、battle 正在重放 CommitBattleOutcome 的局,不能因为 match 或它的库不可用跨过了截止加宽限就被作废。
// 与提前作废(global_no_room_abort.go 的 tombstoneAndConfirmAborted)共用同一次交互 askOriginalInstance,不另写探测:
// DestroyBattle(global_match_abort_if_absent)只对尚无房间的原操作立拒收墓碑、有房间时忽略,截止之后问一次不会伤到
// 还在等提交的房间;再 QueryBattleCreation 读结果。

// originalInstanceAnswer 是一次「立墓碑再查询」的原始结果。queryErr 为 nil 时 query 非 nil。
type originalInstanceAnswer struct {
	query      *battlepb.QueryBattleCreationResponse
	destroyErr error // DestroyBattle 的错误;拨号失败时与 queryErr 相同
	queryErr   error // QueryBattleCreation 的错误;拨号失败也记在这里
}

// askOriginalInstance 向成局的原执行实例(f.TargetEndpoint 上的 f.TargetInstanceId)发 DestroyBattle(原 operation,
// reason=global_match_abort_if_absent),再查询同一 operation。DestroyBattle 失败时照样查询:battle 对不符的实例 UUID 拒绝
// DestroyBattle(placement_operation_mismatch)、查询回空包,截止作废据查询回包认出「原地址上已是另一个实例」;提前作废
// 只认 DestroyBattle 成功之后的 ABORTED(tombstoneAndConfirmAborted)。只发 RPC,不碰 SQL,不在任何事务或行锁里调用;
// ctx 由调用方给期限。
func askOriginalInstance(ctx context.Context, f *pb.MatchFormationRecord) originalInstanceAnswer {
	conn, err := discovery.DialEndpoint(f.TargetEndpoint)
	if err != nil {
		return originalInstanceAnswer{destroyErr: err, queryErr: err}
	}
	client := battlepb.NewBattleNodeClient(conn)
	_, destroyErr := client.DestroyBattle(ctx, &battlepb.DestroyBattleRequest{BattleId: f.BattleId, Reason: "global_match_abort_if_absent", PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId})
	query, queryErr := client.QueryBattleCreation(ctx, &battlepb.QueryBattleCreationRequest{BattleId: f.BattleId, PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId})
	return originalInstanceAnswer{query: query, destroyErr: destroyErr, queryErr: queryErr}
}

// battleRegistryView 是 match 的 battle 注册中心镜像(stable 与 canary 两份)对原执行实例的只读事实。
type battleRegistryView struct {
	synced   bool // 两份镜像都已从 etcd 全量同步过至少一次(NodeWatcher.Synced)
	listed   bool // 原实例 UUID 还登记在某一份镜像里
	replaced bool // 原 node_id 现在登记的是另一个实例(battleInstanceReplaced,与观战同一判据)
}

// battleRegistryOf 读 match 的 battle 镜像。没装配镜像(只在单测里)时什么都不知道:不算同步过、当作仍在册,不给任何证据。
func battleRegistryOf(s *svc.ServiceContext, f *pb.MatchFormationRecord) battleRegistryView {
	if s == nil || s.BattleNodes == nil {
		return battleRegistryView{listed: true}
	}
	view := battleRegistryView{
		synced:   s.BattleNodes.Synced() && (s.CanaryBattleNodes == nil || s.CanaryBattleNodes.Synced()),
		replaced: battleInstanceReplaced(s, f.GetTargetNodeId(), f.GetTargetInstanceId()),
	}
	for _, watcher := range []*discovery.NodeWatcher{s.BattleNodes, s.CanaryBattleNodes} {
		if watcher == nil {
			continue
		}
		for _, entry := range watcher.Snapshot() {
			if entry.NodeUuid != "" && entry.NodeUuid == f.GetTargetInstanceId() {
				view.listed = true
			}
		}
	}
	return view
}

// classifyDeadlineEvidence 把一次「立墓碑再查询」的结果与注册中心镜像归成截止作废证据,纯函数;各取值的含义见
// authority.DeadlineAbortEvidence。拿不准一律 Undetermined,由硬上限兜底。ctxDone:这次交互的期限已到或已取消。
//   - 查询回显的就是原实例:ABORTED 且本次立墓碑成功 → NoRoom;PRESENT → BattleStillHolds;其余(UNKNOWN:同一 battle_id
//     记着别的 operation;立墓碑没成)→ Undetermined。原实例亲口作答时,注册中心说什么都不作数;
//   - 注册中心里原 node_id 已登记成另一个实例 → InstanceGone;
//   - 查询成功却没有回显实例:原地址上应答的是另一个 battle 进程(实例 UUID 每个进程生成一次,不符时 battle 回空包)
//     → InstanceGone;
//   - 查询失败:在期限之内就失败了(拨号被拒、不可达、对端不是 battle)、镜像同步过且原实例已不在册 → InstanceGone;
//     等到期限才失败(黑洞、对端卡住)、镜像没同步过、或原实例仍在册 → Undetermined。只凭「注册中心查不到」不算。
func classifyDeadlineEvidence(f *pb.MatchFormationRecord, answer originalInstanceAnswer, ctxDone bool, registry battleRegistryView) authority.DeadlineAbortEvidence {
	instance := f.GetTargetInstanceId()
	if answer.queryErr == nil && instance != "" && answer.query.GetTargetInstanceId() == instance {
		switch answer.query.GetState() {
		case battlepb.BattleCreationState_BATTLE_CREATION_ABORTED:
			if answer.destroyErr == nil {
				return authority.DeadlineEvidenceNoRoom
			}
		case battlepb.BattleCreationState_BATTLE_CREATION_PRESENT:
			return authority.DeadlineEvidenceBattleStillHolds
		}
		return authority.DeadlineEvidenceUndetermined
	}
	if registry.replaced {
		return authority.DeadlineEvidenceInstanceGone
	}
	if answer.queryErr == nil {
		if answer.query.GetTargetInstanceId() == "" {
			return authority.DeadlineEvidenceInstanceGone
		}
		return authority.DeadlineEvidenceUndetermined
	}
	if !ctxDone && registry.synced && !registry.listed {
		return authority.DeadlineEvidenceInstanceGone
	}
	return authority.DeadlineEvidenceUndetermined
}

// deadlineEvidenceProbe 组装截止作废取证:ask 是与原实例的那一次交互,registry 读注册中心镜像(在交互之后读,反映最新登记)。
// 可被多个 goroutine 同时调用(到期恢复一页并行取证):ask 与 registry 都只读共享状态。
func deadlineEvidenceProbe(ask func(context.Context, *pb.MatchFormationRecord) originalInstanceAnswer, registry func(*pb.MatchFormationRecord) battleRegistryView) authority.DeadlineAbortProbe {
	return func(ctx context.Context, f *pb.MatchFormationRecord) authority.DeadlineAbortEvidence {
		answer := ask(ctx, f)
		return classifyDeadlineEvidence(f, answer, ctx.Err() != nil, registry(f))
	}
}

// BindDeadlineAbortEvidence 把生产取证接到成局权威上(authority.Store.BindDeadlineAbortProbe)。match_service 在起任何后台循环、
// 注册任何 RPC 之前调用一次;stable 与 canary 副本都要接(帮会取消是 RPC,两条轨道都可能收到)。
// 没接时一律判不了:截止 ABORT 只在硬上限之后才写(fail-closed,但全员要多占约 10 分钟)。
func BindDeadlineAbortEvidence(s *svc.ServiceContext) {
	s.Authority.BindDeadlineAbortProbe(deadlineEvidenceProbe(askOriginalInstance, func(f *pb.MatchFormationRecord) battleRegistryView {
		return battleRegistryOf(s, f)
	}))
}
