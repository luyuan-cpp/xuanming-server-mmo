#pragma once

// scene → match 的 SQL 终局只读查询客户端(MatchInternal.GetBattleTerminalDecision,2026-11-06;
// docs/design/turn-battle-gap-closure.md §11)。只由 PlayerBattleSystem 的 reaper 在 FIGHTING 冻结到期时调用,
// 判定规则在 battle_freeze_hold_rules.h。
//
// 鉴权与回包签名沿用 CommitBattleOutcome 那一套(服务端 go/match/internal/logic/battle_terminal_decision.go):
//   - 请求由固定调用方 "scene" 用场景密钥签(MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE,与 TeamAuthority.ReadMembership
//     同一把,签名里带方法名,两条方法之间不能互相重放);
//   - 回包必须由 "match" 以方法 + "/reply" 签,request_digest 必须等于本次请求清空 auth 之后的摘要;任一不符按 kUnknown。
// match 读的是 SQL 终局行本身(终局唯一权威),不是发件箱投影:发件箱积压时投影同样查不到。
//
// 线程模型:只在 scene loop 线程调用;完成回调也在 loop 线程。不持有任何实体句柄或组件指针。

#include <cstdint>
#include <functional>
#include <string_view>

#include "battle_freeze_hold_rules.h"

namespace match
{
	class GetBattleTerminalDecisionResponse;
}

namespace battle_terminal_query
{
	// gRPC :path(线上契约,与 Go 侧 pb.MatchInternal_GetBattleTerminalDecision_FullMethodName 一致)。
	inline constexpr std::string_view kMethod = "/match.MatchInternal/GetBattleTerminalDecision";
	inline constexpr std::string_view kSceneCaller = "scene";
	inline constexpr std::string_view kMatchCaller = "match";
	// 与 go/shared/accountdirectory.SceneSecretEnv 同一个环境变量。
	inline constexpr char kSceneSecretEnv[] = "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE";

	using Done = std::function<void(battle_freeze_hold::TerminalAnswer)>;

	// 发一次查询,deadlineMs 是这一次调用的期限。
	// 返回 true = 已发出:done 之后在 loop 线程上恰好调用一次,从不在本次调用栈内同步调用(生成客户端的 GrpcDone 契约,
	// 截止、断线、节点摘除都以非 OK 完成);
	// 返回 false = 没发出去(battleId 为 0 / done 为空 / 没有场景密钥 / 没有就绪的 match 节点 / 签名失败):done 不会被调用,
	// 调用方按 kUnknown 处理。
	bool Send(uint64_t battleId, uint32_t deadlineMs, Done done);

	// 把一次调用的结果归一化成回答。Send 的完成回调与单测走同一个函数。
	//   callOk:gRPC 状态是否 OK;reply:只在 callOk 时可能非空;expectedDigest:本次请求清空 auth 之后的摘要;
	//   secret:场景密钥(空 = 没配);nowMs:核签名时间窗用的当前时刻(Unix 毫秒)。
	// 调用失败、缺回包、摘要不符、验签不过、带内 retry、决定取值不认识一律 kUnknown;UNKNOWN(没有终局决定)看
	// terminal_expected:有可裁决的 v1 原件是 kPending(终局还没落定),没有是 kNoOriginal(永远不会有结算)。
	// 只读参数,不碰任何状态。
	battle_freeze_hold::TerminalAnswer ClassifyReply(bool callOk, const match::GetBattleTerminalDecisionResponse* reply,
													  std::string_view expectedDigest, std::string_view secret,
													  uint64_t nowMs);
} // namespace battle_terminal_query
