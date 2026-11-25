// ServerSession(服务端纯协议引擎)、MethodTable、IsCanonicalIpv4 单测(docs/design/grpc-muduo-transport.md §15.4、§15.5)。
//
// 两种对端,都在内存里对接、完全确定性:
//   * ClientSession(第 1~3 批的客户端引擎):验证真实客户端与服务端的往返(metadata 两层编码、截止、trailers);
//   * TestH2Client(内存 nghttp2 客户端):能发任意头部与任意帧序,覆盖 §15.5.3 拒绝表的每一行、
//     帧顺序、早拒后的 RST_STREAM(NO_ERROR)、两段 GOAWAY、对端 RST、rapid reset、请求体累计上限。
//
// 只走公开接口:ServerSession 的方法 + Observer 回调;路由、在途上限、排空到期由测试里的 Observer 按脚本给 Verdict。

#include <gtest/gtest.h>

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <functional>
#include <map>
#include <memory>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "muduo_grpc/address.h"
#include "muduo_grpc/client_session.h"
#include "muduo_grpc/message_framing.h"
#include "muduo_grpc/metadata.h"
#include "muduo_grpc/method_table.h"
// MethodTable 用例要从 lambda 构造 UnaryMethodHandler,需要完整的 ServerCall 类型(handler 不会被调用)。
#include "muduo_grpc/server_call.h"
#include "muduo_grpc/server_session.h"
#include "muduo_grpc/status.h"

#include "test_h2_client.h"
#include "test_h2_peer.h"

namespace
{

using muduo_grpc::ClientSession;
using muduo_grpc::ClientSessionOptions;
using muduo_grpc::Metadata;
using muduo_grpc::MethodTable;
using muduo_grpc::RequestHeaders;
using muduo_grpc::ServerSession;
using muduo_grpc::ServerSessionOptions;
using muduo_grpc::Status;
using muduo_grpc::StatusCode;
using muduo_grpc::StreamOutcome;
using muduo_grpc::UnaryMethodHandler;
using muduo_grpc::UnaryRequestSpec;
using muduo_grpc_test::ClientFrame;
using muduo_grpc_test::ClientStream;
using muduo_grpc_test::HeaderList;
using muduo_grpc_test::SettingEntry;
using muduo_grpc_test::TestH2Client;
using muduo_grpc_test::TestH2Peer;
namespace h2 = muduo_grpc_test::h2;

constexpr char kPath[] = "/scene_node.SceneNodeGrpc/CreateScene";
constexpr char kAuthority[] = "127.0.0.1:50600";
constexpr char kUserAgent[] = "muduo-grpc-test/1.0";
// gate 的 x-session-detail-bin 应用层值(与 client_session_test 同一样例)。
constexpr char kSessionAppValue[] = "CgQIARABEgIIAQ==";

// ── 测试替身 ────────────────────────────────────────────────────────────

struct CompletedRequest
{
    RequestHeaders headers;
    std::string message;
};

// 记录 Observer 的全部事件;Verdict 由用例按需给出(默认一律 kAccept)。按头文件约定,回调里只记状态。
class RecordingServerObserver : public ServerSession::Observer
{
public:
    using VerdictFn = std::function<ServerSession::Verdict(int32_t streamId, const RequestHeaders &headers)>;

    void OnClientSettings() override { ++clientSettings; }

    ServerSession::Verdict OnRequestHeaders(int32_t streamId, const RequestHeaders &headers) override
    {
        events.push_back("headers:" + std::to_string(streamId));
        headerCalls[streamId] = headers;
        return headersVerdict ? headersVerdict(streamId, headers) : ServerSession::Verdict{};
    }

    ServerSession::Verdict OnRequestComplete(int32_t streamId, RequestHeaders headers, std::string message) override
    {
        events.push_back("complete:" + std::to_string(streamId));
        ServerSession::Verdict verdict =
            completeVerdict ? completeVerdict(streamId, headers) : ServerSession::Verdict{};
        completed[streamId] = CompletedRequest{std::move(headers), std::move(message)};
        return verdict;
    }

    void OnStreamAborted(int32_t streamId, uint32_t http2ErrorCode) override
    {
        aborted.emplace_back(streamId, http2ErrorCode);
    }

    void OnShutdownPingAck() override { ++shutdownPingAcks; }

    void OnRequestRejected(int32_t streamId, int httpStatus, const Status &status) override
    {
        rejected[streamId] = std::make_pair(httpStatus, status);
    }

    VerdictFn headersVerdict;
    VerdictFn completeVerdict;
    int clientSettings{0};
    int shutdownPingAcks{0};
    std::vector<std::string> events;
    std::map<int32_t, RequestHeaders> headerCalls;
    std::map<int32_t, CompletedRequest> completed;
    std::vector<std::pair<int32_t, uint32_t>> aborted;
    std::map<int32_t, std::pair<int, Status>> rejected; // 会话自己的拒绝:httpStatus + 状态
};

ServerSession::Verdict RejectVerdict(StatusCode code, std::string message)
{
    ServerSession::Verdict verdict;
    verdict.kind = ServerSession::Verdict::Kind::kReject;
    verdict.status = Status(code, std::move(message));
    return verdict;
}

ServerSession::Verdict RefuseVerdict()
{
    ServerSession::Verdict verdict;
    verdict.kind = ServerSession::Verdict::Kind::kRefuse;
    return verdict;
}

// 一个合法的 gRPC 请求头(线上值),extra 追加在末尾。
HeaderList GrpcRequest(std::string path = kPath, const HeaderList &extra = {})
{
    HeaderList headers{{":method", "POST"},          {":scheme", "http"},
                       {":path", std::move(path)},   {":authority", kAuthority},
                       {"te", "trailers"},           {"content-type", "application/grpc"}};
    headers.insert(headers.end(), extra.begin(), extra.end());
    return headers;
}

// 把第一个名为 name 的头的值换成 value(没有就追加)。
HeaderList Replace(HeaderList headers, std::string_view name, std::string value)
{
    for (auto &[key, current] : headers)
    {
        if (key == name)
        {
            current = std::move(value);
            return headers;
        }
    }
    headers.emplace_back(std::string(name), std::move(value));
    return headers;
}

HeaderList Without(HeaderList headers, std::string_view name)
{
    HeaderList kept;
    for (auto &entry : headers)
    {
        if (entry.first != name)
        {
            kept.push_back(std::move(entry));
        }
    }
    return kept;
}

// trailers-only 应答的完整头块(线上值)。grpcMessageWire 为空时不应出现 grpc-message。
HeaderList ExpectedTrailersOnly(const std::string &httpStatus, uint32_t grpcStatus, const std::string &grpcMessageWire,
                                const HeaderList &extra = {})
{
    HeaderList headers{
        {":status", httpStatus}, {"content-type", "application/grpc"}, {"grpc-status", std::to_string(grpcStatus)}};
    if (!grpcMessageWire.empty())
    {
        headers.emplace_back("grpc-message", grpcMessageWire);
    }
    headers.insert(headers.end(), extra.begin(), extra.end());
    return headers;
}

// 本流上收到的帧,简写:H / H!(带 END_STREAM)、D / D!、R<错误码>。
std::string FrameTrace(const ClientStream &stream)
{
    std::string trace;
    for (const ClientFrame &frame : stream.frames)
    {
        if (!trace.empty())
        {
            trace.push_back(' ');
        }
        switch (frame.type)
        {
        case h2::kFrameHeaders:
            trace += frame.endStream() ? "H!" : "H";
            break;
        case h2::kFrameData:
            trace += frame.endStream() ? "D!" : "D";
            break;
        case h2::kFrameRstStream:
            trace += "R" + std::to_string(frame.errorCode);
            break;
        default:
            trace += "?" + std::to_string(frame.type);
            break;
        }
    }
    return trace;
}

const ClientStream &StreamOf(const TestH2Client &client, int32_t streamId)
{
    static const ClientStream kMissing{};
    const ClientStream *stream = client.FindStream(streamId);
    if (stream == nullptr)
    {
        ADD_FAILURE() << "client has no record of stream " << streamId;
        return kMissing;
    }
    return *stream;
}

// 断言流以 trailers-only 结束;请求没收完时(requestComplete=false)回完必须紧跟 RST_STREAM(NO_ERROR)(§15.5.3)。
void ExpectTrailersOnly(const TestH2Client &client, int32_t streamId, const HeaderList &expected, bool requestComplete)
{
    const ClientStream &stream = StreamOf(client, streamId);
    EXPECT_EQ(stream.responseHeaders, expected) << "stream " << streamId;
    EXPECT_TRUE(stream.trailers.empty()) << "stream " << streamId;
    EXPECT_TRUE(stream.body.empty()) << "stream " << streamId;
    EXPECT_EQ(FrameTrace(stream), requestComplete ? "H!" : "H! R0") << "stream " << streamId;
}

::testing::AssertionResult Feed(ServerSession &server, const std::string &bytes)
{
    const Status status = server.Receive(reinterpret_cast<const uint8_t *>(bytes.data()), bytes.size());
    if (!status.ok())
    {
        return ::testing::AssertionFailure() << "server Receive: " << status.ToString();
    }
    return ::testing::AssertionSuccess();
}

struct Drained
{
    Status status;
    std::string bytes;
};

Drained Drain(ServerSession &server)
{
    Drained drained;
    drained.status = server.DrainOutput([&drained](std::string_view chunk)
                                        {
        drained.bytes.append(chunk.data(), chunk.size());
        return true; });
    return drained;
}

// 「观察者 + 内存客户端 + 被测服务端会话」。成员顺序即构造顺序:会话最后构造、最先析构(析构不回调观察者)。
struct ServerHarness
{
    explicit ServerHarness(ServerSessionOptions options = {}) : server(options, observer) {}

