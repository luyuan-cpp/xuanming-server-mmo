// ClientSession(纯协议引擎)单测:与内存里的 nghttp2 服务端会话(TestH2Peer)对接,完全确定性。
// 覆盖 docs/design/grpc-muduo-transport.md §4 的线上契约:请求头与请求体、响应 / trailers / trailers-only、
// 状态合成规则、RST_STREAM 与 GOAWAY、接收上限、本端 RST、PING、断线时的 unprocessed 判定。
//
// 只走公开接口:ClientSession 的方法 + Observer 回调;服务端行为由 TestH2Peer 按脚本给出。

#include <gtest/gtest.h>

#include <cstdint>
#include <map>
#include <memory>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "muduo_grpc/client_session.h"
#include "muduo_grpc/message_framing.h"
#include "muduo_grpc/metadata.h"
#include "muduo_grpc/status.h"

#include "test_h2_peer.h"

namespace
{

using muduo_grpc::ClientSession;
using muduo_grpc::ClientSessionOptions;
using muduo_grpc::Metadata;
using muduo_grpc::Status;
using muduo_grpc::StatusCode;
using muduo_grpc::StreamOutcome;
using muduo_grpc::UnaryRequestSpec;
using muduo_grpc_test::HeaderList;
using muduo_grpc_test::SettingEntry;
using muduo_grpc_test::TestH2Peer;
using muduo_grpc_test::TestH2PeerOptions;
namespace h2 = muduo_grpc_test::h2;

constexpr char kPath[] = "/client_rpc_router.ClientRpcRouter/Forward";
constexpr char kAuthority[] = "127.0.0.1:50600";
constexpr char kUserAgent[] = "muduo-grpc-test/1.0";
// gate 的 x-session-detail-bin 应用层值(go/client_rpc_router/internal/logic/forwardlogic_test.go 的样例)
// 与它在线上的样子(按 -bin 规则再编一层,不带 padding)。
constexpr char kSessionAppValue[] = "CgQIARABEgIIAQ==";
constexpr char kSessionWireValue[] = "Q2dRSUFSQUJFZ0lJQVE9PQ";

struct GoawayEvent
{
    int32_t lastStreamId{0};
    uint32_t errorCode{0};
    std::string debugData;
};

// 记录 Observer 的全部事件。只记状态:按头文件约定,回调里不销毁会话、不同步提交新流。
class RecordingObserver : public ClientSession::Observer
{
public:
    void OnSessionReady() override { ++readyCount; }
    void OnGoawayReceived(int32_t lastStreamId, uint32_t errorCode, std::string_view debugData) override
    {
        goaways.push_back(GoawayEvent{lastStreamId, errorCode, std::string(debugData)});
    }
    void OnFrameReceived(uint8_t frameType) override { framesReceived.push_back(frameType); }
    void OnPingAck() override { ++pingAcks; }
    void OnStreamClosed(int32_t streamId, StreamOutcome outcome) override
    {
        ++closeCounts[streamId];
        outcomes[streamId] = std::move(outcome);
    }

    bool Closed(int32_t streamId) const { return outcomes.count(streamId) != 0; }

    int readyCount{0};
    int pingAcks{0};
    std::vector<GoawayEvent> goaways;
    std::vector<uint8_t> framesReceived;
    std::map<int32_t, StreamOutcome> outcomes;
    std::map<int32_t, int> closeCounts;
};

ClientSessionOptions MakeOptions()
{
    ClientSessionOptions options;
    options.authority = kAuthority;
    options.userAgent = kUserAgent;
    return options;
}

UnaryRequestSpec MakeSpec(std::string_view payload, Metadata metadata = {}, std::string grpcTimeout = "800000u")
{
    UnaryRequestSpec spec;
    spec.path = kPath;
    auto framed = std::make_shared<std::string>();
    muduo_grpc::AppendLengthPrefixedMessage(*framed, payload);
    spec.framedMessage = std::move(framed);
    spec.metadata = std::move(metadata);
    spec.grpcTimeout = std::move(grpcTimeout);
    return spec;
}

HeaderList GrpcResponseHeaders(const HeaderList &extra = {})
{
    HeaderList headers{{":status", "200"}, {"content-type", "application/grpc"}};
    headers.insert(headers.end(), extra.begin(), extra.end());
    return headers;
}

// 一组「观察者 + 内存服务端 + 被测会话」。成员顺序即构造顺序:会话最后构造、最先析构
// (析构只释放 nghttp2 会话,不再回调观察者)。
struct SessionHarness
{
    explicit SessionHarness(ClientSessionOptions options = MakeOptions(), TestH2PeerOptions peerOptions = {})
        : peer(std::move(peerOptions)), client(std::move(options), observer)
    {
    }

    // Start + 握手(收到服务端 SETTINGS)。
    ::testing::AssertionResult Handshake()
    {
        const Status started = client.Start();
        if (!started.ok())
        {
            return ::testing::AssertionFailure() << "Start: " << started.ToString();
        }
        if (!peer.Pump(client))
        {
            return ::testing::AssertionFailure() << "Pump: " << peer.LastError();
        }
        if (observer.readyCount != 1)
        {
            return ::testing::AssertionFailure() << "readyCount=" << observer.readyCount;
        }
        return ::testing::AssertionSuccess();
    }

