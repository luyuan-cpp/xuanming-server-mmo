#pragma once

// gate 的「客户端 RPC 路由模式」开关:环境变量 GATE_CLIENT_RPC_ROUTER
// (设计文档 docs/design/client-rpc-router.md D34)。
//
// 两种模式:
//   * 关(默认):行为与改前完全一致 —— gate 对 login / scene_manager / battle /
//     match 每类各持 gRPC stub,按 message_id 用 typed sender 直连业务服务;
//   * 开:gate 对 Go 业务服务**零 stub**,gRPC 类客户端消息与断线通知一律原包
//     转给路由服(ClientRpcRouterNodeService),连接数 = 路由服副本数;
//     BindBattle / UnbindBattle 事件忽略(战斗流量走客户端直连,D33)。
//
// 为什么单独一个头,并且**不依赖 muduo / protobuf / 引擎**:与 gate_security.h
// 同一条纪律 —— 这里全是纯函数(输入 -> 结论),能在没有整套 C++ 引擎的机器上直接
// 编单测(cpp/nodes/gate/tests/gate_security_test.cpp);日志由调用点打。
//
// 依赖:标准库 + 引擎层 core/security/token_security.h 的 TrimAscii / ToLowerAscii
// (那个头同样只依赖标准库 + OpenSSL,gate 已链 ssl/crypto;不再抄第二份 trim)。

#include "security/token_security.h"

#include <cstdlib>
#include <string>
#include <string_view>

namespace gate_router_mode
{

inline constexpr char kRouterModeEnv[] = "GATE_CLIENT_RPC_ROUTER";

// 纯函数:"1" / "true" / "on"(忽略大小写与首尾空白)为开,其它一切为关 ——
// 包括空串、"0"、"false"、"off",也包括 "yes" / "enable" 这类没被列进白名单的
// 肯定词。刻意只认三个显式肯定词:这是灰度开关,拼错时宁可落在旧模式
// (行为与改前一致,AGENTS.md §11.3 向后兼容优先),而不是悄悄切到新路径。
inline bool ParseRouterModeFlag(std::string_view raw)
{
	const std::string value = token_security::ToLowerAscii(token_security::TrimAscii(raw));
	return value == "1" || value == "true" || value == "on";
}

// 读一次环境变量并解析;envName 为空指针或变量未设置一律视为关。
// 单测用自己的变量名调它,不与下面的进程级缓存耦合。
inline bool ResolveRouterModeFromEnv(const char *envName)
{
	const char *raw = envName != nullptr ? std::getenv(envName) : nullptr;
	return ParseRouterModeFlag(raw != nullptr ? std::string_view(raw) : std::string_view{});
}

// 进程内解析一次并缓存(照 gate_security::ResolveRunModeOnce):模式在进程
// 生命周期内不会变,而每条客户端 gRPC 类消息、每次断线都会读到它,不能每次 getenv。
inline bool IsRouterModeEnabled()
{
	static const bool enabled = ResolveRouterModeFromEnv(kRouterModeEnv);
	return enabled;
}

inline const char *RouterModeName(bool enabled)
{
	return enabled ? "router" : "direct";
}

} // namespace gate_router_mode