    // Start + 握手(双方 SETTINGS 互换完毕)。
    ::testing::AssertionResult Handshake()
    {
        const Status started = server.Start();
        if (!started.ok())
        {
            return ::testing::AssertionFailure() << "Start: " << started.ToString();
        }
        if (!client.Pump(server))
        {
            return ::testing::AssertionFailure() << "Pump: " << client.LastError();
        }
        if (observer.clientSettings != 1 || !server.HandshakeComplete())
        {
            return ::testing::AssertionFailure() << "clientSettings=" << observer.clientSettings;
        }
        if (client.SettingsFramesReceived() != 1)
        {
            return ::testing::AssertionFailure() << "server SETTINGS frames seen by client="
                                                 << client.SettingsFramesReceived();
        }
        return ::testing::AssertionSuccess();
    }

    ::testing::AssertionResult Pump()
    {
        if (client.Pump(server))
        {
            return ::testing::AssertionSuccess();
        }
        return ::testing::AssertionFailure() << client.LastError();
    }

    RecordingServerObserver observer;
    TestH2Client client;
    ServerSession server;
};

class ServerSessionTest : public ::testing::Test
{
protected:
    void SetUp() override { ASSERT_TRUE(h.Handshake()); }

    // 发一个完整请求(HEADERS + 一条消息 + END_STREAM)并送到服务端。
    int32_t SendUnary(const HeaderList &headers, std::string_view payload = "ping")
    {
        const int32_t id = h.client.OpenStream(headers, false);
        EXPECT_GT(id, 0) << h.client.LastError();
        EXPECT_TRUE(h.client.SendData(id, TestH2Peer::Frame(payload), true)) << h.client.LastError();
        EXPECT_TRUE(h.Pump());
        return id;
    }

    ServerHarness h;
};

// ClientSession 侧的观察者:只记状态。
class RecordingClientObserver : public ClientSession::Observer
{
public:
    void OnSessionReady() override { ++readyCount; }
    void OnGoawayReceived(int32_t, uint32_t, std::string_view) override { ++goaways; }
    void OnFrameReceived(uint8_t) override {}
    void OnPingAck() override {}
    void OnStreamClosed(int32_t streamId, StreamOutcome outcome) override
    {
        ++closeCounts[streamId];
        outcomes[streamId] = std::move(outcome);
    }

    int readyCount{0};
    int goaways{0};
    std::map<int32_t, StreamOutcome> outcomes;
    std::map<int32_t, int> closeCounts;
};

ClientSessionOptions MakeClientOptions()
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

// 「ClientSession + ServerSession」直接在内存里对接。
struct ClientServerHarness
{
    ClientServerHarness() : server(ServerSessionOptions{}, serverObserver), client(MakeClientOptions(), clientObserver)
    {
    }

    ::testing::AssertionResult Start()
    {
        const Status serverStarted = server.Start();
        const Status clientStarted = client.Start();
        if (!serverStarted.ok() || !clientStarted.ok())
        {
            return ::testing::AssertionFailure() << "Start: server=" << serverStarted.ToString()
                                                 << " client=" << clientStarted.ToString();
        }
        const auto pumped = Pump();
        if (!pumped)
        {
            return pumped;
        }
        if (clientObserver.readyCount != 1 || serverObserver.clientSettings != 1)
        {
            return ::testing::AssertionFailure() << "client ready=" << clientObserver.readyCount
                                                 << " server clientSettings=" << serverObserver.clientSettings;
        }
        return ::testing::AssertionSuccess();
    }

    ::testing::AssertionResult Pump(int maxRounds = 256)
    {
        for (int round = 0; round < maxRounds; ++round)
        {
            std::string toServer;
            const Status drainedClient = client.DrainOutput([&toServer](std::string_view chunk)
                                                            {
                toServer.append(chunk.data(), chunk.size());
                return true; });
            if (!drainedClient.ok())
            {
                return ::testing::AssertionFailure() << "client DrainOutput: " << drainedClient.ToString();
            }
            if (!toServer.empty())
            {
                const auto fed = Feed(server, toServer);
                if (!fed)
                {
                    return fed;
                }
            }
            const Drained toClient = Drain(server);
            if (!toClient.status.ok())
            {
                return ::testing::AssertionFailure() << "server DrainOutput: " << toClient.status.ToString();
            }
            if (!toClient.bytes.empty())
            {
                const Status received = client.Receive(reinterpret_cast<const uint8_t *>(toClient.bytes.data()),
                                                       toClient.bytes.size());
                if (!received.ok())
                {
                    return ::testing::AssertionFailure() << "client Receive: " << received.ToString();
                }
            }
            if (toServer.empty() && toClient.bytes.empty())
            {
                return ::testing::AssertionSuccess();
            }
        }
        return ::testing::AssertionFailure() << "Pump did not settle within " << maxRounds << " rounds";
    }

    const StreamOutcome &OutcomeOf(int32_t streamId)
    {
        static const StreamOutcome kMissing{};
        const auto it = clientObserver.outcomes.find(streamId);
        if (it == clientObserver.outcomes.end())
        {
            ADD_FAILURE() << "stream " << streamId << " has not been closed on the client";
            return kMissing;
        }
        EXPECT_EQ(clientObserver.closeCounts[streamId], 1) << "OnStreamClosed must fire exactly once";
        return it->second;
    }

