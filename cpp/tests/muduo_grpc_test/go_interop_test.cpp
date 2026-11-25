// 与真实 Go client_rpc_router 的互通用例(docs/design/grpc-muduo-transport.md §11:第 1 批验证)。
//
// 默认跳过:设 MUDUO_GRPC_INTEROP_ROUTER=ip:port 才跑(本地默认 127.0.0.1:50600,
// 见 go/client_rpc_router/etc/client_rpc_router.yaml 的 ListenOn)。
//
// 只发**无副作用**的请求(go/client_rpc_router/internal/logic/forwardlogic.go):
//   * 不带 x-session-detail-bin:第 1 步直接回 UNAUTHENTICATED(16),不查表、不转发;
//   * 带会话但 ClientRequest.message_id 不在生成路由表里:第 2 步回 OK + MessageContent.error_message
//     = tip 1005(constants.ErrInvalidParameter = table.CommonError_kInvalidParameter),不选实例、不转发到任何业务服务;
//     第 6 步的响应头回写在第 2 步之前登记,所以初始 metadata 里一定有 x-session-detail-bin。
//
// 测试不链 protobuf:请求与响应按 proto 字段号手工编解码。
//   ForwardRequest { ClientRequest request = 1; uint32 zone_id = 2; }        proto/client_rpc_router/client_rpc_router.proto
//   ClientRequest  { uint64 id = 1; ...; uint32 message_id = 5; }             proto/common/base/message.proto
//   MessageContent { bytes serialized_message = 1; uint32 message_id = 2; uint64 id = 3; TipInfoMessage error_message = 4; }
//                                                                             proto/common/base/message.proto
//   TipInfoMessage { uint32 id = 1; repeated string parameters = 2; }        proto/common/base/tip.proto
//   SessionDetails { uint32 session_id = 1; uint64 player_id = 2; uint32 gate_node_id = 3; string gate_instance_id = 4; ... }
//                                                                             proto/common/base/session.proto
//
// 空闲用例再设 MUDUO_GRPC_INTEROP_IDLE=1 才跑(约 6 分钟):go-zero 服务端 MaxConnectionIdle = 5min 会发两段式 GOAWAY,
// 之后的下一次调用仍须成功。run_cpp_tests.ps1 默认 120s 超时,这个用例要直接跑 exe(见交付说明)。

#include <gtest/gtest.h>

#include <algorithm>
#include <charconv>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <deque>
#include <functional>
#include <iostream>
#include <map>
#include <memory>
#include <optional>
#include <random>
#include <set>
#include <string>
#include <string_view>
#include <unordered_map>
#include <utility>
#include <vector>

#include "muduo_test_loop.h"

// muduo 头放在所有标准库头之后(它的 Windows 适配头 #define 了 off_t / pid_t 等名字)。
#include "muduo_grpc/channel.h"
#include "muduo_grpc/metadata.h"
#include "muduo_grpc/status.h"

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

