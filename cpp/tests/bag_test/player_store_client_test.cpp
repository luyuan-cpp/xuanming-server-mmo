#include <chrono>
#include <deque>
#include <functional>
#include <optional>
#include <string>
#include <vector>

#include <gtest/gtest.h>

#include "core/system/scene_task_scope.h"
#include "modules/audit/audit_outbox_stats.h"
#include "muduo/net/EventLoop.h"
#include "player/system/player_store_client.h"
#include "player/system/player_store_projection.h"

// 存盘状态机(docs/design/player-save-direct-commit.md §5.1)与片段投影。传输、定时器、发号都是假的。
namespace
{
    constexpr Guid kPlayer = 7;
    constexpr uint64_t kEpoch = 100;
    constexpr auto kCharacter = static_cast<std::size_t>(playerstore::PLAYER_STORE_KIND_CHARACTER);
    constexpr auto kWallet = static_cast<std::size_t>(playerstore::PLAYER_STORE_KIND_WALLET);
    constexpr auto kProfile = static_cast<std::size_t>(playerstore::PLAYER_STORE_KIND_PROFILE);

    PlayerStoreClient::SubmitOptions Released()
    {
        PlayerStoreClient::SubmitOptions options;
        options.release = true;
        return options;
    }

    PlayerStoreClient::SubmitOptions Forced()
    {
        PlayerStoreClient::SubmitOptions options;
        options.force = true;
        return options;
    }

    player_store_rows::KindBytes Rows(const std::string &hot, const std::string &asset = "a0")
    {
        player_store_rows::KindBytes rows;
        rows[kCharacter] = hot;
        rows[kWallet] = asset;
        return rows;
    }

    struct Fixture
    {
        muduo::net::EventLoop loop;
        SceneTaskScope tasks{loop};
        struct Sent
        {
            playerstore::SavePlayerRequest request;
            PlayerStoreClient::SendDone done;
        };
        std::deque<Sent> sent;
        struct SentClaim
        {
            playerstore::LoadAndClaimRequest request;
            PlayerStoreClient::ClaimDone done;
        };
        std::deque<SentClaim> sentClaims;
        // 认领结束的记录:没有结论时 second 为空。
        std::vector<std::pair<Guid, std::optional<playerstore::LoadAndClaimResult>>> claimed;
        bool loadAbandoned{false};
        uint64_t claimEpoch{kEpoch};
        PlayerStoreClient::Clock::time_point clock{};
        std::vector<std::function<void()>> timers;
        std::vector<double> delays;
        std::vector<Guid> settled;
        bool nodeAvailable{true};
        bool seqExhausted{false};
        uint64_t nextSeq{1000};
        PlayerStoreClient::Options options;

        Fixture()
        {
            options.send = [this](const playerstore::SavePlayerRequest &request, PlayerStoreClient::SendDone done)
            {
                if (!nodeAvailable)
                {
                    return false;
                }
                sent.push_back({request, std::move(done)});
                return true;
            };
            options.schedule = [this](double delay, std::function<void()> fn)
            {
                delays.push_back(delay);
                timers.push_back(std::move(fn));
            };
            options.nextSeq = [this](Guid) -> std::optional<uint64_t>
            {
                if (seqExhausted)
                {
                    return std::nullopt;
                }
                return ++nextSeq;
            };
            options.onSettled = [this](Guid player) { settled.push_back(player); };
            options.instanceId = [] { return std::string("instance-a"); };
            options.sendClaim = [this](const playerstore::LoadAndClaimRequest &request, PlayerStoreClient::ClaimDone done)
            {
                if (!nodeAvailable)
                {
                    return false;
                }
                sentClaims.push_back({request, std::move(done)});
                return true;
            };
            options.claimRequest = [this](Guid player) -> std::optional<playerstore::LoadAndClaimRequest>
            {
                if (loadAbandoned)
                {
                    return std::nullopt;
                }
                playerstore::LoadAndClaimRequest request;
                request.set_player_id(player);
                request.set_owner_epoch(claimEpoch);
                request.set_instance_id("instance-a");
                return request;
            };
            options.onClaimed = [this](Guid player, const playerstore::LoadAndClaimResponse *response)
            {
                std::optional<playerstore::LoadAndClaimResult> result;
                if (response != nullptr)
                {
                    result = response->result();
                }
                claimed.emplace_back(player, result);
            };
            options.now = [this] { return clock; };
        }

        void Reply(playerstore::SavePlayerResult result, uint64_t rowEpoch = 0, uint64_t rowSeq = 0)
        {
            ASSERT_FALSE(sent.empty());
            Sent front = std::move(sent.front());
            sent.pop_front();
            playerstore::SavePlayerResponse response;
            response.set_result(result);
            response.set_row_owner_epoch(rowEpoch);
            response.set_row_save_seq(rowSeq);
            front.done(true, &response);
        }

        void ReplyClaimWith(const playerstore::LoadAndClaimResponse &response)
        {
            ASSERT_FALSE(sentClaims.empty());
            SentClaim front = std::move(sentClaims.front());
            sentClaims.pop_front();
            front.done(true, &response);
        }

        void ReplyClaim(playerstore::LoadAndClaimResult result)
        {
            playerstore::LoadAndClaimResponse response;
            response.set_result(result);
            ReplyClaimWith(response);
        }

        void ReplyClaimed(uint32_t storageId, uint64_t ownerEpoch, uint64_t saveSeq, const player_store_rows::KindBytes &rows)
        {
            playerstore::LoadAndClaimResponse response;
            response.set_result(playerstore::LOAD_AND_CLAIM_CLAIMED);
            response.set_storage_id(storageId);
            response.set_owner_epoch(ownerEpoch);
            response.set_save_seq(saveSeq);
            for (std::size_t kind = 1; kind < player_store_rows::kKindCount; ++kind)
            {
                if (!rows[kind].empty())
                {
                    auto *row = response.add_rows();
                    row->set_kind(static_cast<playerstore::PlayerStoreKind>(kind));
                    row->set_payload(rows[kind]);
                }
            }
            ReplyClaimWith(response);
        }

        void ReplyClaimUnknown()
        {
            ASSERT_FALSE(sentClaims.empty());
            SentClaim front = std::move(sentClaims.front());
            sentClaims.pop_front();
            front.done(false, nullptr);
        }

        void ReplyUnknown()
        {
            ASSERT_FALSE(sent.empty());
            Sent front = std::move(sent.front());
            sent.pop_front();
            front.done(false, nullptr);
        }

        void FireTimers()
        {
            auto fired = std::move(timers);
            timers.clear();
            for (auto &timer : fired)
            {
                timer();
            }
        }
    };

    std::string PayloadOf(const playerstore::SavePlayerRequest &request, std::size_t kind)
    {
        for (const auto &row : request.rows())
        {
            if (static_cast<std::size_t>(row.kind()) == kind)
            {
                return row.payload();
            }
        }
        return "<absent>";
    }
}

TEST(PlayerStoreClientTest, NotSeededIsReported)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kNotSeeded, client.Submit(kPlayer, Rows("h"), {}, nullptr));
    EXPECT_TRUE(f.sent.empty());
}

TEST(PlayerStoreClientTest, OnlyDirtyKindsAreSent)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 42, kEpoch, 0, Rows("h0", "a0")));
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, Rows("h0", "a0"), {}, nullptr));
    EXPECT_TRUE(f.sent.empty()) << "没有变化不发,也不取序号";
    EXPECT_EQ(1000u, f.nextSeq);

    PlayerStoreClient::SubmitOptions withGen;
    withGen.schemaGen = 3;
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h1", "a0"), withGen, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    const auto &request = f.sent.front().request;
    EXPECT_EQ(3u, request.schema_gen());
    EXPECT_EQ(kPlayer, request.player_id());
    EXPECT_EQ(42u, request.storage_id());
    EXPECT_EQ(kEpoch, request.owner_epoch());
    EXPECT_EQ(1001u, request.save_seq());
    EXPECT_EQ("instance-a", request.instance_id());
    ASSERT_EQ(1, request.rows_size());
    EXPECT_EQ("h1", PayloadOf(request, kCharacter));
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, Rows("h1", "a0"), {}, nullptr))
        << "提交成功后比较基准更新";
}

// 单在途:在途期间的快照互相覆盖,只发最新的,回调全部保留。
TEST(PlayerStoreClientTest, SingleInflightCoalescesPendingSnapshots)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    std::vector<std::string> calls;
    auto record = [&calls](const char *name)
    {
        return [&calls, name](const PlayerStoreClient::SaveResult &result)
        {
            calls.push_back(std::string(name) + (result.status == PlayerStoreClient::SaveStatus::kCommitted ? ":ok" : ":other") +
                            (result.superseded ? ":superseded" : ""));
        };
    };
    client.Submit(kPlayer, Rows("h1"), {}, record("first"));
    client.Submit(kPlayer, Rows("h2"), {}, record("second"));
    client.Submit(kPlayer, Rows("h3"), {}, record("third"));
    ASSERT_EQ(1u, f.sent.size()) << "同时只有一个在途";
    EXPECT_TRUE(client.HasUnsettled(kPlayer));

    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    ASSERT_EQ(1u, calls.size());
    EXPECT_EQ("first:ok", calls[0]);
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h3", PayloadOf(f.sent.front().request, kCharacter)) << "只发最新的快照";
    EXPECT_TRUE(f.settled.empty()) << "有 pending 时不算完成";

    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    ASSERT_EQ(3u, calls.size());
    EXPECT_EQ("second:ok:superseded", calls[1]) << "被取代的快照没有被写,回调要能分辨";
    EXPECT_EQ("third:ok", calls[2]);
    ASSERT_EQ(1u, f.settled.size());
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
}

// 结果未知:同内容、同序号、同代次重试;退避翻倍封顶;拿到结果才发下一笔。
TEST(PlayerStoreClientTest, UnknownOutcomeRetriesSameRequest)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    const std::string original = f.sent.front().request.SerializeAsString();

    client.Submit(kPlayer, Rows("h2"), {}, nullptr);
    client.SetOwnerEpoch(kPlayer, kEpoch + 50);

    for (int i = 0; i < 6; ++i)
    {
        f.ReplyUnknown();
        EXPECT_TRUE(f.sent.empty()) << "退避期内不发";
        f.FireTimers();
        ASSERT_EQ(1u, f.sent.size());
        EXPECT_EQ(original, f.sent.front().request.SerializeAsString()) << "重试不换序号,也不跟着改代次";
    }
    EXPECT_EQ((std::vector<double>{0.5, 1.0, 2.0, 4.0, 5.0, 5.0}), f.delays);
    EXPECT_EQ(1001u, f.sent.front().request.save_seq());

    f.Reply(playerstore::SAVE_PLAYER_EQUAL);
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(1002u, f.sent.front().request.save_seq());
    EXPECT_EQ("h2", PayloadOf(f.sent.front().request, kCharacter));
    EXPECT_EQ(kEpoch + 50, f.sent.front().request.owner_epoch()) << "下一笔用新代次";
}