    RecordingClientObserver clientObserver;
    RecordingServerObserver serverObserver;
    ServerSession server;
    ClientSession client;
};

// ── IsCanonicalIpv4 ────────────────────────────────────────────────────

TEST(AddressTest, AcceptsOnlyCanonicalDottedQuad)
{
    for (const char *ip : {"127.0.0.1", "10.0.0.255", "0.0.0.0", "255.255.255.255", "192.168.10.3"})
    {
        EXPECT_TRUE(muduo_grpc::IsCanonicalIpv4(ip)) << ip;
    }
    for (const char *ip : {"", "1.2.3", "1.2.3.4.5", "01.2.3.4", "1.2.3.04", "256.1.1.1", "1..2.3", "1.2.3.",
                           ".1.2.3", "a.b.c.d", "1.2.3.4 ", " 1.2.3.4", "::1", "localhost", "-1.2.3.4", "1.2.3.-0",
                           "+1.2.3.4", "1.2.3.1000"})
    {
        EXPECT_FALSE(muduo_grpc::IsCanonicalIpv4(ip)) << "'" << ip << "'";
    }
}

// ── MethodTable ────────────────────────────────────────────────────────

UnaryMethodHandler NoopHandler()
{
    return [](muduo_grpc::ServerCall, std::string_view) {};
}

TEST(MethodTableTest, SplitMethodPath)
{
    const auto scene = muduo_grpc::SplitMethodPath("/scene_node.SceneNodeGrpc/CreateScene");
    ASSERT_TRUE(scene.has_value());
    EXPECT_EQ(scene->first, "scene_node.SceneNodeGrpc");
    EXPECT_EQ(scene->second, "CreateScene");
    // battle 的服务没有 package。
    const auto battle = muduo_grpc::SplitMethodPath("/BattleNode/CreateBattle");
    ASSERT_TRUE(battle.has_value());
    EXPECT_EQ(battle->first, "BattleNode");
    EXPECT_EQ(battle->second, "CreateBattle");

    for (const char *path : {"", "/", "//", "BattleNode/CreateBattle", "//CreateBattle", "/BattleNode/",
                             "/BattleNode", "/a/b/c"})
    {
        EXPECT_FALSE(muduo_grpc::SplitMethodPath(path).has_value()) << "'" << path << "'";
    }
}

TEST(MethodTableTest, FindIsExactAndHasServiceTracksServices)
{
    MethodTable table;
    EXPECT_TRUE(table.AddUnary("/scene_node.SceneNodeGrpc/CreateScene", NoopHandler()));
    EXPECT_TRUE(table.AddUnary("/scene_node.SceneNodeGrpc/DestroyScene", NoopHandler()));
    EXPECT_TRUE(table.Valid());
    EXPECT_EQ(table.size(), 2u);

    EXPECT_NE(table.Find("/scene_node.SceneNodeGrpc/CreateScene"), nullptr);
    EXPECT_EQ(table.Find("/scene_node.SceneNodeGrpc/Nope"), nullptr);
    EXPECT_EQ(table.Find("/scene_node.SceneNodeGrpc/createscene"), nullptr) << "按 :path 精确匹配";
    EXPECT_EQ(table.Find("/scene_node.SceneNodeGrpc/CreateScene/"), nullptr);

    EXPECT_TRUE(table.HasService("scene_node.SceneNodeGrpc"));
    EXPECT_FALSE(table.HasService("scene_node"));
    EXPECT_FALSE(table.HasService("BattleNode"));
}

TEST(MethodTableTest, InvalidRegistrationMarksTableInvalid)
{
    MethodTable duplicate;
    ASSERT_TRUE(duplicate.AddUnary("/BattleNode/CreateBattle", NoopHandler()));
    EXPECT_FALSE(duplicate.AddUnary("/BattleNode/CreateBattle", NoopHandler()));
    EXPECT_FALSE(duplicate.Valid());
    EXPECT_EQ(duplicate.size(), 1u) << "先登记的方法不受影响";
    EXPECT_NE(duplicate.Find("/BattleNode/CreateBattle"), nullptr);

    MethodTable malformed;
    EXPECT_FALSE(malformed.AddUnary("BattleNode/CreateBattle", NoopHandler()));
    EXPECT_FALSE(malformed.Valid());
    EXPECT_EQ(malformed.size(), 0u);

    MethodTable empty;
    EXPECT_FALSE(empty.AddUnary("/BattleNode/CreateBattle", UnaryMethodHandler{}));
    EXPECT_FALSE(empty.Valid());
    EXPECT_EQ(empty.Find("/BattleNode/CreateBattle"), nullptr);
}

class FakeBattleService final : public muduo_grpc::Service
{
public:
    void RegisterMethods(MethodTable &table) override
    {
        table.AddUnary("/BattleNode/QueryBattleCreation", NoopHandler());
        table.AddUnary("/BattleNode/CreateBattle", NoopHandler());
    }
};

TEST(MethodTableTest, ServiceRegistersItsMethods)
{
    MethodTable table;
    FakeBattleService service;
    service.RegisterMethods(table);
    EXPECT_TRUE(table.Valid());
    EXPECT_EQ(table.size(), 2u);
    EXPECT_NE(table.Find("/BattleNode/QueryBattleCreation"), nullptr);
    EXPECT_TRUE(table.HasService("BattleNode"));
}

// ── 握手、探针与坏 magic(§15.5.1、§15.5.2) ────────────────────────────

TEST(ServerSessionHandshake, SettingsDeferredUntilFirstPeerByte)
{
    RecordingServerObserver observer;
    ServerSession server(ServerSessionOptions{}, observer);
    ASSERT_TRUE(server.Start().ok());

    // 收到对端首字节之前一个字节都不写:探针连上就关,本端不会触发 EPIPE / RST。
    Drained beforeAnyByte = Drain(server);
    ASSERT_TRUE(beforeAnyByte.status.ok());
    EXPECT_TRUE(beforeAnyByte.bytes.empty());
    EXPECT_FALSE(server.ReceivedAnyBytes());
    EXPECT_FALSE(server.ShouldClose()) << "还没收到字节的连接由握手超时兜底,不是会话层要关的";

    TestH2Client client;
    const std::string preface = client.TakeOutput(); // client magic + 客户端 SETTINGS
    ASSERT_GT(preface.size(), 1u);
    ASSERT_EQ(preface[0], 'P');

    // 只喂 magic 的第一个字节:本端 SETTINGS 随即可以写出,且必须是第一帧。
    ASSERT_TRUE(Feed(server, preface.substr(0, 1)));
    EXPECT_TRUE(server.ReceivedAnyBytes());
    EXPECT_FALSE(server.HandshakeComplete());
    const Drained first = Drain(server);
    ASSERT_TRUE(first.status.ok());
    ASSERT_GE(first.bytes.size(), 9u);
    const uint32_t length = (static_cast<uint32_t>(static_cast<uint8_t>(first.bytes[0])) << 16) |
                            (static_cast<uint32_t>(static_cast<uint8_t>(first.bytes[1])) << 8) |
                            static_cast<uint32_t>(static_cast<uint8_t>(first.bytes[2]));
    EXPECT_EQ(static_cast<int>(static_cast<uint8_t>(first.bytes[3])), static_cast<int>(h2::kFrameSettings));
    EXPECT_EQ(static_cast<int>(static_cast<uint8_t>(first.bytes[4])), 0) << "第一帧是非 ACK 的 SETTINGS";
    EXPECT_EQ(length, 3u * 6u) << "三项设置";

    // 补齐前言,完成握手;从客户端视角核对设置项与连接级窗口。
    ASSERT_TRUE(Feed(server, preface.substr(1)));
    const Drained rest = Drain(server);
    ASSERT_TRUE(rest.status.ok());
    ASSERT_TRUE(client.Receive(first.bytes + rest.bytes)) << client.LastError();
    ASSERT_TRUE(client.Pump(server)) << client.LastError();

    ASSERT_FALSE(client.FrameTypes().empty());
    EXPECT_EQ(client.FrameTypes().front(), h2::kFrameSettings);
    // MAX_CONCURRENT_STREAMS=256、INITIAL_WINDOW_SIZE=1MiB、MAX_HEADER_LIST_SIZE=16KiB;不发 ENABLE_PUSH。
    const std::vector<SettingEntry> expected = {
        {h2::kSettingsMaxConcurrentStreams, 256},
        {h2::kSettingsInitialWindowSize, 1024 * 1024},
        {h2::kSettingsMaxHeaderListSize, 16 * 1024},
    };
    EXPECT_TRUE(client.LastPeerSettings() == expected);
    EXPECT_EQ(client.RemoteConnectionWindow(), 16 * 1024 * 1024);
    EXPECT_EQ(observer.clientSettings, 1);
    EXPECT_TRUE(server.HandshakeComplete());
    EXPECT_FALSE(server.ShouldClose());
}

TEST(ServerSessionHandshake, BadClientMagicIsFatalAndWritesNothing)
{
    RecordingServerObserver observer;
    ServerSession server(ServerSessionOptions{}, observer);
    ASSERT_TRUE(server.Start().ok());

    const std::string http1 = "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n";
    const Status received = server.Receive(reinterpret_cast<const uint8_t *>(http1.data()), http1.size());
    EXPECT_FALSE(received.ok());
    EXPECT_TRUE(server.ReceivedAnyBytes());

    // 对端根本不说 HTTP/2:连本端 SETTINGS 都不写,会话要求关闭。
    const Drained drained = Drain(server);
    EXPECT_TRUE(drained.status.ok());
    EXPECT_TRUE(drained.bytes.empty());
    EXPECT_TRUE(server.ShouldClose());
    EXPECT_FALSE(server.HandshakeComplete());
    EXPECT_EQ(observer.clientSettings, 0);

    // 致命错误是粘滞的。
    EXPECT_FALSE(server.Receive(reinterpret_cast<const uint8_t *>(http1.data()), http1.size()).ok());
}

TEST_F(ServerSessionTest, LaterSettingsDoNotRepeatClientSettings)
{
    ASSERT_TRUE(h.client.SendSettings({{h2::kSettingsInitialWindowSize, 128 * 1024}}));
    ASSERT_TRUE(h.Pump());
    EXPECT_EQ(h.observer.clientSettings, 1);
}

// ── 与 ClientSession 往返 ───────────────────────────────────────────────

TEST(ServerSessionRoundTrip, UnaryOkWithClientSession)
{
    ClientServerHarness h;
    ASSERT_TRUE(h.Start());
    const auto submitted =
        h.client.SubmitUnary(MakeSpec("ping", {{"x-session-detail-bin", kSessionAppValue}, {"x-trace", "abc"}}));
    ASSERT_GT(submitted.streamId, 0) << submitted.error.ToString();
    const int32_t id = submitted.streamId;
    ASSERT_TRUE(h.Pump());

    const std::vector<std::string> expectedEvents = {"headers:" + std::to_string(id),
                                                     "complete:" + std::to_string(id)};
    EXPECT_EQ(h.serverObserver.events, expectedEvents);
    ASSERT_EQ(h.serverObserver.completed.count(id), 1u);
    const CompletedRequest &request = h.serverObserver.completed.at(id);
    EXPECT_EQ(request.headers.path, kPath);
    EXPECT_EQ(request.message, "ping");
    // ClientSession 按 -bin 规则编一层、ServerSession 解一层:两端看到的都是应用层值(§4.2)。
    // te / content-type / user-agent / grpc-timeout / 伪头都不进 metadata。
    const Metadata expectedMetadata = {{"x-session-detail-bin", kSessionAppValue}, {"x-trace", "abc"}};
    EXPECT_EQ(request.headers.metadata, expectedMetadata);
    EXPECT_EQ(h.serverObserver.headerCalls.at(id).metadata, expectedMetadata);
    ASSERT_TRUE(request.headers.timeout.has_value());
    EXPECT_EQ(request.headers.timeout->count(), int64_t{800'000'000}) << "800000u = 800ms";

    ASSERT_TRUE(h.server.SubmitResponse(id, "pong"));
    ASSERT_TRUE(h.Pump());
    const StreamOutcome &outcome = h.OutcomeOf(id);
    EXPECT_TRUE(outcome.status.ok()) << outcome.status.ToString();
    EXPECT_EQ(outcome.response, "pong");
    EXPECT_TRUE(outcome.initialMetadata.empty());
    EXPECT_TRUE(outcome.trailingMetadata.empty());
    EXPECT_FALSE(outcome.unprocessed);
    EXPECT_EQ(h.client.ActiveStreamCount(), 0u);
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);
    EXPECT_TRUE(h.serverObserver.aborted.empty());
}

