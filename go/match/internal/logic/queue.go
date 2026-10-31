package logic

import (
	"context"
	"time"

	"match/internal/pkg/ctxkeys"
	"match/internal/playercontract"
	"match/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// authoritativePlayerID 取权威 player_id:客户端直达协议(gate gRPC)时以
// x-session-detail-bin 里的身份为准,请求体里的 player_id 仅供内部调用使用
// (照 login 的 session metadata 模式)。带了 session 但与请求体不一致时记日志。
func authoritativePlayerID(ctx context.Context, reqPlayerId uint64) uint64 {
	if detail, ok := ctxkeys.GetSessionDetails(ctx); ok && detail.GetPlayerId() != 0 {
		if reqPlayerId != 0 && reqPlayerId != detail.GetPlayerId() {
			logx.Errorf("[match] Request player_id=%d differs from authoritative session identity %d; using session identity",
				reqPlayerId, detail.GetPlayerId())
		}
		return detail.GetPlayerId()
	}
	return reqPlayerId
}

// isPlayerBattleLocked 咨询性检查 battle:lock:{player_id}(scene 写,契约 key
// 走 PlayerRedis)。权威判定仍在 scene 的 InBattleComp;这里只是提前挡明显
// 不合法的请求。Redis 出错时按"有锁"处理(fail-closed:宁可拒绝排队,不可放进双战斗)。
// 实现收口在 playercontract.IsBattleLocked(team-system.md §A.2)。
func isPlayerBattleLocked(svcCtx *svc.ServiceContext, playerId uint64) (bool, error) {
	return playercontract.IsBattleLocked(context.Background(), svcCtx, playerId)
}

// gatherCreateStageWorst 是全员备战完成后到 CreateBattle 返回为止的最坏耗时预算:CreateBattle(createBattleTimeout)按两轮算 = 2 × 5s = 10s。
// 成局原件固定了目标实例,这条路径不换节点;保留两轮是因为建房前还有能力查询、存原件、核对工作租约三次调用(各 3s 上限),
// 公式没有单独计它们,由这一轮和 matchedTicketTTLFor 的 10s 余量吸收。
const gatherCreateStageWorst = 2 * createBattleTimeout

// matchedTicketTTLFor 按组大小算一局的备战预算(秒,设计决策 D5):
//
//	max(MatchedTicketTTLSeconds,
//	    ⌈required × (removeObserverTimeout + prepareBattleTimeout)
//	     + gatherCreateStageWorst + rollbackTimeout⌉ + 10s)
//
// 5v5 十人 = ⌈10×6 + 10 + 3⌉ + 10 = 83s。
// 它决定 scene 侧 PREPARING 的作废期限(runFormationGather 的 prepare_deadline_ms,PrepareBattle 时已下发、事后改不了),
// 也是组队开战锁时长的基数(MatchLockTTLSeconds)。
// 下游依赖(改公式或上调 MatchedTicketTTLSeconds 时一并核对):scene 备战锁 EX = 该期限 +
// PlayerBattleSystem::kLockExtraTtlSec(60s),十人组锁最长到 gather 起点 +143s;battle 的确认事件
// 补发窗口 kConfirmResendWindowMs(battle_room_manager.cpp)从建房起算,必须 ≥ 这个值。
// 配置值只是下限,组越大窗口越长。常量与 gather.go / spectate.go 同源,不另写数字。
func matchedTicketTTLFor(svcCtx *svc.ServiceContext, required uint32) int {
	ttl := int(svcCtx.Config.MatchedTicketTTLSeconds)
	if ttl <= 0 {
		ttl = 30
	}
	chain := time.Duration(required)*(removeObserverTimeout+prepareBattleTimeout) +
		gatherCreateStageWorst + rollbackTimeout
	// 向上取整到秒:超时常量带零头时,截断会把预算算小。
	worst := int((chain+time.Second-1)/time.Second) + 10
	if worst > ttl {
		return worst
	}
	return ttl
}

// compensationTicketTTLFor 补偿路径的窗口:已冻结者逐人 CancelBattlePrepare(每人最长 rollbackTimeout)+ 余量 10s。
func compensationTicketTTLFor(prepared int) int {
	return prepared*int(rollbackTimeout/time.Second) + 10
}

// nowMs 返回当前 Unix 毫秒。
func nowMs() uint64 {
	return uint64(time.Now().UnixMilli())
}