// 超过传输上限的请求发不出去:通道在本地就拒了,那是「结果未知」,会永远重试,退出也收不了尾。按 TOO_LARGE 处理。
TEST(PlayerStoreClientTest, OversizedRequestIsRefusedWithoutSending)
{
    Fixture f;
    f.options.maxRequestBytes = 128;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    std::vector<playerstore::SavePlayerResult> results;
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows(std::string(256, 'x')), {},
        [&results](const PlayerStoreClient::SaveResult &result) { results.push_back(result.result); }));
    EXPECT_TRUE(results.empty()) << "调用方还在自己的栈上,不能同步回调";
    EXPECT_TRUE(client.HasUnsettled(kPlayer));

    ASSERT_EQ(1u, f.timers.size());
    f.FireTimers();
    ASSERT_EQ(1u, results.size());
    EXPECT_EQ(playerstore::SAVE_PLAYER_TOO_LARGE, results[0]);
    EXPECT_TRUE(f.sent.empty());
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
    EXPECT_EQ(1u, f.settled.size());

    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h1"), {}, nullptr));
    ASSERT_EQ(1u, f.sent.size()) << "变小了照常发";
    EXPECT_EQ("h1", PayloadOf(f.sent.front().request, kCharacter));
}

// 无状态释放只要说明不用再试就能写标记,否则玩家锁死到节点重启;部署和代码问题不算。
TEST(PlayerStoreClientTest, DetachedReleaseSettledTable)
{
    for (const auto result : {playerstore::SAVE_PLAYER_COMMITTED, playerstore::SAVE_PLAYER_EQUAL, playerstore::SAVE_PLAYER_NO_OWNER,
                              playerstore::SAVE_PLAYER_UNCLAIMED, playerstore::SAVE_PLAYER_STALE_OWNER, playerstore::SAVE_PLAYER_NOT_HOLDER,
                              playerstore::SAVE_PLAYER_OLDER_SEQ})
    {
        EXPECT_TRUE(DetachedReleaseSettled(result)) << playerstore::SavePlayerResult_Name(result);
    }
    for (const auto result : {playerstore::SAVE_PLAYER_RESULT_UNSPECIFIED, playerstore::SAVE_PLAYER_INVALID, playerstore::SAVE_PLAYER_TOO_LARGE,
                              playerstore::SAVE_PLAYER_SCHEMA_REGRESSED, playerstore::SAVE_PLAYER_SCHEMA_TOO_NEW})
    {
        EXPECT_FALSE(DetachedReleaseSettled(result)) << playerstore::SavePlayerResult_Name(result);
    }
}

// 重试延迟经过抖动,基础延迟照常翻倍。认领走同一个抖动。
TEST(PlayerStoreClientTest, RetryDelayGoesThroughJitter)
{
    Fixture f;
    f.options.jitter = [](double delaySec) { return delaySec / 2.0; };
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    f.ReplyUnknown();
    f.FireTimers();
    f.ReplyUnknown();
    ASSERT_EQ(2u, f.delays.size());
    EXPECT_DOUBLE_EQ(0.25, f.delays[0]);
    EXPECT_DOUBLE_EQ(0.5, f.delays[1]);

    f.delays.clear();
    client.RequestClaim(kPlayer + 1);
    f.ReplyClaimUnknown();
    ASSERT_EQ(1u, f.delays.size());
    EXPECT_DOUBLE_EQ(0.25, f.delays[0]);
}

TEST(PlayerStoreClientTest, NoNodeRetriesWithoutConsumingTheRequest)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    f.nodeAvailable = false;
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h1"), {}, nullptr));
    EXPECT_TRUE(f.sent.empty());
    f.FireTimers();
    EXPECT_TRUE(f.sent.empty());
    f.nodeAvailable = true;
    f.FireTimers();
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(1001u, f.sent.front().request.save_seq()) << "序号不因为发不出去而变化";
}

// 被拒绝之前结果未知的那次可能已经提交:这些行必须并入脏集,否则改回原值的行不写。
TEST(PlayerStoreClientTest, RejectedAfterUnknownKeepsRowsDirty)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    PlayerStoreClient::SaveResult seen;
    client.Submit(kPlayer, Rows("h1"), {}, [&seen](const PlayerStoreClient::SaveResult &result) { seen = result; });
    f.ReplyUnknown();
    f.FireTimers();
    f.Reply(playerstore::SAVE_PLAYER_STALE_OWNER, 200, 55);
    EXPECT_EQ(PlayerStoreClient::SaveStatus::kRejected, seen.status);
    EXPECT_EQ(playerstore::SAVE_PLAYER_STALE_OWNER, seen.result);
    EXPECT_EQ(200u, seen.rowOwnerEpoch);
    EXPECT_EQ(55u, seen.rowSaveSeq);
    EXPECT_FALSE(client.HasUnsettled(kPlayer));

    // 内存改回了原值:和 confirmed 相同,但库里可能是 h1,必须写。
    client.SetOwnerEpoch(kPlayer, 300);
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h0"), {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h0", PayloadOf(f.sent.front().request, kCharacter));
}

// 明确拒绝也不提交比较基准。
TEST(PlayerStoreClientTest, RejectionDoesNotAdvanceConfirmed)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    f.Reply(playerstore::SAVE_PLAYER_TOO_LARGE);
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h1"), {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(1002u, f.sent.front().request.save_seq()) << "被拒绝的序号作废,用新序号";
}

// 写权行还没有、go/db 比 scene 旧:这笔没有提交,同一笔过一会儿再试。
TEST(PlayerStoreClientTest, TransientRejectionsRetrySameRequest)
{
    for (const auto result : {playerstore::SAVE_PLAYER_NO_OWNER, playerstore::SAVE_PLAYER_SCHEMA_TOO_NEW})
    {
        Fixture f;
        PlayerStoreClient client(f.options);
        ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
        bool called = false;
        client.Submit(kPlayer, Rows("h1"), {}, [&called](const PlayerStoreClient::SaveResult &) { called = true; });
        f.Reply(result);
        EXPECT_FALSE(called);
        EXPECT_TRUE(client.HasUnsettled(kPlayer));
        f.FireTimers();
        ASSERT_EQ(1u, f.sent.size());
        EXPECT_EQ(1001u, f.sent.front().request.save_seq());
    }
}

// 下线快路径:没有脏行也要发空行 release,否则写权永远不释放。
TEST(PlayerStoreClientTest, ReleaseWithoutChangesStillSends)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h0"), Released(), nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_TRUE(f.sent.front().request.release());
    EXPECT_EQ(0, f.sent.front().request.rows_size());
}

// 最后一份快照决定 release:断线重连复用实体时,后来的存盘要把释放撤销。
TEST(PlayerStoreClientTest, LatestSnapshotDecidesRelease)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    client.Submit(kPlayer, Rows("h2"), Released(), nullptr);
    client.Submit(kPlayer, Rows("h3"), {}, nullptr);
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_FALSE(f.sent.front().request.release());
}

// 回调里再次 Submit:这份比 pending 新,不能抢到它前面发出去。
TEST(PlayerStoreClientTest, SubmitFromCallbackDoesNotOvertakePending)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, [&client](const PlayerStoreClient::SaveResult &)
    {
        client.Submit(kPlayer, Rows("h3"), {}, nullptr);
    });
    client.Submit(kPlayer, Rows("h2"), {}, nullptr);
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h3", PayloadOf(f.sent.front().request, kCharacter));
}

// 排队的快照和已提交的相同:回调照常调用,状态是不需要写。
TEST(PlayerStoreClientTest, PendingWithoutChangesCompletesImmediately)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    PlayerStoreClient::SaveStatus status = PlayerStoreClient::SaveStatus::kRejected;
    client.Submit(kPlayer, Rows("h1"), {}, [&status](const PlayerStoreClient::SaveResult &result) { status = result.status; });
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_EQ(PlayerStoreClient::SaveStatus::kAlreadyPersisted, status);
    EXPECT_TRUE(f.sent.empty());
    EXPECT_EQ(1u, f.settled.size());
}

// 存盘未落定时不能重新播种,所以迟到的回包不可能改到重新加载之后的状态。
TEST(PlayerStoreClientTest, CannotReseedOrForgetWhileUnsettled)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    EXPECT_FALSE(client.Seed(kPlayer, 1, kEpoch + 1, 0, Rows("h0")));
    EXPECT_FALSE(client.Forget(kPlayer));
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_TRUE(client.Seed(kPlayer, 1, kEpoch + 1, 0, Rows("h1")));
    EXPECT_TRUE(client.Forget(kPlayer));
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kNotSeeded, client.Submit(kPlayer, Rows("h2"), {}, nullptr));
}

// 压测探针:比脏不看探针,否则恒脏;有脏行时才打戳,打戳后 kProfile 一起发。
TEST(PlayerStoreClientTest, ProbeStampDoesNotMakePlayerAlwaysDirty)
{
    Fixture f;
    f.options.stampProfile = [](Guid, uint64_t saveSeq, std::string &profile) { profile += "|probe" + std::to_string(saveSeq); };
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, Rows("h0"), {}, nullptr))
        << "没有脏行不打戳";
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("|probe1001", PayloadOf(f.sent.front().request, kProfile));
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, Rows("h1"), {}, nullptr))
        << "探针不进比较基准";
}

// 不同玩家互不影响。
TEST(PlayerStoreClientTest, PlayersAreIndependent)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(1, 1, kEpoch, 0, Rows("h0")));
    ASSERT_TRUE(client.Seed(2, 1, kEpoch, 0, Rows("h0")));
    client.Submit(1, Rows("h1"), {}, nullptr);
    client.Submit(2, Rows("h1"), {}, nullptr);
    EXPECT_EQ(2u, f.sent.size());
    EXPECT_NE(f.sent[0].request.save_seq(), f.sent[1].request.save_seq());
}

// ── 片段投影 ───────────────────────────────────────────────────────────────

namespace
{
    player_database SamplePlayer()
    {
        player_database db;
        db.set_player_id(7);
        db.mutable_level_component()->set_level(30);
        db.mutable_currency()->add_values(1000);
        db.mutable_profile_component()->set_name("alice");
        db.mutable_skill_list();
        return db;
    }