namespace
{

using muduo_grpc::Channel;
using muduo_grpc::ConnectivityState;
using muduo_grpc::StatusCode;
using muduo_grpc::UnaryCallRequest;
using muduo_grpc::UnaryCallResult;
using namespace std::chrono_literals;

constexpr char kForwardMethod[] = "/client_rpc_router.ClientRpcRouter/Forward";
constexpr char kSessionKey[] = "x-session-detail-bin";
// 路由表里不存在的消息号(go/client_rpc_router/generated/pb/game/message_id.go 现有消息号都在 300 以内)。
constexpr uint32_t kUnknownMessageId = 0x7FFFFFF0u;
// table.CommonError_kInvalidParameter(go/shared/generated/pb/table/common_error_tip.pb.go)。
constexpr uint32_t kInvalidParameterTip = 1005;
constexpr uint32_t kZoneId = 1;

// ── protobuf 手工编解码:只覆盖本测试涉及的 varint 与 length-delimited ──

constexpr uint32_t kWireVarint = 0;
constexpr uint32_t kWireLengthDelimited = 2;

void AppendVarint(std::string &out, uint64_t value)
{
    while (value >= 0x80)
    {
        out.push_back(static_cast<char>((value & 0x7F) | 0x80));
        value >>= 7;
    }
    out.push_back(static_cast<char>(value));
}

void AppendTag(std::string &out, uint32_t field, uint32_t wireType)
{
    AppendVarint(out, (static_cast<uint64_t>(field) << 3) | wireType);
}

void AppendVarintField(std::string &out, uint32_t field, uint64_t value)
{
    AppendTag(out, field, kWireVarint);
    AppendVarint(out, value);
}

void AppendBytesField(std::string &out, uint32_t field, std::string_view bytes)
{
    AppendTag(out, field, kWireLengthDelimited);
    AppendVarint(out, bytes.size());
    out.append(bytes.data(), bytes.size());
}

std::optional<uint64_t> ReadVarint(std::string_view &in)
{
    uint64_t value = 0;
    for (int shift = 0; shift < 64; shift += 7)
    {
        if (in.empty())
        {
            return std::nullopt;
        }
        const auto byte = static_cast<uint8_t>(in.front());
        in.remove_prefix(1);
        value |= static_cast<uint64_t>(byte & 0x7F) << shift;
        if ((byte & 0x80) == 0)
        {
            return value;
        }
    }
    return std::nullopt;
}

struct ProtoField
{
    uint32_t number{0};
    uint32_t wireType{0};
    uint64_t varint{0};
    std::string bytes;
};

std::optional<std::vector<ProtoField>> ParseFields(std::string_view in)
{
    std::vector<ProtoField> fields;
    while (!in.empty())
    {
        const auto tag = ReadVarint(in);
        if (!tag)
        {
            return std::nullopt;
        }
        ProtoField field;
        field.number = static_cast<uint32_t>(*tag >> 3);
        field.wireType = static_cast<uint32_t>(*tag & 0x7);
        if (field.wireType == kWireVarint)
        {
            const auto value = ReadVarint(in);
            if (!value)
            {
                return std::nullopt;
            }
            field.varint = *value;
        }
        else if (field.wireType == kWireLengthDelimited)
        {
            const auto length = ReadVarint(in);
            if (!length || *length > in.size())
            {
                return std::nullopt;
            }
            field.bytes = std::string(in.substr(0, static_cast<size_t>(*length)));
            in.remove_prefix(static_cast<size_t>(*length));
        }
        else
        {
            return std::nullopt; // 这几个消息里没有 fixed32 / fixed64
        }
        fields.push_back(std::move(field));
    }
    return fields;
}

std::string EncodeSessionDetails(uint32_t sessionId, uint64_t playerId, uint32_t gateNodeId,
                                 std::string_view gateInstanceId)
{
    std::string out;
    AppendVarintField(out, 1, sessionId);
    AppendVarintField(out, 2, playerId);
    AppendVarintField(out, 3, gateNodeId);
    AppendBytesField(out, 4, gateInstanceId);
    return out;
}

std::string EncodeForwardRequest(uint64_t requestId, uint32_t messageId, uint32_t zoneId)
{
    std::string clientRequest;
    AppendVarintField(clientRequest, 1, requestId);
    AppendVarintField(clientRequest, 5, messageId);
    std::string forward;
    AppendBytesField(forward, 1, clientRequest);
    AppendVarintField(forward, 2, zoneId);
    return forward;
}

struct DecodedMessageContent
{
    uint64_t id{0};
    uint32_t messageId{0};
    std::string serializedMessage;
    bool hasError{false};
    uint32_t tipId{0};
};

std::optional<DecodedMessageContent> DecodeMessageContent(std::string_view bytes)
{
    const auto fields = ParseFields(bytes);
    if (!fields)
    {
        return std::nullopt;
    }
    DecodedMessageContent content;
    for (const auto &field : *fields)
    {
        switch (field.number)
        {
        case 1: content.serializedMessage = field.bytes; break;
        case 2: content.messageId = static_cast<uint32_t>(field.varint); break;
        case 3: content.id = field.varint; break;
        case 4:
        {
            content.hasError = true;
            const auto tip = ParseFields(field.bytes);
            if (!tip)
            {
                return std::nullopt;
            }
            for (const auto &tipField : *tip)
            {
                if (tipField.number == 1 && tipField.wireType == kWireVarint)
                {
                    content.tipId = static_cast<uint32_t>(tipField.varint);
                }
            }
            break;
        }
        default: break;
        }
    }
    return content;
}

// gate 的应用层值:SessionDetails 原始字节的标准 base64(带 padding),与旧生成客户端的 Base64Encode 相同。
std::string Base64EncodePadded(std::string_view raw)
{
    std::string encoded = muduo_grpc::Base64EncodeUnpadded(raw);
    encoded.append((4 - encoded.size() % 4) % 4, '=');
    return encoded;
}

std::optional<std::pair<std::string, uint16_t>> ParseRouterAddress(std::string_view raw)
{
    const size_t colon = raw.rfind(':');
    if (colon == std::string_view::npos || colon == 0 || colon + 1 >= raw.size())
    {
        return std::nullopt;
    }
    const std::string_view portText = raw.substr(colon + 1);
    uint32_t port = 0;
    const auto [ptr, ec] = std::from_chars(portText.data(), portText.data() + portText.size(), port);
    if (ec != std::errc{} || ptr != portText.data() + portText.size() || port == 0 || port > 65535)
    {
        return std::nullopt;
    }
    return std::make_pair(std::string(raw.substr(0, colon)), static_cast<uint16_t>(port));
}

struct CallRecord
{
    int count{0};
    UnaryCallResult result;
};
using CallRecordPtr = std::shared_ptr<CallRecord>;

muduo_grpc::UnaryCallback Recorder(const CallRecordPtr &record)
{
    return [record](UnaryCallResult &&result)
    {
        ++record->count;
        record->result = std::move(result);
    };
}

class GoRouterInteropTest : public ::testing::Test
{
protected:
    static void SetUpTestSuite()
    {
        muduo::Logger::setLogLevel(muduo::Logger::WARN);
        muduo_grpc_test::EnsureSocketsInitialized();
    }

