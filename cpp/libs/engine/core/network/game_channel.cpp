#include "game_channel.h"
#include <boost/get_pointer.hpp>
#include <google/protobuf/descriptor.h>
#include <sstream>
#include "muduo/base/Logging.h"
#include "muduo/net/TcpConnection.h"
#include "network/message_statistics.h"
#include "network/traffic_statistics.h"
#include "proto/common/base/empty.pb.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "core/utils/stat/stat.h"
#include "network/codec/message_response_dispatcher.h"
#include "core/utils/debug/stacktrace_system.h"
#include "error_reporter/error_reporter.h"
#include "thread_context/trace_context_tls.h"
#include <thread_context/rpc_request_context.h>

using namespace std::placeholders;

// ====================== Global Variables ======================
// Threshold for the "large RPC" dump. Set above the legitimate enter-scene
// AOI batch (SceneSceneClientPlayerNotifyActorListCreate, which can be ~2-3KB
// for a populated grid) so it does not trigger on normal traffic. A real
// concern is messages in the tens of KB.
constexpr size_t kMaxMessageByteSize = 4096;

void HandleUnknownProtobufMessage(const TcpConnectionPtr &, const MessagePtr &message, muduo::Timestamp)
{
    LOG_ERROR << "Unknown Protobuf message received: " << message->GetTypeName().data();
}

// Temporary debug switch: set to true at runtime to dump all oversized RPC bodies to file.
// Usage: set gDumpOversizedRpc = true in debugger, or call EnableOversizedRpcDump(true).
// Default OFF -- enter-scene AOI batches and similar legitimately-large traffic
// would otherwise fill the dump file under load.
bool gDumpOversizedRpc = false;

void EnableOversizedRpcDump(bool enable) { gDumpOversizedRpc = enable; }

size_t LogIfMessageTooLarge(const GameRpcMessage &rpcMessage)
{
    const size_t messageSize = rpcMessage.ByteSizeLong();
    if (messageSize <= kMaxMessageByteSize)
        return messageSize;

    const auto msgId = rpcMessage.message_id();
    const char *methodName = "(unknown)";
    std::string decodedBody;

    if (msgId < gRpcMethodRegistry.size())
    {
        const auto &meta = gRpcMethodRegistry[msgId];

        if (meta.methodName)
            methodName = meta.methodName;

        const bool isRequest = !rpcMessage.request().empty();
        const auto &body = isRequest ? rpcMessage.request() : rpcMessage.response();
        const auto *proto = isRequest ? meta.requestProto.get() : meta.responseProto.get();

        if (proto && !body.empty())
        {
            std::unique_ptr<::google::protobuf::Message> decoded(proto->New());
            if (decoded->ParsePartialFromArray(body.data(), static_cast<int32_t>(body.size())))
                decodedBody = decoded->ShortDebugString();
        }
    }

    if (gDumpOversizedRpc && !decodedBody.empty())
    {
        FILE *f = fopen("logs/oversized_rpc.log", "a");
        if (f)
        {
            std::time_t now = std::time(nullptr);
            char timeBuf[32];
            std::strftime(timeBuf, sizeof(timeBuf), "%Y-%m-%d %H:%M:%S", std::localtime(&now));
            fprintf(f, "[%s] method: %s, msg_id: %u, size: %zu, body(%zu): ",
                    timeBuf, methodName, msgId, messageSize, decodedBody.size());
            fwrite(decodedBody.data(), 1, decodedBody.size(), f);
            fputc('\n', f);
            fclose(f);
        }
    }

    // muduo log: short summary only (safe within 4000-byte buffer).
    // WARN, not ERROR -- a few KB is informational, not a fault. Real anomalies
    // are in the tens of KB and stand out either way.
    LOG_WARN << "RPC message size exceeds threshold (" << kMaxMessageByteSize << " bytes)"
             << ", method: " << methodName
             << ", message ID: " << msgId
             << ", size: " << messageSize;

    return messageSize;
}

// Global response dispatcher
MessageResponseDispatcher gRpcResponseDispatcher(std::bind(&HandleUnknownProtobufMessage, _1, _2, _3));

// ====================== GameChannel Implementation ======================

GameChannel::GameChannel()
    : codec_(std::bind(&GameChannel::HandleRpcMessage, this, _1, _2, _3)),
      services_(nullptr),
      dispatcher_(std::bind(&HandleUnknownProtobufMessage, _1, _2, _3))
{
    LOG_DEBUG << "GameChannel created: " << this;
}