    google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> ToRows(const player_store_rows::KindBytes &bytes)
    {
        google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> rows;
        for (std::size_t kind = 1; kind < player_store_rows::kKindCount; ++kind)
        {
            // 退役号(1、2、5)的空位不是行:库里不会有,送进 AssemblePlayer 会按旧布局整份拒绝。
            if (!player_store_rows::IsDataKind(kind))
            {
                continue;
            }
            auto *row = rows.Add();
            row->set_kind(static_cast<playerstore::PlayerStoreKind>(kind));
            row->set_payload(bytes[kind]);
        }
        return rows;
    }
}

TEST(PlayerStoreProjectionTest, EveryFieldBelongsToExactlyOneGroup)
{
    std::string error;
    EXPECT_TRUE(player_store_rows::ValidatePersistGroups(error)) << error;
}

TEST(PlayerStoreProjectionTest, ProjectThenAssembleRestoresTheMessage)
{
    player_database db = SamplePlayer();
    const std::string before = db.SerializeAsString();
    player_store_rows::KindBytes bytes;
    std::string error;
    ASSERT_TRUE(player_store_rows::ProjectPlayer(db, bytes, error)) << error;
    EXPECT_EQ(before, db.SerializeAsString()) << "投影之后消息要恢复原样";

    player_database restored;
    ASSERT_TRUE(player_store_rows::AssemblePlayer(ToRows(bytes), restored, error)) << error;
    EXPECT_EQ(30u, restored.level_component().level());
    ASSERT_EQ(1, restored.currency().values_size());
    EXPECT_EQ(1000u, restored.currency().values(0));
    EXPECT_EQ("alice", restored.profile_component().name());
    EXPECT_EQ(0u, restored.player_id()) << "元数据不进片段,由调用方填回";
}

// 两次投影结果逐字节相同:比脏靠它。
TEST(PlayerStoreProjectionTest, ProjectionIsDeterministic)
{
    player_database db = SamplePlayer();
    player_store_rows::KindBytes first;
    player_store_rows::KindBytes second;
    std::string error;
    ASSERT_TRUE(player_store_rows::ProjectPlayer(db, first, error)) << error;
    ASSERT_TRUE(player_store_rows::ProjectPlayer(db, second, error)) << error;
    EXPECT_EQ(first, second);
}

// 字段换组之后库里两行各有一份:只认当前分组那一行,repeated 不能追加出双份。
TEST(PlayerStoreProjectionTest, FieldInTwoRowsIsNotMergedTwice)
{
    player_database inAsset;
    inAsset.mutable_currency()->add_values(1000);
    // 模拟换行之前的布局:货币曾经写在 kCharacter 行里,现在属于 kWallet。
    player_database inHot;
    inHot.mutable_currency()->add_values(1000);
    inHot.mutable_level_component()->set_level(30);

    google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> rows;
    auto *hot = rows.Add();
    hot->set_kind(playerstore::PLAYER_STORE_KIND_CHARACTER);
    hot->set_payload(inHot.SerializeAsString());
    auto *asset = rows.Add();
    asset->set_kind(playerstore::PLAYER_STORE_KIND_WALLET);
    asset->set_payload(inAsset.SerializeAsString());

    player_database restored;
    std::string error;
    ASSERT_TRUE(player_store_rows::AssemblePlayer(rows, restored, error)) << error;
    EXPECT_EQ(1, restored.currency().values_size()) << "repeated 字段不能翻倍";
    EXPECT_EQ(30u, restored.level_component().level());
}

// 走到投影的消息顶层不该有未知字段:行里不认识的顶层字段不进内存,由存盘状态机按行保管。
// 有就说明这份消息不是从行里加载来的,投影不知道该把它们写回哪一行,拒绝比写丢强。
TEST(PlayerStoreProjectionTest, ProjectRefusesAMessageWithUnknownTopLevelFields)
{
    player_database db;
    db.GetReflection()->MutableUnknownFields(&db)->AddVarint(9999, 1);
    player_store_rows::KindBytes bytes;
    std::string error;
    EXPECT_FALSE(player_store_rows::ProjectPlayer(db, bytes, error));
    EXPECT_FALSE(error.empty());
}

// 新旧版本混跑:更新版本在等级那一行里写了本二进制不认识的顶层字段。拼装照常成功,这些字段不并进消息;
// CarriedTopLevelBytes 把它们原样取出来。本二进制改了这一行里自己认识的字段再存,投影结果接上取出来的字节,
// 更新版本读回来的还是全部字段。
TEST(PlayerStoreProjectionTest, UnknownTopLevelFieldsOfARowAreCarriedOutsideTheMessage)
{
    player_database newer;
    newer.mutable_level_component()->set_level(30);
    newer.GetReflection()->MutableUnknownFields(&newer)->AddVarint(9999, 100);
    newer.GetReflection()->MutableUnknownFields(&newer)->AddLengthDelimited(9998, "specialization");
    const std::string stored = newer.SerializeAsString();

    google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> rows;
    auto *row = rows.Add();
    row->set_kind(playerstore::PLAYER_STORE_KIND_CHARACTER);
    row->set_payload(stored);

    player_database restored;
    std::string error;
    ASSERT_TRUE(player_store_rows::AssemblePlayer(rows, restored, error)) << error;
    EXPECT_EQ(30u, restored.level_component().level());
    EXPECT_TRUE(restored.GetReflection()->GetUnknownFields(restored).empty()) << "不认识的顶层字段不并进消息";

    std::string carried;
    ASSERT_TRUE(player_store_rows::CarriedTopLevelBytes(stored, carried));
    ASSERT_FALSE(carried.empty());

    restored.mutable_level_component()->set_level(31);
    player_store_rows::KindBytes bytes;
    ASSERT_TRUE(player_store_rows::ProjectPlayer(restored, bytes, error)) << error;
    player_database reread;
    ASSERT_TRUE(reread.ParseFromString(bytes[kCharacter] + carried));
    EXPECT_EQ(31u, reread.level_component().level());
    const auto &unknown = reread.GetReflection()->GetUnknownFields(reread);
    ASSERT_EQ(2, unknown.field_count());
    bool sawVarint = false;
    bool sawBytes = false;
    for (int i = 0; i < unknown.field_count(); ++i)
    {
        const auto &field = unknown.field(i);
        if (field.number() == 9999)
        {
            ASSERT_EQ(google::protobuf::UnknownField::TYPE_VARINT, field.type());
            EXPECT_EQ(100u, field.varint());
            sawVarint = true;
        }
        if (field.number() == 9998)
        {
            ASSERT_EQ(google::protobuf::UnknownField::TYPE_LENGTH_DELIMITED, field.type());
            EXPECT_EQ("specialization", std::string(field.length_delimited()));
            sawBytes = true;
        }
    }
    EXPECT_TRUE(sawVarint);
    EXPECT_TRUE(sawBytes);
}

// 行里没有不认识的顶层字段时什么都不带;子消息里不认识的字段跟着对象走,不算这一行的顶层内容;解析不了的行报失败。
TEST(PlayerStoreProjectionTest, CarriedTopLevelBytesCoverOnlyTheTopLevelOfARow)
{
    std::string carried = "stale";
    ASSERT_TRUE(player_store_rows::CarriedTopLevelBytes(SamplePlayer().SerializeAsString(), carried));
    EXPECT_TRUE(carried.empty());

    player_database nested = SamplePlayer();
    auto *level = nested.mutable_level_component();
    level->GetReflection()->MutableUnknownFields(level)->AddVarint(9999, 1);
    carried = "stale";
    ASSERT_TRUE(player_store_rows::CarriedTopLevelBytes(nested.SerializeAsString(), carried));
    EXPECT_TRUE(carried.empty());

    // 字段 1 按长度前缀类型开头,长度却没写完。
    EXPECT_FALSE(player_store_rows::CarriedTopLevelBytes("\x0a\xff", carried));
}

// 更新版本才有的行(号比本二进制认识的都大):不读也不拒绝,库里那一行原样留着。
TEST(PlayerStoreProjectionTest, RowOfANewerKindIsLeftAlone)
{
    EXPECT_TRUE(player_store_rows::IsNewerKind(player_store_rows::kKindCount));
    EXPECT_TRUE(player_store_rows::IsNewerKind(99));
    EXPECT_FALSE(player_store_rows::IsNewerKind(1)) << "退役号不是更新版本的行";
    EXPECT_FALSE(player_store_rows::IsNewerKind(kCharacter));

    google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> rows;
    auto *newer = rows.Add();
    newer->set_kind(static_cast<playerstore::PlayerStoreKind>(99));
    newer->set_payload("bytes this build cannot read");
    player_database character;
    character.mutable_level_component()->set_level(30);
    auto *known = rows.Add();
    known->set_kind(playerstore::PLAYER_STORE_KIND_CHARACTER);
    known->set_payload(character.SerializeAsString());

    player_database restored;
    std::string error;
    ASSERT_TRUE(player_store_rows::AssemblePlayer(rows, restored, error)) << error;
    EXPECT_EQ(30u, restored.level_component().level());
}

// 提交结果带着这一笔的代次和序号,落盘快照要用它们判断能不能跳过存盘。
TEST(PlayerStoreClientTest, CommitReportsEpochAndSeq)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 55, Rows("h0")));
    PlayerStoreClient::SaveResult seen;
    client.Submit(kPlayer, Rows("h1"), {}, [&seen](const PlayerStoreClient::SaveResult &result) { seen = result; });
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED, kEpoch, 1001);
    EXPECT_EQ(PlayerStoreClient::SaveStatus::kCommitted, seen.status);
    EXPECT_EQ(kEpoch, seen.ownerEpoch);
    EXPECT_EQ(1001u, seen.saveSeq);
    EXPECT_FALSE(seen.superseded);
}

// 没变化的排队快照报告上一次提交的值,而不是 0:否则落盘快照以为这份从没提交过,下一次又要强制写。
TEST(PlayerStoreClientTest, UnchangedPendingReportsLastCommit)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 55, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    PlayerStoreClient::SaveResult seen;
    client.Submit(kPlayer, Rows("h1"), {}, [&seen](const PlayerStoreClient::SaveResult &result) { seen = result; });
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_EQ(PlayerStoreClient::SaveStatus::kAlreadyPersisted, seen.status);
    EXPECT_EQ(kEpoch, seen.ownerEpoch);
    EXPECT_EQ(1001u, seen.saveSeq);
}

// force:业务字节没变也要写一笔。
TEST(PlayerStoreClientTest, ForceWritesWithoutDirtyRows)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h0"), Forced(), nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(0, f.sent.front().request.rows_size());
    EXPECT_EQ(kEpoch, f.sent.front().request.owner_epoch());
}

