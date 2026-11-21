#pragma once

#include <algorithm>
#include <cstdint>
#include <memory>
#include <string>
#include <string_view>
#include <google/protobuf/io/coded_stream.h>
#include <google/protobuf/io/zero_copy_stream_impl_lite.h>
#include <google/protobuf/message.h>
#include <openssl/sha.h>
#include "core/security/token_security.h"
#include "proto/common/base/service_call_auth.pb.h"

// 与go/shared/serviceauth完全相同的确定性PB正文证明。方法和固定调用者由真实入口指定。
namespace service_call_auth {
inline std::string Digest(const google::protobuf::Message& request) {
    std::unique_ptr<google::protobuf::Message> copy(request.New());
    copy->CopyFrom(request);
    const auto* field = copy->GetDescriptor()->FindFieldByName("auth");
    if (field == nullptr || field->cpp_type() != google::protobuf::FieldDescriptor::CPPTYPE_MESSAGE) return {};
    copy->GetReflection()->ClearField(copy.get(), field);
    std::string bytes;
    {
        google::protobuf::io::StringOutputStream output(&bytes);
        google::protobuf::io::CodedOutputStream coded(&output);
        coded.SetSerializationDeterministic(true);
        if (!copy->SerializeToCodedStream(&coded) || coded.HadError()) return {};
    }
    unsigned char sum[SHA256_DIGEST_LENGTH];
    if (SHA256(reinterpret_cast<const unsigned char*>(bytes.data()), bytes.size(), sum) == nullptr) return {};
    return token_security::BytesToHex(sum, SHA256_DIGEST_LENGTH);
}
inline bool ValidCaller(std::string_view caller) {
    if (caller.empty() || caller.size() > 32 || caller.front() < 'a' || caller.front() > 'z') return false;
    return std::all_of(caller.begin(), caller.end(), [](char c) {
        return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-';
    });
}
inline bool ValidDigestHex(std::string_view digest) {
    return digest.size() == 64 && std::all_of(digest.begin(), digest.end(), [](char c) {
        return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f');
    });
}
inline std::string SignatureForDigest(std::string_view method, std::string_view digest,
    std::string_view caller, std::string_view secret, uint64_t timestampMs) {
    const std::string normalizedSecret = token_security::TrimAscii(secret);
    if (method.empty() || method.front() != '/' || method.find_first_of("\r\n") != std::string_view::npos
        || !ValidCaller(caller) || normalizedSecret.size() < 32 || timestampMs == 0 || !ValidDigestHex(digest)) return {};
    return token_security::HmacSha256Hex(normalizedSecret, "v1\n" + std::string(method) + "\n" + std::string(caller)
        + "\n" + std::to_string(timestampMs) + "\n" + std::string(digest));
}
inline std::string Signature(std::string_view method, const google::protobuf::Message& request,
    std::string_view caller, std::string_view secret, uint64_t timestampMs) {
    const std::string normalizedSecret = token_security::TrimAscii(secret);
    if (method.empty() || method.front() != '/' || method.find_first_of("\r\n") != std::string_view::npos
        || !ValidCaller(caller) || normalizedSecret.size() < 32 || timestampMs == 0) return {};
    return SignatureForDigest(method, Digest(request), caller, normalizedSecret, timestampMs);
}
inline bool SignForDigest(std::string_view method, std::string_view digest,
    common::base::ServiceCallAuth& auth, std::string_view caller, std::string_view secret, uint64_t timestampMs) {
    const std::string signature = SignatureForDigest(method, digest, caller, secret, timestampMs);
    if (signature.empty()) return false;
    auth.set_caller(std::string(caller)); auth.set_timestamp_ms(timestampMs); auth.set_signature_hex(signature);
    return true;
}
inline bool Sign(std::string_view method, google::protobuf::Message& request,
    common::base::ServiceCallAuth& auth, std::string_view caller, std::string_view secret, uint64_t timestampMs) {
    const std::string signature = Signature(method, request, caller, secret, timestampMs);
    if (signature.empty()) return false;
    auth.set_caller(std::string(caller)); auth.set_timestamp_ms(timestampMs); auth.set_signature_hex(signature);
    return true;
}
inline bool Verify(std::string_view method, const google::protobuf::Message& request,
    const common::base::ServiceCallAuth& auth, std::string_view expectedCaller, std::string_view secret, uint64_t nowMs) {
    const uint64_t sent = auth.timestamp_ms();
    if (auth.caller() != expectedCaller || sent == 0 || sent > INT64_MAX || auth.signature_hex().size() != 64
        || (sent > nowMs ? sent - nowMs : nowMs - sent) > 300000) return false;
    return token_security::ConstantTimeEquals(auth.signature_hex(), Signature(method, request, expectedCaller, secret, sent));
}
} // namespace service_call_auth
