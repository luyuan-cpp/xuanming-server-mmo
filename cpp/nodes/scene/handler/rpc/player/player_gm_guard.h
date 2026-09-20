#pragma once

// scene 侧 GM 客户端指令的**第二道锁**(P0-a)。
//
// 第一道锁在 gate(cpp/nodes/gate/handler/rpc/client_message_processor.cpp,
// 清单 gate_gm_client_messages.h)。这里再拦一次,防的是绕开 gate 的三条路:
//   1) scene 的 RPC 端口在集群网络里对其它 Pod 是通的,任何能连上的进程都能直接
//      发 SceneCurrencyClientPlayer.GmAddCurrency —— gate 的闸对它不存在;
//   2) 路由模式(GATE_CLIENT_RPC_ROUTER=1)下包由 client_rpc_router 原样透传,
//      将来若有第二个转发入口忘了带闸,scene 这一侧仍然成立;
//   3) 有人在 proto 里新加一条 Gm* RPC 却忘了登记进 gate 的清单。
//
// 判据与 gate 同一套原语(token_security.h 的 RunMode),但读**自己的**环境变量
// SCENE_RUN_MODE —— 每个节点的运行模式各自声明,是既有约定(gate: GATE_RUN_MODE,
// battle: BATTLE_RUN_MODE,见 token_security.h「运行模式」一节)。默认值在安全侧:
// 未设置 / 设了不认识的值 = prod = 拒绝。本地联调由 tools/scripts/start_game.ps1
// 显式设 dev。
//
// ⚠️ 跨节点相对包含与 scene_admin_handler.cpp 同一形态、同一理由(gate_security.h
// 的正确归宿是 cpp/libs/engine/core/,搬迁牵动九个文件,另开一件事做)。

#include <string_view>

#include <muduo/base/Logging.h>

#include "../../../../gate/gate_security.h"

namespace scene_gm_guard
{

inline constexpr char kSceneRunModeEnv[] = "SCENE_RUN_MODE";

// 进程内解析一次并缓存:运行模式在进程生命周期内不变,而这个值在每条 GM 包上都会
// 被读到,不能每次都 getenv(与 gate_security::ResolveRunModeOnce 同一条理由)。
inline const token_security::RunModeResolution &ResolveSceneRunModeOnce()
{
	static const token_security::RunModeResolution resolution =
		token_security::ResolveRunModeFromEnv(kSceneRunModeEnv);
	return resolution;
}

inline token_security::RunMode CurrentSceneRunMode()
{
	return ResolveSceneRunModeOnce().mode;
}

// 方法名是不是 GM 指令:形如 `Gm` + 一个大写字母开头(GmAddCurrency / GmGrantPet)。
//
// 这是分发入口统一闸门(guild-phase2.md §S4 4.34)的判据。为什么按**名字**而不是
// 像 gate 那样按 message_id 清单:
//   * scene 这一侧拦的是 `ProcessClientPlayerMessage`,那里手上现成就有
//     `MethodDescriptor::name()`,不需要再维护一份会漂移的号表;
//   * 清单要人记得往里加 —— P0-a 的六条清单就漏掉了 SceneRollbackClientPlayer 的
//     十二条(它们的 service 没标 OptionIsClientProtocolService,gate 挡得住,但
//     scene 的 ProcessClientPlayerMessage 不看这个 option,直连 scene 端口照样打得进来)。
//     前缀判据不需要任何人记得。
//
// 代价是它拦不住不叫 `Gm*` 的后门(`DebugGrant`、`GMGrant`)。这条缝由
// client_gm_gate_test.cpp 的描述符扫描用例守住:遍历客户端 player service 的方法,
// 凡是**不区分大小写**以 `gm` 开头的,都必须满足本谓词。
inline bool IsClientGmMethodName(std::string_view name)
{
	return name.size() >= 3 && name[0] == 'G' && name[1] == 'm' && name[2] >= 'A' && name[2] <= 'Z';
}

// true = 本次调用应当被拒绝(调用点负责 SetTip + return)。
// 拒绝一律留痕:这是"有人在直连 scene 打 GM"的唯一信号源。
inline bool RejectGmClientRpc(const char *rpcName)
{
	if (gate_security::ClassifyGmClientMessage(CurrentSceneRunMode()) ==
		gate_security::GmClientMessageVerdict::kAllow)
	{
		return false;
	}
	LOG_WARN << rpcName << " rejected: GM client RPC is disabled outside dev/test. "
			 << "SCENE_RUN_MODE=" << token_security::RunModeName(CurrentSceneRunMode());
	return true;
}

} // namespace scene_gm_guard