// 路由刷新之后业务字节没变也要写一笔把新代次写进库,写过就不再写。
TEST(PlayerStoreClientTest, RaisedEpochIsWrittenOnce)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.SetOwnerEpoch(kPlayer, kEpoch + 1);
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h0"), {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(0, f.sent.front().request.rows_size());
    EXPECT_EQ(kEpoch + 1, f.sent.front().request.owner_epoch());
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, Rows("h0"), {}, nullptr));
}

// force 粘性:被覆盖的快照要求一定写一笔,这个要求不能跟着它一起丢。
TEST(PlayerStoreClientTest, ForceSurvivesBeingOverwritten)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    client.Submit(kPlayer, Rows("h1"), Forced(), nullptr);
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    ASSERT_EQ(1u, f.sent.size()) << "没有脏行,但有一份要求强制写";
    EXPECT_EQ(0, f.sent.front().request.rows_size());
}

// 释放提交之后复用实体:库里 released=1,没有脏行也要写一笔把它清掉,否则玩家在线却被当成离线。
TEST(PlayerStoreClientTest, CommittedReleaseIsClearedByNextSave)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h0"), Released(), nullptr);
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h0"), {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_FALSE(f.sent.front().request.release());
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, Rows("h0"), {}, nullptr));
}

// 被接管时,排队的快照带着同一个代次,发出去也只会再被拒,一并丢弃。代次变了的照常发。
TEST(PlayerStoreClientTest, OwnerRejectionDropsPendingOfSameEpoch)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    bool droppedSuperseded = false;
    client.Submit(kPlayer, Rows("h2"), {}, [&droppedSuperseded](const PlayerStoreClient::SaveResult &result) { droppedSuperseded = result.superseded; });
    f.Reply(playerstore::SAVE_PLAYER_STALE_OWNER, 200, 1);
    EXPECT_TRUE(droppedSuperseded);
    EXPECT_TRUE(f.sent.empty());
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
}

TEST(PlayerStoreClientTest, OwnerRejectionKeepsPendingOfNewerEpoch)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    client.SetOwnerEpoch(kPlayer, kEpoch + 50);
    client.Submit(kPlayer, Rows("h2"), {}, nullptr);
    f.Reply(playerstore::SAVE_PLAYER_STALE_OWNER, kEpoch + 10, 1);
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h2", PayloadOf(f.sent.front().request, kCharacter));
    EXPECT_EQ(kEpoch + 50, f.sent.front().request.owner_epoch());
}

// 序号饱和:不回绕。调用方还在自己的栈上,所以同步告诉它,不走回调。
TEST(PlayerStoreClientTest, ExhaustedSeqIsReportedSynchronously)
{
    Fixture f;
    f.seqExhausted = true;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    bool called = false;
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kSeqExhausted,
              client.Submit(kPlayer, Rows("h1"), {}, [&called](const PlayerStoreClient::SaveResult &) { called = true; }));
    EXPECT_FALSE(called);
    EXPECT_TRUE(f.sent.empty());
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
}

// 排队的快照开始时序号才饱和:调用方早已离开,结局走回调。
TEST(PlayerStoreClientTest, ExhaustedSeqOnPendingRejectsThroughCallback)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    bool rejected = false;
    client.Submit(kPlayer, Rows("h2"), {}, [&rejected](const PlayerStoreClient::SaveResult &result) { rejected = result.status == PlayerStoreClient::SaveStatus::kRejected; });
    f.seqExhausted = true;
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_TRUE(rejected);
    EXPECT_TRUE(f.sent.empty());
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
}

// 不带状态的空行释放:storage_id 是 0,由 go/db 查落点;发不出去就直接报失败,不重试。
TEST(PlayerStoreClientTest, DetachedReleaseSendsEmptyRequestWithoutState)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    bool ok = false;
    playerstore::SavePlayerResult seen = playerstore::SAVE_PLAYER_RESULT_UNSPECIFIED;
    client.ReleaseDetached(kPlayer, kEpoch, 3, [&](bool callOk, playerstore::SavePlayerResult result) { ok = callOk; seen = result; });
    ASSERT_EQ(1u, f.sent.size());
    const auto &request = f.sent.front().request;
    EXPECT_EQ(0u, request.storage_id());
    EXPECT_TRUE(request.release());
    EXPECT_EQ(0, request.rows_size());
    EXPECT_EQ(kEpoch, request.owner_epoch());
    EXPECT_TRUE(client.HasUnsettled(kPlayer)) << "在途期间不能重新认领:它提交时会把 released 置 1";
    EXPECT_FALSE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    EXPECT_EQ((std::vector<Guid>{kPlayer}), client.UnsettledPlayers());
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_TRUE(ok);
    EXPECT_EQ(playerstore::SAVE_PLAYER_COMMITTED, seen);
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
    EXPECT_TRUE(f.timers.empty()) << "不重试";

    f.nodeAvailable = false;
    bool called = false;
    client.ReleaseDetached(kPlayer, kEpoch, 3, [&](bool callOk, playerstore::SavePlayerResult) { called = true; ok = callOk; });
    EXPECT_TRUE(called);
    EXPECT_FALSE(ok);
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
}

// 停机留证据用:只列还有在途或排队的玩家。
TEST(PlayerStoreClientTest, UnsettledPlayersListsOnlyPlayersWithWork)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    ASSERT_TRUE(client.Seed(kPlayer + 1, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    EXPECT_EQ((std::vector<Guid>{kPlayer}), client.UnsettledPlayers());
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_TRUE(client.UnsettledPlayers().empty());
}

// ── 认领(规格 §3)────────────────────────────────────────────────────────

// 播种用库里的原字节:没变化的行不写,变了的行带着认领返回的落点、代次发出去。
TEST(PlayerStoreClientTest, FinalStopKeepsAcceptedSaveAndClaimUnsettled)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("old")));
    bool saved = false;
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted,
        client.Submit(kPlayer, Rows("new"), {}, [&](const PlayerStoreClient::SaveResult&) { saved = true; }));
    client.RequestClaim(kPlayer + 1);
    ASSERT_EQ(1u, f.sent.size());
    ASSERT_EQ(1u, f.sentClaims.size());

    f.tasks.StopAll();
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED, kEpoch, f.sent.front().request.save_seq());
    f.ReplyClaimed(1, kEpoch, 1, Rows("claimed"));
    EXPECT_TRUE(client.HasUnsettled(kPlayer));
    EXPECT_TRUE(client.HasPendingClaim(kPlayer + 1));
    EXPECT_FALSE(saved);
    EXPECT_TRUE(f.claimed.empty());
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kStopped, client.Submit(kPlayer, Rows("never queued"), {}, {}));
    client.RequestClaim(kPlayer + 2);
    EXPECT_FALSE(client.HasPendingClaim(kPlayer + 2));
    EXPECT_TRUE(f.sent.empty());
    EXPECT_TRUE(f.sentClaims.empty());
}

TEST(PlayerStoreClientTest, InlineReplyIsDeliveredAfterSendReturns)
{
    Fixture f;
    bool saved = false;
    f.options.send = [&](const playerstore::SavePlayerRequest& request, PlayerStoreClient::SendDone done) {
        playerstore::SavePlayerResponse reply;
        reply.set_result(playerstore::SAVE_PLAYER_COMMITTED);
        done(true, &reply);
        EXPECT_FALSE(saved);
        EXPECT_EQ(kPlayer, request.player_id());
        return true;
    };
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("old")));
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted,
        client.Submit(kPlayer, Rows("new"), {}, [&](const PlayerStoreClient::SaveResult& result) {
            EXPECT_EQ(PlayerStoreClient::SaveStatus::kCommitted, result.status);
            saved = true;
        }));
    EXPECT_TRUE(saved);
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
}

TEST(PlayerStoreClientTest, LateSaveReplyCannotCompleteTheNextAttempt)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("old")));
    unsigned completed = 0;
    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted,
        client.Submit(kPlayer, Rows("new"), {}, [&](const PlayerStoreClient::SaveResult&) { ++completed; }));
    ASSERT_EQ(1u, f.sent.size());
    const auto obsolete = f.sent.front().done;
    const auto seq = f.sent.front().request.save_seq();
    f.ReplyUnknown();
    f.FireTimers();
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(seq, f.sent.front().request.save_seq());

    playerstore::SavePlayerResponse late;
    late.set_result(playerstore::SAVE_PLAYER_COMMITTED);
    obsolete(true, &late);
    EXPECT_TRUE(client.HasUnsettled(kPlayer));
    EXPECT_EQ(0u, completed);
    f.Reply(playerstore::SAVE_PLAYER_EQUAL, kEpoch, seq);
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
    EXPECT_EQ(1u, completed);
}

TEST(PlayerStoreClientTest, ClaimSeedsConfirmedWithRawBytes)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    EXPECT_TRUE(client.HasPendingClaim(kPlayer));
    ASSERT_EQ(1u, f.sentClaims.size());
    EXPECT_EQ(kEpoch, f.sentClaims.front().request.owner_epoch());

    f.ReplyClaimed(9, kEpoch, 55, Rows("h0", "a0"));

    EXPECT_FALSE(client.HasPendingClaim(kPlayer));
    ASSERT_EQ(1u, f.claimed.size());
    EXPECT_EQ(playerstore::LOAD_AND_CLAIM_CLAIMED, f.claimed.front().second);
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, Rows("h0", "a0"), {}, nullptr));
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h1", "a0"), {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(9u, f.sent.front().request.storage_id());
    EXPECT_EQ(kEpoch, f.sent.front().request.owner_epoch());
    EXPECT_EQ("h1", PayloadOf(f.sent.front().request, kCharacter));
    EXPECT_EQ("<absent>", PayloadOf(f.sent.front().request, kWallet));
}

// 新号库里只有部分行:缺的 kind 按空比较,投影结果也是空就不写。
TEST(PlayerStoreClientTest, ClaimWithMissingRowsTreatsThemAsEmpty)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    player_store_rows::KindBytes rows;
    rows[kProfile] = "p0";
    f.ReplyClaimed(1, kEpoch, 1, rows);
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, rows, {}, nullptr));
}

// 在途的认领不重发。
TEST(PlayerStoreClientTest, ClaimIsNotResentWhileInFlight)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    client.RequestClaim(kPlayer);
    EXPECT_EQ(1u, f.sentClaims.size());
}

// 存盘未落定就等:同代次销毁重载时,晚到的那一笔会让库和内存分叉。
TEST(PlayerStoreClientTest, ClaimWaitsForUnsettledSave)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);

    client.RequestClaim(kPlayer);
    EXPECT_TRUE(client.HasPendingClaim(kPlayer));
    EXPECT_TRUE(f.sentClaims.empty());

    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_EQ(1u, f.sentClaims.size());
}