    void SetUp() override
    {
        const char *raw = std::getenv("MUDUO_GRPC_INTEROP_ROUTER");
        if (raw == nullptr || *raw == '\0')
        {
            GTEST_SKIP() << "MUDUO_GRPC_INTEROP_ROUTER (ip:port of a running Go client_rpc_router) is not set";
        }
        const auto address = ParseRouterAddress(raw);
        ASSERT_TRUE(address.has_value()) << "MUDUO_GRPC_INTEROP_ROUTER must be ip:port, got '" << raw << "'";
        channel = Channel::Create(loop, address->first, address->second);
        ASSERT_NE(channel, nullptr) << "only IPv4 literals are supported, got '" << raw << "'";
        // 任意合法的 SessionDetails;10 字节 → base64 带 "==" padding,顺带覆盖 padding 的往返。
        sessionValue = Base64EncodePadded(EncodeSessionDetails(1, 2, 3, "ab"));
        ASSERT_EQ(sessionValue, "CAEQAhgDIgJhYg==");
    }

    void TearDown() override
    {
        if (!channel)
        {
            return; // 跳过的用例没有建通道,也就没有要转空的 loop
        }
        channel.reset();
        // 让排队的完成回调、连接释放、forceClose、connectDestroyed 跑完,再析构 EventLoop。
        muduo_grpc_test::RunLoopFor(loop, 500ms);
    }

    UnaryCallRequest MakeForward(uint64_t requestId, bool withSession) const
    {
        UnaryCallRequest request;
        request.method = kForwardMethod;
        request.message = EncodeForwardRequest(requestId, kUnknownMessageId, kZoneId);
        if (withSession)
        {
            request.metadata.emplace_back(kSessionKey, sessionValue);
        }
        request.deadline = std::chrono::steady_clock::now() + 5s;
        return request;
    }

    CallRecordPtr Call(UnaryCallRequest request)
    {
        const auto record = std::make_shared<CallRecord>();
        channel->StartUnaryCall(std::move(request), Recorder(record));
        EXPECT_TRUE(muduo_grpc_test::RunLoopUntil(loop, [&] { return record->count > 0; }, 10s))
            << "call did not complete";
        return record;
    }

