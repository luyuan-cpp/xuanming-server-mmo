#pragma once

// gate 拿到 scene 对一条客户端请求的 RPC0 结局后,该给客户端回什么(纯判定)。
// 契约:docs/design/client-rpc-callback.md §2.1(请求-响应恰好一个回包、id 原样回显;id = 0 不关联)、§7 R1。
//
// 前提(调用方保证):只为「请求-响应类」客户端消息调用 —— 内层方法的响应类型不是 Empty。响应为 Empty 的(移动、
// SceneInfoC2S 等)由场景 lane 单向发出(client_scene_lane.cpp),成功本就不回包,根本不登记回调,不会走到这里多回一个包。
// 调用方是场景 lane 的完成处置 gate_scene_lane::OnSceneCallDone(docs/design/client-scene-request-handoff-hold.md §4.5)。
//
// scene 的 ProcessClientPlayerMessage 在派发前有 11 处早退(request 为空、消息号 / 服务 / 方法查不到、会话不在、
// 反向身份栅栏、实体未载入、旧会话、请求解析失败、字段体积 / 负数校验),另有归属交接在途时不派发的一处:RPC0 照常以
// RESPONSE 完成,但应答里没有 message_content。这些都发生在玩家 handler 之前,请求确定未执行;改造前 gate 直接丢弃,
// 客户端只能等 15s 超时(R1)。其中会话不在、实体未载入、交接在途三处,对换节点执行含义不变的请求另带 RETRY_LATER(kRetryLater)。
// 判定不需要区分是哪一处早退:scene 的玩家 handler 全部同步完成、推送一律 id = 0,没有「RPC0 回空、真回包另走一条路」
// 的写法,所以「成功但无 message_content」就等于「scene 不会再为这个请求回任何东西」,回 1003 不会造成双回包。
//
// 纯函数、零状态,单独成文件是为了在 routing_identity_test 里直接测,不把会话表、连接、RPC 注册链拖进测试工程
// (与 login_admission.h 同一做法)。

#include <cstdint>

#include "network/rpc_status.h"
#include "proto/scene/scene.pb.h"

namespace gate_scene_client_reply
{

enum class Disposition : uint8_t
{
	kForwardContent,             // 原样下发 scene 的 message_content(id / message_id 由 scene 回显)
	kServiceUnavailableEnvelope, // 回带请求 id 与原 message_id 的 1003 信封(gate_lane::SendEnvelope)
	kDrop,                       // 不下发
	// scene 明确答复「确定未执行、稍后重投」(dispatch_verdict = RETRY_LATER):玩家归属正在交接,或还没在目标节点就绪。
	// gate 的场景 lane 把这一条放回队头,在有界等待内按收包顺序重投(docs/design/client-scene-request-handoff-hold.md §4)。
	// 请求号为 0 的请求同样重投:重投不靠请求号关联,只有最终放弃时才按「没法关联」不回执。
	kRetryLater,
};

// status:RPC0 调用结局;response:成功时的应答,失败时为空(RpcClient::CallMethod 以空 shared_ptr 回调);
// clientRequestId:客户端信封里的请求号(ClientRequest.id)。
inline Disposition Decide(const RpcStatus &status, const ProcessClientPlayerMessageResponse *response,
						  uint64_t clientRequestId)
{
	if (!status.ok())
	{
		// 确定未执行(没发出去 / 对端派发前拒绝)或结果未知(超时 / 断连 / 关机):都回 1003。
		// 请求号为 0 时信封 id 也是 0,与改造前相同,本次不动。
		return Disposition::kServiceUnavailableEnvelope;
	}
	if (response != nullptr && response->has_message_content())
	{
		return Disposition::kForwardContent;
	}
	// scene 派发前早退,但明确说了「稍后重投」:只有这一种早退是「等一等就能受理」的,其余早退等也没用。
	// 旧版 scene 不填这个字段(默认 UNSPECIFIED),落到下面的 1003,与改造前相同。
	if (response != nullptr && response->dispatch_verdict() == CLIENT_MESSAGE_DISPATCH_VERDICT_RETRY_LATER)
	{
		return Disposition::kRetryLater;
	}
	// scene 派发前早退(R1):请求号非 0 才能让客户端按 id 认领,回 1003(确定未执行,重试安全)。
	// 请求号为 0 的(旧 robot 的 SendRequest)没法关联,维持不下发:回一个空 body 的信封会被按 message_id
	// 派发的 handler 解成全零响应,看上去像受理。
	return clientRequestId != 0 ? Disposition::kServiceUnavailableEnvelope : Disposition::kDrop;
}

} // namespace gate_scene_client_reply
