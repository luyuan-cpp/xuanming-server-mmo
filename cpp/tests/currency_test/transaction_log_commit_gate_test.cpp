// 审计发件箱的 scene 一侧(audit_outbox.h,docs/design/player-audit-outbox.md §5):流水与快照请求按玩家排队,
// 随存盘请求交出去、提交后放行、改动没落库时丢弃;没开发件箱的进程流水直接发。
// 文件名沿用 11-05 的流水提交闸门(它被审计发件箱取代);存盘状态机何时交出、何时放行在 player_store_client_test.cpp。
#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <vector>

#include "modules/audit/audit_outbox.h"
#include "modules/transaction_log/transaction_log_system.h"

namespace
{
    constexpr uint64_t kPlayerA = 7001;
    constexpr uint64_t kPlayerB = 7002;

    class AuditOutboxTest : public testing::Test
    {
    protected:
        void SetUp() override
        {
            AuditOutbox::Enable(false);
            TransactionLogSystem::SetPublisherForTest([this](const TransactionLogEntry &entry) { published.push_back(entry.tx_id()); });
        }

        void TearDown() override
        {
            for (const uint64_t player : {kPlayerA, kPlayerB})
            {
                (void)AuditOutbox::Discard(player, "test teardown");
            }
            AuditOutbox::Enable(false);
            // 用例里的钩子按引用捕获了栈上的变量,不能留到下一个用例。
            AuditOutbox::SetTransactionLogQueuedHook(nullptr);
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

        static PlayerSnapshotEntry SnapshotRequest(uint64_t snapshotId, uint64_t player)
        {
            PlayerSnapshotEntry request;
            request.set_snapshot_id(snapshotId);
            request.set_player_id(player);
            request.set_trigger(SNAPSHOT_LOGIN);
            return request;
        }

        struct Attached
        {
            google::protobuf::RepeatedPtrField<TransactionLogEntry> logs;
            google::protobuf::RepeatedPtrField<PlayerSnapshotEntry> snapshots;

            std::vector<uint64_t> TxIds() const
            {
                std::vector<uint64_t> ids;
                for (const auto &entry : logs)
                {
                    ids.push_back(entry.tx_id());
                }
                return ids;
            }
        };

        static Attached Attach(uint64_t player, uint64_t mark)
        {
            Attached out;
            AuditOutbox::AttachDue(player, mark, out.logs, out.snapshots);
            return out;
        }

        std::vector<uint64_t> published;
    };

    TEST_F(AuditOutboxTest, WithoutTheOutboxEntriesArePublishedImmediately)
    {
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));

