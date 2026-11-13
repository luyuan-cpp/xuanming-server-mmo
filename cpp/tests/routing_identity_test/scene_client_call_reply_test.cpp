#include <gtest/gtest.h>

#include <array>
#include <cstddef>
#include <cstdint>
#include <string>

#include "../../nodes/gate/handler/rpc/scene_client_call_reply.h"

// ---------------------------------------------------------------------------
// scene_client_call_reply_test —— gate 对 scene RPC0 结局的处置(scene_client_call_reply.h 的 Decide)。
//
// 回归对象是 docs/design/client-rpc-callback.md §7 R1:scene 在派发前早退时 RPC0 照常回 RESPONSE、但没有
// message_content,改造前 gate 直接丢弃,带请求号的客户端回调只能等 15s 超时。现在请求号非 0 时回 1003 信封,
// 请求号为 0(不关联)维持不下发;scene 正常回包(哪怕响应体序列化后是空字节)照旧原样下发,不得被当成早退。
// ForwardSceneClientCallResult 本身只是「查会话 + 按 Decide 的结果发包 + 采样日志」的薄适配,不在本文件覆盖。
// ---------------------------------------------------------------------------

namespace
{
	using gate_scene_client_reply::Decide;
	using gate_scene_client_reply::Disposition;

	constexpr uint64_t kCorrelatedRequestId = 42;
	constexpr uint64_t kUncorrelatedRequestId = 0;
	constexpr uint32_t kInnerMessageId = 63;

	// scene 正常派发后的应答形状:回填 id / message_id,serialized_message 是玩家响应体。
	ProcessClientPlayerMessageResponse DispatchedReply(uint64_t requestId, const std::string &body)
	{
		ProcessClientPlayerMessageResponse response;
		response.mutable_message_content()->set_id(requestId);
		response.mutable_message_content()->set_message_id(kInnerMessageId);
		response.mutable_message_content()->set_serialized_message(body);
		return response;
	}

	// 模拟一次 RPC0 线上往返:gate 拿到的是从字节流解析出来的应答,不是 scene 进程里的那个对象。
	ProcessClientPlayerMessageResponse OverTheWire(const ProcessClientPlayerMessageResponse &sent)
	{
		ProcessClientPlayerMessageResponse received;
		EXPECT_TRUE(received.ParseFromString(sent.SerializeAsString()));
		return received;
	}
} // namespace

TEST(SceneClientCallReplyTest, DispatchedReplyIsForwardedAsIs)
{
	const auto reply = OverTheWire(DispatchedReply(kCorrelatedRequestId, "body"));
	EXPECT_EQ(Decide(RpcStatus::Ok(), &reply, kCorrelatedRequestId), Disposition::kForwardContent);
}

TEST(SceneClientCallReplyTest, DispatchedReplyWithEmptyBodyIsStillForwarded)
{
	// 玩家响应全是默认值时序列化结果为空字节;scene 仍然 mutable_message_content() 并回填了 id,
	// 线上往返后 message_content 必须仍然存在,否则会把一次正常受理误判成早退、回 1003。
	const auto reply = OverTheWire(DispatchedReply(kCorrelatedRequestId, std::string{}));
	ASSERT_TRUE(reply.has_message_content());
	EXPECT_EQ(Decide(RpcStatus::Ok(), &reply, kCorrelatedRequestId), Disposition::kForwardContent);
}

TEST(SceneClientCallReplyTest, DispatchedReplyForUncorrelatedRequestIsForwarded)
{
	// 请求号为 0 的旧调用方(robot SendRequest)照旧按 message_id 收回包,行为与改造前相同。
	const auto reply = OverTheWire(DispatchedReply(kUncorrelatedRequestId, "body"));
	EXPECT_EQ(Decide(RpcStatus::Ok(), &reply, kUncorrelatedRequestId), Disposition::kForwardContent);
}

TEST(SceneClientCallReplyTest, EarlyReturnWithRequestIdGetsServiceUnavailableEnvelope)
{
	// scene 派发前早退:handler 返回时什么都没写,框架照常回 RESPONSE(空的 ProcessClientPlayerMessageResponse)。
	const auto reply = OverTheWire(ProcessClientPlayerMessageResponse{});
	ASSERT_FALSE(reply.has_message_content());
	EXPECT_EQ(Decide(RpcStatus::Ok(), &reply, kCorrelatedRequestId), Disposition::kServiceUnavailableEnvelope);
}

TEST(SceneClientCallReplyTest, EarlyReturnWithoutRequestIdIsDropped)
{
	// 请求号为 0 没法关联:回一个空 body 的信封会被按 message_id 派发的 handler 解成全零响应,看上去像受理。
	const auto reply = OverTheWire(ProcessClientPlayerMessageResponse{});
	EXPECT_EQ(Decide(RpcStatus::Ok(), &reply, kUncorrelatedRequestId), Disposition::kDrop);
}

TEST(SceneClientCallReplyTest, SuccessWithoutResponseObjectIsTreatedAsEarlyReturn)
{
	// 防御:成功却拿不到应答对象时不解引用,按「没有应答内容」处置。
	EXPECT_EQ(Decide(RpcStatus::Ok(), nullptr, kCorrelatedRequestId), Disposition::kServiceUnavailableEnvelope);
	EXPECT_EQ(Decide(RpcStatus::Ok(), nullptr, kUncorrelatedRequestId), Disposition::kDrop);
}