TEST(ServerSessionRoundTrip, StatusAndRefuseWithClientSession)
{
    ClientServerHarness h;
    ASSERT_TRUE(h.Start());
    const int32_t unavailable = h.client.SubmitUnary(MakeSpec("a")).streamId;
    const int32_t precondition = h.client.SubmitUnary(MakeSpec("b")).streamId;
    const int32_t refused = h.client.SubmitUnary(MakeSpec("c")).streamId;
    ASSERT_GT(unavailable, 0);
    ASSERT_GT(precondition, 0);
    ASSERT_GT(refused, 0);
    ASSERT_TRUE(h.Pump());
    ASSERT_EQ(h.serverObserver.completed.size(), 3u);

    // 跨语言契约文案逐字节透传;含 '%'、换行与 UTF-8 的文案经百分号编码后在客户端还原。
    const std::string tricky = std::string("placement_operation_mismatch: 50% \n") + "\xE5\xA4\xB1\xE8\xB4\xA5";
    ASSERT_TRUE(h.server.SubmitStatus(unavailable, Status(StatusCode::kUnavailable, "battle_not_allocatable")));
    ASSERT_TRUE(h.server.SubmitStatus(precondition, Status(StatusCode::kFailedPrecondition, tricky)));
    ASSERT_TRUE(h.server.RefuseStream(refused));
    ASSERT_TRUE(h.Pump());

    const StreamOutcome &first = h.OutcomeOf(unavailable);
    EXPECT_EQ(first.status.Code(), StatusCode::kUnavailable);
    EXPECT_EQ(first.status.message(), "battle_not_allocatable");
    EXPECT_FALSE(first.unprocessed);
    const StreamOutcome &second = h.OutcomeOf(precondition);
    EXPECT_EQ(second.status.Code(), StatusCode::kFailedPrecondition);
    EXPECT_EQ(second.status.message(), tricky);
    // REFUSED_STREAM = 对端可证明没处理:ClientSession 判为 unprocessed(grpc-go 同样据此透明重试)。
    const StreamOutcome &third = h.OutcomeOf(refused);
    EXPECT_EQ(third.status.Code(), StatusCode::kUnavailable);
    EXPECT_TRUE(third.unprocessed);
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);
}

// ── 应答形状(§15.5.5) ────────────────────────────────────────────────

TEST_F(ServerSessionTest, SuccessResponseIsHeadersDataTrailersInOrder)
{
    const int32_t id = SendUnary(GrpcRequest(), "ping");
    ASSERT_EQ(h.observer.completed.count(id), 1u);
    EXPECT_EQ(h.observer.completed.at(id).message, "ping");

    ASSERT_TRUE(h.server.SubmitResponse(id, "pong"));
    ASSERT_TRUE(h.Pump());
    const ClientStream &stream = StreamOf(h.client, id);
    EXPECT_EQ(FrameTrace(stream), "H D H!") << "trailers 不得插到 DATA 前面";
    const HeaderList expectedHeaders = {{":status", "200"}, {"content-type", "application/grpc"}};
    EXPECT_EQ(stream.responseHeaders, expectedHeaders);
    const HeaderList expectedTrailers = {{"grpc-status", "0"}};
    EXPECT_EQ(stream.trailers, expectedTrailers) << "成功时不发 grpc-message";
    EXPECT_EQ(stream.body, TestH2Peer::Frame("pong"));
    EXPECT_FALSE(stream.rstReceived) << "请求已收完,不补 RST";
    EXPECT_TRUE(stream.closed);
    EXPECT_EQ(stream.closeErrorCode, h2::kNoError);
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);

    // 空消息也有 5 字节前缀。
    const int32_t empty = SendUnary(GrpcRequest(), "");
    EXPECT_EQ(h.observer.completed.at(empty).message, "");
    ASSERT_TRUE(h.server.SubmitResponse(empty, ""));
    ASSERT_TRUE(h.Pump());
    EXPECT_EQ(FrameTrace(StreamOf(h.client, empty)), "H D H!");
    EXPECT_EQ(StreamOf(h.client, empty).body, std::string(5, '\0'));
}

TEST_F(ServerSessionTest, LargeResponseSpansFramesAndFlowControlWindows)
{
    // 100KiB:超过 16KiB 帧上限,也超过客户端默认 64KiB 流窗口(要靠客户端的 WINDOW_UPDATE 续发)。
    std::string payload(100 * 1024, '\0');
    for (size_t i = 0; i < payload.size(); ++i)
    {
        payload[i] = static_cast<char>(i * 31 % 251);
    }
    const int32_t id = SendUnary(GrpcRequest());
    ASSERT_TRUE(h.server.SubmitResponse(id, payload));
    ASSERT_TRUE(h.Pump());
    const ClientStream &stream = StreamOf(h.client, id);
    EXPECT_EQ(stream.body, TestH2Peer::Frame(payload));
    ASSERT_GE(stream.frames.size(), 4u);
    EXPECT_EQ(stream.frames.front().type, h2::kFrameHeaders);
    EXPECT_EQ(stream.frames.back().type, h2::kFrameHeaders);
    EXPECT_TRUE(stream.frames.back().endStream());
    for (size_t i = 1; i + 1 < stream.frames.size(); ++i)
    {
        EXPECT_EQ(stream.frames[i].type, h2::kFrameData) << "frame " << i;
        EXPECT_FALSE(stream.frames[i].endStream()) << "frame " << i;
    }
    const HeaderList expectedTrailers = {{"grpc-status", "0"}};
    EXPECT_EQ(stream.trailers, expectedTrailers);
}

TEST_F(ServerSessionTest, TrailersOnlyStatusEncodesGrpcMessage)
{
    const int32_t plain = SendUnary(GrpcRequest());
    const int32_t encoded = SendUnary(GrpcRequest());
    const int32_t noMessage = SendUnary(GrpcRequest());
    ASSERT_EQ(h.observer.completed.size(), 3u);

    ASSERT_TRUE(h.server.SubmitStatus(plain, Status(StatusCode::kUnavailable, "agones allocate not confirmed")));
    const std::string message = std::string("50% done\n") + "\xE5\xA4\xB1\xE8\xB4\xA5";
    ASSERT_TRUE(h.server.SubmitStatus(encoded, Status(StatusCode::kFailedPrecondition, message)));
    ASSERT_TRUE(h.server.SubmitStatus(noMessage, Status(StatusCode::kNotFound, "")));
    ASSERT_TRUE(h.Pump());

    ExpectTrailersOnly(h.client, plain, ExpectedTrailersOnly("200", 14, "agones allocate not confirmed"), true);
    // 空格与可打印 ASCII 原样;'%'、控制字符、UTF-8 字节写成 %XX(大写)。
    EXPECT_EQ(muduo_grpc::PercentEncodeGrpcMessage(message), "50%25 done%0A%E5%A4%B1%E8%B4%A5");
    ExpectTrailersOnly(h.client, encoded, ExpectedTrailersOnly("200", 9, "50%25 done%0A%E5%A4%B1%E8%B4%A5"), true);
    // 文案为空时不发 grpc-message。
    ExpectTrailersOnly(h.client, noMessage, ExpectedTrailersOnly("200", 5, ""), true);
}

// ── §15.5.3 拒绝表:逐行 ─────────────────────────────────────────────────

