// 小游戏客户端的 WebSocket 入口的单测(libs/engine/core/network/codec/websocket.h,
// docs/design/minigame-websocket-gate.md W1、W3 ~ W5)。gate 与 battle 的客户端面共用这一层。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj。被测代码在 core.lib 里:
// 改了 websocket.cpp / codec.cpp 要先重编 core,再编这个测试工程。
//
// 连接是真实的回环 TCP(loopback_connection.h)。入站不经 socket:把「读到的字节」追加进一个缓冲再交给
// ProtobufCodec::onMessage,和 muduo 的读回调做的事一样,好处是能精确控制一次读到多少。出站在客户端侧收字节逐字节比对。
// 「没有多写」不靠等待判定:核对时先让服务端再发一帧哨兵,TCP 保序,收到哨兵时它前面的字节就是全部结果;
// 连接被关闭的用例等两端都断开再比对,断开之后不会再有字节。

#include <gtest/gtest.h>

#include <cstddef>
#include <cstdint>
#include <memory>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "handler/gate_push_delivery.h"
#include "loopback_connection.h"
#include "network/codec/codec.h"
#include "network/codec/websocket.h"
#include "proto/common/base/message.pb.h"

namespace
{
	using gate_test::DriveLoopUntil;
	using gate_test::LoopbackConnection;

	// RFC 6455 §1.3 的示例握手:这个 Key 对应这个 Accept。
	constexpr char kClientKey[] = "dGhlIHNhbXBsZSBub25jZQ==";
	constexpr char kAcceptValue[] = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=";
	// RFC 6455 §5.7 示例用的掩码。
	constexpr unsigned char kMask[4] = {0x37, 0xfa, 0x21, 0x3d};

	constexpr uint8_t kContinuation = 0x0;
	constexpr uint8_t kText = 0x1;
	constexpr uint8_t kBinary = 0x2;
	constexpr uint8_t kClose = 0x8;
	constexpr uint8_t kPing = 0x9;
	constexpr uint8_t kPong = 0xA;

	constexpr uint32_t kRequestMessageId = 900001;
	constexpr uint32_t kSentinelMessageId = 900099;

	const std::string kBadRequest = "HTTP/1.1 400 Bad Request\r\nConnection: close\r\nContent-Length: 0\r\n\r\n";

	// 边缘 nginx 转过来的升级请求的样子:头名大小写不定,Connection 是个列表,带 X-Forwarded-For。
	std::string UpgradeRequest(const std::string &version = "13")
	{
		return std::string("GET /ws/10.244.0.5/18000 HTTP/1.1\r\n"
						   "Host: 10.244.0.5:18000\r\n"
						   "upgrade: WebSocket\r\n"
						   "Connection: keep-alive, Upgrade\r\n"
						   "Sec-WebSocket-Key: ") +
			   kClientKey +
			   "\r\n"
			   "Sec-WebSocket-Version: " +
			   version +
			   "\r\n"
			   "X-Forwarded-For: 203.0.113.9\r\n"
			   "\r\n";
	}

	std::string SwitchingProtocols()
	{
		return std::string("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
						   "Sec-WebSocket-Accept: ") +
			   kAcceptValue + "\r\n\r\n";
	}

	void AppendLength(std::string &frame, std::size_t length, uint8_t maskBit)
	{
		if (length <= 125)
		{
			frame.push_back(static_cast<char>(maskBit | length));
			return;
		}
		frame.push_back(static_cast<char>(maskBit | 126));
		frame.push_back(static_cast<char>(length >> 8));
		frame.push_back(static_cast<char>(length & 0xFF));
	}

	// 客户端发出的帧。合法的客户端帧都加掩码;masked=false 只用来造违规帧。
	std::string ClientFrame(uint8_t opcode, const std::string &payload, bool fin = true, bool masked = true)
	{
		std::string frame;
		frame.push_back(static_cast<char>((fin ? 0x80 : 0x00) | opcode));
		AppendLength(frame, payload.size(), masked ? 0x80 : 0x00);
		if (!masked)
		{
			return frame + payload;
		}
		frame.append(reinterpret_cast<const char *>(kMask), sizeof(kMask));
		for (std::size_t i = 0; i < payload.size(); ++i)
		{
			frame.push_back(static_cast<char>(payload[i] ^ kMask[i & 3]));
		}
		return frame;
	}