GameChannel::GameChannel(const TcpConnectionPtr &connection)
    : codec_(std::bind(&GameChannel::HandleRpcMessage, this, _1, _2, _3)),
      connection_(connection),
      services_(nullptr),
      dispatcher_(std::bind(&HandleUnknownProtobufMessage, _1, _2, _3))
{
    LOG_DEBUG << "GameChannel created with connection: " << this;
}

GameChannel::~GameChannel()
{
    LOG_DEBUG << "GameChannel destroyed: " << this;
}

// ====================== Private Methods ======================

bool GameChannel::IsValidMessageId(uint32_t messageId) const
{
    if (messageId >= gRpcMethodRegistry.size())
    {
        LOG_ERROR << "Invalid message ID: " << messageId
                  << " (valid range: 0 to " << gRpcMethodRegistry.size() << ")";
        return false;
    }
    return true;
}

bool GameChannel::SerializeMessage(const ProtobufMessage &message, std::string *output) const
{
    output->resize(message.ByteSizeLong());
    return message.SerializePartialToArray(output->data(), static_cast<int32_t>(output->size()));
}

void GameChannel::SendRpcRequestMessage(GameMessageType type, uint32_t messageId, const ProtobufMessage *content)
{
    if (!IsValidMessageId(messageId))
        return;

    GameRpcMessage rpcMessage;
    rpcMessage.set_type(type);
    rpcMessage.set_message_id(messageId);

    if (content && !SerializeMessage(*content, rpcMessage.mutable_request()))
    {
        LOG_ERROR << "Failed to serialize message for ID: " << messageId;
        return;
    }

    const auto rpcSize = static_cast<uint32_t>(LogIfMessageTooLarge(rpcMessage));

    codec_.send(connection_, rpcMessage);

    LogMessageStatistics(rpcMessage, rpcSize);
    TrafficStatsCollector::Instance().RecordSend(messageId, rpcSize);
}

void GameChannel::SendRpcResponseMessage(GameMessageType type, uint32_t messageId, const ProtobufMessage *content)
{
    if (!IsValidMessageId(messageId))
        return;

    GameRpcMessage rpcMessage;
    rpcMessage.set_type(type);
    rpcMessage.set_message_id(messageId);

    if (content && !SerializeMessage(*content, rpcMessage.mutable_response()))
    {
        LOG_ERROR << "Failed to serialize message for ID: " << messageId;
        return;
    }

    const auto rpcSize = static_cast<uint32_t>(LogIfMessageTooLarge(rpcMessage));

    codec_.send(connection_, rpcMessage);

    LogMessageStatistics(rpcMessage, rpcSize);
    TrafficStatsCollector::Instance().RecordSend(messageId, rpcSize);
}

void GameChannel::LogMessageStatistics(const GameRpcMessage &message, uint32_t cachedSize) const
{
    if (!gFeatureSwitches[kTestMessageStatistics])
        return;

    uint32_t messageId = message.message_id();
    if (messageId >= kMaxRpcMethodCount)
    {
        LOG_WARN << "Message statistics skipped, messageId out of range: " << messageId
                 << " (max: " << kMaxRpcMethodCount << ")";
        return;
    }

    uint64_t messageSize = cachedSize;

    auto now = std::chrono::steady_clock::now();
    auto &statistic = gMessageStatistics[messageId];

    // Initialize start time on first record
    if (gStartTimes[messageId].time_since_epoch().count() <= 0)
    {
        gStartTimes[messageId] = now;
    }

    statistic.set_count(statistic.count() + 1);
    statistic.set_flow_rate_total(statistic.flow_rate_total() + messageSize);
    gTotalFlow += messageSize;

    auto duration = std::chrono::duration_cast<std::chrono::seconds>(now - gStartTimes[messageId]).count();
    if (duration > 0)
    {
        uint64_t flowRatePerSecond = statistic.flow_rate_total() / duration;
        statistic.set_flow_rate_second(flowRatePerSecond);
    }

    LOG_INFO << "Message ID: " << messageId
             << ", Count: " << statistic.count()
             << ", Total Flow: " << statistic.flow_rate_total()
             << " bytes, Flow Rate: " << statistic.flow_rate_second()
             << " bytes/sec, Duration: " << duration << " sec";
}