// 伪头缺失 / 连接级头 / te≠trailers / 头名大写:nghttp2 自动 RST_STREAM(PROTOCOL_ERROR),观察者看不到。
TEST_F(ServerSessionTest, MalformedHttpRequestsAreResetByNghttp2)
{
    const std::vector<std::pair<std::string, HeaderList>> cases = {
        {"missing :path", Without(GrpcRequest(), ":path")},
        {"missing :method", Without(GrpcRequest(), ":method")},
        {"connection header", GrpcRequest(kPath, {{"connection", "keep-alive"}})},
        {"te gzip", Replace(GrpcRequest(), "te", "gzip")},
        {"uppercase name", GrpcRequest(kPath, {{"X-Upper", "1"}})},
    };
    for (const auto &[name, headers] : cases)
    {
        SCOPED_TRACE(name);
        // 普通提交会把 X-Upper 变成合法的小写头;只有此场景显式保留非法原名,拒绝断言不变。
        const int32_t id = name == "uppercase name"
                               ? h.client.OpenStreamWithUnnormalizedNames(headers, true)
                               : h.client.OpenStream(headers, true);
        ASSERT_GT(id, 0) << h.client.LastError();
        ASSERT_TRUE(h.Pump());
        const ClientStream &stream = StreamOf(h.client, id);
        EXPECT_EQ(FrameTrace(stream), "R" + std::to_string(h2::kProtocolError));
        EXPECT_TRUE(stream.responseHeaders.empty());
        EXPECT_EQ(h.observer.headerCalls.count(id), 0u);
    }
    EXPECT_TRUE(h.observer.completed.empty());
    EXPECT_TRUE(h.observer.rejected.empty()) << "nghttp2 自己 RST 的请求不经 OnRequestRejected";
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);
}

TEST_F(ServerSessionTest, HeaderListTooLargeIsResourceExhausted)
{
    // 名 + 值 + 32 累计超过 16KiB;请求没收完,回完紧跟 RST_STREAM(NO_ERROR)。
    const int32_t id = h.client.OpenStream(GrpcRequest(kPath, {{"x-big", std::string(17000, 'a')}}), false);
    ASSERT_GT(id, 0) << h.client.LastError();
    ASSERT_TRUE(h.Pump());
    ExpectTrailersOnly(h.client, id,
                       ExpectedTrailersOnly("200", 8, "received header list larger than max (16384)"), false);
    EXPECT_EQ(h.observer.headerCalls.count(id), 0u);
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);
}

TEST_F(ServerSessionTest, MethodMustBePost)
{
    const int32_t complete = h.client.OpenStream(Replace(GrpcRequest(), ":method", "GET"), true);
    const int32_t incomplete = h.client.OpenStream(Replace(GrpcRequest(), ":method", "PUT"), false);
    ASSERT_TRUE(h.Pump());
    const HeaderList expected = ExpectedTrailersOnly("405", 13, "method must be POST");
    ExpectTrailersOnly(h.client, complete, expected, true);
    ExpectTrailersOnly(h.client, incomplete, expected, false);
    EXPECT_TRUE(h.observer.headerCalls.empty());
    // 会话自己的拒绝经 OnRequestRejected 回报给上层计数。
    ASSERT_EQ(h.observer.rejected.count(complete), 1u);
    EXPECT_EQ(h.observer.rejected.at(complete).first, 405);
    EXPECT_EQ(h.observer.rejected.at(complete).second.Code(), StatusCode::kInternal);
    EXPECT_EQ(h.observer.rejected.count(incomplete), 1u);
}

TEST_F(ServerSessionTest, ContentTypeMustBeGrpc)
{
    const std::vector<std::pair<HeaderList, std::string>> rejected = {
        {Replace(GrpcRequest(), "content-type", "text/plain"), "text/plain"},
        {Replace(GrpcRequest(), "content-type", "application/grpcx"), "application/grpcx"},
        {Replace(GrpcRequest(), "content-type", "application/json"), "application/json"},
        {Without(GrpcRequest(), "content-type"), ""},
    };
    for (const auto &[headers, value] : rejected)
    {
        SCOPED_TRACE(value);
        const int32_t id = h.client.OpenStream(headers, true);
        ASSERT_TRUE(h.Pump());
        ExpectTrailersOnly(h.client, id,
                           ExpectedTrailersOnly("415", 3, "invalid gRPC request content-type \"" + value + "\""), true);
        EXPECT_EQ(h.observer.headerCalls.count(id), 0u);
    }
    // application/grpc 之后紧跟结束、'+' 或 ';' 都合法。
    for (const char *value : {"application/grpc", "application/grpc+proto", "application/grpc;charset=utf-8"})
    {
        SCOPED_TRACE(value);
        const int32_t id = SendUnary(Replace(GrpcRequest(), "content-type", value));
        EXPECT_EQ(h.observer.completed.count(id), 1u);
    }
}

TEST_F(ServerSessionTest, MalformedGrpcTimeoutIsBadRequest)
{
    for (const char *value : {"1x", "123456789S", "S", "", "1.5S", "-1S"})
    {
        SCOPED_TRACE(value);
        const int32_t id = h.client.OpenStream(GrpcRequest(kPath, {{"grpc-timeout", value}}), true);
        ASSERT_TRUE(h.Pump());
        ExpectTrailersOnly(h.client, id, ExpectedTrailersOnly("400", 13, "malformed grpc-timeout"), true);
        EXPECT_EQ(h.observer.headerCalls.count(id), 0u);
    }
    // 8 位数字配 H 超出 int64 纳秒:钳到最大值,视为合法(截止的饱和加法由 Server 负责)。
    const int32_t huge = SendUnary(GrpcRequest(kPath, {{"grpc-timeout", "99999999H"}}));
    ASSERT_EQ(h.observer.completed.count(huge), 1u);
    ASSERT_TRUE(h.observer.completed.at(huge).headers.timeout.has_value());
    EXPECT_TRUE(*h.observer.completed.at(huge).headers.timeout == std::chrono::nanoseconds::max());
    // 没有 grpc-timeout = 无截止。
    const int32_t none = SendUnary(GrpcRequest());
    EXPECT_FALSE(h.observer.completed.at(none).headers.timeout.has_value());
}

TEST_F(ServerSessionTest, MalformedBinaryMetadataIsBadRequest)
{
    const int32_t bad = h.client.OpenStream(GrpcRequest(kPath, {{"x-data-bin", "!!!!"}}), true);
    ASSERT_TRUE(h.Pump());
    ExpectTrailersOnly(h.client, bad, ExpectedTrailersOnly("400", 13, "malformed binary metadata \"x-data-bin\""),
                       true);
    EXPECT_EQ(h.observer.headerCalls.count(bad), 0u);

    // 有无 padding、逗号分隔的多值都接受;只解线上这一层。
    const int32_t good = SendUnary(GrpcRequest(
        kPath, {{"x-data-bin", "AAEC"}, {"x-pad-bin", "AAE="}, {"x-multi-bin", "AA==,AQ=="}}));
    ASSERT_EQ(h.observer.completed.count(good), 1u);
    const Metadata expected = {{"x-data-bin", std::string("\x00\x01\x02", 3)},
                               {"x-pad-bin", std::string("\x00\x01", 2)},
                               {"x-multi-bin", std::string("\x00\x01", 2)}};
    EXPECT_EQ(h.observer.completed.at(good).headers.metadata, expected);
}

TEST_F(ServerSessionTest, UnsupportedGrpcEncodingIsUnimplemented)
{
    const int32_t gzip = h.client.OpenStream(GrpcRequest(kPath, {{"grpc-encoding", "gzip"}}), true);
    ASSERT_TRUE(h.Pump());
    ExpectTrailersOnly(h.client, gzip,
                       ExpectedTrailersOnly("200", 12, "unsupported grpc-encoding \"gzip\"",
                                            {{"grpc-accept-encoding", "identity"}}),
                       true);
    EXPECT_EQ(h.observer.headerCalls.count(gzip), 0u);

    const int32_t identity = SendUnary(GrpcRequest(kPath, {{"grpc-encoding", "identity"}}));
    EXPECT_EQ(h.observer.completed.count(identity), 1u);
}

TEST_F(ServerSessionTest, MalformedMethodNameIsUnimplemented)
{
    for (const char *path : {"/nopath", "//CreateScene", "/scene_node.SceneNodeGrpc/", "/a/b/c"})
    {
        SCOPED_TRACE(path);
        const int32_t id = h.client.OpenStream(GrpcRequest(path), true);
        ASSERT_TRUE(h.Pump());
        ExpectTrailersOnly(h.client, id,
                           ExpectedTrailersOnly("200", 12, std::string("malformed method name: \"") + path + "\""),
                           true);
        EXPECT_EQ(h.observer.headerCalls.count(id), 0u);
    }
}

