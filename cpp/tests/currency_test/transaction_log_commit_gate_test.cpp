// 流水提交闸门:流水跟着存盘提交走(transaction_log_system.h 类注释)。
// 只测闸门本身的排队、放行与丢弃;存盘状态机何时调用放行在 player_store_client_test.cpp。
#include <gtest/gtest.h>

#include <cstdint>
#include <vector>

#include "modules/transaction_log/transaction_log_system.h"

namespace
{
    constexpr uint64_t kPlayerA = 7001;
    constexpr uint64_t kPlayerB = 7002;

    class TransactionLogCommitGateTest : public testing::Test
    {
    protected:
        void SetUp() override
        {
            TransactionLogSystem::EnableCommitGating(false);
            TransactionLogSystem::SetPublisherForTest([this](const TransactionLogEntry &entry) { published.push_back(entry.tx_id()); });
        }

        void TearDown() override
        {
            for (const uint64_t player : {kPlayerA, kPlayerB})
            {
                (void)TransactionLogSystem::DiscardPending(player, "test teardown");
            }
            TransactionLogSystem::EnableCommitGating(false);
            TransactionLogSystem::SetPublisherForTest(nullptr);
        }

        // 只涉及一名玩家的收益:与 LogCurrencyAdd 同形状(只填 to_player)。
        static TransactionLogEntry Gain(uint64_t txId, uint64_t player)
        {
            TransactionLogEntry entry;
            entry.set_tx_id(txId);
            entry.set_tx_type(TX_CURRENCY_ADD);
            entry.set_to_player(player);
            entry.set_currency_delta(10);
            return entry;
        }

        // 只涉及一名玩家的扣减:与 LogCurrencyDeduct 同形状(只填 from_player)。
        static TransactionLogEntry Spend(uint64_t txId, uint64_t player)
        {
            TransactionLogEntry entry;
            entry.set_tx_id(txId);
            entry.set_tx_type(TX_CURRENCY_DEDUCT);
            entry.set_from_player(player);
            entry.set_currency_delta(-10);
            return entry;
        }

        std::vector<uint64_t> published;
    };

    TEST_F(TransactionLogCommitGateTest, WithoutGatingEntriesArePublishedImmediately)
    {
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));

        EXPECT_EQ(published, (std::vector<uint64_t>{1}));
        EXPECT_EQ(TransactionLogSystem::PendingCount(kPlayerA), 0u);
    }

    TEST_F(TransactionLogCommitGateTest, GatedEntriesWaitForTheCommitThatContainsThem)
    {
        TransactionLogSystem::EnableCommitGating(true);
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        TransactionLogSystem::RecordEntry(Spend(2, kPlayerA));
        const uint64_t snapshotMark = TransactionLogSystem::CommitMark();
        // 取快照之后才发生的改动不在这份快照里,这份快照提交了也不能放它。
        TransactionLogSystem::RecordEntry(Gain(3, kPlayerA));

        EXPECT_TRUE(published.empty());
        EXPECT_EQ(TransactionLogSystem::PendingCount(kPlayerA), 3u);

        TransactionLogSystem::PublishThrough(kPlayerA, snapshotMark);

        EXPECT_EQ(published, (std::vector<uint64_t>{1, 2}));
        EXPECT_EQ(TransactionLogSystem::PendingCount(kPlayerA), 1u);

        TransactionLogSystem::PublishThrough(kPlayerA, TransactionLogSystem::CommitMark());

        EXPECT_EQ(published, (std::vector<uint64_t>{1, 2, 3}));
        EXPECT_EQ(TransactionLogSystem::PendingCount(kPlayerA), 0u);
    }

    TEST_F(TransactionLogCommitGateTest, ACommitOfOnePlayerDoesNotReleaseAnother)
    {
        TransactionLogSystem::EnableCommitGating(true);
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        TransactionLogSystem::RecordEntry(Gain(2, kPlayerB));

        TransactionLogSystem::PublishThrough(kPlayerA, TransactionLogSystem::CommitMark());

        EXPECT_EQ(published, (std::vector<uint64_t>{1}));
        EXPECT_EQ(TransactionLogSystem::PendingCount(kPlayerB), 1u);
    }

    TEST_F(TransactionLogCommitGateTest, DiscardDropsChangesThatNeverCommitted)
    {
        TransactionLogSystem::EnableCommitGating(true);
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        TransactionLogSystem::RecordEntry(Gain(2, kPlayerA));
        const uint64_t staleMark = TransactionLogSystem::CommitMark();

        EXPECT_EQ(TransactionLogSystem::DiscardPending(kPlayerA, "deposed"), 2u);
        EXPECT_EQ(TransactionLogSystem::PendingCount(kPlayerA), 0u);

        // 实体重建之后的新改动拿到更大的水位:旧快照迟到的放行盖不到它。
        TransactionLogSystem::RecordEntry(Gain(3, kPlayerA));
        TransactionLogSystem::PublishThrough(kPlayerA, staleMark);

        EXPECT_TRUE(published.empty());
        EXPECT_EQ(TransactionLogSystem::PendingCount(kPlayerA), 1u);
        EXPECT_EQ(TransactionLogSystem::DiscardPending(kPlayerB, "nothing queued"), 0u);
    }

    TEST_F(TransactionLogCommitGateTest, EntriesWithoutASingleOwnerAreNotGated)
    {
        TransactionLogSystem::EnableCommitGating(true);
        TransactionLogEntry transfer = Gain(1, kPlayerB);
        transfer.set_from_player(kPlayerA);
        TransactionLogEntry system;
        system.set_tx_id(2);
        system.set_tx_type(TX_CURRENCY_ADD);

        TransactionLogSystem::RecordEntry(transfer);
        TransactionLogSystem::RecordEntry(system);

        EXPECT_EQ(published, (std::vector<uint64_t>{1, 2}));
        EXPECT_EQ(TransactionLogSystem::PendingCount(kPlayerA), 0u);
        EXPECT_EQ(TransactionLogSystem::PendingCount(kPlayerB), 0u);
    }

    TEST_F(TransactionLogCommitGateTest, InvalidTxIdIsStillRefusedBeforeQueueing)
    {
        TransactionLogSystem::EnableCommitGating(true);
        TransactionLogSystem::RecordEntry(Gain(0, kPlayerA));

        EXPECT_EQ(TransactionLogSystem::PendingCount(kPlayerA), 0u);
        EXPECT_TRUE(published.empty());
    }

    TEST_F(TransactionLogCommitGateTest, OverflowDropsTheOldestInsteadOfPublishingUncommittedEntries)
    {
        TransactionLogSystem::EnableCommitGating(true);
        // 与 transaction_log_system.cpp 的 kMaxPendingPerPlayer 同值。
        constexpr uint64_t kCap = 4096;
        for (uint64_t txId = 1; txId <= kCap + 2; ++txId)
        {
            TransactionLogSystem::RecordEntry(Gain(txId, kPlayerA));
        }

        EXPECT_TRUE(published.empty());
        EXPECT_EQ(TransactionLogSystem::PendingCount(kPlayerA), kCap);

        TransactionLogSystem::PublishThrough(kPlayerA, TransactionLogSystem::CommitMark());

        ASSERT_EQ(published.size(), kCap);
        EXPECT_EQ(published.front(), 3u);
        EXPECT_EQ(published.back(), kCap + 2);
    }
} // namespace