void GameChannel::StartMessageStatistics()
{
    gFeatureSwitches[kTestMessageStatistics] = true;
    gMessageStatistics.fill(MessageStatistics{});
    gStartTimes.fill(std::chrono::steady_clock::time_point{});
    gTotalFlow = 0;

    LOG_INFO << "Message statistics started.";
}

void GameChannel::StopMessageStatistics()
{
    gFeatureSwitches[kTestMessageStatistics] = false;

    std::cout << "Final statistics:" << std::endl;
    for (uint32_t messageId = 0; messageId < gMessageStatistics.size(); ++messageId)
    {
        const auto &stats = gMessageStatistics[messageId];
        LOG_INFO << "Message ID: " << messageId
                 << ", Count: " << stats.count()
                 << ", Total Flow: " << stats.flow_rate_total()
                 << " bytes, Flow Rate: " << stats.flow_rate_second()
                 << " bytes/sec";
    }

    LOG_INFO << "Total Flow of All Messages: " << gTotalFlow << " bytes";
    gMessageStatistics.fill(MessageStatistics{});
    gStartTimes.fill(std::chrono::steady_clock::time_point{});

    LOG_INFO << "Message statistics stopped.";
}

// ====================== Public Methods ======================

void GameChannel::CallRemoteMethod(const uint32_t messageId, const ProtobufMessage &request)
{
    if (!IsValidMessageId(messageId))
    {
        LOG_ERROR << "Failed to validate message ID for remote method call: " << messageId;
        return;
    }

    GameRpcMessage rpcMessage;
    rpcMessage.set_type(GameMessageType::REQUEST);
    rpcMessage.set_message_id(messageId);

    if (!SerializeMessage(request, rpcMessage.mutable_request()))
    {
        LOG_ERROR << "Failed to serialize request for message ID: " << messageId;
        return;
    }

    LOG_TRACE << "Sending remote method call, message ID: " << messageId;
    SendGameRpcMessage(rpcMessage);
}

void GameChannel::SendRequest(uint32_t messageId, const ProtobufMessage &request)
{
    SendRpcRequestMessage(GameMessageType::REQUEST, messageId, &request);
}

void GameChannel::RouteMessageToNode(uint32_t messageId, const ProtobufMessage &request)
{
    SendRpcRequestMessage(GameMessageType::NODE_ROUTE, messageId, &request);
}

void GameChannel::HandleIncomingMessage(const TcpConnectionPtr &connection, muduo::net::Buffer *buffer, muduo::Timestamp receiveTime)
{
    codec_.onMessage(connection, buffer, receiveTime);
}

// 2KB threshold for oversized-message logging

void GameChannel::HandleRpcMessage(const TcpConnectionPtr &conn, const RpcMessagePtr &messagePtr, muduo::Timestamp receiveTime)
{
    assert(conn == connection_);
    const auto &rpcMessage = *messagePtr;

    const size_t messageSize = LogIfMessageTooLarge(rpcMessage);

    LOG_TRACE << "RPC message received, type: " << rpcMessage.type() << ", message ID: " << rpcMessage.message_id();

    TrafficStatsCollector::Instance().RecordRecv(rpcMessage.message_id(), static_cast<uint32_t>(messageSize));

    tlsRpc.conn = conn;

    switch (rpcMessage.type())
    {
    case GameMessageType::RESPONSE:
        HandleResponseMessage(conn, rpcMessage, receiveTime);
        break;
    case GameMessageType::REQUEST:
    case GameMessageType::RPC_CLIENT_REQUEST:
        HandleRequestMessage(conn, rpcMessage, receiveTime);
        break;
    case GameMessageType::NODE_ROUTE:
        HandleNodeRouteMessage(conn, rpcMessage, receiveTime);
        break;
    case GameMessageType::RPC_ERROR:
    {
        // todo.md #70 + #125: on error path, log the triage context.
        // The peer rejected our request; surface which RPC fired and which
        // error code came back so the on-call can grep by error_code or
        // by message_id without round-tripping the peer.
        //
        // Review R3 fix (2026-05-17): NO stacktrace on this path. Under
        // an attack where the peer hammers us with malformed requests
        // that produce RPC_ERROR replies, boost::stacktrace allocation
        // would become a CPU hotspot on the gate. The error_reporter
        // record below + the IllegalPacketCounter forceClose path give
        // enough breadcrumbs for triage; a real crash still gets a
        // full stack via HandleFatalSignal (todo #105).
        const auto methodName = IsValidMessageId(rpcMessage.message_id())
            ? gRpcMethodRegistry[rpcMessage.message_id()].methodName
            : std::string("<unknown_method>");
        // GameRpcMessage carries payload in either `request` or `response`
        // depending on type; an RPC_ERROR response itself has neither set,
        // so just record the size of whichever is non-empty (typically 0).
        const auto bodySize = rpcMessage.request().size() + rpcMessage.response().size();
        LOG_ERROR << "[GameChannel] RPC_ERROR received: message_id="
                  << rpcMessage.message_id()
                  << " method=" << methodName
                  << " error_code=" << rpcMessage.error()
                  << " body_size=" << bodySize;
        // Per R3 fix (2026-05-17): NO stacktrace on this path — under an
        // attack, boost::stacktrace allocation would become a CPU hotspot.
        // The error_reporter::Record() below + IllegalPacketCounter
        // forceClose path give enough breadcrumbs for triage.

        // todo.md #250 slice A — record into process-wide buffer.
        std::ostringstream msg;
        msg << "method=" << methodName << " message_id=" << rpcMessage.message_id()
            << " body_size=" << bodySize;
        error_reporter::Record(rpcMessage.error(), "rpc_error_in", msg.str());
        break;
    }
    default:
        LOG_ERROR << "Unknown RPC message type, message ID: " << rpcMessage.message_id();
        break;
    }
}