// 服务未知 / 方法未知:会话只负责把 Observer 的 kReject 回成 200 + trailers-only;文案由 Server 拼(这里按 §15.5.3 演示)。
TEST_F(ServerSessionTest, RoutingRejectFromObserver)
{
    MethodTable table;
    ASSERT_TRUE(table.AddUnary(kPath, NoopHandler()));
    h.observer.headersVerdict = [&table](int32_t, const RequestHeaders &headers) -> ServerSession::Verdict
    {
        if (table.Find(headers.path) != nullptr)
        {
            return ServerSession::Verdict{};
        }
        const auto parts = muduo_grpc::SplitMethodPath(headers.path); // 会话保证一定能拆开
        if (!table.HasService(parts->first))
        {
            return RejectVerdict(StatusCode::kUnimplemented, "unknown service " + std::string(parts->first));
        }
        return RejectVerdict(StatusCode::kUnimplemented, "unknown method " + std::string(parts->second) +
                                                             " for service " + std::string(parts->first));
    };

    const int32_t unknownMethod = h.client.OpenStream(GrpcRequest("/scene_node.SceneNodeGrpc/Nope"), false);
    const int32_t unknownService = h.client.OpenStream(GrpcRequest("/other.Service/CreateScene"), true);
    ASSERT_TRUE(h.Pump());
    ExpectTrailersOnly(h.client, unknownMethod,
                       ExpectedTrailersOnly("200", 12, "unknown method Nope for service scene_node.SceneNodeGrpc"),
                       false);
    ExpectTrailersOnly(h.client, unknownService, ExpectedTrailersOnly("200", 12, "unknown service other.Service"),
                       true);

    const int32_t known = SendUnary(GrpcRequest());
    EXPECT_EQ(h.observer.completed.count(known), 1u);
    EXPECT_EQ(h.observer.completed.size(), 1u);
    EXPECT_TRUE(h.observer.rejected.empty()) << "Observer 自己的 kReject 不再经 OnRequestRejected 回报";
}

TEST_F(ServerSessionTest, CompressedFlagIsInternal)
{
    // 请求没收完就立即回(并补 RST_STREAM(NO_ERROR));文案沿用 MessageDeframer。
    const int32_t compressed = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_TRUE(h.client.SendData(compressed, TestH2Peer::Frame("x", 1), false));
    const int32_t invalidFlag = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_TRUE(h.client.SendData(invalidFlag, TestH2Peer::Frame("x", 2), true));
    ASSERT_TRUE(h.Pump());
    ExpectTrailersOnly(
        h.client, compressed,
        ExpectedTrailersOnly("200", 13, "received a compressed message but no message encoding was negotiated"), false);
    ExpectTrailersOnly(h.client, invalidFlag,
                       ExpectedTrailersOnly("200", 13, "invalid compressed-flag byte 2 in message prefix"), true);
    EXPECT_TRUE(h.observer.completed.empty());
}

TEST_F(ServerSessionTest, DeclaredLengthOverLimitIsRejectedImmediately)
{
    // 只发 5 字节前缀、声明 4MiB+1:不等消息体,立即回。
    const int32_t id = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_TRUE(h.client.SendData(id, TestH2Peer::PrefixOnly(4 * 1024 * 1024 + 1), false));
    ASSERT_TRUE(h.Pump());
    ExpectTrailersOnly(h.client, id,
                       ExpectedTrailersOnly("200", 8, "received message larger than max (4194305 vs. 4194304)"),
                       false);
    EXPECT_TRUE(h.observer.completed.empty());
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);
}

TEST(ServerSessionLimits, BufferedRequestBytesArePerConnection)
{
    ServerSessionOptions options;
    options.maxBufferedRequestBytes = 100;
    ServerHarness h(options);
    ASSERT_TRUE(h.Handshake());

    // A:声明 50 字节,先到 5 + 30 = 35 字节。
    const int32_t a = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_TRUE(h.client.SendData(a, TestH2Peer::PrefixOnly(50) + std::string(30, 'a'), false));
    // B:5 + 70 = 75 字节,累计 110 > 100:B 被拒,A 不受影响。
    const int32_t b = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_TRUE(h.client.SendData(b, TestH2Peer::PrefixOnly(80) + std::string(70, 'b'), false));
    ASSERT_TRUE(h.Pump());
    ExpectTrailersOnly(h.client, b, ExpectedTrailersOnly("200", 8, "request buffer limit exceeded"), false);
    EXPECT_TRUE(StreamOf(h.client, a).frames.empty());

    // A 收完:B 的字节已释放,A 的 55 字节在上限内。
    ASSERT_TRUE(h.client.SendData(a, std::string(20, 'a'), true));
    ASSERT_TRUE(h.Pump());
    ASSERT_EQ(h.observer.completed.count(a), 1u);
    EXPECT_EQ(h.observer.completed.at(a).message, std::string(50, 'a'));

    // A 交出之后它的字节也释放了:C 一次带 95 字节照样能收完。
    const int32_t c = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_TRUE(h.client.SendData(c, TestH2Peer::Frame(std::string(90, 'c')), true));
    ASSERT_TRUE(h.Pump());
    EXPECT_EQ(h.observer.completed.count(c), 1u);
}

TEST_F(ServerSessionTest, UnaryRequestMustContainExactlyOneMessage)
{
    const int32_t none = h.client.OpenStream(GrpcRequest(), true);
    const int32_t two = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_TRUE(h.client.SendData(two, TestH2Peer::Frame("a") + TestH2Peer::Frame("b"), true));
    const int32_t partial = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_TRUE(h.client.SendData(partial, TestH2Peer::PrefixOnly(10) + "abc", true));
    ASSERT_TRUE(h.Pump());

    const HeaderList expected = ExpectedTrailersOnly("200", 13, "unary request must contain exactly one message");
    for (const int32_t id : {none, two, partial})
    {
        ExpectTrailersOnly(h.client, id, expected, true);
        EXPECT_EQ(h.observer.headerCalls.count(id), 1u) << "头部合法,已问过 Observer";
    }
    EXPECT_TRUE(h.observer.completed.empty());
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);
}

TEST_F(ServerSessionTest, NonPositiveTimeoutIsDeadlineExceededBeforeDispatch)
{
    const int32_t id = SendUnary(GrpcRequest(kPath, {{"grpc-timeout", "0n"}}));
    ExpectTrailersOnly(h.client, id, ExpectedTrailersOnly("200", 4, "deadline exceeded before dispatch"), true);
    ASSERT_EQ(h.observer.headerCalls.count(id), 1u);
    ASSERT_TRUE(h.observer.headerCalls.at(id).timeout.has_value());
    EXPECT_EQ(h.observer.headerCalls.at(id).timeout->count(), 0);
    EXPECT_TRUE(h.observer.completed.empty()) << "不交给 OnRequestComplete";
    ASSERT_EQ(h.observer.rejected.count(id), 1u);
    EXPECT_EQ(h.observer.rejected.at(id).first, 200);
    EXPECT_EQ(h.observer.rejected.at(id).second.Code(), StatusCode::kDeadlineExceeded);
}

TEST_F(ServerSessionTest, InFlightLimitRejectFromObserver)
{
    h.observer.completeVerdict = [](int32_t, const RequestHeaders &)
    { return RejectVerdict(StatusCode::kResourceExhausted, "server in-flight call limit (1024) reached"); };
    const int32_t id = SendUnary(GrpcRequest());
    EXPECT_EQ(h.observer.completed.count(id), 1u);
    ExpectTrailersOnly(h.client, id,
                       ExpectedTrailersOnly("200", 8, "server in-flight call limit (1024) reached"), true);
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);
    // 被拒的流没有登记,对端再怎么关也不报 aborted;Observer 自己的拒绝也不回报 OnRequestRejected。
    EXPECT_TRUE(h.observer.aborted.empty());
    EXPECT_TRUE(h.observer.rejected.empty());
}

TEST_F(ServerSessionTest, ObserverRejectWithOkStatusBecomesInternal)
{
    h.observer.completeVerdict = [](int32_t, const RequestHeaders &)
    {
        ServerSession::Verdict verdict;
        verdict.kind = ServerSession::Verdict::Kind::kReject;
        return verdict; // 缺陷:kReject 却没给状态
    };
    const int32_t id = SendUnary(GrpcRequest());
    ExpectTrailersOnly(h.client, id,
                       ExpectedTrailersOnly("200", 13, "server rejected the request without a status"), true);
}

// 排空到期之后才开或才收完的流:RST_STREAM(REFUSED_STREAM)。
TEST_F(ServerSessionTest, RefuseVerdictResetsWithRefusedStream)
{
    const std::string refuseAtHeaders = "/scene_node.SceneNodeGrpc/RefuseAtHeaders";
    const std::string refuseAtComplete = "/scene_node.SceneNodeGrpc/RefuseAtComplete";
    h.observer.headersVerdict = [refuseAtHeaders](int32_t, const RequestHeaders &headers)
    { return headers.path == refuseAtHeaders ? RefuseVerdict() : ServerSession::Verdict{}; };
    h.observer.completeVerdict = [refuseAtComplete](int32_t, const RequestHeaders &headers)
    { return headers.path == refuseAtComplete ? RefuseVerdict() : ServerSession::Verdict{}; };

    const int32_t atHeaders = h.client.OpenStream(GrpcRequest(refuseAtHeaders), false);
    ASSERT_GT(atHeaders, 0);
    const int32_t atComplete = SendUnary(GrpcRequest(refuseAtComplete));
    ASSERT_TRUE(h.Pump());

    const std::string refused = "R" + std::to_string(h2::kRefusedStream);
    EXPECT_EQ(FrameTrace(StreamOf(h.client, atHeaders)), refused);
    EXPECT_TRUE(StreamOf(h.client, atHeaders).responseHeaders.empty());
    EXPECT_EQ(FrameTrace(StreamOf(h.client, atComplete)), refused);
    EXPECT_TRUE(StreamOf(h.client, atComplete).responseHeaders.empty());
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);
    EXPECT_TRUE(h.observer.aborted.empty());
}