// 无状态释放在途同样要等:它提交时会把 released 置 1。发不出去也算结束。
TEST(PlayerStoreClientTest, ClaimWaitsForDetachedRelease)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    client.ReleaseDetached(kPlayer, kEpoch, 3, [](bool, playerstore::SavePlayerResult) {});
    client.RequestClaim(kPlayer);
    EXPECT_TRUE(f.sentClaims.empty());
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_EQ(1u, f.sentClaims.size());
}

// 结果未知就重试(认领幂等),每次重新组请求:路由可能已经带来新的代次。预算用完报告没有结论。
TEST(PlayerStoreClientTest, ClaimRetriesUnknownOutcomeUntilBudget)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    f.ReplyClaimUnknown();
    EXPECT_TRUE(f.claimed.empty());
    ASSERT_EQ(1u, f.timers.size());

    f.claimEpoch = kEpoch + 2;
    f.FireTimers();
    ASSERT_EQ(1u, f.sentClaims.size());
    EXPECT_EQ(kEpoch + 2, f.sentClaims.front().request.owner_epoch());

    f.clock += std::chrono::seconds(31);
    f.ReplyClaimUnknown();
    EXPECT_TRUE(f.timers.empty());
    EXPECT_FALSE(client.HasPendingClaim(kPlayer));
    ASSERT_EQ(1u, f.claimed.size());
    EXPECT_FALSE(f.claimed.front().second.has_value());
}

// 发不出去(没有可用节点)同样按退避重试。
TEST(PlayerStoreClientTest, ClaimRetriesWhenNoNodeIsAvailable)
{
    Fixture f;
    f.nodeAvailable = false;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    EXPECT_TRUE(f.sentClaims.empty());
    ASSERT_EQ(1u, f.timers.size());
    f.nodeAvailable = true;
    f.FireTimers();
    EXPECT_EQ(1u, f.sentClaims.size());
}

// 载入已经放弃(待入场条目没了):不发,报告没有结论。
TEST(PlayerStoreClientTest, ClaimIsNotSentWhenTheLoadWasAbandoned)
{
    Fixture f;
    f.loadAbandoned = true;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    EXPECT_TRUE(f.sentClaims.empty());
    EXPECT_FALSE(client.HasPendingClaim(kPlayer));
    ASSERT_EQ(1u, f.claimed.size());
    EXPECT_FALSE(f.claimed.front().second.has_value());
}

// 明确拒绝不播种。
TEST(PlayerStoreClientTest, ClaimRefusalDoesNotSeed)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    f.ReplyClaim(playerstore::LOAD_AND_CLAIM_NO_OWNER);
    ASSERT_EQ(1u, f.claimed.size());
    EXPECT_EQ(playerstore::LOAD_AND_CLAIM_NO_OWNER, f.claimed.front().second);
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kNotSeeded, client.Submit(kPlayer, Rows("h"), {}, nullptr));
}

// 响应里的 kind 不认识:不能播一份残缺的基准,否则后面的脏判断全错。
TEST(PlayerStoreClientTest, ClaimWithUnknownKindHasNoConclusion)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    playerstore::LoadAndClaimResponse response;
    response.set_result(playerstore::LOAD_AND_CLAIM_CLAIMED);
    response.set_owner_epoch(kEpoch);
    response.add_rows()->set_kind(static_cast<playerstore::PlayerStoreKind>(99));
    f.ReplyClaimWith(response);
    ASSERT_EQ(1u, f.claimed.size());
    EXPECT_FALSE(f.claimed.front().second.has_value());
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kNotSeeded, client.Submit(kPlayer, Rows("h"), {}, nullptr));
}

// 超时之后才到的旧回包不算数:只认当次发送。
TEST(PlayerStoreClientTest, LateClaimReplyOfAnOldAttemptIsIgnored)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    auto staleDone = f.sentClaims.front().done;
    f.ReplyClaimUnknown();
    f.FireTimers();
    ASSERT_EQ(1u, f.sentClaims.size());

    playerstore::LoadAndClaimResponse stale;
    stale.set_result(playerstore::LOAD_AND_CLAIM_NO_OWNER);
    staleDone(true, &stale);

    EXPECT_TRUE(f.claimed.empty());
    EXPECT_TRUE(client.HasPendingClaim(kPlayer));
}

// ── 落定通知 ──────────────────────────────────────────────────────────────

// 没有未落定的存盘就立刻调用;有就等到都落定,中间那一笔提交不算。
TEST(PlayerStoreClientTest, RunWhenSettledWaitsForEverySave)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    int calls = 0;
    client.RunWhenSettled(kPlayer, [&calls] { ++calls; });
    EXPECT_EQ(1, calls) << "此刻没有未落定的存盘";

    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    client.Submit(kPlayer, Rows("h2"), {}, nullptr);
    client.RunWhenSettled(kPlayer, [&calls] { ++calls; });
    EXPECT_EQ(1, calls);
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_EQ(1, calls) << "排队的那一笔还没落定";
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_EQ(2, calls);
}

// 被拒也算落定:调用方要的是「可以去看结果了」,不是「提交成功了」。
TEST(PlayerStoreClientTest, RunWhenSettledFiresOnRejection)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    int calls = 0;
    client.RunWhenSettled(kPlayer, [&calls] { ++calls; });
    f.Reply(playerstore::SAVE_PLAYER_TOO_LARGE);
    EXPECT_EQ(1, calls);
}

// 回调里又发了一笔:等着的认领接着等,不和它交错。
TEST(PlayerStoreClientTest, ClaimKeepsWaitingWhenASettledCallbackSavesAgain)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    client.RunWhenSettled(kPlayer, [&client] { client.Submit(kPlayer, Rows("h2"), {}, nullptr); });
    client.RequestClaim(kPlayer);
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_TRUE(f.sentClaims.empty());
    ASSERT_EQ(1u, f.sent.size());
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_EQ(1u, f.sentClaims.size());
}

// 回调里回收了状态(被废黜时销毁实体):等着落定的调用方照样要通知。
TEST(PlayerStoreClientTest, RunWhenSettledFiresEvenIfTheCallbackForgotThePlayer)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, [&client](const PlayerStoreClient::SaveResult &) { client.Forget(kPlayer); });
    int calls = 0;
    client.RunWhenSettled(kPlayer, [&calls] { ++calls; });
    f.Reply(playerstore::SAVE_PLAYER_STALE_OWNER, kEpoch + 1, 1);
    EXPECT_EQ(1, calls);
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kNotSeeded, client.Submit(kPlayer, Rows("h1"), {}, nullptr));
}

// ── 提交水位(资产流水跟着存盘提交走,TransactionLogSystem 类注释)──────────────────────
namespace
{
    struct WatermarkProbe
    {
        uint64_t mark{0};
        std::vector<std::pair<Guid, uint64_t>> reported;

        void Install(PlayerStoreClient::Options &options)
        {
            options.commitMark = [this] { return mark; };
            options.onCommittedThrough = [this](Guid player, uint64_t commitMark) { reported.emplace_back(player, commitMark); };
        }
    };
}

// 只有确定提交了才报告水位,而且先报告、后回调:回调里可能销毁实体,那之后没放行的流水会被丢掉。
TEST(PlayerStoreClientTest, CommitWatermarkIsReportedOnlyAfterTheCommitAndBeforeTheCallback)
{
    Fixture f;
    WatermarkProbe probe;
    probe.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    probe.mark = 5;
    std::size_t reportedAtCallback = 99;
    client.Submit(kPlayer, Rows("h1"), {}, [&](const PlayerStoreClient::SaveResult &) { reportedAtCallback = probe.reported.size(); });
    probe.mark = 6; // 取快照之后才记的流水,这份快照提交了也不能放
    EXPECT_TRUE(probe.reported.empty()) << "发出去不算提交";

    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);

    ASSERT_EQ(1u, probe.reported.size());
    EXPECT_EQ(kPlayer, probe.reported[0].first);
    EXPECT_EQ(5u, probe.reported[0].second);
    EXPECT_EQ(1u, reportedAtCallback);
}

// 被拒、结果未知都不报告:这份快照里的改动不一定在库里。
TEST(PlayerStoreClientTest, RejectedOrUnknownSavesDoNotReportTheWatermark)
{
    Fixture f;
    WatermarkProbe probe;
    probe.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    probe.mark = 5;
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);

    f.ReplyUnknown();
    EXPECT_TRUE(probe.reported.empty());
    f.FireTimers();
    ASSERT_EQ(1u, f.sent.size()) << "结果未知按同内容重发";
    f.Reply(playerstore::SAVE_PLAYER_STALE_OWNER, kEpoch + 1, 1);

    EXPECT_TRUE(probe.reported.empty());
}

// 被更新的快照取代的那份不单独报告;取代它的那份提交时水位更大,一并放行。
TEST(PlayerStoreClientTest, SupersededSnapshotsAreReleasedByTheLaterCommit)
{
    Fixture f;
    WatermarkProbe probe;
    probe.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    probe.mark = 5;
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    probe.mark = 8;
    client.Submit(kPlayer, Rows("h2"), {}, nullptr);
    probe.mark = 9;
    client.Submit(kPlayer, Rows("h3"), {}, nullptr);

    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    ASSERT_EQ(1u, probe.reported.size());
    EXPECT_EQ(5u, probe.reported[0].second);
    ASSERT_EQ(1u, f.sent.size()) << "只发最新的那份";
    EXPECT_EQ("h3", PayloadOf(f.sent.front().request, kCharacter));

    f.Reply(playerstore::SAVE_PLAYER_EQUAL);
    ASSERT_EQ(2u, probe.reported.size()) << "EQUAL 也是提交";
    EXPECT_EQ(9u, probe.reported[1].second);
}

// 和已确认的内容一样、不用写的快照,改动早已在库里:立刻报告,不管是新交的还是排队轮到的。
TEST(PlayerStoreClientTest, SnapshotsWithNothingToWriteReportTheWatermark)
{
    Fixture f;
    WatermarkProbe probe;
    probe.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    probe.mark = 3;
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, Rows("h0"), {}, nullptr));
    ASSERT_EQ(1u, probe.reported.size());
    EXPECT_EQ(3u, probe.reported[0].second);

    probe.mark = 4;
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    probe.mark = 7;
    client.Submit(kPlayer, Rows("h1"), {}, nullptr); // 排队,内容和在途那笔一样
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);

    EXPECT_TRUE(f.sent.empty()) << "排队的那份不用写";
    ASSERT_EQ(3u, probe.reported.size());
    EXPECT_EQ(4u, probe.reported[1].second);
    EXPECT_EQ(7u, probe.reported[2].second);
}

// ── 新分行与「只顺带」的行(docs/design/player-save-direct-commit.md §1.1)────────────────────