    RecordingObserver observer;
    TestH2Peer peer;
    ClientSession client;
};

class ClientSessionTest : public ::testing::Test
{
protected:
    void SetUp() override { ASSERT_TRUE(h.Handshake()); }

    // 提交一个请求(不发送)。
    int32_t Submit(std::string_view payload, Metadata metadata = {})
    {
        const auto result = h.client.SubmitUnary(MakeSpec(payload, std::move(metadata)));
        EXPECT_GT(result.streamId, 0) << result.error.ToString();
        return result.streamId;
    }

    // 提交并把请求完整送到服务端。
    int32_t SubmitAndDeliver(std::string_view payload, Metadata metadata = {})
    {
        const int32_t streamId = Submit(payload, std::move(metadata));
        EXPECT_TRUE(h.peer.Pump(h.client)) << h.peer.LastError();
        const auto *stream = h.peer.FindStream(streamId);
        EXPECT_TRUE(stream != nullptr && stream->requestEnded) << "request not delivered, stream " << streamId;
        return streamId;
    }

    ::testing::AssertionResult Pump()
    {
        if (h.peer.Pump(h.client))
        {
            return ::testing::AssertionSuccess();
        }
        return ::testing::AssertionFailure() << h.peer.LastError();
    }

    const StreamOutcome &OutcomeOf(int32_t streamId)
    {
        static const StreamOutcome kMissing{};
        const auto it = h.observer.outcomes.find(streamId);
        if (it == h.observer.outcomes.end())
        {
            ADD_FAILURE() << "stream " << streamId << " has not been closed";
            return kMissing;
        }
        EXPECT_EQ(h.observer.closeCounts[streamId], 1) << "OnStreamClosed must fire exactly once";
        return it->second;
    }