// 排空到期要立即拒掉的流以会话状态为准(§15.17):头部已被接受、请求还没收完因而未派发、本端也还没应答 —— 按 id 升序。
// 已派发的、头部阶段被拒的、对端已 RST 的都不在其中;拒掉之后流已标记应答,RST 写出之前(仍在会话的流表里)就不再列出,
// 之后收完请求也不再交给上层。
TEST_F(ServerSessionTest, AcceptedUndispatchedStreamsListsOnlyRequestsStillUploading)
{
    const std::string refuseAtHeaders = "/scene_node.SceneNodeGrpc/RefuseAtHeaders";
    h.observer.headersVerdict = [refuseAtHeaders](int32_t, const RequestHeaders &headers)
    { return headers.path == refuseAtHeaders ? RefuseVerdict() : ServerSession::Verdict{}; };
    const std::string frame = TestH2Peer::Frame("payload");

    const int32_t uploading = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_GT(uploading, 0);
    ASSERT_TRUE(h.client.SendData(uploading, std::string_view(frame).substr(0, 3), false));
    const int32_t dispatched = SendUnary(GrpcRequest());
    const int32_t refused = h.client.OpenStream(GrpcRequest(refuseAtHeaders), false);
    ASSERT_GT(refused, 0);
    const int32_t reset = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_GT(reset, 0);
    const int32_t headersOnly = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_GT(headersOnly, 0);
    ASSERT_TRUE(h.Pump());
    ASSERT_TRUE(h.client.SendRstStream(reset, h2::kCancel));
    ASSERT_TRUE(h.Pump());
    ASSERT_EQ(h.observer.completed.count(dispatched), 1u);
    ASSERT_EQ(h.observer.headerCalls.count(reset), 1u);

    EXPECT_EQ(h.server.AcceptedUndispatchedStreams(), (std::vector<int32_t>{uploading, headersOnly}));

    for (const int32_t streamId : h.server.AcceptedUndispatchedStreams())
    {
        EXPECT_TRUE(h.server.RefuseStream(streamId));
    }
    EXPECT_TRUE(h.server.AcceptedUndispatchedStreams().empty());
    // 对端随后把请求传完:流已应答,不再交给上层。
    ASSERT_TRUE(h.client.SendData(uploading, std::string_view(frame).substr(3), true));
    ASSERT_TRUE(h.Pump());
    EXPECT_EQ(h.observer.completed.count(uploading), 0u);
    const std::string refusedTrace = "R" + std::to_string(h2::kRefusedStream);
    EXPECT_EQ(FrameTrace(StreamOf(h.client, uploading)), refusedTrace);
    EXPECT_EQ(FrameTrace(StreamOf(h.client, headersOnly)), refusedTrace);
    EXPECT_TRUE(h.server.AcceptedUndispatchedStreams().empty());
}

// ── metadata 收取规则(§15.5.4) ─────────────────────────────────────────

TEST_F(ServerSessionTest, MetadataKeepsOnlyCustomKeysInWireOrder)
{
    const int32_t id = SendUnary(GrpcRequest(kPath, {{"user-agent", "grpc-go/1.70.0"},
                                                     {"grpc-timeout", "5S"},
                                                     {"grpc-accept-encoding", "gzip"},
                                                     {"grpc-trace-bin", "AAEC"},
                                                     {"x-a", "1"},
                                                     {"x-a", "2"},
                                                     {"x-bin-bin", "AQI"}}));
    ASSERT_EQ(h.observer.completed.count(id), 1u);
    const RequestHeaders &headers = h.observer.completed.at(id).headers;
    EXPECT_EQ(headers.path, kPath);
    const Metadata expected = {{"x-a", "1"}, {"x-a", "2"}, {"x-bin-bin", std::string("\x01\x02", 2)}};
    EXPECT_EQ(headers.metadata, expected) << "伪头、te、content-type、user-agent、grpc-* 都不进 metadata";
    ASSERT_TRUE(headers.timeout.has_value());
    EXPECT_EQ(headers.timeout->count(), int64_t{5'000'000'000});
}

// ── 对端 RST 与恰好一次(§15.5.6、§15.7) ────────────────────────────────

TEST_F(ServerSessionTest, PeerResetAfterDispatchAbortsExactlyOnce)
{
    const int32_t id = SendUnary(GrpcRequest());
    ASSERT_EQ(h.observer.completed.count(id), 1u);
    ASSERT_TRUE(h.client.SendRstStream(id, h2::kCancel));
    ASSERT_TRUE(h.Pump());
    ASSERT_EQ(h.observer.aborted.size(), 1u);
    EXPECT_EQ(h.observer.aborted[0].first, id);
    EXPECT_EQ(h.observer.aborted[0].second, h2::kCancel);
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);

    // 之后的完成一律失败、不产生任何帧。
    EXPECT_FALSE(h.server.SubmitResponse(id, "late"));
    EXPECT_FALSE(h.server.SubmitStatus(id, Status(StatusCode::kInternal, "late")));
    EXPECT_FALSE(h.server.RefuseStream(id));
    ASSERT_TRUE(h.Pump());
    EXPECT_EQ(h.observer.aborted.size(), 1u);
}

TEST_F(ServerSessionTest, PeerResetBeforeRequestCompleteIsSilent)
{
    const int32_t id = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_TRUE(h.client.SendData(id, TestH2Peer::PrefixOnly(10) + "abc", false));
    ASSERT_TRUE(h.Pump());
    ASSERT_EQ(h.observer.headerCalls.count(id), 1u);
    ASSERT_TRUE(h.client.SendRstStream(id, h2::kCancel));
    ASSERT_TRUE(h.Pump());
    EXPECT_TRUE(h.observer.aborted.empty()) << "请求还没交出,直接丢弃";
    EXPECT_TRUE(h.observer.completed.empty());
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);
}

TEST_F(ServerSessionTest, PeerResetAfterResponseSubmittedIsSilent)
{
    const int32_t id = SendUnary(GrpcRequest());
    ASSERT_TRUE(h.server.SubmitResponse(id, "pong")); // 应答已提交、还没写出
    ASSERT_TRUE(h.client.SendRstStream(id, h2::kCancel));
    ASSERT_TRUE(h.Pump());
    EXPECT_TRUE(h.observer.aborted.empty()) << "上层已完成的调用,流再怎么关都不回调";
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);
}

TEST_F(ServerSessionTest, EachStreamIsAnsweredAtMostOnce)
{
    const int32_t id = SendUnary(GrpcRequest());
    EXPECT_FALSE(h.server.SubmitStatus(id, Status::Ok())) << "OK 不能走 trailers-only";
    ASSERT_TRUE(h.server.SubmitStatus(id, Status(StatusCode::kUnavailable, "first")));
    EXPECT_FALSE(h.server.SubmitResponse(id, "second"));
    EXPECT_FALSE(h.server.SubmitStatus(id, Status(StatusCode::kInternal, "third")));
    EXPECT_FALSE(h.server.RefuseStream(id));
    ASSERT_TRUE(h.Pump());
    ExpectTrailersOnly(h.client, id, ExpectedTrailersOnly("200", 14, "first"), true);

    EXPECT_FALSE(h.server.SubmitResponse(9999, "x")) << "未知流";
    EXPECT_FALSE(h.server.SubmitStatus(9999, Status(StatusCode::kInternal, "x")));
    EXPECT_FALSE(h.server.RefuseStream(9999));
    EXPECT_TRUE(h.observer.rejected.empty()) << "上层自己的 SubmitStatus 不经 OnRequestRejected";
}

// ── 两段 GOAWAY(§15.5.6) ──────────────────────────────────────────────

