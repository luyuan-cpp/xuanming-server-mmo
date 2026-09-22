#pragma once

// 「哪些客户端消息号是 GM 指令」的唯一清单(P0-a)。
//
// 为什么与 gate_security.h 分开两个头:
//   gate_security.h 刻意**不依赖 muduo / protobuf / 引擎**,好让
//   cpp/nodes/gate/tests/gate_security_test.cpp 能在没有整套 C++ 引擎的机器上
//   直接 g++ 编出来(那份测试的文件头写了编译命令)。而消息号常量住在生成的
//   service_metadata 头里,它们 `#include` 对应的 `.pb.h` —— 一旦把这些包含拉进
//   gate_security.h,那份独立单测就再也编不动了。所以策略(看运行模式给结论)
//   留在 gate_security.h,清单(看 message_id)放这里。
//
// 为什么用生成常量而不是字面量 37/49/94/95/175/187:
//   message_id 由导出器按 proto 顺序分配,proto 一改就漂移(proto/message_id.txt
//   是产物不是契约)。写字面量的清单会在某次 regen 之后静默指向别的 RPC ——
//   那时闸门要么拦错人,要么把 GM 指令重新放出去,而且没有任何编译错误。
//
// 维护规矩:**新加任何 Gm* / Debug* / Test* 客户端 RPC,必须同时加进本表**。
// 更好的做法是根本不要把它们标 `OptionIsClientProtocolService`(参照
// proto/trade/trade_admin.proto:14 与 go/guild/internal/session/session.go 的
// ClientMethods 白名单),本表只是给"已经放出去的那六条"兜底。

#include "rpc/service_metadata/player_attribute_service_metadata.h"
#include "rpc/service_metadata/player_currency_service_metadata.h"
#include "rpc/service_metadata/player_pet_service_metadata.h"

namespace gate_gm_client_messages
{

// 今天经 gate 对客户端可达的全部 GM 指令。
// 口径 = proto 里 `rpc Gm*` 且所属 service 标了 `OptionIsClientProtocolService`。
//
// 不在本表里的 GM RPC,是因为它们本来就到不了客户端,别往里加:
//   * SceneRollbackClientPlayer.Gm*(102-117):service 没标
//     OptionIsClientProtocolService,不进 IsClientMessageId;
//   * Gate/Scene.GmGracefulShutdown(125/126)、TradeAdmin.SeedListing(199)、
//     LoginAdmin.*(111)、dbTest(3)、SceneSceneTest(18):同上,且前两条另有
//     HMAC 签名鉴权(gate_security.h VerifyGmRequestFromEnv)。
inline constexpr uint32_t kGmClientMessageIds[] = {
	SceneCurrencyClientPlayerGmAddCurrencyMessageId,
	SceneCurrencyClientPlayerGmDeductCurrencyMessageId,
	SceneCurrencyClientPlayerGmBlockCurrencyMessageId,
	SceneCurrencyClientPlayerGmUnblockCurrencyMessageId,
	SceneAttributeClientPlayerGmSetPlayerLevelMessageId,
	ScenePetClientPlayerGmGrantPetMessageId,
};

// 线性扫 6 个 uint32 —— 在每条客户端包的热路径上,但这比任何哈希表都快,
// 而且没有静态初始化顺序问题。加到几十条再考虑换 switch。
inline bool IsGmClientMessage(uint32_t messageId)
{
	for (const uint32_t gmId : kGmClientMessageIds)
	{
		if (gmId == messageId)
		{
			return true;
		}
	}
	return false;
}

} // namespace gate_gm_client_messages