    // 校验一条「未知消息号」应答:OK、会话头逐字节回显、MessageContent 带 tip 1005 且 id 回显。
    void ExpectRejectedEnvelope(const UnaryCallResult &result, uint64_t requestId) const
    {
        ASSERT_TRUE(result.status.ok()) << result.status.ToString();
        const std::string *echoed = muduo_grpc::FindMetadata(result.initialMetadata, kSessionKey);
        ASSERT_NE(echoed, nullptr) << "路由服必须在响应头回写 " << kSessionKey;
        EXPECT_EQ(*echoed, sessionValue) << "应用层值逐字节往返(传输层只解线上那一层)";
        const auto content = DecodeMessageContent(result.response);
        ASSERT_TRUE(content.has_value()) << "response is not a valid MessageContent";
        EXPECT_EQ(content->id, requestId);
        EXPECT_EQ(content->messageId, kUnknownMessageId);
        EXPECT_TRUE(content->serializedMessage.empty());
        ASSERT_TRUE(content->hasError);
        EXPECT_EQ(content->tipId, kInvalidParameterTip);
    }

    muduo::net::EventLoop loop;
    std::shared_ptr<Channel> channel;
    std::string sessionValue;
};

TEST_F(GoRouterInteropTest, MissingSessionMetadataIsUnauthenticated)
{
    const auto record = Call(MakeForward(41, false));
    ASSERT_EQ(record->count, 1);
    const auto &status = record->result.status;
    EXPECT_EQ(status.Code(), StatusCode::kUnauthenticated) << status.ToString();
    EXPECT_EQ(status.code_value(), 16u);
    EXPECT_NE(status.message().find(kSessionKey), std::string::npos) << status.message();
    EXPECT_TRUE(record->result.initialMetadata.empty()) << "grpc-go 对还没写过响应头的错误回 trailers-only";
}

TEST_F(GoRouterInteropTest, UnknownMessageIdReturnsTipAndEchoesSessionHeader)
{
    const auto record = Call(MakeForward(42, true));
    ASSERT_EQ(record->count, 1);
    ExpectRejectedEnvelope(record->result, 42);
}

TEST_F(GoRouterInteropTest, TwoHundredConcurrentCallsAllSucceed)
{
    constexpr int kCalls = 200;
    constexpr uint64_t kFirstId = 10000;
    std::vector<CallRecordPtr> records;
    for (int i = 0; i < kCalls; ++i)
    {
        records.push_back(std::make_shared<CallRecord>());
        channel->StartUnaryCall(MakeForward(kFirstId + i, true), Recorder(records.back()));
    }
    const auto allDone = [&]
    {
        return std::all_of(records.begin(), records.end(), [](const CallRecordPtr &r) { return r->count > 0; });
    };
    ASSERT_TRUE(muduo_grpc_test::RunLoopUntil(loop, allDone, 20s));
    for (int i = 0; i < kCalls; ++i)
    {
        SCOPED_TRACE("call " + std::to_string(i));
        EXPECT_EQ(records[i]->count, 1);
        // 每个应答回显自己的 id:并发流之间没有串号。
        ExpectRejectedEnvelope(records[i]->result, kFirstId + i);
    }
}

TEST_F(GoRouterInteropTest, CallAfterServerIdleTimeoutStillSucceeds)
{
    const char *idle = std::getenv("MUDUO_GRPC_INTEROP_IDLE");
    if (idle == nullptr || std::string_view(idle) != "1")
    {
        GTEST_SKIP() << "set MUDUO_GRPC_INTEROP_IDLE=1 to run the 6-minute idle case";
    }
    const auto first = Call(MakeForward(51, true));
    ExpectRejectedEnvelope(first->result, 51);

    muduo_grpc_test::RunLoopFor(loop, 6min);
    // 收到 go-zero 的空闲 GOAWAY 后连接被退役,通道回 IDLE;只打印,不作断言(服务端配置可能不同)。
    std::cout << "[ INFO     ] channel state after 6 minutes idle: "
              << muduo_grpc::ConnectivityStateName(channel->GetState(false)) << std::endl;

    const auto second = Call(MakeForward(52, true));
    ExpectRejectedEnvelope(second->result, 52);
}

} // namespace
