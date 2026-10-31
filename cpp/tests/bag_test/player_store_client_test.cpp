#include <chrono>
#include <deque>
#include <functional>
#include <optional>
#include <string>
#include <vector>

#include <gtest/gtest.h>

#include "player/system/player_store_client.h"
#include "player/system/player_store_projection.h"

// 存盘状态机(docs/design/player-save-direct-commit.md §5.1)与片段投影。传输、定时器、发号都是假的。
namespace
{
    constexpr Guid kPlayer = 7;
    constexpr uint64_t kEpoch = 100;
    constexpr auto kHot = static_cast<std::size_t>(playerstore::PLAYER_STORE_KIND_HOT);
    constexpr auto kAsset = static_cast<std::size_t>(playerstore::PLAYER_STORE_KIND_ASSET);
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
        rows[kHot] = hot;
        rows[kAsset] = asset;
        return rows;
    }

    struct Fixture
    {
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
    EXPECT_EQ("h1", PayloadOf(request, kHot));
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
    EXPECT_EQ("h3", PayloadOf(f.sent.front().request, kHot)) << "只发最新的快照";
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
    EXPECT_EQ("h2", PayloadOf(f.sent.front().request, kHot));
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
    EXPECT_EQ("h1", PayloadOf(f.sent.front().request, kHot));
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
    EXPECT_EQ("h0", PayloadOf(f.sent.front().request, kHot));
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
    EXPECT_EQ("h3", PayloadOf(f.sent.front().request, kHot));
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
    player_database_1 db1;
    db1.set_player_id(7);
    player_store_rows::KindBytes bytes;
    std::string error;
    ASSERT_TRUE(player_store_rows::ProjectPlayer(db, db1, bytes, error)) << error;
    EXPECT_EQ(before, db.SerializeAsString()) << "投影之后消息要恢复原样";

    player_database restored;
    player_database_1 restored1;
    ASSERT_TRUE(player_store_rows::AssemblePlayer(ToRows(bytes), restored, restored1, error)) << error;
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
    player_database_1 db1;
    player_store_rows::KindBytes first;
    player_store_rows::KindBytes second;
    std::string error;
    ASSERT_TRUE(player_store_rows::ProjectPlayer(db, db1, first, error)) << error;
    ASSERT_TRUE(player_store_rows::ProjectPlayer(db, db1, second, error)) << error;
    EXPECT_EQ(first, second);
}

// 字段换组之后库里两行各有一份:只认当前分组那一行,repeated 不能追加出双份。
TEST(PlayerStoreProjectionTest, FieldInTwoRowsIsNotMergedTwice)
{
    player_database inAsset;
    inAsset.mutable_currency()->add_values(1000);
    // 旧分组把货币写在 kHot 行里。
    player_database inHot;
    inHot.mutable_currency()->add_values(1000);
    inHot.mutable_level_component()->set_level(30);

    google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> rows;
    auto *hot = rows.Add();
    hot->set_kind(playerstore::PLAYER_STORE_KIND_HOT);
    hot->set_payload(inHot.SerializeAsString());
    auto *asset = rows.Add();
    asset->set_kind(playerstore::PLAYER_STORE_KIND_ASSET);
    asset->set_payload(inAsset.SerializeAsString());

    player_database restored;
    player_database_1 restored1;
    std::string error;
    ASSERT_TRUE(player_store_rows::AssemblePlayer(rows, restored, restored1, error)) << error;
    EXPECT_EQ(1, restored.currency().values_size()) << "repeated 字段不能翻倍";
    EXPECT_EQ(30u, restored.level_component().level());
}

// 新二进制写的字段被旧二进制丢掉,比拒写更糟。
TEST(PlayerStoreProjectionTest, UnknownFieldsAreRejected)
{
    player_database db;
    db.GetReflection()->MutableUnknownFields(&db)->AddVarint(9999, 1);
    player_database_1 db1;
    player_store_rows::KindBytes bytes;
    std::string error;
    EXPECT_FALSE(player_store_rows::ProjectPlayer(db, db1, bytes, error));
    EXPECT_FALSE(error.empty());

    google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> rows;
    auto *row = rows.Add();
    row->set_kind(playerstore::PLAYER_STORE_KIND_HOT);
    row->set_payload(db.SerializeAsString());
    player_database restored;
    player_database_1 restored1;
    EXPECT_FALSE(player_store_rows::AssemblePlayer(rows, restored, restored1, error));
}

TEST(PlayerStoreProjectionTest, UnknownKindIsRejected)
{
    google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> rows;
    rows.Add()->set_kind(static_cast<playerstore::PlayerStoreKind>(99));
    player_database restored;
    player_database_1 restored1;
    std::string error;
    EXPECT_FALSE(player_store_rows::AssemblePlayer(rows, restored, restored1, error));
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
    EXPECT_EQ("h2", PayloadOf(f.sent.front().request, kHot));
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
    EXPECT_EQ("h1", PayloadOf(f.sent.front().request, kHot));
    EXPECT_EQ("<absent>", PayloadOf(f.sent.front().request, kAsset));
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