	// 服务端发出的帧:FIN=1,不加掩码。
	std::string ServerFrame(uint8_t opcode, const std::string &payload)
	{
		std::string frame;
		frame.push_back(static_cast<char>(0x80 | opcode));
		AppendLength(frame, payload.size(), 0x00);
		return frame + payload;
	}

	std::string CloseCode(uint16_t code)
	{
		return {static_cast<char>(code >> 8), static_cast<char>(code & 0xFF)};
	}

	// 一个客户端帧的字节,与原始 TCP 上逐字节相同。
	std::string Bytes(const google::protobuf::Message &message)
	{
		muduo::net::Buffer buffer;
		ProtobufCodec::fillEmptyBuffer(&buffer, message);
		return buffer.retrieveAllAsString();
	}

	ClientRequest Request(uint64_t id)
	{
		ClientRequest request;
		request.set_id(id);
		request.set_message_id(kRequestMessageId);
		return request;
	}

	class WebSocketEntry : public ::testing::Test
	{
	protected:
		void SetUp() override
		{
			loop_ = std::make_unique<muduo::net::EventLoop>();
			before_ = websocket::GetCounters();
			Reopen();
		}

		void TearDown() override
		{
			CloseLink();
			loop_.reset();
		}

		// 换一条新连接(一个用例里要测多种违规帧时用)。
		void Reopen()
		{
			CloseLink();
			input_.retrieveAll();
			receivedIds_.clear();
			link_ = std::make_unique<LoopbackConnection>(*loop_);
			ASSERT_TRUE(link_->Open("websocket_entry"));
		}

		muduo::net::TcpConnectionPtr Conn() const { return link_->ServerSide(); }

		// 模拟 muduo 读到这些字节:追加进这条连接的输入缓冲再交给 codec,没吃完的留在缓冲里等下一次读。
		void Read(const std::string &bytes)
		{
			input_.append(bytes);
			codec_.onMessage(Conn(), &input_, muduo::Timestamp::now());
		}

		// 连接还开着:客户端至今收到的字节必须恰好是 expected。每条连接只核对一次(核对本身会多发一帧哨兵)。
		void ExpectReceived(const std::string &expected)
		{
			const auto conn = Conn();
			ASSERT_NE(conn, nullptr);
			MessageContent sentinel;
			sentinel.set_message_id(kSentinelMessageId);
			const std::string all =
				expected + (websocket::IsUpgraded(conn) ? ServerFrame(kBinary, Bytes(sentinel)) : Bytes(sentinel));
			ProtobufCodec::send(conn, sentinel);
			ASSERT_TRUE(DriveLoopUntil(*loop_, [this, &all] { return link_->ClientReceived().size() >= all.size(); }));
			EXPECT_EQ(link_->ClientReceived(), all);
		}

		// 连接被服务端关掉:等两端都断开,客户端收到的全部字节必须恰好是 expected。
		void ExpectClosedAfter(const std::string &expected)
		{
			ASSERT_TRUE(DriveLoopUntil(*loop_, [this] { return link_->BothSidesDown(); }));
			EXPECT_EQ(link_->ClientReceived(), expected);
		}

		const websocket::Counters &Before() const { return before_; }
		const std::vector<uint64_t> &ReceivedIds() const { return receivedIds_; }
		std::size_t UnreadInput() const { return input_.readableBytes(); }

	private:
		void CloseLink()
		{
			if (!link_)
			{
				return;
			}
			// 生产里由 gate / battle 的连接回调在断开时调。
			if (const auto conn = Conn())
			{
				websocket::Forget(conn);
			}
			link_.reset();
		}

		std::unique_ptr<muduo::net::EventLoop> loop_;
		std::unique_ptr<LoopbackConnection> link_;
		muduo::net::Buffer input_;
		std::vector<uint64_t> receivedIds_;
		websocket::Counters before_;
		ProtobufCodec codec_{[this](const muduo::net::TcpConnectionPtr &, const MessagePtr &message, muduo::Timestamp) {
			const auto request = std::dynamic_pointer_cast<ClientRequest>(message);
			ASSERT_NE(request, nullptr);
			receivedIds_.push_back(request->id());
		}};
	};
} // namespace

// ── 纯函数 ──────────────────────────────────────────────────────────

TEST(WebSocketHandshake, AcceptKeyMatchesTheRfc6455Example)
{
	EXPECT_EQ(websocket::AcceptKey(kClientKey), kAcceptValue);
}