TEST_F(ServerSessionTest, TwoPhaseGoaway)
{
    // A:通知之前已经派发、还没回的调用。
    const int32_t idA = SendUnary(GrpcRequest());
    ASSERT_EQ(h.observer.completed.count(idA), 1u);

    // 第一段:GOAWAY(2^31-1, NO_ERROR) + PING。先只让服务端产出字节,暂不交给客户端。
    h.server.SubmitShutdownNotice();
    EXPECT_FALSE(h.server.FinalGoawaySubmitted());
    const Drained notice = Drain(h.server);
    ASSERT_TRUE(notice.status.ok());
    ASSERT_FALSE(notice.bytes.empty());

    // 通知还在路上时客户端开的新流:照常处理(两段式存在的意义)。
    const int32_t idB = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_GT(idB, 0);
    ASSERT_TRUE(h.client.SendData(idB, TestH2Peer::Frame("b"), true));
    ASSERT_TRUE(Feed(h.server, h.client.TakeOutput()));
    EXPECT_EQ(h.observer.completed.count(idB), 1u);

    // C:客户端已经发出,但等最终 GOAWAY 写出之后才到达服务端。
    const int32_t idC = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_GT(idC, 0);
    ASSERT_TRUE(h.client.SendData(idC, TestH2Peer::Frame("c"), true));
    const std::string bytesC = h.client.TakeOutput();

    // 第二段(生产上由 Server 在 PING ACK 或满 1s 时发;这里先发,只验证会话机制)。
    h.server.SubmitFinalGoaway();
    EXPECT_TRUE(h.server.FinalGoawaySubmitted());
    const Drained finalGoaway = Drain(h.server);
    ASSERT_TRUE(finalGoaway.status.ok());
    ASSERT_FALSE(finalGoaway.bytes.empty());

    // 最终 GOAWAY 写出之后才到的新流:nghttp2 忽略,观察者看不到,也不算连接错误。
    ASSERT_TRUE(Feed(h.server, bytesC));
    EXPECT_EQ(h.observer.headerCalls.count(idC), 0u);
    EXPECT_EQ(h.observer.completed.count(idC), 0u);

    // 两段 GOAWAY 交给客户端:last_stream_id 依次是 2^31-1 与最后处理的流 B。
    ASSERT_TRUE(h.client.Receive(notice.bytes + finalGoaway.bytes)) << h.client.LastError();
    const auto &goaways = h.client.GoawaysReceived();
    ASSERT_EQ(goaways.size(), 2u);
    EXPECT_EQ(goaways[0].lastStreamId, h2::kMaxStreamId);
    EXPECT_EQ(goaways[0].errorCode, h2::kNoError);
    EXPECT_EQ(goaways[1].lastStreamId, idB);
    EXPECT_EQ(goaways[1].errorCode, h2::kNoError);
    EXPECT_EQ(h.client.PingsReceived(), 1u);
    // 线上先 GOAWAY 后 PING(与 grpc-go 一致):ACK 才能证明对端处理过 GOAWAY 之前发来的全部新流。
    const std::vector<uint8_t> &types = h.client.FrameTypes();
    const auto goawayAt = std::find(types.begin(), types.end(), h2::kFrameGoaway);
    const auto pingAt = std::find(types.begin(), types.end(), h2::kFramePing);
    ASSERT_NE(goawayAt, types.end());
    ASSERT_NE(pingAt, types.end());
    EXPECT_LT(goawayAt - types.begin(), pingAt - types.begin());
    // 客户端按 last_stream_id 把 C 判为未处理(nghttp2 以 REFUSED_STREAM 关流)。
    EXPECT_TRUE(StreamOf(h.client, idC).closed);
    EXPECT_EQ(StreamOf(h.client, idC).closeErrorCode, h2::kRefusedStream);

    // 客户端自动回 PING ACK:恰好一次 OnShutdownPingAck。
    ASSERT_TRUE(h.Pump());
    EXPECT_EQ(h.observer.shutdownPingAcks, 1);
    // 重复提交两段都无副作用。
    h.server.SubmitShutdownNotice();
    h.server.SubmitFinalGoaway();
    ASSERT_TRUE(h.Pump());
    EXPECT_EQ(h.client.GoawaysReceived().size(), 2u);
    EXPECT_EQ(h.observer.shutdownPingAcks, 1);

    // A、B 在最终 GOAWAY 之后照常回完;没有打开的流之后会话要求关闭。
    EXPECT_FALSE(h.server.ShouldClose());
    ASSERT_TRUE(h.server.SubmitResponse(idA, "a"));
    ASSERT_TRUE(h.server.SubmitResponse(idB, "b"));
    ASSERT_TRUE(h.Pump());
    EXPECT_EQ(FrameTrace(StreamOf(h.client, idA)), "H D H!");
    EXPECT_EQ(FrameTrace(StreamOf(h.client, idB)), "H D H!");
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);
    EXPECT_TRUE(h.server.ShouldClose());
    EXPECT_TRUE(h.observer.aborted.empty());
}

// 最终 GOAWAY 已提交、还没写出(生产上是输出被背压)时到达的新流:GOAWAY 写出时 nghttp2 按 last_stream_id
// 把它以 REFUSED_STREAM 关掉、客户端判为未处理并透明重试,所以绝不能交给 Observer —— 否则业务可能已执行、
// 应答却随流被丢弃,重试即执行两次。
TEST_F(ServerSessionTest, StreamArrivingBeforeFinalGoawayIsWrittenIsNotDispatched)
{
    const int32_t idA = SendUnary(GrpcRequest());
    ASSERT_EQ(h.observer.completed.count(idA), 1u);

    h.server.SubmitFinalGoaway(); // 只提交,暂不 Drain
    const int32_t idD = h.client.OpenStream(GrpcRequest(), false);
    ASSERT_GT(idD, 0);
    ASSERT_TRUE(h.client.SendData(idD, TestH2Peer::Frame("d"), true));
    ASSERT_TRUE(Feed(h.server, h.client.TakeOutput()));
    EXPECT_EQ(h.observer.headerCalls.count(idD), 0u);
    EXPECT_EQ(h.observer.completed.count(idD), 0u);

    ASSERT_TRUE(h.Pump());
    const auto &goaways = h.client.GoawaysReceived();
    ASSERT_EQ(goaways.size(), 1u);
    EXPECT_EQ(goaways[0].lastStreamId, idA);
    EXPECT_EQ(goaways[0].errorCode, h2::kNoError);
    const ClientStream &d = StreamOf(h.client, idD);
    EXPECT_TRUE(d.frames.empty()) << "不回应,也不发 RST";
    EXPECT_TRUE(d.closed);
    EXPECT_EQ(d.closeErrorCode, h2::kRefusedStream) << "客户端按 last_stream_id 判为未处理";
    EXPECT_TRUE(h.observer.aborted.empty()) << "没交给上层的流被关闭不报 aborted";
    EXPECT_TRUE(h.observer.rejected.empty()) << "不算本层拒绝";

    ASSERT_TRUE(h.server.SubmitResponse(idA, "a"));
    ASSERT_TRUE(h.Pump());
    EXPECT_EQ(FrameTrace(StreamOf(h.client, idA)), "H D H!");
    EXPECT_EQ(h.server.OpenStreamCount(), 0u);
    EXPECT_TRUE(h.server.ShouldClose());
}

// ── rapid reset(CVE-2023-44487,§15.5.2) ─────────────────────────────────

TEST_F(ServerSessionTest, RapidResetTriggersGoawayAndConnectionError)
{
    // nghttp2 的 RST 令牌桶:初始 1000 个,每秒补 33 个。1500 对 HEADERS + RST 必然耗尽。
    std::string toClient;
    bool connectionError = false;
    for (int i = 0; i < 1500 && !connectionError; ++i)
    {
        const int32_t id = h.client.OpenStream(GrpcRequest(), false);
        ASSERT_GT(id, 0) << h.client.LastError();
        ASSERT_TRUE(h.client.SendRstStream(id, h2::kCancel)) << h.client.LastError();
        ASSERT_TRUE(Feed(h.server, h.client.TakeOutput())) << "iteration " << i;
        const Drained drained = Drain(h.server);
        toClient += drained.bytes;
        connectionError = !drained.status.ok();
    }
    EXPECT_TRUE(connectionError) << "rapid reset was never reported by DrainOutput";

    ASSERT_TRUE(h.client.Receive(toClient)) << h.client.LastError();
    ASSERT_FALSE(h.client.GoawaysReceived().empty());
    EXPECT_EQ(h.client.GoawaysReceived().back().errorCode, h2::kInternalError);
    EXPECT_TRUE(h.observer.aborted.empty()) << "没派发过的流被 RST 不报 aborted";
    EXPECT_TRUE(h.observer.completed.empty());
}

// ── 协议错误由 DrainOutput 报告 ─────────────────────────────────────────

TEST(ServerSessionProtocolError, FrameLevelViolationIsReportedOnceByDrainOutput)
{
    RecordingServerObserver observer;
    ServerSession server(ServerSessionOptions{}, observer);
    ASSERT_TRUE(server.Start().ok());
    // 合法 magic 之后第一帧不是 SETTINGS(这里是一个 PING):nghttp2 排 GOAWAY(PROTOCOL_ERROR),Receive 本身返回 OK。
    std::string bytes = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n";
    const char ping[] = {0, 0, 8, 6, 0, 0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8};
    bytes.append(ping, sizeof(ping));
    ASSERT_TRUE(Feed(server, bytes));

    const Drained first = Drain(server);
    EXPECT_FALSE(first.status.ok()) << "写出带错误码的 GOAWAY 时报告连接错误";
    EXPECT_FALSE(first.bytes.empty()) << "GOAWAY 的字节已交给 write";
    EXPECT_TRUE(server.ShouldClose());
    const Drained second = Drain(server);
    EXPECT_TRUE(second.status.ok()) << "只报告一次";
    EXPECT_TRUE(second.bytes.empty());
    EXPECT_EQ(observer.clientSettings, 0);
}

} // namespace