TEST(SceneClientCallReplyTest, EveryRpcFailureGetsServiceUnavailableEnvelope)
{
	// 失败分支与改造前一致:无论确定未执行还是结果未知、无论请求号是否为 0,都回 1003(失败时应答为空)。
	constexpr std::array kFailures{
		RpcStatusCode::kNotSent,      RpcStatusCode::kRemoteRejected, RpcStatusCode::kRemoteFailed,
		RpcStatusCode::kReplyDropped, RpcStatusCode::kTimeout,        RpcStatusCode::kDisconnected,
		RpcStatusCode::kChannelClosed, RpcStatusCode::kInvalidResponse,
	};
	static_assert(kFailures.size() + 1 == static_cast<std::size_t>(RpcStatusCode::kCount),
				  "a new RpcStatusCode must be classified here");
	for (const auto code : kFailures)
	{
		const RpcStatus status{code};
		EXPECT_EQ(Decide(status, nullptr, kCorrelatedRequestId), Disposition::kServiceUnavailableEnvelope)
			<< RpcStatusCodeName(code);
		EXPECT_EQ(Decide(status, nullptr, kUncorrelatedRequestId), Disposition::kServiceUnavailableEnvelope)
			<< RpcStatusCodeName(code);
	}
}

// ---------------------------------------------------------------------------
// 「稍后重投」(docs/design/client-scene-request-handoff-hold.md §4.3、§4.5):scene 在派发前早退、但明确答复
// dispatch_verdict = RETRY_LATER 时,gate 不回 1003,而是把请求放回会话队头按序重投。回归对象:玩家换场景节点的
// 窗口里,发往 scene 的请求原先一律被当成普通早退回 1003,由客户端自己重发。
// ---------------------------------------------------------------------------

TEST(SceneClientCallReplyTest, RetryLaterVerdictIsRetriedInsteadOfAnswered)
{
	ProcessClientPlayerMessageResponse sent;
	sent.set_dispatch_verdict(CLIENT_MESSAGE_DISPATCH_VERDICT_RETRY_LATER);
	const auto reply = OverTheWire(sent);
	ASSERT_FALSE(reply.has_message_content());
	EXPECT_EQ(Decide(RpcStatus::Ok(), &reply, kCorrelatedRequestId), Disposition::kRetryLater);
	// 请求号为 0 的同样重投:重投不靠请求号关联,只有最终放弃时才按「没法关联」不回执。
	EXPECT_EQ(Decide(RpcStatus::Ok(), &reply, kUncorrelatedRequestId), Disposition::kRetryLater);
}

TEST(SceneClientCallReplyTest, EarlyReturnWithoutVerdictIsStillAnsweredAtOnce)
{
	// 旧版 scene 不填这个字段,新版 scene 的其余早退(请求解析失败、旧会话、身份栅栏等)也不填:等也没用,照旧回 1003。
	ProcessClientPlayerMessageResponse sent;
	sent.set_dispatch_verdict(CLIENT_MESSAGE_DISPATCH_VERDICT_UNSPECIFIED);
	const auto reply = OverTheWire(sent);
	EXPECT_EQ(Decide(RpcStatus::Ok(), &reply, kCorrelatedRequestId), Disposition::kServiceUnavailableEnvelope);
}

TEST(SceneClientCallReplyTest, DispatchedContentWinsOverRetryLaterVerdict)
{
	// 防御:应答既带内容又带「稍后重投」时,以内容为准 —— 带内容就说明 handler 跑过了,重投会让请求执行两次。
	auto sent = DispatchedReply(kCorrelatedRequestId, "body");
	sent.set_dispatch_verdict(CLIENT_MESSAGE_DISPATCH_VERDICT_RETRY_LATER);
	const auto reply = OverTheWire(sent);
	EXPECT_EQ(Decide(RpcStatus::Ok(), &reply, kCorrelatedRequestId), Disposition::kForwardContent);
}

TEST(SceneClientCallReplyTest, RpcFailureIsNeverRetried)
{
	// 结果未知的失败(超时 / 断连等)重投可能让请求执行两次;确定未执行的失败(没发出去 / 对端派发前拒绝)等也未必有用。
	// 两类都不重投,照旧回 1003,由客户端决定是否重发(有副作用的请求靠幂等键)。
	constexpr std::array kFailures{
		RpcStatusCode::kNotSent,      RpcStatusCode::kRemoteRejected, RpcStatusCode::kRemoteFailed,
		RpcStatusCode::kReplyDropped, RpcStatusCode::kTimeout,        RpcStatusCode::kDisconnected,
		RpcStatusCode::kChannelClosed, RpcStatusCode::kInvalidResponse,
	};
	for (const auto code : kFailures)
	{
		EXPECT_NE(Decide(RpcStatus{code}, nullptr, kCorrelatedRequestId), Disposition::kRetryLater)
			<< RpcStatusCodeName(code);
	}
}