TEST(WebSocketHandshake, UpgradeRequestIsAcceptedAndOnlyTheHeadIsConsumed)
{
	const std::string request = UpgradeRequest();
	std::size_t consumed = 0;
	std::string response;
	// 请求头后面紧跟的字节是第一个帧,不能被握手吃掉。
	EXPECT_EQ(websocket::ParseHandshake(request + "\x82\x80", consumed, response), websocket::Handshake::kAccepted);
	EXPECT_EQ(consumed, request.size());
	EXPECT_EQ(response, SwitchingProtocols());
}

TEST(WebSocketHandshake, WaitsUntilTheBlankLineArrives)
{
	const std::string request = UpgradeRequest();
	std::size_t consumed = 0;
	std::string response;
	for (const std::size_t length : {std::size_t{1}, std::size_t{3}, std::size_t{40}, request.size() - 1})
	{
		EXPECT_EQ(websocket::ParseHandshake(request.substr(0, length), consumed, response), websocket::Handshake::kIncomplete)
			<< length;
		EXPECT_TRUE(response.empty()) << length;
	}
}

TEST(WebSocketHandshake, RequestsThatAreNotValidUpgradesAreRejected)
{
	const std::string key = std::string("Sec-WebSocket-Key: ") + kClientKey + "\r\n";
	const std::string upgrade = "Upgrade: websocket\r\nConnection: Upgrade\r\n";
	const std::string version = "Sec-WebSocket-Version: 13\r\n";
	const std::vector<std::pair<std::string_view, std::string>> cases = {
		{"not GET, rejected before the head is complete", "POST /ws"},
		{"plain HTTP request", "GET /healthz HTTP/1.1\r\nHost: gate\r\n\r\n"},
		{"HTTP/1.0", "GET /ws HTTP/1.0\r\n" + upgrade + key + version + "\r\n"},
		{"no Upgrade header", "GET /ws HTTP/1.1\r\nConnection: Upgrade\r\n" + key + version + "\r\n"},
		{"Connection without upgrade", "GET /ws HTTP/1.1\r\nUpgrade: websocket\r\nConnection: keep-alive\r\n" + key + version + "\r\n"},
		{"no key", "GET /ws HTTP/1.1\r\n" + upgrade + version + "\r\n"},
		{"key is not 16 bytes of base64", "GET /ws HTTP/1.1\r\n" + upgrade + "Sec-WebSocket-Key: c2hvcnQ=\r\n" + version + "\r\n"},
		{"head larger than the limit", "GET /ws HTTP/1.1\r\nX-Pad: " + std::string(websocket::kMaxHandshakeBytes, 'a')},
	};
	for (const auto &[name, request] : cases)
	{
		std::size_t consumed = 99;
		std::string response;
		EXPECT_EQ(websocket::ParseHandshake(request, consumed, response), websocket::Handshake::kRejected) << name;
		EXPECT_EQ(response, kBadRequest) << name;
		EXPECT_EQ(consumed, 0u) << name;
	}

	// 版本不是 13:按 RFC 6455 §4.4 回 426,并告诉对端本端支持的版本。
	std::size_t consumed = 0;
	std::string response;
	EXPECT_EQ(websocket::ParseHandshake(UpgradeRequest("8"), consumed, response), websocket::Handshake::kRejected);
	EXPECT_EQ(response.rfind("HTTP/1.1 426 ", 0), 0u) << response;
	EXPECT_NE(response.find("Sec-WebSocket-Version: 13\r\n"), std::string::npos) << response;
}

TEST(WebSocketFrame, BinaryHeaderUsesTheShortestLengthForm)
{
	char header[10];
	ASSERT_EQ(websocket::EncodeBinaryHeader(0, header), 2u);
	EXPECT_EQ(std::string(header, 2), std::string("\x82\x00", 2));
	ASSERT_EQ(websocket::EncodeBinaryHeader(125, header), 2u);
	EXPECT_EQ(std::string(header, 2), std::string("\x82\x7d", 2));
	ASSERT_EQ(websocket::EncodeBinaryHeader(126, header), 4u);
	EXPECT_EQ(std::string(header, 4), std::string("\x82\x7e\x00\x7e", 4));
	ASSERT_EQ(websocket::EncodeBinaryHeader(65535, header), 4u);
	EXPECT_EQ(std::string(header, 4), std::string("\x82\x7e\xff\xff", 4));
	ASSERT_EQ(websocket::EncodeBinaryHeader(65536, header), 10u);
	EXPECT_EQ(std::string(header, 10), std::string("\x82\x7f\x00\x00\x00\x00\x00\x01\x00\x00", 10));
}