namespace
{
    constexpr auto kLocation = static_cast<std::size_t>(playerstore::PLAYER_STORE_KIND_LOCATION);

    player_store_rows::KindBytes WithLocation(player_store_rows::KindBytes rows, const std::string &location)
    {
        rows[kLocation] = location;
        return rows;
    }
}

// 写权行和退役号不是数据行;每行数据都声明了存盘方式,只有位置只顺带。
TEST(PlayerStoreProjectionTest, EveryDataKindDeclaresASavePolicy)
{
    EXPECT_FALSE(player_store_rows::IsDataKind(0)) << "写权行";
    EXPECT_FALSE(player_store_rows::IsDataKind(1)) << "退役的 HOT";
    EXPECT_FALSE(player_store_rows::IsDataKind(2)) << "退役的 ASSET";
    EXPECT_FALSE(player_store_rows::IsDataKind(5)) << "退役的 LEGACY1(player_database_1)";
    EXPECT_FALSE(player_store_rows::IsDataKind(player_store_rows::kKindCount));
    for (const auto kind : {playerstore::PLAYER_STORE_KIND_GROWTH, playerstore::PLAYER_STORE_KIND_PROFILE,
                            playerstore::PLAYER_STORE_KIND_CHARACTER,
                            playerstore::PLAYER_STORE_KIND_WALLET, playerstore::PLAYER_STORE_KIND_BAG})
    {
        const auto index = static_cast<std::size_t>(kind);
        EXPECT_TRUE(player_store_rows::IsDataKind(index)) << index;
        EXPECT_EQ(PERSIST_ROW_SAVE_PERIODIC, player_store_rows::RowSavePolicy(index)) << index;
    }
    EXPECT_TRUE(player_store_rows::IsDataKind(kLocation));
    EXPECT_EQ(PERSIST_ROW_SAVE_PIGGYBACK, player_store_rows::RowSavePolicy(kLocation));
}

// 只改位置时只有位置那一行有内容;位置那一行里只有 transform,资产、账本不会被推迟。
TEST(PlayerStoreProjectionTest, LocationRowHoldsOnlyTheTransform)
{
    player_database db;
    db.mutable_transform()->mutable_location()->set_x(1.5);
    player_store_rows::KindBytes bytes;
    std::string error;
    ASSERT_TRUE(player_store_rows::ProjectPlayer(db, bytes, error)) << error;
    for (std::size_t kind = 1; kind < player_store_rows::kKindCount; ++kind)
    {
        if (kind == kLocation)
        {
            continue;
        }
        EXPECT_TRUE(bytes[kind].empty()) << "kind " << kind << " 不该有内容";
    }
    player_database location;
    ASSERT_TRUE(location.ParseFromString(bytes[kLocation]));
    EXPECT_DOUBLE_EQ(1.5, location.transform().location().x());
    std::vector<const google::protobuf::FieldDescriptor *> present;
    location.GetReflection()->ListFields(location, &present);
    ASSERT_EQ(1u, present.size());
    EXPECT_EQ("transform", std::string(present.front()->name()));
}

// 旧布局写的行(HOT=1、ASSET=2、LEGACY1=5)按现在的分行解读会丢字段,整份拒绝。
TEST(PlayerStoreProjectionTest, RetiredKindIsRejected)
{
    for (const int retired : {1, 2, 5})
    {
        google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> rows;
        auto *row = rows.Add();
        row->set_kind(static_cast<playerstore::PlayerStoreKind>(retired));
        row->set_payload(SamplePlayer().SerializeAsString());
        player_database restored;
        std::string error;
        EXPECT_FALSE(player_store_rows::AssemblePlayer(rows, restored, error)) << retired;
        EXPECT_FALSE(error.empty());
    }
}

TEST(PlayerStoreClientTest, ClaimWithRetiredKindHasNoConclusion)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    playerstore::LoadAndClaimResponse response;
    response.set_result(playerstore::LOAD_AND_CLAIM_CLAIMED);
    response.set_storage_id(1);
    response.set_owner_epoch(kEpoch);
    response.add_rows()->set_kind(static_cast<playerstore::PlayerStoreKind>(1));
    f.ReplyClaimWith(response);
    ASSERT_EQ(1u, f.claimed.size());
    EXPECT_FALSE(f.claimed.front().second.has_value()) << "退役行不播种";
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kNotSeeded, client.Submit(kPlayer, Rows("h0"), {}, nullptr));
}

// ── 新旧版本混跑:行里本二进制不认识的内容(混版本设计 §4.2)──────────────────────────────
namespace
{
    // 假的 Options::carriedBytes:'#' 起到行尾算本二进制不认识的内容。这里的行不是真的 protobuf,
    // 真实的取法(player_store_rows::CarriedTopLevelBytes)在上面的投影用例里测。
    bool FakeCarriedBytes(const std::string &payload, std::string &carried)
    {
        if (payload == "corrupt")
        {
            return false;
        }
        const auto mark = payload.find('#');
        carried = mark == std::string::npos ? std::string() : payload.substr(mark);
        return true;
    }
}

// 更新版本在 kCharacter 行里写了本二进制不认识的内容("#new")。投影结果只有认识的部分,状态机接上带回的内容
// 再比脏、再发送:没改就不写;改了认识的部分,写回去的行里它还在;提交之后照样如此。
TEST(PlayerStoreClientTest, CarriedBytesOfARowAreWrittenBackWithEveryWriteOfThatRow)
{
    Fixture f;
    f.options.carriedBytes = FakeCarriedBytes;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    f.ReplyClaimed(1, kEpoch, 55, Rows("h0#new", "a0"));
    ASSERT_EQ(1u, f.claimed.size());
    EXPECT_EQ(playerstore::LOAD_AND_CLAIM_CLAIMED, f.claimed.front().second);

    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, Rows("h0", "a0"), {}, nullptr));
    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h1", "a1"), {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h1#new", PayloadOf(f.sent.front().request, kCharacter));
    EXPECT_EQ("a1", PayloadOf(f.sent.front().request, kWallet)) << "没有带回内容的行照旧";
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);

    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, Rows("h1", "a1"), {}, nullptr));
    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h2", "a1"), {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h2#new", PayloadOf(f.sent.front().request, kCharacter));
    EXPECT_EQ("<absent>", PayloadOf(f.sent.front().request, kWallet));
}

// 在途时排队的快照也带着:排队的是接好之后的整行,轮到它时不会少掉带回的内容,也不会接两遍。
TEST(PlayerStoreClientTest, QueuedSnapshotKeepsItsCarriedBytes)
{
    Fixture f;
    f.options.carriedBytes = FakeCarriedBytes;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    f.ReplyClaimed(1, kEpoch, 55, Rows("h0#new", "a0"));

    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h1", "a0"), {}, nullptr));
    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h2", "a0"), {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h1#new", PayloadOf(f.sent.front().request, kCharacter));
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h2#new", PayloadOf(f.sent.front().request, kCharacter));
}

// 只顺带的行也一样:带回的内容不算变化,定时存盘照常推迟;真要写位置时带回的内容跟着写。
TEST(PlayerStoreClientTest, PeriodicDeferComparesRowsWithTheirCarriedBytes)
{
    Fixture f;
    f.options.carriedBytes = FakeCarriedBytes;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    f.ReplyClaimed(1, kEpoch, 55, WithLocation(Rows("h0#new"), "l0#loc"));

    EXPECT_FALSE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h0"), "l0"))) << "没有任何变化,交给 Submit 判已落盘";
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, WithLocation(Rows("h0"), "l0"), {}, nullptr));
    EXPECT_TRUE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h0"), "l1"))) << "只有位置变了";

    const auto rows = WithLocation(Rows("h1"), "l1");
    EXPECT_FALSE(client.DeferPeriodicSave(kPlayer, rows)) << "别的行也变了";
    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, rows, {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h1#new", PayloadOf(f.sent.front().request, kCharacter));
    EXPECT_EQ("l1#loc", PayloadOf(f.sent.front().request, kLocation));
}

// 更新版本才有的行:不播种也不拒绝。本二进制不会写它,库里那一行原样留着。
TEST(PlayerStoreClientTest, ClaimLeavesRowsOfNewerKindsUntouched)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    playerstore::LoadAndClaimResponse response;
    response.set_result(playerstore::LOAD_AND_CLAIM_CLAIMED);
    response.set_storage_id(1);
    response.set_owner_epoch(kEpoch);
    auto *known = response.add_rows();
    known->set_kind(playerstore::PLAYER_STORE_KIND_CHARACTER);
    known->set_payload("h0");
    auto *newer = response.add_rows();
    newer->set_kind(static_cast<playerstore::PlayerStoreKind>(99));
    newer->set_payload("from-a-newer-version");
    f.ReplyClaimWith(response);
    ASSERT_EQ(1u, f.claimed.size());
    EXPECT_EQ(playerstore::LOAD_AND_CLAIM_CLAIMED, f.claimed.front().second);

    player_store_rows::KindBytes rows;
    rows[kCharacter] = "h0";
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, rows, {}, nullptr));
    rows[kCharacter] = "h1";
    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, rows, {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    ASSERT_EQ(1, f.sent.front().request.rows_size()) << "只写自己认识的行";
    EXPECT_EQ("h1", PayloadOf(f.sent.front().request, kCharacter));
}

// 行解析不了:取不出要带回的内容就不能播种,否则第一次重写这一行可能把它们抹掉。
TEST(PlayerStoreClientTest, ClaimWithAnUnreadableRowHasNoConclusion)
{
    Fixture f;
    f.options.carriedBytes = FakeCarriedBytes;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    f.ReplyClaimed(1, kEpoch, 55, Rows("corrupt"));
    ASSERT_EQ(1u, f.claimed.size());
    EXPECT_FALSE(f.claimed.front().second.has_value());
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kNotSeeded, client.Submit(kPlayer, Rows("h0"), {}, nullptr));
}

// 重新认领以新回包为准:上一次带回的内容不留到这一次。
TEST(PlayerStoreClientTest, ReclaimReplacesCarriedBytes)
{
    Fixture f;
    f.options.carriedBytes = FakeCarriedBytes;
    PlayerStoreClient client(f.options);
    client.RequestClaim(kPlayer);
    f.ReplyClaimed(1, kEpoch, 55, Rows("h0#new", "a0"));
    client.RequestClaim(kPlayer);
    f.ReplyClaimed(1, kEpoch, 56, Rows("h0", "a0"));
    ASSERT_EQ(2u, f.claimed.size());

    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, Rows("h0", "a0"), {}, nullptr));
    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h1", "a0"), {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h1", PayloadOf(f.sent.front().request, kCharacter));
}