void GameChannel::HandleResponseMessage(const TcpConnectionPtr &conn, const GameRpcMessage &rpcMessage, muduo::Timestamp receiveTime)
{
    if (!IsValidMessageId(rpcMessage.message_id()))
        return;

    const auto &messageInfo = gRpcMethodRegistry[rpcMessage.message_id()];
    if (!messageInfo.handler)
    {
        LOG_ERROR << "Message service implementation not found for message ID: " << rpcMessage.message_id();
        return;
    }
    MessagePtr response(messageInfo.handler->GetResponsePrototype(
                                               messageInfo.handler->GetDescriptor()->FindMethodByName(messageInfo.methodName))
                            .New());

    if (!response->ParsePartialFromArray(rpcMessage.response().data(), static_cast<int32_t>(rpcMessage.response().size())))
    {
        LOG_ERROR << "Failed to parse response for message ID: " << rpcMessage.message_id();
        return;
    }

    LOG_TRACE << "Dispatching response for message ID: " << rpcMessage.message_id();
    gRpcResponseDispatcher.onProtobufMessage(rpcMessage.message_id(), conn, response, receiveTime);
}

void GameChannel::HandleRequestMessage(const TcpConnectionPtr &connection, const GameRpcMessage &rpcMessage, muduo::Timestamp receiveTime)
{
    ProcessMessage(connection, rpcMessage, receiveTime);
}

void GameChannel::HandleClientRequestMessage(const TcpConnectionPtr &connection, const GameRpcMessage &rpcMessage, muduo::Timestamp receiveTime)
{
    ProcessMessage(connection, rpcMessage, receiveTime);
}

void GameChannel::HandleNodeRouteMessage(const TcpConnectionPtr &conn, const GameRpcMessage &rpcMessage, muduo::Timestamp receiveTime)
{
    // Add routing logic here
}

