#include "battle_binding_helper.h"

#include <unordered_map>

#include <session/manager/session_manager.h>
#include "muduo/base/Logging.h"
#include "node/system/node/node_util.h"
#include "proto/common/base/node.pb.h"

namespace
{
	// session -> 当前绑定的 battle_id。
	// gate 单 loop 线程访问,无并发问题;条目在解绑、会话断开、battle 节点
	// 被摘除三条路径上清理(后两条见 client_message_processor.cpp)。
	std::unordered_map<SessionId, uint64_t> boundBattleIdBySession;
}

void gate_battle_binding::HandleBindBattle(const contracts::kafka::BindBattleEvent &event)
{
	const auto sessionId = event.session_id();
	auto &sessions = tlsSessionManager.sessions();
	const auto it = sessions.find(sessionId);
	if (it == sessions.end())
	{
		// 正常竞态:CreateBattle 到 Bind 落地之间玩家断线。战斗照打(回合超时
		// 默认普攻),重连进场时 scene 检查 InBattleComp 后会重发 BindBattleEvent。
		LOG_DEBUG << "BindBattle: 会话已不存在(断线竞态), session_id=" << sessionId
				  << " battle_id=" << event.battle_id()
				  << " player_id=" << event.player_id();
		return;
	}

	// fail-safe:会话已归属其他玩家时拒绝绑定(session_id 是 gate 本地发号,
	// 理论上不复用,校验便宜,照 PlayerLeaseExpired 处理器同款防御)。
	if (event.player_id() != 0 && it->second.playerId != kInvalidGuid &&
		it->second.playerId != event.player_id())
	{
		LOG_WARN << "BindBattle: 会话玩家不匹配,拒绝绑定. session_id=" << sessionId
				 << " event_player=" << event.player_id()
				 << " session_player=" << it->second.playerId;
		return;
	}

	const auto nodeEntityOpt =
		NodeUtils::FindNodeEntityByNodeId(eNodeType::BattleNodeService, event.battle_node_id());
	if (!nodeEntityOpt)
	{
		// 本 gate 尚未发现该 battle 节点(服务发现 2s 轮询窗口)或节点已下线。
		// 不做重试:客户端此后发战斗消息会收 kServiceUnavailable,恢复路径是
		// scene 重连重发 Bind;战斗无法继续的兜底是 scene reaper 按 deadline 作废。
		LOG_ERROR << "BindBattle: 本地未发现 battle 节点, session_id=" << sessionId
				  << " battle_node_id=" << event.battle_node_id()
				  << " battle_id=" << event.battle_id();
		return;
	}

	it->second.SetEntityId(eNodeType::BattleNodeService, entt::to_integral(*nodeEntityOpt));
	boundBattleIdBySession[sessionId] = event.battle_id();

	LOG_DEBUG << "BindBattle: 绑定成功, session_id=" << sessionId
			  << " battle_node_id=" << event.battle_node_id()
			  << " battle_id=" << event.battle_id()
			  << " battle_entity=" << entt::to_integral(*nodeEntityOpt);
}

void gate_battle_binding::HandleUnbindBattle(const contracts::kafka::UnbindBattleEvent &event)
{
	const auto sessionId = event.session_id();
	const auto recordIt = boundBattleIdBySession.find(sessionId);
	if (recordIt == boundBattleIdBySession.end())
	{
		// 无绑定记录:会话已断开清理过,或 Bind 从未落地。幂等返回。
		LOG_DEBUG << "UnbindBattle: 无绑定记录,忽略. session_id=" << sessionId
				  << " battle_id=" << event.battle_id();
		return;
	}

	if (recordIt->second != event.battle_id())
	{
		// 迟到/重复的解绑(Kafka at-least-once):当前已绑到新战斗,不能清。
		LOG_INFO << "UnbindBattle: battle_id 不匹配当前绑定,忽略迟到解绑. session_id=" << sessionId
				 << " event_battle_id=" << event.battle_id()
				 << " current_battle_id=" << recordIt->second;
		return;
	}

	boundBattleIdBySession.erase(recordIt);

	auto &sessions = tlsSessionManager.sessions();
	const auto it = sessions.find(sessionId);
	if (it != sessions.end())
	{
		it->second.SetEntityId(eNodeType::BattleNodeService, SessionInfo::kInvalidEntityId);
	}

	LOG_DEBUG << "UnbindBattle: 解绑完成, session_id=" << sessionId
			  << " battle_id=" << event.battle_id();
}

void gate_battle_binding::ClearBattleRecord(const SessionId sessionId)
{
	boundBattleIdBySession.erase(sessionId);
}