// 定时存盘只遇到位置变化:不发、不取号、不进状态机。
TEST(PlayerStoreClientTest, PeriodicSaveDefersWhenOnlyLocationChanged)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, WithLocation(Rows("h0"), "l0")));
    EXPECT_TRUE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h0"), "l1")));
    EXPECT_TRUE(f.sent.empty());
    EXPECT_EQ(1000u, f.nextSeq) << "推迟不取序号";
    EXPECT_FALSE(client.HasUnsettled(kPlayer)) << "推迟的快照不进状态机";
}

// 有别的行要写时位置顺带写出去。
TEST(PlayerStoreClientTest, LocationRidesAlongWithOtherRows)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, WithLocation(Rows("h0"), "l0")));
    const auto rows = WithLocation(Rows("h1"), "l1");
    EXPECT_FALSE(client.DeferPeriodicSave(kPlayer, rows));
    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, rows, {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h1", PayloadOf(f.sent.front().request, kCharacter));
    EXPECT_EQ("l1", PayloadOf(f.sent.front().request, kLocation));
}

// 没有任何变化不归推迟管,照旧由 Submit 判已落盘。
TEST(PlayerStoreClientTest, UnchangedSnapshotIsNotDeferred)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    const auto rows = WithLocation(Rows("h0"), "l0");
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, rows));
    EXPECT_FALSE(client.DeferPeriodicSave(kPlayer, rows));
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, rows, {}, nullptr));
}

// 从第一次推迟算,到兜底间隔就照常写;写出去之后重新计时。
TEST(PlayerStoreClientTest, DeferredLocationIsWrittenAtTheFallback)
{
    Fixture f;
    f.options.piggybackFallbackSec = 300;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, WithLocation(Rows("h0"), "l0")));
    EXPECT_TRUE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h0"), "l1")));
    f.clock += std::chrono::seconds(299);
    EXPECT_TRUE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h0"), "l2")));
    f.clock += std::chrono::seconds(1);
    const auto rows = WithLocation(Rows("h0"), "l3");
    EXPECT_FALSE(client.DeferPeriodicSave(kPlayer, rows)) << "到兜底间隔";
    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, rows, {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    ASSERT_EQ(1, f.sent.front().request.rows_size());
    EXPECT_EQ("l3", PayloadOf(f.sent.front().request, kLocation));
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);

    f.clock += std::chrono::seconds(1);
    EXPECT_TRUE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h0"), "l4"))) << "写出去之后重新计时";
}

// 别的入口(资产、关键进度)写出了位置:推迟从下一次重新计时,不按最早那次判超时。
TEST(PlayerStoreClientTest, DeferralRestartsWhenAnotherSaveWritesLocation)
{
    Fixture f;
    f.options.piggybackFallbackSec = 300;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, WithLocation(Rows("h0"), "l0")));
    EXPECT_TRUE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h0"), "l1")));
    f.clock += std::chrono::seconds(200);
    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, WithLocation(Rows("h1"), "l2"), {}, nullptr));
    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    f.clock += std::chrono::seconds(200);
    EXPECT_TRUE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h1"), "l3")));
}

// 有在途时不推迟,照常并进排队。
TEST(PlayerStoreClientTest, NoDeferralWhileASaveIsInFlight)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, WithLocation(Rows("h0"), "l0")));
    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, WithLocation(Rows("h1"), "l0"), {}, nullptr));
    EXPECT_FALSE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h1"), "l1")));
}

// 位置那一行结果未决(库里是哪一份不知道):不拖,照常写。
TEST(PlayerStoreClientTest, UncertainLocationIsNotDeferred)
{
    Fixture f;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, WithLocation(Rows("h0"), "l0")));
    client.Submit(kPlayer, WithLocation(Rows("h0"), "l1"), {}, nullptr);
    f.ReplyUnknown();
    f.FireTimers();
    f.Reply(playerstore::SAVE_PLAYER_STALE_OWNER, 200, 55);
    ASSERT_FALSE(client.HasUnsettled(kPlayer));
    EXPECT_FALSE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h0"), "l2")));
}

// 兜底间隔配 0:不推迟,定时存盘遇到位置变化也写(压测对照)。
TEST(PlayerStoreClientTest, ZeroFallbackNeverDefers)
{
    Fixture f;
    f.options.piggybackFallbackSec = 0;
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, WithLocation(Rows("h0"), "l0")));
    EXPECT_FALSE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h0"), "l1")));
}

// ── 审计发件箱(docs/design/player-audit-outbox.md §5.2)──────────────────────────────────
namespace
{
    // 假的审计队列,语义与 AuditOutbox 相同:按水位排队,按快照水位交出(一笔有预算),放行时删掉水位不大于它的。
    struct AuditProbe
    {
        uint64_t mark{0};
        std::vector<std::pair<uint64_t, uint64_t>> queued; // (水位, tx_id)
        std::vector<uint64_t> acked;
        // 一笔最多带几条。AuditOutbox 的预算按字节算(kMaxBytesPerSave),这里按条数模拟;缺省不限。
        std::size_t perSave{static_cast<std::size_t>(-1)};

        void Record(uint64_t txId) { queued.emplace_back(++mark, txId); }

        void Install(PlayerStoreClient::Options &options)
        {
            options.commitMark = [this] { return mark; };
            options.auditDue = [this](Guid, uint64_t commitMark) { return !queued.empty() && queued.front().first <= commitMark; };
            options.attachAudit = [this](Guid, uint64_t commitMark, playerstore::SavePlayerRequest &request)
            {
                PlayerStoreClient::AuditAttachment attached;
                std::size_t count = 0;
                for (const auto &[recordMark, txId] : queued)
                {
                    if (recordMark > commitMark)
                    {
                        break;
                    }
                    if (count == perSave)
                    {
                        attached.more = true;
                        break;
                    }
                    request.add_transaction_logs()->set_tx_id(txId);
                    attached.through = recordMark;
                    ++count;
                }
                return attached;
            };
            options.onCommittedThrough = [this](Guid, uint64_t commitMark)
            {
                acked.push_back(commitMark);
                while (!queued.empty() && queued.front().first <= commitMark)
                {
                    queued.erase(queued.begin());
                }
            };
        }
    };

    std::vector<uint64_t> TxIdsOf(const playerstore::SavePlayerRequest &request)
    {
        std::vector<uint64_t> ids;
        for (const auto &entry : request.transaction_logs())
        {
            ids.push_back(entry.tx_id());
        }
        return ids;
    }

    // 认识审计字段的 go/db 的回包:COMMITTED / EQUAL 时 audit_outbox_written 为真。传 false 模拟旧 go/db。
    void ReplyWithAudit(Fixture &f, playerstore::SavePlayerResult result, bool auditWritten)
    {
        ASSERT_FALSE(f.sent.empty());
        Fixture::Sent front = std::move(f.sent.front());
        f.sent.pop_front();
        playerstore::SavePlayerResponse response;
        response.set_result(result);
        response.set_audit_outbox_written(auditWritten);
        front.done(true, &response);
    }
}

// 有到期的审计记录:没有脏行也写一笔(净变化为零的改动同样有流水要交),提交并确认写了发件箱才放行。
TEST(PlayerStoreClientTest, DueAuditForcesAWriteAndIsReleasedAfterTheOutboxWrite)
{
    Fixture f;
    AuditProbe audit;
    audit.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    audit.Record(1);

    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h0"), {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(0, f.sent.front().request.rows_size()) << "行没变,只带审计记录";
    EXPECT_EQ(TxIdsOf(f.sent.front().request), (std::vector<uint64_t>{1}));
    EXPECT_TRUE(audit.acked.empty()) << "发出去不算提交";

    ReplyWithAudit(f, playerstore::SAVE_PLAYER_COMMITTED, true);
    EXPECT_EQ(audit.acked, (std::vector<uint64_t>{1}));
    EXPECT_TRUE(audit.queued.empty());
    EXPECT_EQ(PlayerStoreClient::SubmitResult::kAlreadyPersisted, client.Submit(kPlayer, Rows("h0"), {}, nullptr))
        << "交完了、行也没变,不再写";
}

// 取快照之后才记的不在这份快照里:留给下一笔。
TEST(PlayerStoreClientTest, AuditRecordedAfterTheSnapshotWaitsForTheNextSave)
{
    Fixture f;
    AuditProbe audit;
    audit.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    audit.Record(1);
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    audit.Record(2);

    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(TxIdsOf(f.sent.front().request), (std::vector<uint64_t>{1}));
    ReplyWithAudit(f, playerstore::SAVE_PLAYER_COMMITTED, true);
    ASSERT_EQ(1u, audit.queued.size());
    EXPECT_EQ(2u, audit.queued.front().second);

    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h1"), {}, nullptr));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(TxIdsOf(f.sent.front().request), (std::vector<uint64_t>{2}));
}

// 结果未知按同内容重发:审计记录和行一样,不重新组装;之后记的不混进来。
TEST(PlayerStoreClientTest, UnknownOutcomeResendsTheSameAuditRecords)
{
    Fixture f;
    AuditProbe audit;
    audit.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    audit.Record(1);
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    const std::string original = f.sent.front().request.SerializeAsString();
    audit.Record(2);

    f.ReplyUnknown();
    f.FireTimers();
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(original, f.sent.front().request.SerializeAsString());

    // 之前那次其实提交了:EQUAL 也是提交,确认写了发件箱就放行这份快照的水位。
    ReplyWithAudit(f, playerstore::SAVE_PLAYER_EQUAL, true);
    EXPECT_EQ(audit.acked, (std::vector<uint64_t>{1}));
    ASSERT_EQ(1u, audit.queued.size());
    EXPECT_EQ(2u, audit.queued.front().second);
}

// 旧 go/db 不认识审计字段:存档提交了,审计没写。行照常确认,审计记录不放行,下一笔再交。
TEST(PlayerStoreClientTest, AuditIsKeptWhenTheStoreDidNotWriteTheOutbox)
{
    Fixture f;
    AuditProbe audit;
    audit.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    audit.Record(1);
    PlayerStoreClient::SaveStatus status{PlayerStoreClient::SaveStatus::kRejected};
    client.Submit(kPlayer, Rows("h1"), {}, [&status](const PlayerStoreClient::SaveResult &result) { status = result.status; });
    const uint64_t tooOldBefore = audit_outbox_stats::ReadStoreTooOld();

    ReplyWithAudit(f, playerstore::SAVE_PLAYER_COMMITTED, false);
    EXPECT_EQ(PlayerStoreClient::SaveStatus::kCommitted, status) << "存档本身提交了";
    EXPECT_TRUE(audit.acked.empty());
    EXPECT_EQ(1u, audit.queued.size());
    EXPECT_EQ(1u, audit_outbox_stats::ReadStoreTooOld() - tooOldBefore) << "每一笔都计数,告警 SceneAuditStoreTooOld 读它";

    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h1"), {}, nullptr)) << "行已确认,只为审计再写一笔";
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(0, f.sent.front().request.rows_size());
    EXPECT_EQ(TxIdsOf(f.sent.front().request), (std::vector<uint64_t>{1}));
}