void GameChannel::ProcessMessage(const TcpConnectionPtr &conn, const GameRpcMessage &rpcMessage, muduo::Timestamp receiveTime)
{
    // todo.md #152 slice C-mini: install a fresh trace context at the
    // start of each RPC dispatch. Once gRPC interceptors / Kafka header
    // propagation (slice C/D) land, the incoming traceparent will be
    // parsed here instead and only the root case falls through to
    // NewRoot. For now, every RPC entry starts its own trace — gives
    // each handler chain its own grep-able id in TLOG_* output, no
    // cross-process correlation yet.
    //
    // Review O2 fix (2026-05-17): use Set() rather than direct assignment
    // so the TLS log-prefix cache stays in sync. Direct assignment
    // skips RefreshLogPrefix() and TLOG_* would emit stale or missing
    // prefixes until the next mutator path ran.
    tracing::Set(tracing::NewRoot());

    if (!IsValidMessageId(rpcMessage.message_id()))
    {
        SendErrorResponse(rpcMessage, GameErrorCode::INVALID_REQUEST);
        return;
    }

    const auto &messageInfo = gRpcMethodRegistry[rpcMessage.message_id()];
    auto it = services_->find(messageInfo.serviceName);
    if (it == services_->end())
    {
        SendErrorResponse(rpcMessage, GameErrorCode::NO_SERVICE);
        return;
    }

    auto *service = it->second;
    const auto *method = service->GetDescriptor()->FindMethodByName(messageInfo.methodName);
    if (!method)
    {
        SendErrorResponse(rpcMessage, GameErrorCode::NO_METHOD);
        return;
    }

    MessagePtr request(service->GetRequestPrototype(method).New());
    if (!request->ParsePartialFromArray(rpcMessage.request().data(), static_cast<int32_t>(rpcMessage.request().size())))
    {
        SendErrorResponse(rpcMessage, GameErrorCode::INVALID_REQUEST);
        return;
    }

    MessagePtr response(service->GetResponsePrototype(method).New());
    service->CallMethod(method, nullptr, boost::get_pointer(request), boost::get_pointer(response), nullptr);

    if (Empty::GetDescriptor() == response->GetDescriptor())
    {
        return;
    }
    if (response->ByteSizeLong() > 0)
    {
        SendRpcResponseMessage(GameMessageType::RESPONSE, rpcMessage.message_id(), response.get());
    }
}

void GameChannel::SendErrorResponse(const GameRpcMessage &message, GameErrorCode errorCode)
{
    // todo.md #70 + #125: log the rejected request before responding so the
    // server side has a trail even if the peer never reports the failure
    // upstream.
    //
    // Review R3 fix (2026-05-17): NO stacktrace here either. This is the
    // gate's reject hot-path; an adversarial client sending packets that
    // produce SendErrorResponse on every receive would turn boost::stacktrace
    // into a DoS amplifier. error_reporter::Record below + the
    // IllegalPacketCounter kill switch (todo #236) plus the fatal handler
    // (todo #105) cover the legitimate triage needs without exposing this
    // path.
    const auto methodName = IsValidMessageId(message.message_id())
        ? gRpcMethodRegistry[message.message_id()].methodName
        : std::string("<unknown_method>");
    // The rejected message is typically a REQUEST; its payload is in the
    // `request` field. RESPONSE-type rejects fall back to `response` size.
    const auto requestBodySize = message.request().size() + message.response().size();
    LOG_ERROR << "[GameChannel] sending RPC_ERROR: message_id=" << message.message_id()
              << " method=" << methodName
              << " error_code=" << errorCode
              << " request_body_size=" << requestBodySize;
    // Per R3 fix: NO stacktrace on RPC_ERROR send path either — same
    // attack-cost concern as the receive path above. error_reporter::Record
    // captures the breadcrumb for triage.

    // todo.md #250 slice A — record outgoing rejection into process-wide buffer.
    std::ostringstream msg;
    msg << "method=" << methodName << " message_id=" << message.message_id()
        << " request_body_size=" << requestBodySize;
    error_reporter::Record(static_cast<uint32_t>(errorCode), "rpc_error_out", msg.str());

    GameRpcMessage errorResponse;
    errorResponse.set_type(GameMessageType::RPC_ERROR);
    errorResponse.set_error(errorCode);
    errorResponse.set_message_id(message.message_id());
    SendGameRpcMessage(errorResponse);
}

void GameChannel::SendRouteResponse(uint32_t messageId, uint64_t id, const std::string &body)
{
    if (messageId >= gRpcMethodRegistry.size())
    {
        LOG_ERROR << "Invalid message_id: " << messageId;
        return;
    }

    GameRpcMessage response;
    response.set_type(GameMessageType::RESPONSE);
    response.set_message_id(messageId);
    response.set_response(body);

    SendGameRpcMessage(response);
}

void GameChannel::SendGameRpcMessage(const GameRpcMessage &message)
{
    const auto cachedSize = LogIfMessageTooLarge(message);

    codec_.send(connection_, message);

    LogMessageStatistics(message, static_cast<uint32_t>(cachedSize));
    TrafficStatsCollector::Instance().RecordSend(message.message_id(), static_cast<uint32_t>(cachedSize));
}