TEST(WebSocketFrame, FrameKeepsThePayloadForBothHeaderPlacements)
{
	// 小帧:帧头放进缓冲的预留区。
	const std::string smallPayload = Bytes(Request(1));
	muduo::net::Buffer smallFrame;
	ProtobufCodec::fillEmptyBuffer(&smallFrame, Request(1));
	websocket::Frame(&smallFrame);
	EXPECT_EQ(smallFrame.retrieveAllAsString(), ServerFrame(kBinary, smallPayload));

	// 超过 65535 字节:帧头 10 字节,预留区放不下,换一块缓冲重排。
	const std::string large(70000, 'x');
	muduo::net::Buffer largeFrame;
	largeFrame.append(large);
	websocket::Frame(&largeFrame);
	EXPECT_EQ(largeFrame.retrieveAllAsString(), std::string("\x82\x7f\x00\x00\x00\x00\x00\x01\x11\x70", 10) + large);
}

// ── 连接 ────────────────────────────────────────────────────────────

// 升级之后,上下行承载的就是原来的客户端帧:上行去掉帧头与掩码交给 codec,下行每帧包成一条二进制消息。
// 广播用的 EncodedFrame 对 WebSocket 连接多一个帧头,它自己那份原始字节不变(TCP 连接还要用)。
TEST_F(WebSocketEntry, UpgradedConnectionCarriesClientFramesBothWays)
{
	Read(UpgradeRequest());
	ASSERT_TRUE(websocket::IsUpgraded(Conn()));
	EXPECT_EQ(UnreadInput(), 0u);

	Read(ClientFrame(kBinary, Bytes(Request(7))));
	EXPECT_EQ(ReceivedIds(), (std::vector<uint64_t>{7}));

	const ClientRequest reply = Request(8);
	ProtobufCodec::send(Conn(), reply);
	MessageContent broadcast;
	broadcast.set_message_id(kRequestMessageId);
	const gate_push::EncodedFrame encoded(broadcast);
	encoded.SendTo(Conn());

	ExpectReceived(SwitchingProtocols() + ServerFrame(kBinary, Bytes(reply)) + ServerFrame(kBinary, Bytes(broadcast)));
	EXPECT_EQ(std::string(encoded.data(), encoded.size()), Bytes(broadcast));
	EXPECT_EQ(websocket::GetCounters().accepted, Before().accepted + 1);
	EXPECT_EQ(websocket::GetCounters().rejected, Before().rejected);
}

// 握手、帧头、掩码、载荷被切在任意位置都能接上;一个客户端帧拆成两个 WebSocket 帧(中间夹一个 Ping)、
// 两个客户端帧装进一条消息,都按字节流还原。Ping 只回 Pong,不算业务消息。
TEST_F(WebSocketEntry, SplitReadsFragmentsAndBatchedFramesAreReassembled)
{
	const std::string request = UpgradeRequest();
	Read(request.substr(0, 40));
	EXPECT_FALSE(websocket::IsUpgraded(Conn()));
	EXPECT_EQ(UnreadInput(), 40u) << "an incomplete upgrade request stays in the input buffer";
	Read(request.substr(40));
	ASSERT_TRUE(websocket::IsUpgraded(Conn()));

	const std::string first = Bytes(Request(1));
	const std::string wire = ClientFrame(kBinary, first.substr(0, 5), /*fin=*/false) + ClientFrame(kPing, "hb") +
							 ClientFrame(kContinuation, first.substr(5)) +
							 ClientFrame(kBinary, Bytes(Request(2)) + Bytes(Request(3)));
	for (std::size_t offset = 0; offset < wire.size(); offset += 3)
	{
		Read(wire.substr(offset, 3));
	}

	EXPECT_EQ(ReceivedIds(), (std::vector<uint64_t>{1, 2, 3}));
	EXPECT_EQ(UnreadInput(), 0u);
	ExpectReceived(SwitchingProtocols() + ServerFrame(kPong, "hb"));
}

