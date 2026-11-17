#pragma once

// 独立后台玩家命令的鉴权与进程内去重；调用者持有 Scene EventLoop，不新增锁或线程。
#include <google/protobuf/io/coded_stream.h>
#include <google/protobuf/io/zero_copy_stream_impl_lite.h>
#include <openssl/evp.h>
#include <algorithm>
#include <string>
#include <unordered_map>
#include "proto/scene_manager/scene_node_service.pb.h"
#include "../../../gate/gate_security.h"

namespace gm_player_command
{
inline constexpr char kMethod[] = "/scene_node.SceneNodeGrpc/GmExecutePlayerCommand";
inline constexpr char kSecretEnv[] = "MMORPG_GM_PLAYER_COMMAND_SECRET";

inline std::string Digest(const scene_node::GmExecutePlayerCommandRequest& request)
{
    auto unsignedRequest = request;
    unsignedRequest.clear_auth();
    std::string bytes;
    {
        google::protobuf::io::StringOutputStream stream(&bytes);
        google::protobuf::io::CodedOutputStream output(&stream);
        output.SetSerializationDeterministic(true);
        if (!unsignedRequest.SerializeToCodedStream(&output) || output.HadError()) return {};
    }
    unsigned char digest[EVP_MAX_MD_SIZE];
    unsigned int length = 0;
    if (EVP_Digest(bytes.data(), bytes.size(), digest, &length, EVP_sha256(), nullptr) != 1 || length != 32) return {};
    constexpr char hex[] = "0123456789abcdef";
    std::string result;
    result.reserve(64);
    for (unsigned int i = 0; i < length; ++i) {
        result.push_back(hex[digest[i] >> 4]);
        result.push_back(hex[digest[i] & 15]);
    }
    return result;
}

inline bool HasValidEnvelopeFields(const scene_node::GmExecutePlayerCommandRequest& request)
{
    return request.command_id() != 0 && request.player_id() != 0 && request.issued_at_ms() != 0 &&
        request.target_node_id() != 0 && request.expected_session_id() != 0 &&
        request.expected_session_id() != UINT32_MAX && request.expected_login_generation() != 0 &&
        !request.operator_().empty() && request.operator_().size() <= 64 &&
        std::none_of(request.operator_().begin(), request.operator_().end(),
            [](unsigned char ch) { return ch < 32 || ch == 127 || ch == '|'; }) &&
        !request.reason().empty() && request.reason().size() <= 1000 &&
        request.auth().size() <= 512 && request.ByteSizeLong() <= 16 * 1024 &&
        request.command_case() != scene_node::GmExecutePlayerCommandRequest::COMMAND_NOT_SET;
}

inline gate_security::GmAuthOutcome Verify(const scene_node::GmExecutePlayerCommandRequest& request,
    uint32_t selfNodeId, std::string_view secret, int64_t nowUnix, int64_t skewSeconds,
    gate_security::GmNonceCache& nonces)
{
    if (secret.size() < 32) return {gate_security::GmAuthResult::kSecretNotConfigured, {}};
    gate_security::GmEnvelope envelope;
    if (!HasValidEnvelopeFields(request) || request.target_node_id() != selfNodeId ||
        !gate_security::ParseGmEnvelope(request.auth(), envelope) || envelope.operatorName != request.operator_())
        return {gate_security::GmAuthResult::kMalformedEnvelope, {}};
    // 草稿时间冻结且最长五分钟；不能用刷新 auth 时间戳延长旧命令寿命。
    const auto issuedSeconds = request.issued_at_ms() / 1000;
    if (nowUnix < 0 || envelope.timestampRaw != std::to_string(issuedSeconds) ||
        issuedSeconds > static_cast<uint64_t>(nowUnix) + 300 ||
        issuedSeconds + 300 < static_cast<uint64_t>(nowUnix))
        return {gate_security::GmAuthResult::kTimestampOutOfWindow, {}};
    const auto digest = Digest(request);
    if (digest.empty()) return {gate_security::GmAuthResult::kMalformedEnvelope, {}};
    return gate_security::VerifyGmRequest(kMethod, std::to_string(selfNodeId), request.auth(),
        "gm-player-command-v1\n" + digest, secret, nowUnix, skewSeconds, nonces);
}

// 不淘汰已执行命令：达到上限后拒绝新命令，避免淘汰后旧命令用新 nonce 再执行。
// 这只是进程生命周期内的防线，重启会丢失；Java 永久拒绝重发 UNKNOWN/SENDING 命令。
class CommandCache
{
public:
    enum class Admission : uint8_t { kNew, kReplay, kConflict, kFull };
    explicit CommandCache(size_t capacity = 10000) : capacity_(capacity) {}
    Admission Begin(const scene_node::GmExecutePlayerCommandRequest& request,
        scene_node::GmExecutePlayerCommandResponse& response)
    {
        const auto key = Key(request);
        const auto digest = Digest(request);
        if (const auto it = entries_.find(key); it != entries_.end()) {
            if (it->second.digest != digest) return Admission::kConflict;
            response = it->second.response;
            return Admission::kReplay;
        }
        if (entries_.size() >= capacity_) return Admission::kFull;
        response.set_command_id(request.command_id());
        entries_.emplace(key, Entry{digest, response});
        return Admission::kNew;
    }
    void Complete(const scene_node::GmExecutePlayerCommandRequest& request,
        const scene_node::GmExecutePlayerCommandResponse& response)
    {
        entries_.at(Key(request)).response = response;
    }
private:
    struct Entry { std::string digest; scene_node::GmExecutePlayerCommandResponse response; };
    static std::string Key(const scene_node::GmExecutePlayerCommandRequest& request)
    { return request.operator_() + "\n" + std::to_string(request.command_id()); }
    size_t capacity_;
    std::unordered_map<std::string, Entry> entries_;
};

inline std::string AuditText(std::string value)
{
    for (auto& ch : value) if (static_cast<unsigned char>(ch) < 32 || ch == 127) ch = ' ';
    return value;
}
} // namespace gm_player_command