        EXPECT_EQ(published, (std::vector<uint64_t>{1}));
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);
    }

    // 进队列、按快照水位交出、提交后才删;取快照之后才记的不在这份快照里。
    TEST_F(AuditOutboxTest, EntriesRideWithTheSaveWhoseSnapshotContainsThem)
    {
        AuditOutbox::Enable(true);
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        TransactionLogSystem::RecordEntry(Spend(2, kPlayerA));
        const uint64_t snapshotMark = AuditOutbox::Mark();
        TransactionLogSystem::RecordEntry(Gain(3, kPlayerA));

        EXPECT_TRUE(published.empty()) << "进发件箱的流水不直接发 Kafka";
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 3u);
        EXPECT_TRUE(AuditOutbox::HasDue(kPlayerA, snapshotMark));

        const auto attached = Attach(kPlayerA, snapshotMark);
        EXPECT_EQ(attached.TxIds(), (std::vector<uint64_t>{1, 2}));
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 3u) << "交出去不删:被拒时要随下一笔再交";

        AuditOutbox::AckThrough(kPlayerA, snapshotMark);
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 1u);
        EXPECT_FALSE(AuditOutbox::HasDue(kPlayerA, snapshotMark)) << "水位之后的那条不算到期";

        EXPECT_EQ(Attach(kPlayerA, AuditOutbox::Mark()).TxIds(), (std::vector<uint64_t>{3}));
        AuditOutbox::AckThrough(kPlayerA, AuditOutbox::Mark());
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);
        EXPECT_EQ(AuditOutbox::PendingBytes(kPlayerA), 0u);
        EXPECT_TRUE(published.empty());
    }

    // 被拒的那一笔不放行:记录留在队列,下一笔(水位更大)照样把它们交出去。
    TEST_F(AuditOutboxTest, RecordsOfARejectedSaveAreAttachedAgain)
    {
        AuditOutbox::Enable(true);
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        const uint64_t firstMark = AuditOutbox::Mark();
        EXPECT_EQ(Attach(kPlayerA, firstMark).TxIds(), (std::vector<uint64_t>{1}));
        TransactionLogSystem::RecordEntry(Gain(2, kPlayerA));

        EXPECT_EQ(Attach(kPlayerA, AuditOutbox::Mark()).TxIds(), (std::vector<uint64_t>{1, 2}));
    }

    TEST_F(AuditOutboxTest, AckOfOnePlayerDoesNotReleaseAnother)
    {
        AuditOutbox::Enable(true);
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        TransactionLogSystem::RecordEntry(Gain(2, kPlayerB));

        AuditOutbox::AckThrough(kPlayerA, AuditOutbox::Mark());

        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerB), 1u);
        EXPECT_EQ(Attach(kPlayerA, AuditOutbox::Mark()).logs_size(), 0);
    }

    TEST_F(AuditOutboxTest, DiscardDropsChangesThatNeverCommitted)
    {
        AuditOutbox::Enable(true);
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        TransactionLogSystem::RecordEntry(Gain(2, kPlayerA));
        const uint64_t staleMark = AuditOutbox::Mark();

        EXPECT_EQ(AuditOutbox::Discard(kPlayerA, "deposed"), 2u);
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);

        // 实体重建之后的新改动拿到更大的水位:旧快照迟到的放行盖不到它。
        TransactionLogSystem::RecordEntry(Gain(3, kPlayerA));
        AuditOutbox::AckThrough(kPlayerA, staleMark);

        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 1u);
        EXPECT_FALSE(AuditOutbox::HasDue(kPlayerA, staleMark));
        EXPECT_EQ(AuditOutbox::Discard(kPlayerB, "nothing queued"), 0u);
        EXPECT_TRUE(published.empty());
    }

    TEST_F(AuditOutboxTest, EntriesWithoutASingleOwnerArePublishedDirectly)
    {
        AuditOutbox::Enable(true);
        TransactionLogEntry transfer = Gain(1, kPlayerB);
        transfer.set_from_player(kPlayerA);
        TransactionLogEntry system;
        system.set_tx_id(2);
        system.set_tx_type(TX_CURRENCY_ADD);

        TransactionLogSystem::RecordEntry(transfer);
        TransactionLogSystem::RecordEntry(system);

        EXPECT_EQ(published, (std::vector<uint64_t>{1, 2}));
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerB), 0u);
    }

    TEST_F(AuditOutboxTest, InvalidTxIdIsRefusedBeforeQueueing)
    {
        AuditOutbox::Enable(true);
        TransactionLogSystem::RecordEntry(Gain(0, kPlayerA));

        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);
        EXPECT_TRUE(published.empty());
        EXPECT_FALSE(AuditOutbox::EnqueueTransactionLog(kPlayerA, Gain(0, kPlayerA)));
        EXPECT_FALSE(AuditOutbox::EnqueueSnapshot(kPlayerA, SnapshotRequest(0, kPlayerA)));
        EXPECT_FALSE(AuditOutbox::EnqueueTransactionLog(0, Gain(5, kPlayerA))) << "没有玩家号就没有存盘可随";
    }

    TEST_F(AuditOutboxTest, NothingIsQueuedWhileTheOutboxIsOff)
    {
        EXPECT_FALSE(AuditOutbox::EnqueueTransactionLog(kPlayerA, Gain(1, kPlayerA)));
        EXPECT_FALSE(AuditOutbox::EnqueueSnapshot(kPlayerA, SnapshotRequest(9, kPlayerA)));
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);
    }

    // 快照请求与流水同一条水位数轴,按记录顺序交出。
    TEST_F(AuditOutboxTest, SnapshotRequestsShareTheMarkAxis)
    {
        AuditOutbox::Enable(true);
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        ASSERT_TRUE(AuditOutbox::EnqueueSnapshot(kPlayerA, SnapshotRequest(77, kPlayerA)));
        const uint64_t mark = AuditOutbox::Mark();
        TransactionLogSystem::RecordEntry(Gain(2, kPlayerA));

        const auto attached = Attach(kPlayerA, mark);
        EXPECT_EQ(attached.TxIds(), (std::vector<uint64_t>{1}));
        ASSERT_EQ(attached.snapshots_size(), 1);
        EXPECT_EQ(attached.snapshots(0).snapshot_id(), 77u);

        AuditOutbox::AckThrough(kPlayerA, mark);
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 1u);
    }

    TEST_F(AuditOutboxTest, OverflowByCountDropsTheOldestInsteadOfPublishing)
    {
        AuditOutbox::Enable(true);
        constexpr uint64_t kCap = AuditOutbox::kMaxRecordsPerPlayer;
        for (uint64_t txId = 1; txId <= kCap + 2; ++txId)
        {
            TransactionLogSystem::RecordEntry(Gain(txId, kPlayerA));
        }

        EXPECT_TRUE(published.empty());
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), kCap);

        const auto attached = Attach(kPlayerA, AuditOutbox::Mark());
        ASSERT_EQ(static_cast<uint64_t>(attached.logs_size()), kCap);
        EXPECT_EQ(attached.logs(0).tx_id(), 3u);
        EXPECT_EQ(attached.logs(attached.logs_size() - 1).tx_id(), kCap + 2);
    }

    // 字节上限:一笔存盘总能带走全部到期记录(设计 §5.1)。到了就丢最旧的,不拆成两笔。
    TEST_F(AuditOutboxTest, OverflowByBytesKeepsTheQueueWithinOneSave)
    {
        AuditOutbox::Enable(true);
        const std::string extra(10 * 1024, 'x');
        uint64_t txId = 0;
        while (txId < 200)
        {
            TransactionLogEntry entry = Gain(++txId, kPlayerA);
            entry.set_extra(extra);
            TransactionLogSystem::RecordEntry(entry);
        }

        EXPECT_LE(AuditOutbox::PendingBytes(kPlayerA), AuditOutbox::kMaxBytesPerPlayer);
        EXPECT_LT(AuditOutbox::PendingCount(kPlayerA), 200u) << "200 条 10 KiB 的流水超过每人 512 KiB,最旧的被丢";
        const auto attached = Attach(kPlayerA, AuditOutbox::Mark());
        ASSERT_GT(attached.logs_size(), 0);
        EXPECT_EQ(attached.logs(attached.logs_size() - 1).tx_id(), 200u) << "丢的是最旧的,最新的还在";
        std::size_t bytes = 0;
        for (const auto &entry : attached.logs)
        {
            bytes += entry.ByteSizeLong();
        }
        EXPECT_LE(bytes, AuditOutbox::kMaxBytesPerPlayer);
        EXPECT_TRUE(published.empty());
    }

    TEST_F(AuditOutboxTest, OversizedRecordIsRefusedNotPublished)
    {
        AuditOutbox::Enable(true);
        TransactionLogEntry entry = Gain(1, kPlayerA);
        entry.set_extra(std::string(AuditOutbox::kMaxRecordBytes, 'x'));

        TransactionLogSystem::RecordEntry(entry);

        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);
        EXPECT_TRUE(published.empty()) << "宁可缺一条,也不绕开存盘直接发";
    }

    // 资产流水一入队就按那名玩家通知一次(总体方案 §4.5):scene 用它请求合并存盘,货币、物品变动约 1 秒内提交,
    // 不靠每个玩法各自记得触发。通知发生在入队之后,被通知的一方此刻取快照就带得上这一条。
    TEST_F(AuditOutboxTest, QueuedTransactionLogNotifiesTheHookAfterItIsQueued)
    {
        struct Notice
        {
            uint64_t player;
            std::size_t pendingWhenNotified;
            bool operator==(const Notice &other) const { return player == other.player && pendingWhenNotified == other.pendingWhenNotified; }
        };
        std::vector<Notice> notices;
        AuditOutbox::SetTransactionLogQueuedHook([&notices](uint64_t playerId)
        {
            notices.push_back(Notice{playerId, AuditOutbox::PendingCount(playerId)});
        });

        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        EXPECT_TRUE(notices.empty()) << "没开发件箱:流水直接发,没有要随存盘提交的记录,不通知";

        AuditOutbox::Enable(true);
        TransactionLogSystem::RecordEntry(Gain(2, kPlayerA));
        TransactionLogSystem::RecordEntry(Spend(3, kPlayerB));
        TransactionLogSystem::RecordEntry(Spend(4, kPlayerA));

        EXPECT_EQ(notices, (std::vector<Notice>{{kPlayerA, 1}, {kPlayerB, 1}, {kPlayerA, 2}}))
            << "每条入队通知一次、通知时这条已在队列里;合并成一笔存盘是请求存盘那一方的事";
    }

    // 没进队列的不通知:拒收的流水没有要提交的东西;两边都有玩家的流水直接发;快照请求要不要尽快提交由请求方自己决定。
    TEST_F(AuditOutboxTest, RefusedDirectAndSnapshotRecordsDoNotNotifyTheHook)
    {
        AuditOutbox::Enable(true);
        int notified = 0;
        AuditOutbox::SetTransactionLogQueuedHook([&notified](uint64_t) { ++notified; });

        TransactionLogEntry oversized = Gain(1, kPlayerA);
        oversized.set_extra(std::string(AuditOutbox::kMaxRecordBytes, 'x'));
        TransactionLogSystem::RecordEntry(oversized);
        EXPECT_FALSE(AuditOutbox::EnqueueTransactionLog(0, Gain(2, kPlayerA))) << "没有玩家号:拒收";
        TransactionLogEntry transfer = Gain(3, kPlayerA);
        transfer.set_from_player(kPlayerB);
        TransactionLogSystem::RecordEntry(transfer);
        EXPECT_EQ(published, (std::vector<uint64_t>{3})) << "两边都有玩家的流水跟不了某一次提交,直接发";
        EXPECT_TRUE(AuditOutbox::EnqueueSnapshot(kPlayerA, SnapshotRequest(4, kPlayerA)));
        EXPECT_EQ(notified, 0);

        // 清掉钩子之后照常入队,只是不再通知。
        AuditOutbox::SetTransactionLogQueuedHook(nullptr);
        TransactionLogSystem::RecordEntry(Gain(5, kPlayerA));
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 2u) << "快照请求和这条流水";
        EXPECT_EQ(notified, 0);
    }

    // 钩子里换掉钩子自己(scene 重新装配存盘状态机时会重设)不能毁掉正在执行的函数对象。
    TEST_F(AuditOutboxTest, HookMayReplaceItselfWhileRunning)
    {
        AuditOutbox::Enable(true);
        int first = 0;
        int second = 0;
        AuditOutbox::SetTransactionLogQueuedHook([&first, &second](uint64_t)
        {
            AuditOutbox::SetTransactionLogQueuedHook([&second](uint64_t) { ++second; });
            ++first;
        });

        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        TransactionLogSystem::RecordEntry(Gain(2, kPlayerA));

        EXPECT_EQ(first, 1);
        EXPECT_EQ(second, 1);
    }
} // namespace