// 与 TCP「先处理数据、后处理 FIN」同序:排在 Close 之前的请求照常交给上层,然后回显 Close 并关闭;Close 之后的帧不处理。
TEST_F(WebSocketEntry, DataBeforeCloseIsDeliveredThenCloseIsEchoed)
{
	Read(UpgradeRequest() + ClientFrame(kBinary, Bytes(Request(9))) + ClientFrame(kClose, CloseCode(1000)) +
		 ClientFrame(kBinary, Bytes(Request(10))));

	EXPECT_EQ(ReceivedIds(), (std::vector<uint64_t>{9}));
	EXPECT_EQ(UnreadInput(), 0u);
	ExpectClosedAfter(SwitchingProtocols() + ServerFrame(kClose, CloseCode(1000)));
	EXPECT_EQ(websocket::GetCounters().protocolClosed, Before().protocolClosed) << "a clean close is not a protocol error";
}

// 不合协议的帧:回带对应关闭码的 Close 并关闭,帧里的数据不交给上层。
TEST_F(WebSocketEntry, ProtocolViolationsAreClosedWithTheMatchingCode)
{
	struct Violation
	{
		std::string_view name; // 不用 const char *:成员裸指针过不了 no-raw-pointer-member 门禁
		std::string wire;
		uint16_t closeCode;
	};
	// 声明 70000 字节载荷的帧头(127 形式的 8 字节长度 + 掩码),超过客户端帧上限;不用真的带载荷,看到帧头就拒。
	const std::string oversized = std::string("\x82\xff\x00\x00\x00\x00\x00\x01\x11\x70", 10) +
								  std::string(reinterpret_cast<const char *>(kMask), sizeof(kMask));
	const std::vector<Violation> violations = {
		{"unmasked client frame", ClientFrame(kBinary, Bytes(Request(1)), true, /*masked=*/false), websocket::kCloseProtocolError},
		{"text frame", ClientFrame(kText, "Hello"), websocket::kCloseUnsupportedData},
		{"frame larger than a client frame", oversized, websocket::kCloseMessageTooBig},
		{"continuation without a started message", ClientFrame(kContinuation, Bytes(Request(1))), websocket::kCloseProtocolError},
		{"new message inside an unfinished one",
		 ClientFrame(kBinary, "ab", /*fin=*/false) + ClientFrame(kBinary, "cd"), websocket::kCloseProtocolError},
		{"fragmented control frame", ClientFrame(kPing, "hb", /*fin=*/false), websocket::kCloseProtocolError},
		{"reserved bit set", std::string("\xc2", 1) + ClientFrame(kBinary, "ab").substr(1), websocket::kCloseProtocolError},
		{"reserved opcode", ClientFrame(0x3, "ab"), websocket::kCloseProtocolError},
	};
	for (const Violation &violation : violations)
	{
		SCOPED_TRACE(violation.name);
		Reopen();
		Read(UpgradeRequest() + violation.wire);
		EXPECT_TRUE(ReceivedIds().empty());
		ExpectClosedAfter(SwitchingProtocols() + ServerFrame(kClose, CloseCode(violation.closeCode)));
	}
	EXPECT_EQ(websocket::GetCounters().protocolClosed, Before().protocolClosed + violations.size());
}

// 首字节是 'G' 但不是合法的升级请求:回 HTTP 400 并关闭,不当成 WebSocket。
TEST_F(WebSocketEntry, RequestThatIsNotAnUpgradeGetsAnHttpErrorAndIsClosed)
{
	Read("GET /healthz HTTP/1.1\r\nHost: gate\r\n\r\n");

	EXPECT_FALSE(websocket::IsUpgraded(Conn()));
	EXPECT_TRUE(ReceivedIds().empty());
	ExpectClosedAfter(kBadRequest);
	EXPECT_EQ(websocket::GetCounters().rejected, Before().rejected + 1);
	EXPECT_EQ(websocket::GetCounters().accepted, Before().accepted);
}

// 原生客户端(原始 TCP)不受影响:上下行都没有 WebSocket 帧头。
TEST_F(WebSocketEntry, RawTcpClientsAreUntouched)
{
	Read(Bytes(Request(5)) + Bytes(Request(6)));

	EXPECT_FALSE(websocket::IsUpgraded(Conn()));
	EXPECT_EQ(ReceivedIds(), (std::vector<uint64_t>{5, 6}));
	const ClientRequest reply = Request(7);
	ProtobufCodec::send(Conn(), reply);
	ExpectReceived(Bytes(reply));
	EXPECT_EQ(websocket::GetCounters().accepted, Before().accepted);
}
