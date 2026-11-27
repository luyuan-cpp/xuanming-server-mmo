// 验签之前的单帧上限(ProtobufCodec::setPreAuthMessageLimit,docs/design/gate-connection-admission-control.md G10)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj。codec 要求连接处于已连接状态,
// 所以用回环 TCP 夹具(loopback_connection.h)拿一条真实的服务端连接,直接把拼好的字节交给 codec.onMessage。
// 被锁定的契约:
//   * 未验签的连接声明一个超过上限的帧:看到长度头就按 kInvalidLength 拒,不等整帧到齐;
//   * 已验签的连接、以及没设上限的 codec:同样的长度头照常等后面的字节;
//   * 验签请求本身远小于上限,未验签的连接照常收得到;
//   * gate 的判据 IsVerifiedConnection 跟着会话表走。

#include <gtest/gtest.h>

#include <memory>
#include <string>
#include <vector>

#include "handler/rpc/connection_admission.h"
#include "loopback_connection.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "network/accept_admission.h"
#include "network/codec/codec.h"
#include "proto/common/base/message.pb.h"
#include "session/manager/session_manager.h"

namespace
{

struct CodecProbe
{
	int messages = 0;
	std::vector<ProtobufCodec::ErrorCode> errors;
};

// 错误回调只记下错误码、清掉缓冲,不关连接(默认回调会 forceClose,这里要看清是哪一种错)。回调只捕获 weak。
std::unique_ptr<ProtobufCodec> MakeCodec(const std::shared_ptr<CodecProbe> &probe)
{
	const std::weak_ptr<CodecProbe> weak(probe);
	return std::make_unique<ProtobufCodec>(
		[weak](const muduo::net::TcpConnectionPtr &, const MessagePtr &, muduo::Timestamp)
		{
			if (const auto p = weak.lock())
			{
				++p->messages;
			}
		},
		[weak](const muduo::net::TcpConnectionPtr &, muduo::net::Buffer *buf, muduo::Timestamp,
			   ProtobufCodec::ErrorCode code)
		{
			buf->retrieveAll();
			if (const auto p = weak.lock())
			{
				p->errors.push_back(code);
			}
		});
}

// 一个大帧的开头:长度头声明 declaredLen 字节,后面只到了一小段(够 codec 读长度头,远不够整帧)。
void AppendLargeFrameStart(muduo::net::Buffer &buf, int32_t declaredLen)
{
	buf.appendInt32(declaredLen);
	buf.append(std::string(16, 'x'));
}

// 一条按真实形状填满字段的验签请求(票据带 32 字节的会话密钥,签名 32 字节)。
ClientTokenVerifyRequest RealisticVerifyRequest()
{
	GateTokenPayload payload;
	payload.set_gate_node_id(32767);
	payload.set_zone_id(4000000000u);
	payload.set_expire_timestamp(4102444800);
	payload.set_hmac_session_key(std::string(32, 'k'));
	ClientTokenVerifyRequest request;
	request.set_payload(payload.SerializeAsString());
	request.set_signature(std::string(32, 's'));
	return request;
}

class PreAuthMessageLimit : public ::testing::Test
{
protected:
	void SetUp() override
	{
		ASSERT_TRUE(connection_.Open("pre_auth_message_limit"));
		tlsSessionManager.sessions().clear();
	}
	void TearDown() override { tlsSessionManager.sessions().clear(); }

	muduo::net::TcpConnectionPtr ServerSide() const { return connection_.ServerSide(); }