// 没带审计记录的一笔不看 audit_outbox_written:旧回包照常放行水位(只有行,没有可丢的审计)。
TEST(PlayerStoreClientTest, SavesWithoutAuditIgnoreTheOutboxFlag)
{
    Fixture f;
    AuditProbe audit;
    audit.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);
    EXPECT_TRUE(TxIdsOf(f.sent.front().request).empty());

    f.Reply(playerstore::SAVE_PLAYER_COMMITTED);
    EXPECT_EQ(1u, audit.acked.size());
}

// 被拒的那一笔不放行,审计记录随下一笔再交。
TEST(PlayerStoreClientTest, RejectedSaveKeepsAuditForTheNextSave)
{
    Fixture f;
    AuditProbe audit;
    audit.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    audit.Record(1);
    client.Submit(kPlayer, Rows("h1"), {}, nullptr);

    f.Reply(playerstore::SAVE_PLAYER_TOO_LARGE);
    EXPECT_TRUE(audit.acked.empty());
    ASSERT_FALSE(client.HasUnsettled(kPlayer));

    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted, client.Submit(kPlayer, Rows("h1"), {}, nullptr));
    EXPECT_EQ(TxIdsOf(f.sent.front().request), (std::vector<uint64_t>{1}));
}

// 有到期的审计记录时定时存盘不推迟,哪怕只有位置变了。
TEST(PlayerStoreClientTest, PeriodicSaveIsNotDeferredWhileAuditIsDue)
{
    Fixture f;
    AuditProbe audit;
    audit.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, WithLocation(Rows("h0"), "l0")));
    EXPECT_TRUE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h0"), "l1"))) << "对照:没有审计记录时照常推迟";

    audit.Record(1);
    EXPECT_FALSE(client.DeferPeriodicSave(kPlayer, WithLocation(Rows("h0"), "l2")));
}

// 到期的审计记录超过一笔的预算:分笔,不丢。行随第一笔提交;调用方要的 release 与回调留给最后一笔 ——
// release 之后别的节点可以认领,剩下的记录就没有机会由本节点提交了。
TEST(PlayerStoreClientTest, AuditOverTheSaveBudgetIsCarriedByFollowUpSaves)
{
    Fixture f;
    AuditProbe audit;
    audit.perSave = 2;
    audit.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    for (uint64_t txId = 1; txId <= 5; ++txId)
    {
        audit.Record(txId);
    }
    std::vector<PlayerStoreClient::SaveResult> results;
    ASSERT_EQ(PlayerStoreClient::SubmitResult::kAccepted,
              client.Submit(kPlayer, Rows("h1"), Released(), [&results](const PlayerStoreClient::SaveResult &result) { results.push_back(result); }));

    // 第一笔:行和前两条记录,不带 release。
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h1", PayloadOf(f.sent.front().request, kCharacter));
    EXPECT_EQ(TxIdsOf(f.sent.front().request), (std::vector<uint64_t>{1, 2}));
    EXPECT_FALSE(f.sent.front().request.release());
    const uint64_t firstSeq = f.sent.front().request.save_seq();
    ReplyWithAudit(f, playerstore::SAVE_PLAYER_COMMITTED, true);
    EXPECT_TRUE(results.empty()) << "记录还没交完,这次存盘对调用方来说还没完成";
    EXPECT_TRUE(client.HasUnsettled(kPlayer));
    EXPECT_TRUE(f.settled.empty());
    EXPECT_EQ(audit.acked, (std::vector<uint64_t>{2})) << "只放行到带上的最后一条";

    // 第二笔:没有脏行,接着带两条,仍不带 release。
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(0, f.sent.front().request.rows_size());
    EXPECT_EQ(TxIdsOf(f.sent.front().request), (std::vector<uint64_t>{3, 4}));
    EXPECT_FALSE(f.sent.front().request.release());
    EXPECT_GT(f.sent.front().request.save_seq(), firstSeq) << "每一笔是一次独立的提交,各取各的序号";
    ReplyWithAudit(f, playerstore::SAVE_PLAYER_COMMITTED, true);
    EXPECT_TRUE(results.empty());

    // 最后一笔:带走剩下的一条,带上调用方要的 release;它提交了才回调。
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(0, f.sent.front().request.rows_size());
    EXPECT_EQ(TxIdsOf(f.sent.front().request), (std::vector<uint64_t>{5}));
    EXPECT_TRUE(f.sent.front().request.release());
    const uint64_t lastSeq = f.sent.front().request.save_seq();
    ReplyWithAudit(f, playerstore::SAVE_PLAYER_COMMITTED, true);

    ASSERT_EQ(1u, results.size());
    EXPECT_EQ(PlayerStoreClient::SaveStatus::kCommitted, results[0].status);
    EXPECT_FALSE(results[0].superseded);
    EXPECT_EQ(lastSeq, results[0].saveSeq) << "调用方拿到的是最后一笔的序号:库里的写权行停在它上面";
    EXPECT_EQ(audit.acked, (std::vector<uint64_t>{2, 4, 5}));
    EXPECT_TRUE(audit.queued.empty());
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
    ASSERT_EQ(1u, f.settled.size());
    EXPECT_TRUE(f.sent.empty());
}

// 第一笔在途时又来了更新的快照:不另补续笔,更新的那份水位更大,带走剩下的记录,release 也以它为准;第一笔照常回调。
TEST(PlayerStoreClientTest, NewerSnapshotTakesOverTheRestOfTheAudit)
{
    Fixture f;
    AuditProbe audit;
    audit.perSave = 2;
    audit.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    for (uint64_t txId = 1; txId <= 3; ++txId)
    {
        audit.Record(txId);
    }
    std::vector<std::string> calls;
    auto record = [&calls](const char *name)
    {
        return [&calls, name](const PlayerStoreClient::SaveResult &result)
        {
            calls.push_back(std::string(name) + (result.status == PlayerStoreClient::SaveStatus::kCommitted ? ":ok" : ":other") +
                            (result.superseded ? ":superseded" : ""));
        };
    };
    client.Submit(kPlayer, Rows("h1"), Released(), record("first"));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ(TxIdsOf(f.sent.front().request), (std::vector<uint64_t>{1, 2}));
    EXPECT_FALSE(f.sent.front().request.release());

    audit.Record(4);
    client.Submit(kPlayer, Rows("h2"), {}, record("second")); // 同节点重连取消了退出:这一份不带 release
    ReplyWithAudit(f, playerstore::SAVE_PLAYER_COMMITTED, true);

    EXPECT_EQ(calls, (std::vector<std::string>{"first:ok"}));
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_EQ("h2", PayloadOf(f.sent.front().request, kCharacter));
    EXPECT_EQ(TxIdsOf(f.sent.front().request), (std::vector<uint64_t>{3, 4}));
    EXPECT_FALSE(f.sent.front().request.release()) << "release 以最后一份快照为准";
    ReplyWithAudit(f, playerstore::SAVE_PLAYER_COMMITTED, true);

    EXPECT_EQ(calls, (std::vector<std::string>{"first:ok", "second:ok"}));
    EXPECT_EQ(audit.acked, (std::vector<uint64_t>{2, 4}));
    EXPECT_TRUE(audit.queued.empty());
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
}

// 分笔的第一笔被拒:结局原样交给调用方,不自己接着发 —— 续笔只在提交之后才有。
TEST(PlayerStoreClientTest, RejectedFirstChunkReportsToTheCallerWithoutAFollowUp)
{
    Fixture f;
    AuditProbe audit;
    audit.perSave = 2;
    audit.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    for (uint64_t txId = 1; txId <= 3; ++txId)
    {
        audit.Record(txId);
    }
    std::vector<PlayerStoreClient::SaveResult> results;
    client.Submit(kPlayer, Rows("h1"), Released(), [&results](const PlayerStoreClient::SaveResult &result) { results.push_back(result); });
    ASSERT_EQ(1u, f.sent.size());

    f.Reply(playerstore::SAVE_PLAYER_STALE_OWNER, kEpoch + 1, 9);

    ASSERT_EQ(1u, results.size());
    EXPECT_EQ(PlayerStoreClient::SaveStatus::kRejected, results[0].status);
    EXPECT_TRUE(f.sent.empty());
    EXPECT_TRUE(audit.acked.empty());
    EXPECT_EQ(3u, audit.queued.size()) << "记录留在队列,由调用方的处置决定去留";
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
}

// go/db 不认识审计字段(比 scene 旧)又赶上记录超过一笔的预算:记录交不出去,分笔只会一笔接一笔空转。
// 第一次发现之后不再分笔:补一笔把调用方要的 release 带上就收住,记录留在队列。
TEST(PlayerStoreClientTest, StoreWithoutTheOutboxDoesNotLoopOnAuditOverTheBudget)
{
    Fixture f;
    AuditProbe audit;
    audit.perSave = 2;
    audit.Install(f.options);
    PlayerStoreClient client(f.options);
    ASSERT_TRUE(client.Seed(kPlayer, 1, kEpoch, 0, Rows("h0")));
    for (uint64_t txId = 1; txId <= 3; ++txId)
    {
        audit.Record(txId);
    }
    std::vector<PlayerStoreClient::SaveResult> results;
    client.Submit(kPlayer, Rows("h1"), Released(), [&results](const PlayerStoreClient::SaveResult &result) { results.push_back(result); });
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_FALSE(f.sent.front().request.release()) << "还不知道 go/db 太旧:照常分笔";

    ReplyWithAudit(f, playerstore::SAVE_PLAYER_COMMITTED, false);
    EXPECT_TRUE(results.empty());
    ASSERT_EQ(1u, f.sent.size());
    EXPECT_TRUE(f.sent.front().request.release()) << "交不出去就不再分笔:这一笔带上调用方要的 release";
    EXPECT_EQ(TxIdsOf(f.sent.front().request), (std::vector<uint64_t>{1, 2}));

    ReplyWithAudit(f, playerstore::SAVE_PLAYER_COMMITTED, false);
    ASSERT_EQ(1u, results.size());
    EXPECT_EQ(PlayerStoreClient::SaveStatus::kCommitted, results[0].status);
    EXPECT_TRUE(f.sent.empty()) << "不会一笔接一笔空转";
    EXPECT_TRUE(audit.acked.empty());
    EXPECT_EQ(3u, audit.queued.size());
    EXPECT_FALSE(client.HasUnsettled(kPlayer));
}
