// 管理端命令以真实 protobuf 做跨语言签名向量与重放回归，不依赖运行中的游戏服。
#include <gtest/gtest.h>
#include "../../nodes/scene/handler/grpc/gm_player_command_security.h"
#include <functional>

namespace
{
using Request = scene_node::GmExecutePlayerCommandRequest;
using Response = scene_node::GmExecutePlayerCommandResponse;
using Result = gate_security::GmAuthResult;
constexpr char kTestSecret[] = "0123456789abcdef0123456789abcdef";
constexpr int64_t kNow = 1700000000;

Request Example()
{
    Request request;
    request.set_command_id(9007199254740993ULL);
    request.set_player_id(9007199254740995ULL);
    request.set_operator_("gm-test");
    request.set_reason("test-only");
    request.set_target_node_id(7);
    request.set_expected_session_id(10);
    request.set_expected_login_generation(9007199254740999ULL);
    request.set_issued_at_ms(1700000000123ULL);
    request.mutable_add_currency()->set_currency_type(2);
    request.mutable_add_currency()->set_amount(123456);
    return request;
}

void Sign(Request& request, std::string nonce = {})
{
    if (nonce.empty()) nonce = std::to_string(request.command_id());
    const auto time = std::to_string(request.issued_at_ms() / 1000);
    const auto canonical = gate_security::BuildGmCanonicalString(gm_player_command::kMethod,
        std::to_string(request.target_node_id()), request.operator_(), time, nonce,
        "gm-player-command-v1\n" + gm_player_command::Digest(request));
    request.set_auth(request.operator_() + "|" + time + "|" + nonce + "|" +
        gate_security::HmacSha256Hex(kTestSecret, canonical));
}

Result Verify(const Request& request, int64_t now = kNow)
{
    gate_security::GmNonceCache nonces;
    return gm_player_command::Verify(request, 7, kTestSecret, now, 300, nonces).result;
}
}

TEST(GmPlayerCommand, PythonJavaCppShareDeterministicSignatureVector)
{
    auto request = Example();
    EXPECT_EQ("596cbd6cc4d472b1ee7bcf964d1f8c990b99ee5d734b3e93c63b4b46b6bd1078",
        gm_player_command::Digest(request));
    Sign(request);
    EXPECT_EQ("gm-test|1700000000|9007199254740993|dedbaa0b050bd6dba806c474571f1adf395fc840e9470f347932fc8d44fea056", request.auth());
    EXPECT_EQ(Result::kOk, Verify(request));
}

TEST(GmPlayerCommand, EveryTargetIdentityAndAuditFieldIsBound)
{
    const std::function<void(Request&)> changes[] = {
        [](auto& r) { r.set_command_id(r.command_id() + 1); },
        [](auto& r) { r.set_player_id(r.player_id() + 1); },
        [](auto& r) { r.set_reason("another reason"); },
        [](auto& r) { r.set_operator_("another-operator"); },
        [](auto& r) { r.set_target_node_id(8); },
        [](auto& r) { r.set_expected_session_id(11); },
        [](auto& r) { r.set_expected_login_generation(r.expected_login_generation() + 1); },
        [](auto& r) { r.set_issued_at_ms(r.issued_at_ms() + 1); },
        [](auto& r) { r.mutable_add_currency()->set_amount(123457); },
        [](auto& r) { r.mutable_add_currency()->set_currency_type(1); },
        [](auto& r) { r.mutable_deduct_currency()->set_amount(123456); },
    };
    for (const auto& change : changes) {
        auto request = Example();
        Sign(request);
        change(request);
        EXPECT_NE(Result::kOk, Verify(request));
    }
}

TEST(GmPlayerCommand, SevenTypedCommandsAuthenticateAndRejectPayloadChanges)
{
    const std::function<void(Request&)> commands[] = {
        [](auto& r) { r.mutable_add_currency()->set_amount(5); },
        [](auto& r) { r.mutable_deduct_currency()->set_amount(5); },
        [](auto& r) { r.mutable_block_currency()->set_currency_type(1); },
        [](auto& r) { r.mutable_unblock_currency()->set_currency_type(1); },
        [](auto& r) { r.mutable_grant_item()->set_config_id(100); r.mutable_grant_item()->set_count(2); },
        [](auto& r) { r.mutable_grant_pet()->set_pet_table_id(100); },
        [](auto& r) { r.mutable_set_level()->set_level(80); },
    };
    for (const auto& command : commands) {
        auto request = Example();
        request.clear_command();
        command(request);
        Sign(request);
        EXPECT_EQ(Result::kOk, Verify(request));
        request.clear_command();
        EXPECT_NE(Result::kOk, Verify(request));
    }
}

TEST(GmPlayerCommand, MissingSecretReplayAndExpiredDraftFailClosed)
{
    auto request = Example();
    Sign(request);
    gate_security::GmNonceCache nonces;
    EXPECT_EQ(Result::kSecretNotConfigured, gm_player_command::Verify(request, 7, "", kNow, 300, nonces).result);
    EXPECT_EQ(Result::kSecretNotConfigured, gm_player_command::Verify(request, 7, "short-key", kNow, 300, nonces).result);
    EXPECT_EQ(Result::kOk, gm_player_command::Verify(request, 7, kTestSecret, kNow, 300, nonces).result);
    EXPECT_EQ(Result::kReplayedNonce, gm_player_command::Verify(request, 7, kTestSecret, kNow, 300, nonces).result);
    EXPECT_EQ(Result::kTimestampOutOfWindow, Verify(request, kNow + 301));
    EXPECT_EQ(Result::kTimestampOutOfWindow, Verify(request, kNow - 301));
    request.set_issued_at_ms(request.issued_at_ms() + 1000); // 信封时间不能脱离草稿时间。
    EXPECT_EQ(Result::kTimestampOutOfWindow, Verify(request));
}

TEST(GmPlayerCommand, SameCommandWithNewAuthReturnsPriorResultAndConflictsAreRejected)
{
    gm_player_command::CommandCache cache(1);
    auto request = Example();
    Sign(request);
    Response response;
    EXPECT_EQ(gm_player_command::CommandCache::Admission::kNew, cache.Begin(request, response));
    response.set_executed(true);
    response.set_balance_after(123456);
    cache.Complete(request, response);
    Sign(request, "different-auth-nonce");
    Response replay;
    EXPECT_EQ(gm_player_command::CommandCache::Admission::kReplay, cache.Begin(request, replay));
    EXPECT_TRUE(replay.executed());
    EXPECT_EQ(123456, replay.balance_after());
    request.mutable_add_currency()->set_amount(99);
    EXPECT_EQ(gm_player_command::CommandCache::Admission::kConflict, cache.Begin(request, replay));
    request.set_command_id(request.command_id() + 1);
    EXPECT_EQ(gm_player_command::CommandCache::Admission::kFull, cache.Begin(request, replay));
}

TEST(GmPlayerCommand, InvalidFieldsAndControlCharactersAreRejectedOrSanitized)
{
    auto request = Example();
    request.set_reason(std::string(1001, 'x'));
    Sign(request);
    EXPECT_NE(Result::kOk, Verify(request));
    request = Example(); request.set_expected_login_generation(0); Sign(request);
    EXPECT_NE(Result::kOk, Verify(request));
    request = Example(); request.set_operator_("operator\ninjected"); Sign(request);
    EXPECT_NE(Result::kOk, Verify(request));
    EXPECT_EQ("reason injected", gm_player_command::AuditText("reason\ninjected"));
}