	// 测试线程上的 loop;回环夹具在它上面建连与拆除。
	muduo::net::EventLoop loop_;
	gate_test::LoopbackConnection connection_{loop_};
	const std::shared_ptr<CodecProbe> probe_ = std::make_shared<CodecProbe>();
	// 判据由用例控制;回调按值捕获它的 shared_ptr。
	const std::shared_ptr<bool> authenticated_ = std::make_shared<bool>(false);
};

TEST_F(PreAuthMessageLimit, UnverifiedConnectionIsCutOffAtTheLengthHeaderOfALargeFrame)
{
	const auto codec = MakeCodec(probe_);
	codec->setPreAuthMessageLimit(accept_admission::kPreAuthMaxMessageBytes,
								  [flag = authenticated_](const muduo::net::TcpConnectionPtr &) { return *flag; });
	muduo::net::Buffer buf;
	AppendLargeFrameStart(buf, accept_admission::kPreAuthMaxMessageBytes + 1);

	codec->onMessage(ServerSide(), &buf, muduo::Timestamp::now());

	ASSERT_EQ(probe_->errors.size(), 1u);
	EXPECT_EQ(probe_->errors.front(), ProtobufCodec::kInvalidLength);
	EXPECT_EQ(probe_->messages, 0);
}

TEST_F(PreAuthMessageLimit, VerifiedConnectionWaitsForTheRestOfALargeFrame)
{
	const auto codec = MakeCodec(probe_);
	codec->setPreAuthMessageLimit(accept_admission::kPreAuthMaxMessageBytes,
								  [flag = authenticated_](const muduo::net::TcpConnectionPtr &) { return *flag; });
	*authenticated_ = true;
	muduo::net::Buffer buf;
	AppendLargeFrameStart(buf, accept_admission::kPreAuthMaxMessageBytes + 1);

	codec->onMessage(ServerSide(), &buf, muduo::Timestamp::now());

	EXPECT_TRUE(probe_->errors.empty()) << "a verified connection may send frames up to maxMessageLen";
	EXPECT_EQ(probe_->messages, 0) << "the frame is incomplete";
}

TEST_F(PreAuthMessageLimit, WithoutTheLimitTheCodecKeepsItsOldBehavior)
{
	const auto codec = MakeCodec(probe_);
	muduo::net::Buffer buf;
	AppendLargeFrameStart(buf, accept_admission::kPreAuthMaxMessageBytes + 1);

	codec->onMessage(ServerSide(), &buf, muduo::Timestamp::now());

	EXPECT_TRUE(probe_->errors.empty());
}

TEST_F(PreAuthMessageLimit, HandshakeRequestFitsWellUnderTheLimit)
{
	const auto codec = MakeCodec(probe_);
	codec->setPreAuthMessageLimit(accept_admission::kPreAuthMaxMessageBytes,
								  [flag = authenticated_](const muduo::net::TcpConnectionPtr &) { return *flag; });
	muduo::net::Buffer buf;
	ProtobufCodec::fillEmptyBuffer(&buf, RealisticVerifyRequest());
	// 设计稿与 accept_admission.h 写的余量:验签请求编码后不到 300 字节,上限是它的十倍以上。
	EXPECT_LT(buf.readableBytes() * 10, static_cast<size_t>(accept_admission::kPreAuthMaxMessageBytes));

	codec->onMessage(ServerSide(), &buf, muduo::Timestamp::now());

	EXPECT_TRUE(probe_->errors.empty());
	EXPECT_EQ(probe_->messages, 1);
}

TEST_F(PreAuthMessageLimit, GateVerdictFollowsTheSessionTable)
{
	const auto conn = ServerSide();
	ASSERT_TRUE(conn);
	EXPECT_FALSE(gate_connection_admission::IsVerifiedConnection(conn)) << "no context yet";

	conn->setContext(kInvalidSessionId);
	EXPECT_FALSE(gate_connection_admission::IsVerifiedConnection(conn)) << "refused at connect";

	constexpr SessionId kSessionId = 7;
	conn->setContext(kSessionId);
	EXPECT_FALSE(gate_connection_admission::IsVerifiedConnection(conn)) << "context without a session";

	tlsSessionManager.sessions()[kSessionId].verified = false;
	EXPECT_FALSE(gate_connection_admission::IsVerifiedConnection(conn));

	tlsSessionManager.sessions()[kSessionId].verified = true;
	EXPECT_TRUE(gate_connection_admission::IsVerifiedConnection(conn));

	// 拆除前把上下文清掉:断开回调不归本用例管,别让夹具的连接带着会话号走。
	conn->setContext(boost::any());
}

} // namespace