    SessionHarness h;
};

// ── 握手 ────────────────────────────────────────────────────────────────

TEST(ClientSessionHandshake, ReadyOnlyAfterPeerSettings)
{
    TestH2PeerOptions peerOptions;
    peerOptions.sendInitialSettings = false;
    SessionHarness h(MakeOptions(), peerOptions);
    ASSERT_TRUE(h.client.Start().ok());
    std::string preface;
    ASSERT_TRUE(h.client
                    .DrainOutput([&preface](std::string_view chunk)
                                 {
                                     preface.append(chunk.data(), chunk.size());
                                     return true;
                                 })
                    .ok());
    ASSERT_TRUE(h.peer.Receive(preface)) << h.peer.LastError();
    // 服务端此刻只产出对我方 SETTINGS 的 ACK。RFC 9113 §3.4 与 nghttp2 的严格前言要求对端首帧是非 ACK 的
    // SETTINGS(先收到 ACK 会被当成协议违约直接终止会话),所以先扣住这帧 ACK,不回灌给客户端。
    const std::string settingsAck = h.peer.TakeOutput();
    EXPECT_EQ(h.observer.readyCount, 0); // 没收到对端 SETTINGS,不算 READY

    // 本端 SETTINGS 与连接级窗口(§4.7):ENABLE_PUSH=0、INITIAL_WINDOW_SIZE=1MiB、MAX_HEADER_LIST_SIZE=16KiB,
    // 连接级接收窗口放大到 16MiB。
    EXPECT_EQ(h.peer.SettingsFramesReceived(), 1u);
    const std::vector<SettingEntry> expected = {
        {h2::kSettingsEnablePush, 0},
        {h2::kSettingsInitialWindowSize, 1024 * 1024},
        {h2::kSettingsMaxHeaderListSize, 16 * 1024},
    };
    EXPECT_TRUE(h.peer.LastPeerSettings() == expected);
    EXPECT_EQ(h.peer.RemoteConnectionWindow(), 16 * 1024 * 1024);

    ASSERT_TRUE(h.peer.SendSettings({{h2::kSettingsMaxConcurrentStreams, 100}}));
    ASSERT_TRUE(h.peer.Pump(h.client)) << h.peer.LastError(); // 先交付服务端 SETTINGS
    EXPECT_EQ(h.observer.readyCount, 1);
    // 再交付扣住的 ACK:握手已完成,ACK 是合法帧,会话不应被终止。
    ASSERT_TRUE(h.client.Receive(reinterpret_cast<const uint8_t *>(settingsAck.data()), settingsAck.size()).ok());
    EXPECT_FALSE(h.client.ShouldClose());
    bool sawSettings = false;
    for (const uint8_t type : h.observer.framesReceived)
    {
        sawSettings = sawSettings || type == h2::kFrameSettings;
    }
    EXPECT_TRUE(sawSettings) << "OnFrameReceived 应报告 SETTINGS 帧";

    // 之后的 SETTINGS 不再重复报 ready。
    ASSERT_TRUE(h.peer.SendSettings({}));
    ASSERT_TRUE(h.peer.Pump(h.client)) << h.peer.LastError();
    EXPECT_EQ(h.observer.readyCount, 1);
}

TEST(ClientSessionHandshake, SubmitBeforeStartFails)
{
    SessionHarness h;
    const auto result = h.client.SubmitUnary(MakeSpec("x"));
    EXPECT_LE(result.streamId, 0);
    EXPECT_EQ(result.error.Code(), StatusCode::kInternal);
    EXPECT_FALSE(result.retryOnNewConnection);
}

// ── 请求 ────────────────────────────────────────────────────────────────

TEST_F(ClientSessionTest, RequestHeadersFollowWireContract)
{
    const int32_t id = SubmitAndDeliver("ping", {{"x-session-detail-bin", kSessionAppValue}, {"x-trace", "abc"}});
    const auto *stream = h.peer.FindStream(id);
    ASSERT_NE(stream, nullptr);

    // §4.1 的头部顺序;键全小写;不带 connection 等连接级头。
    const HeaderList expected = {
        {":method", "POST"},
        {":scheme", "http"},
        {":path", kPath},
        {":authority", kAuthority},
        {"te", "trailers"},
        {"grpc-timeout", "800000u"},
        {"content-type", "application/grpc"},
        {"user-agent", kUserAgent},
        {"x-session-detail-bin", kSessionWireValue},
        {"x-trace", "abc"},
    };
    EXPECT_EQ(stream->headers, expected);

    // -bin 的线上值 == Base64EncodeUnpadded(应用层值):传输层只编一层。
    const std::string *session = stream->FindHeader("x-session-detail-bin");
    ASSERT_NE(session, nullptr);
    EXPECT_EQ(*session, muduo_grpc::Base64EncodeUnpadded(kSessionAppValue));

    // 不发编码协商头:Go 只在请求带了已注册编码时才压缩响应(§4.1)。
    EXPECT_EQ(stream->FindHeader("grpc-encoding"), nullptr);
    EXPECT_EQ(stream->FindHeader("grpc-accept-encoding"), nullptr);
}

TEST_F(ClientSessionTest, EmptyGrpcTimeoutIsNotSent)
{
    const auto result = h.client.SubmitUnary(MakeSpec("x", {}, ""));
    ASSERT_GT(result.streamId, 0);
    ASSERT_TRUE(Pump());
    const auto *stream = h.peer.FindStream(result.streamId);
    ASSERT_NE(stream, nullptr);
    EXPECT_EQ(stream->CountHeader("grpc-timeout"), 0u);
}

TEST_F(ClientSessionTest, RequestBodyIsPrefixedAndEndsStream)
{
    const int32_t id = SubmitAndDeliver("ping");
    const auto *stream = h.peer.FindStream(id);
    ASSERT_NE(stream, nullptr);
    EXPECT_EQ(stream->body, std::string("\x00\x00\x00\x00\x04" "ping", 9));
    EXPECT_TRUE(stream->requestEnded) << "请求体发完即 END_STREAM(unary 半关闭)";

    const int32_t emptyId = SubmitAndDeliver("");
    const auto *emptyStream = h.peer.FindStream(emptyId);
    ASSERT_NE(emptyStream, nullptr);
    EXPECT_EQ(emptyStream->body, std::string(5, '\0')) << "空消息也有 5 字节前缀";
    EXPECT_TRUE(emptyStream->requestEnded);
}

TEST_F(ClientSessionTest, LargeRequestBodySpansFramesAndFlowControlWindows)
{
    // 200KiB:超过 16KiB 帧上限,也超过服务端默认 64KiB 流窗口(要靠 WINDOW_UPDATE 续发)。
    std::string payload(200 * 1024, '\0');
    for (size_t i = 0; i < payload.size(); ++i)
    {
        payload[i] = static_cast<char>(i * 31 % 251);
    }
    const int32_t id = SubmitAndDeliver(payload);
    const auto *stream = h.peer.FindStream(id);
    ASSERT_NE(stream, nullptr);
    ASSERT_EQ(stream->body.size(), payload.size() + 5);
    EXPECT_EQ(stream->body.substr(5), payload);
}

// ── 正常响应 ────────────────────────────────────────────────────────────

TEST_F(ClientSessionTest, NormalResponseDecodesExactlyOneBinaryLayer)
{
    const int32_t id = SubmitAndDeliver("ping", {{"x-session-detail-bin", kSessionAppValue}});
    // 路由服 grpc.SetHeader(B) → 线上 RawStd(B):回程同样只有一层。
    ASSERT_TRUE(h.peer.SendUnaryOk(id, "pong", {{"x-session-detail-bin", kSessionWireValue}, {"x-echo", "hi"}},
                                    {{"x-trailer", "t1"}}))
        << h.peer.LastError();
    ASSERT_TRUE(Pump());

    const StreamOutcome &outcome = OutcomeOf(id);
    EXPECT_TRUE(outcome.status.ok()) << outcome.status.ToString();
    EXPECT_EQ(outcome.response, "pong");
    const Metadata expectedInitial = {{"x-session-detail-bin", kSessionAppValue}, {"x-echo", "hi"}};
    EXPECT_EQ(outcome.initialMetadata, expectedInitial);
    const Metadata expectedTrailing = {{"x-trailer", "t1"}};
    EXPECT_EQ(outcome.trailingMetadata, expectedTrailing);
    EXPECT_FALSE(outcome.unprocessed);
    EXPECT_EQ(h.client.ActiveStreamCount(), 0u);
}

TEST_F(ClientSessionTest, EmptyResponseMessage)
{
    const int32_t id = SubmitAndDeliver("ping");
    ASSERT_TRUE(h.peer.SendUnaryOk(id, "")) << h.peer.LastError();
    ASSERT_TRUE(Pump());
    const StreamOutcome &outcome = OutcomeOf(id);
    EXPECT_TRUE(outcome.status.ok()) << outcome.status.ToString();
    EXPECT_TRUE(outcome.response.empty());
}

TEST_F(ClientSessionTest, ResponseMessageSplitAcrossDataFrames)
{
    const int32_t id = SubmitAndDeliver("ping");
    const std::string framed = TestH2Peer::Frame("split-response");
    ASSERT_TRUE(h.peer.SendHeaders(id, GrpcResponseHeaders(), false));
    // 切在前缀中间、前缀与消息体之间、消息体中间。
    ASSERT_TRUE(h.peer.SendData(id, framed.substr(0, 1), false));
    ASSERT_TRUE(h.peer.SendData(id, framed.substr(1, 4), false));
    ASSERT_TRUE(h.peer.SendData(id, framed.substr(5, 6), false));
    ASSERT_TRUE(h.peer.SendData(id, framed.substr(11), false));
    ASSERT_TRUE(h.peer.SendHeaders(id, {{"grpc-status", "0"}}, true));
    ASSERT_TRUE(Pump());
    const StreamOutcome &outcome = OutcomeOf(id);
    EXPECT_TRUE(outcome.status.ok()) << outcome.status.ToString();
    EXPECT_EQ(outcome.response, "split-response");
}

// ── 错误与状态合成(§4.3) ──────────────────────────────────────────────

TEST_F(ClientSessionTest, TrailersOnlyErrorDecodesGrpcMessage)
{
    const int32_t id = SubmitAndDeliver("ping");
    // "session expired 失效":空格原样,UTF-8 字节百分号编码。
    ASSERT_TRUE(h.peer.SendTrailersOnly(id, 16, "session expired %E5%A4%B1%E6%95%88", {{"x-detail", "d1"}}));
    ASSERT_TRUE(Pump());
    const StreamOutcome &outcome = OutcomeOf(id);
    EXPECT_EQ(outcome.status.Code(), StatusCode::kUnauthenticated);
    EXPECT_EQ(outcome.status.message(), "session expired \xE5\xA4\xB1\xE6\x95\x88");
    EXPECT_TRUE(outcome.initialMetadata.empty()) << "trailers-only 没有初始 metadata";
    const Metadata expectedTrailing = {{"x-detail", "d1"}};
    EXPECT_EQ(outcome.trailingMetadata, expectedTrailing);
    EXPECT_TRUE(outcome.response.empty());
    EXPECT_FALSE(outcome.unprocessed);
}

TEST_F(ClientSessionTest, NonGrpcHttpStatusIsMappedFromHttpStatus)
{
    const int32_t unavailable = SubmitAndDeliver("a");
    ASSERT_TRUE(h.peer.SendHeaders(unavailable, {{":status", "503"}, {"content-type", "text/plain"}}, true));
    const int32_t notFound = SubmitAndDeliver("b");
    ASSERT_TRUE(h.peer.SendHeaders(notFound, {{":status", "404"}, {"content-type", "application/grpc"}}, true));
    ASSERT_TRUE(Pump());
    EXPECT_EQ(OutcomeOf(unavailable).status.Code(), StatusCode::kUnavailable) << OutcomeOf(unavailable).status.ToString();
    // :status != 200 时即使 content-type 是 gRPC 也按 HTTP 状态合成。
    EXPECT_EQ(OutcomeOf(notFound).status.Code(), StatusCode::kUnimplemented) << OutcomeOf(notFound).status.ToString();
}

TEST_F(ClientSessionTest, Status200WithNonGrpcContentTypeIsUnknown)
{
    const int32_t id = SubmitAndDeliver("ping");
    ASSERT_TRUE(h.peer.SendHeaders(id, {{":status", "200"}, {"content-type", "text/html"}}, true));
    ASSERT_TRUE(Pump());
    EXPECT_EQ(OutcomeOf(id).status.Code(), StatusCode::kUnknown) << OutcomeOf(id).status.ToString();
}

// 非 gRPC 响应(代理 / LB 的错误页)带着 HTML 消息体:状态仍按 HTTP 状态合成,消息体不得被当成
// gRPC 消息去拆(grpc-go 在 operateHeaders 看到非 gRPC 响应头就以 HTTP 映射的状态结束流,不读消息体)。
TEST_F(ClientSessionTest, NonGrpcResponseBodyDoesNotOverrideHttpStatus)
{
    const int32_t unavailable = SubmitAndDeliver("a");
    ASSERT_TRUE(h.peer.SendHeaders(unavailable, {{":status", "503"}, {"content-type", "text/html"}}, false));
    ASSERT_TRUE(h.peer.SendData(unavailable, "<html><body>busy</body></html>", true));
    const int32_t unknown = SubmitAndDeliver("b");
    ASSERT_TRUE(h.peer.SendHeaders(unknown, {{":status", "200"}, {"content-type", "text/html"}}, false));
    ASSERT_TRUE(h.peer.SendData(unknown, "<html></html>", true));
    ASSERT_TRUE(Pump());
    EXPECT_EQ(OutcomeOf(unavailable).status.Code(), StatusCode::kUnavailable) << OutcomeOf(unavailable).status.ToString();
    EXPECT_EQ(OutcomeOf(unknown).status.Code(), StatusCode::kUnknown) << OutcomeOf(unknown).status.ToString();
}

TEST_F(ClientSessionTest, MissingGrpcStatusIsUnknown)
{
    const int32_t id = SubmitAndDeliver("ping");
    ASSERT_TRUE(h.peer.SendHeaders(id, GrpcResponseHeaders(), false));
    ASSERT_TRUE(h.peer.SendData(id, TestH2Peer::Frame("pong"), false));
    ASSERT_TRUE(h.peer.SendHeaders(id, {{"x-foo", "bar"}}, true));
    ASSERT_TRUE(Pump());
    EXPECT_EQ(OutcomeOf(id).status.Code(), StatusCode::kUnknown) << OutcomeOf(id).status.ToString();
}

TEST_F(ClientSessionTest, InvalidGrpcStatusIsUnknown)
{
    const int32_t letters = SubmitAndDeliver("a");
    const int32_t outOfRange = SubmitAndDeliver("b");
    for (const auto &[id, value] : {std::pair<int32_t, const char *>{letters, "abc"}, {outOfRange, "17"}})
    {
        ASSERT_TRUE(h.peer.SendHeaders(id, GrpcResponseHeaders(), false));
        ASSERT_TRUE(h.peer.SendData(id, TestH2Peer::Frame("pong"), false));
        ASSERT_TRUE(h.peer.SendHeaders(id, {{"grpc-status", value}}, true));
    }
    ASSERT_TRUE(Pump());
    EXPECT_EQ(OutcomeOf(letters).status.Code(), StatusCode::kUnknown) << OutcomeOf(letters).status.ToString();
    EXPECT_EQ(OutcomeOf(outOfRange).status.Code(), StatusCode::kUnknown) << OutcomeOf(outOfRange).status.ToString();
}

TEST_F(ClientSessionTest, OkWithoutMessageIsInternal)
{
    const int32_t id = SubmitAndDeliver("ping");
    ASSERT_TRUE(h.peer.SendHeaders(id, GrpcResponseHeaders(), false));
    ASSERT_TRUE(h.peer.SendHeaders(id, {{"grpc-status", "0"}}, true));
    ASSERT_TRUE(Pump());
    EXPECT_EQ(OutcomeOf(id).status.Code(), StatusCode::kInternal) << OutcomeOf(id).status.ToString();
}

TEST_F(ClientSessionTest, OkWithTwoMessagesIsInternalAndResetsStream)
{
    const int32_t id = SubmitAndDeliver("ping");
    ASSERT_TRUE(h.peer.SendHeaders(id, GrpcResponseHeaders(), false));
    ASSERT_TRUE(h.peer.SendData(id, TestH2Peer::Frame("one") + TestH2Peer::Frame("two"), false));
    ASSERT_TRUE(Pump());
    EXPECT_EQ(OutcomeOf(id).status.Code(), StatusCode::kInternal) << OutcomeOf(id).status.ToString();
    EXPECT_TRUE(OutcomeOf(id).response.empty());
    const auto *stream = h.peer.FindStream(id);
    ASSERT_NE(stream, nullptr);
    EXPECT_TRUE(stream->rstReceived);
    EXPECT_EQ(stream->rstErrorCode, h2::kCancel);
}

TEST_F(ClientSessionTest, DataEndStreamWithoutTrailersIsInternal)
{
    const int32_t id = SubmitAndDeliver("ping");
    ASSERT_TRUE(h.peer.SendHeaders(id, GrpcResponseHeaders(), false));
    ASSERT_TRUE(h.peer.SendData(id, TestH2Peer::Frame("pong"), true));
    ASSERT_TRUE(Pump());
    EXPECT_EQ(OutcomeOf(id).status.Code(), StatusCode::kInternal) << OutcomeOf(id).status.ToString();
}

TEST_F(ClientSessionTest, PeerRstCancelIsCancelled)
{
    const int32_t id = SubmitAndDeliver("ping");
    ASSERT_TRUE(h.peer.SendRstStream(id, h2::kCancel));
    ASSERT_TRUE(Pump());
    const StreamOutcome &outcome = OutcomeOf(id);
    EXPECT_EQ(outcome.status.Code(), StatusCode::kCancelled) << outcome.status.ToString();
    EXPECT_FALSE(outcome.unprocessed) << "CANCEL 不能证明对端没处理";
}

TEST_F(ClientSessionTest, PeerRstRefusedIsUnavailableAndUnprocessed)
{
    const int32_t id = SubmitAndDeliver("ping");
    ASSERT_TRUE(h.peer.SendRstStream(id, h2::kRefusedStream));
    ASSERT_TRUE(Pump());
    const StreamOutcome &outcome = OutcomeOf(id);
    EXPECT_EQ(outcome.status.Code(), StatusCode::kUnavailable) << outcome.status.ToString();
    EXPECT_TRUE(outcome.unprocessed);
}

TEST_F(ClientSessionTest, GoawayRefusesLaterStreamsAndBlocksNewOnes)
{
    const int32_t first = SubmitAndDeliver("a");
    const int32_t second = SubmitAndDeliver("b");
    const int32_t third = SubmitAndDeliver("c");
    ASSERT_LT(first, second);
    ASSERT_LT(second, third);

    ASSERT_TRUE(h.peer.SendGoaway(first, h2::kNoError, "bye"));
    ASSERT_TRUE(Pump());

    ASSERT_EQ(h.observer.goaways.size(), 1u);
    EXPECT_EQ(h.observer.goaways[0].lastStreamId, first);
    EXPECT_EQ(h.observer.goaways[0].errorCode, h2::kNoError);
    EXPECT_EQ(h.observer.goaways[0].debugData, "bye");
    EXPECT_TRUE(h.client.GoawayReceived());

    // id 大于 last_stream_id 的流:对端可证明没处理。
    for (const int32_t refused : {second, third})
    {
        const StreamOutcome &outcome = OutcomeOf(refused);
        EXPECT_EQ(outcome.status.Code(), StatusCode::kUnavailable) << outcome.status.ToString();
        EXPECT_TRUE(outcome.unprocessed) << refused;
    }
    EXPECT_FALSE(h.observer.Closed(first)) << "已被接收的流照常等完";

    // 之后不再开新流,并提示换连接。
    const auto rejected = h.client.SubmitUnary(MakeSpec("d"));
    EXPECT_LE(rejected.streamId, 0);
    EXPECT_TRUE(rejected.retryOnNewConnection);
    EXPECT_EQ(rejected.error.Code(), StatusCode::kUnavailable);

    ASSERT_TRUE(h.peer.SendUnaryOk(first, "late"));
    ASSERT_TRUE(Pump());
    EXPECT_TRUE(OutcomeOf(first).status.ok()) << OutcomeOf(first).status.ToString();
    EXPECT_EQ(OutcomeOf(first).response, "late");
    EXPECT_EQ(h.client.ActiveStreamCount(), 0u);
    EXPECT_TRUE(h.client.ShouldClose()) << "GOAWAY 之后流清空,连接应关闭";
}

// ── 接收上限与对端违约 ──────────────────────────────────────────────────

TEST_F(ClientSessionTest, OversizedResponseIsResourceExhaustedAndCancelsStream)
{
    const int32_t id = SubmitAndDeliver("ping");
    ASSERT_TRUE(h.peer.SendHeaders(id, GrpcResponseHeaders(), false));
    // 默认上限 4MiB:只发一个声明 4MiB+1 的前缀,看到前缀就必须拒绝。
    ASSERT_TRUE(h.peer.SendData(id, TestH2Peer::PrefixOnly(4 * 1024 * 1024 + 1), false));
    ASSERT_TRUE(Pump());
    EXPECT_EQ(OutcomeOf(id).status.Code(), StatusCode::kResourceExhausted) << OutcomeOf(id).status.ToString();
    const auto *stream = h.peer.FindStream(id);
    ASSERT_NE(stream, nullptr);
    EXPECT_TRUE(stream->rstReceived);
    EXPECT_EQ(stream->rstErrorCode, h2::kCancel);
}

TEST(ClientSessionLimits, CustomReceiveLimitIsInclusive)
{
    ClientSessionOptions options = MakeOptions();
    options.maxReceiveMessageBytes = 8;
    SessionHarness h(options);
    ASSERT_TRUE(h.Handshake());

    const auto atLimit = h.client.SubmitUnary(MakeSpec("a"));
    const auto overLimit = h.client.SubmitUnary(MakeSpec("b"));
    ASSERT_GT(atLimit.streamId, 0);
    ASSERT_GT(overLimit.streamId, 0);
    ASSERT_TRUE(h.peer.Pump(h.client)) << h.peer.LastError();

    ASSERT_TRUE(h.peer.SendUnaryOk(atLimit.streamId, "12345678"));
    // 超限的那条拆成两帧发:声明长度在第一帧里就超了。
    const std::string framed = TestH2Peer::Frame("123456789");
    ASSERT_TRUE(h.peer.SendHeaders(overLimit.streamId, GrpcResponseHeaders(), false));
    ASSERT_TRUE(h.peer.SendData(overLimit.streamId, framed.substr(0, 7), false));
    ASSERT_TRUE(h.peer.SendData(overLimit.streamId, framed.substr(7), false));
    ASSERT_TRUE(h.peer.Pump(h.client)) << h.peer.LastError();

    ASSERT_TRUE(h.observer.Closed(atLimit.streamId));
    ASSERT_TRUE(h.observer.Closed(overLimit.streamId));
    EXPECT_TRUE(h.observer.outcomes[atLimit.streamId].status.ok());
    EXPECT_EQ(h.observer.outcomes[atLimit.streamId].response, "12345678");
    EXPECT_EQ(h.observer.outcomes[overLimit.streamId].status.Code(), StatusCode::kResourceExhausted);
    const auto *stream = h.peer.FindStream(overLimit.streamId);
    ASSERT_NE(stream, nullptr);
    EXPECT_TRUE(stream->rstReceived);
    EXPECT_EQ(stream->rstErrorCode, h2::kCancel);
}

TEST_F(ClientSessionTest, CompressedMessageIsInternal)
{
    const int32_t id = SubmitAndDeliver("ping");
    ASSERT_TRUE(h.peer.SendHeaders(id, GrpcResponseHeaders(), false));
    ASSERT_TRUE(h.peer.SendData(id, TestH2Peer::Frame("gzipped", 1), false));
    ASSERT_TRUE(Pump());
    EXPECT_EQ(OutcomeOf(id).status.Code(), StatusCode::kInternal) << OutcomeOf(id).status.ToString();
    const auto *stream = h.peer.FindStream(id);
    ASSERT_NE(stream, nullptr);
    EXPECT_TRUE(stream->rstReceived);
    EXPECT_EQ(stream->rstErrorCode, h2::kCancel);
}

TEST_F(ClientSessionTest, OversizedHeaderListIsResourceExhausted)
{
    const int32_t id = SubmitAndDeliver("ping");
    // 20 × (名 9 + 值 1000 + 32) ≈ 20KiB > 默认 16KiB。
    HeaderList headers = GrpcResponseHeaders();
    for (int i = 0; i < 20; ++i)
    {
        headers.emplace_back("x-big-" + std::to_string(100 + i), std::string(1000, 'v'));
    }
    ASSERT_TRUE(h.peer.SendHeaders(id, headers, false));
    ASSERT_TRUE(Pump());
    EXPECT_EQ(OutcomeOf(id).status.Code(), StatusCode::kResourceExhausted) << OutcomeOf(id).status.ToString();
    const auto *stream = h.peer.FindStream(id);
    ASSERT_NE(stream, nullptr);
    EXPECT_TRUE(stream->rstReceived) << "本端必须 RST 掉这个流";
    EXPECT_EQ(stream->rstErrorCode, h2::kInternalError);
}

TEST_F(ClientSessionTest, InvalidBase64InBinaryResponseHeaderIsInternal)
{
    const int32_t id = SubmitAndDeliver("ping");
    ASSERT_TRUE(h.peer.SendHeaders(id, GrpcResponseHeaders({{"x-bad-bin", "@@@@"}}), false));
    ASSERT_TRUE(h.peer.SendData(id, TestH2Peer::Frame("pong"), false));
    ASSERT_TRUE(h.peer.SendHeaders(id, {{"grpc-status", "0"}}, true));
    ASSERT_TRUE(Pump());
    EXPECT_EQ(OutcomeOf(id).status.Code(), StatusCode::kInternal) << OutcomeOf(id).status.ToString();
}

// ── 本端操作 ────────────────────────────────────────────────────────────

TEST_F(ClientSessionTest, ResetStreamSendsRstAndReportsLocalStatus)
{
    const int32_t id = SubmitAndDeliver("ping");
    h.client.ResetStream(id, h2::kCancel, Status(StatusCode::kDeadlineExceeded, "deadline exceeded"));
    EXPECT_FALSE(h.observer.Closed(id)) << "RST 发出去之前流还没结束";
    ASSERT_TRUE(Pump());
    const StreamOutcome &outcome = OutcomeOf(id);
    EXPECT_EQ(outcome.status.Code(), StatusCode::kDeadlineExceeded);
    EXPECT_EQ(outcome.status.message(), "deadline exceeded");
    const auto *stream = h.peer.FindStream(id);
    ASSERT_NE(stream, nullptr);
    EXPECT_TRUE(stream->rstReceived);
    EXPECT_EQ(stream->rstErrorCode, h2::kCancel);
}

TEST_F(ClientSessionTest, ResetAfterPeerResultIsIgnored)
{
    // 「若对端结果先到,以先到者为准」:流已结束后再 ResetStream 不改结果、不再回调、不发 RST。
    const int32_t id = SubmitAndDeliver("ping");
    ASSERT_TRUE(h.peer.SendUnaryOk(id, "pong"));
    ASSERT_TRUE(Pump());
    h.client.ResetStream(id, h2::kCancel, Status(StatusCode::kDeadlineExceeded, "too late"));
    ASSERT_TRUE(Pump());
    EXPECT_TRUE(OutcomeOf(id).status.ok());
    EXPECT_EQ(h.observer.closeCounts[id], 1);
    const auto *stream = h.peer.FindStream(id);
    ASSERT_NE(stream, nullptr);
    EXPECT_FALSE(stream->rstReceived);
}

TEST_F(ClientSessionTest, PingIsAcknowledged)
{
    EXPECT_TRUE(h.client.SubmitPing());
    ASSERT_TRUE(Pump());
    EXPECT_EQ(h.observer.pingAcks, 1);
    EXPECT_EQ(h.peer.PingsReceived(), 1u);
}

TEST_F(ClientSessionTest, FailAllStreamsMarksUnsentRequestsUnprocessed)
{
    const int32_t sent = Submit("handed-to-socket");
    std::string socketBuffer; // 字节交给了「socket」,服务端可能见过
    ASSERT_TRUE(h.client.DrainOutput([&socketBuffer](std::string_view chunk)
                                      {
        socketBuffer.append(chunk.data(), chunk.size());
        return true; })
                    .ok());
    ASSERT_FALSE(socketBuffer.empty());
    const int32_t queued = Submit("never-drained");

    h.client.FailAllStreams(Status(StatusCode::kUnavailable, "connection lost"));

    const StreamOutcome &sentOutcome = OutcomeOf(sent);
    EXPECT_EQ(sentOutcome.status.Code(), StatusCode::kUnavailable);
    EXPECT_EQ(sentOutcome.status.message(), "connection lost");
    EXPECT_FALSE(sentOutcome.unprocessed) << "HEADERS 已交给 socket:结果未知,不得透明重试";

    const StreamOutcome &queuedOutcome = OutcomeOf(queued);
    EXPECT_EQ(queuedOutcome.status.Code(), StatusCode::kUnavailable);
    EXPECT_TRUE(queuedOutcome.unprocessed) << "HEADERS 没交给 socket:服务端不可能见过";
    EXPECT_EQ(h.client.ActiveStreamCount(), 0u);
}

// ── 并发 ────────────────────────────────────────────────────────────────

TEST_F(ClientSessionTest, HundredConcurrentStreamsWithInterleavedResponses)
{
    constexpr int kStreams = 100;
    std::vector<int32_t> ids;
    for (int i = 0; i < kStreams; ++i)
    {
        ids.push_back(Submit("req-" + std::to_string(i), {{"x-index", std::to_string(i)}}));
    }
    ASSERT_TRUE(Pump());
    ASSERT_EQ(h.client.ActiveStreamCount(), static_cast<size_t>(kStreams));
    for (int i = 0; i < kStreams; ++i)
    {
        const auto *stream = h.peer.FindStream(ids[i]);
        ASSERT_NE(stream, nullptr) << i;
        EXPECT_EQ(stream->body.substr(5), "req-" + std::to_string(i));
    }

    // 响应头倒序;消息体每条拆两半,先发所有流的前半再发所有流的后半;trailers 奇偶交错。
    for (int i = kStreams - 1; i >= 0; --i)
    {
        ASSERT_TRUE(h.peer.SendHeaders(ids[i], GrpcResponseHeaders({{"x-index", std::to_string(i)}}), false));
    }
    std::vector<std::string> framed;
    for (int i = 0; i < kStreams; ++i)
    {
        framed.push_back(TestH2Peer::Frame("resp-" + std::to_string(i)));
    }
    for (int i = 0; i < kStreams; ++i)
    {
        ASSERT_TRUE(h.peer.SendData(ids[i], framed[i].substr(0, 3), false)) << h.peer.LastError();
    }
    for (int i = kStreams - 1; i >= 0; --i)
    {
        ASSERT_TRUE(h.peer.SendData(ids[i], framed[i].substr(3), false)) << h.peer.LastError();
    }
    for (int parity = 0; parity < 2; ++parity)
    {
        for (int i = parity; i < kStreams; i += 2)
        {
            ASSERT_TRUE(h.peer.SendHeaders(ids[i], {{"grpc-status", "0"}}, true));
        }
    }
    ASSERT_TRUE(Pump());

    for (int i = 0; i < kStreams; ++i)
    {
        const StreamOutcome &outcome = OutcomeOf(ids[i]);
        EXPECT_TRUE(outcome.status.ok()) << i << ": " << outcome.status.ToString();
        EXPECT_EQ(outcome.response, "resp-" + std::to_string(i));
        const Metadata expected = {{"x-index", std::to_string(i)}};
        EXPECT_EQ(outcome.initialMetadata, expected) << i;
    }
    EXPECT_EQ(h.client.ActiveStreamCount(), 0u);
}

// ── metadata 校验 ───────────────────────────────────────────────────────

TEST_F(ClientSessionTest, InvalidMetadataIsRejectedBeforeAnythingIsSent)
{
    for (const std::string key :
         {"X-Upper", "grpc-custom", ":path", "te", "content-type", "user-agent", "connection", "host", ""})
    {
        const auto result = h.client.SubmitUnary(MakeSpec("x", {{key, "v"}}));
        EXPECT_LE(result.streamId, 0) << "'" << key << "'";
        EXPECT_EQ(result.error.Code(), StatusCode::kInternal) << "'" << key << "'";
        EXPECT_FALSE(result.retryOnNewConnection);
    }
    // 非 -bin 键的值必须是可打印 ASCII。
    const auto badValue = h.client.SubmitUnary(MakeSpec("x", {{"x-text", std::string("bad\x01value")}}));
    EXPECT_LE(badValue.streamId, 0);
    EXPECT_EQ(badValue.error.Code(), StatusCode::kInternal);
    EXPECT_EQ(h.client.ActiveStreamCount(), 0u);
    ASSERT_TRUE(Pump());
    EXPECT_TRUE(h.peer.StreamIds().empty()) << "被拒的请求一个字节都不能上线";

    // 同样的字节放在 -bin 键下是合法的:线上是它的 base64。
    const std::string binary("bad\x01value\xff", 10);
    const int32_t id = SubmitAndDeliver("x", {{"x-raw-bin", binary}});
    const auto *stream = h.peer.FindStream(id);
    ASSERT_NE(stream, nullptr);
    const std::string *wire = stream->FindHeader("x-raw-bin");
    ASSERT_NE(wire, nullptr);
    EXPECT_EQ(*wire, muduo_grpc::Base64EncodeUnpadded(binary));
}

} // namespace
