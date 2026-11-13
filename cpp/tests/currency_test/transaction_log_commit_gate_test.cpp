// 审计发件箱的 scene 一侧(audit_outbox.h,docs/design/player-audit-outbox.md §5):流水与快照请求按玩家排队,
// 随存盘请求交出去(一笔带不完就分笔)、提交后放行、改动没落库时丢弃;流水只有这一条出路,没有单一玩家或没开发件箱时拒收。
// 每种丢弃按原因计数(audit_outbox_stats.h);资产变动通知(TransactionLogSystem::SetAssetChangedHook)不论收下还是拒收都发。
// 文件名沿用 11-05 的流水提交闸门(它被审计发件箱取代);存盘状态机何时交出、何时放行在 player_store_client_test.cpp。
#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <vector>

#include "modules/audit/audit_outbox.h"
#include "modules/audit/audit_outbox_stats.h"
#include "modules/transaction_log/transaction_log_system.h"

namespace
{
    constexpr uint64_t kPlayerA = 7001;
    constexpr uint64_t kPlayerB = 7002;

    using audit_outbox_stats::DropReason;

    class AuditOutboxTest : public testing::Test
    {
    protected:
        void SetUp() override
        {
            AuditOutbox::Enable(false);
        }

        void TearDown() override
        {
            for (const uint64_t player : {kPlayerA, kPlayerB})
            {
                (void)AuditOutbox::Discard(player, "test teardown");
            }
            AuditOutbox::Enable(false);
            // 用例里的钩子按引用捕获了栈上的变量,不能留到下一个用例。
            TransactionLogSystem::SetAssetChangedHook(nullptr);
        }

        // 丢弃计数是进程级的累计值:用例只比较前后之差。
        static uint64_t Dropped(DropReason reason)
        {
            return audit_outbox_stats::ReadDropped(reason);
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

        // 带一段 extra 的收益,用来撑大单条的字节数。
        static TransactionLogEntry BigGain(uint64_t txId, uint64_t player, std::size_t extraBytes)
        {
            TransactionLogEntry entry = Gain(txId, player);
            entry.set_extra(std::string(extraBytes, 'x'));
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

        // 一次 AttachDue 的全部产出:两类记录,加上「带到哪条、还有没有」。
        struct Attached
        {
            google::protobuf::RepeatedPtrField<TransactionLogEntry> logs;
            google::protobuf::RepeatedPtrField<PlayerSnapshotEntry> snapshots;
            AuditOutbox::Attached result;

            std::vector<uint64_t> TxIds() const
            {
                std::vector<uint64_t> ids;
                for (const auto &entry : logs)
                {
                    ids.push_back(entry.tx_id());
                }
                return ids;
            }

            std::size_t LogBytes() const
            {
                std::size_t bytes = 0;
                for (const auto &entry : logs)
                {
                    bytes += entry.ByteSizeLong();
                }
                return bytes;
            }
        };

        static Attached Attach(uint64_t player, uint64_t mark)
        {
            Attached out;
            out.result = AuditOutbox::AttachDue(player, mark, out.logs, out.snapshots);
            return out;
        }
    };

    // 没开发件箱的进程没有存盘,流水没有提交的地方:拒收,不排队(也没有别的出路,C++ 不直接发审计 topic)。
    TEST_F(AuditOutboxTest, WithoutTheOutboxEntriesAreRefused)
    {
        const uint64_t before = Dropped(DropReason::kOutboxDisabled);
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));

        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);
        EXPECT_EQ(Dropped(DropReason::kOutboxDisabled) - before, 1u);
        AuditOutbox::Enable(true);
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u) << "开了之后也不会补记";
    }

    // 进队列、按快照水位交出、提交后才删;取快照之后才记的不在这份快照里。
    TEST_F(AuditOutboxTest, EntriesRideWithTheSaveWhoseSnapshotContainsThem)
    {
        AuditOutbox::Enable(true);
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        TransactionLogSystem::RecordEntry(Spend(2, kPlayerA));
        const uint64_t snapshotMark = AuditOutbox::Mark();
        TransactionLogSystem::RecordEntry(Gain(3, kPlayerA));

        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 3u);
        EXPECT_TRUE(AuditOutbox::HasDue(kPlayerA, snapshotMark));

        const auto attached = Attach(kPlayerA, snapshotMark);
        EXPECT_EQ(attached.TxIds(), (std::vector<uint64_t>{1, 2}));
        EXPECT_EQ(attached.result.through, snapshotMark) << "带到这份快照里的最后一条";
        EXPECT_FALSE(attached.result.more);
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 3u) << "交出去不删:被拒时要随下一笔再交";

        AuditOutbox::AckThrough(kPlayerA, snapshotMark);
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 1u);
        EXPECT_FALSE(AuditOutbox::HasDue(kPlayerA, snapshotMark)) << "水位之后的那条不算到期";

        EXPECT_EQ(Attach(kPlayerA, AuditOutbox::Mark()).TxIds(), (std::vector<uint64_t>{3}));
        AuditOutbox::AckThrough(kPlayerA, AuditOutbox::Mark());
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);
        EXPECT_EQ(AuditOutbox::PendingBytes(kPlayerA), 0u);
    }

    // 没有到期记录时什么都不带:through 为 0、more 为假。
    TEST_F(AuditOutboxTest, AttachWithNothingDueCarriesNothing)
    {
        AuditOutbox::Enable(true);
        const uint64_t emptyMark = AuditOutbox::Mark();
        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));

        const auto attached = Attach(kPlayerA, emptyMark);
        EXPECT_EQ(attached.logs_size(), 0);
        EXPECT_EQ(attached.result.through, 0u);
        EXPECT_FALSE(attached.result.more);
        EXPECT_EQ(Attach(kPlayerB, AuditOutbox::Mark()).result.through, 0u) << "没有队列的玩家";
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
    }

    // 一条流水只能随一名玩家的存盘提交:两边都有玩家、两边都没有玩家的拒收,不进任何人的队列。
    TEST_F(AuditOutboxTest, EntriesWithoutASingleOwnerAreRefused)
    {
        AuditOutbox::Enable(true);
        TransactionLogEntry transfer = Gain(1, kPlayerB);
        transfer.set_from_player(kPlayerA);
        TransactionLogEntry system;
        system.set_tx_id(2);
        system.set_tx_type(TX_CURRENCY_ADD);
        const uint64_t before = Dropped(DropReason::kNoSingleOwner);

        TransactionLogSystem::RecordEntry(transfer);
        TransactionLogSystem::RecordEntry(system);

        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerB), 0u);
        EXPECT_EQ(Dropped(DropReason::kNoSingleOwner) - before, 2u);
    }

    TEST_F(AuditOutboxTest, InvalidTxIdIsRefusedBeforeQueueing)
    {
        AuditOutbox::Enable(true);
        const uint64_t before = Dropped(DropReason::kNoId);
        TransactionLogSystem::RecordEntry(Gain(0, kPlayerA));

        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);
        EXPECT_EQ(Dropped(DropReason::kNoId) - before, 1u) << "号段没号的流水按 no_id 计";
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
        EXPECT_EQ(attached.result.through, mark);

        AuditOutbox::AckThrough(kPlayerA, mark);
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 1u);
    }

    // 排着的记录超过一笔的预算时不丢:一笔最多带 kMaxBytesPerSave,放行到带上的最后一条之后,下一笔接着带,直到交完(设计 §5.1)。
    TEST_F(AuditOutboxTest, OneSaveCarriesAtMostItsBudgetAndTheRestFollowsInOrder)
    {
        AuditOutbox::Enable(true);
        constexpr uint64_t kRecords = 200; // 200 条约 10 KiB 的流水 ≈ 2 MiB,是一笔预算的近 4 倍
        for (uint64_t txId = 1; txId <= kRecords; ++txId)
        {
            TransactionLogSystem::RecordEntry(BigGain(txId, kPlayerA, 10 * 1024));
        }
        ASSERT_EQ(AuditOutbox::PendingCount(kPlayerA), kRecords) << "超过一笔的预算并不丢";
        const uint64_t snapshotMark = AuditOutbox::Mark();

        std::vector<uint64_t> carried;
        int saves = 0;
        for (;; ++saves)
        {
            ASSERT_LT(saves, 16) << "分笔必须能交完";
            const auto attached = Attach(kPlayerA, snapshotMark);
            ASSERT_GT(attached.logs_size(), 0) << "每一笔至少带走一条";
            EXPECT_LE(attached.LogBytes(), AuditOutbox::kMaxBytesPerSave);
            const auto ids = attached.TxIds();
            carried.insert(carried.end(), ids.begin(), ids.end());
            if (!attached.result.more)
            {
                AuditOutbox::AckThrough(kPlayerA, snapshotMark);
                break;
            }
            EXPECT_LT(attached.result.through, snapshotMark);
            const std::size_t before = AuditOutbox::PendingCount(kPlayerA);
            // 这一笔提交了:只放行到带上的最后一条,没带上的还没进库。
            AuditOutbox::AckThrough(kPlayerA, attached.result.through);
            EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), before - static_cast<std::size_t>(attached.logs_size()));
        }

        EXPECT_GE(saves + 1, 4) << "2 MiB 的记录至少要 4 笔才交得完(saves 是最后一笔的下标)";
        ASSERT_EQ(carried.size(), kRecords);
        for (uint64_t i = 0; i < kRecords; ++i)
        {
            ASSERT_EQ(carried[i], i + 1) << "按记录顺序,不重不漏";
        }
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u);
        EXPECT_EQ(AuditOutbox::PendingBytes(kPlayerA), 0u);
    }

    // 没放行(这一笔被拒或结果未知)时,下一次还是从同一条开始带。
    TEST_F(AuditOutboxTest, BudgetedAttachRestartsFromTheSameRecordUntilAcked)
    {
        AuditOutbox::Enable(true);
        for (uint64_t txId = 1; txId <= 120; ++txId)
        {
            TransactionLogSystem::RecordEntry(BigGain(txId, kPlayerA, 10 * 1024));
        }
        const uint64_t mark = AuditOutbox::Mark();

        const auto first = Attach(kPlayerA, mark);
        const auto again = Attach(kPlayerA, mark);

        ASSERT_TRUE(first.result.more);
        EXPECT_EQ(first.TxIds(), again.TxIds());
        EXPECT_EQ(first.result.through, again.result.through);
    }

    // 条数上限只为封住内存:排到上限(存盘中断很久)才丢最旧的。
    TEST_F(AuditOutboxTest, OverflowByCountDropsTheOldest)
    {
        AuditOutbox::Enable(true);
        constexpr uint64_t kCap = AuditOutbox::kMaxRecordsPerPlayer;
        const uint64_t before = Dropped(DropReason::kOverflow);
        for (uint64_t txId = 1; txId <= kCap + 2; ++txId)
        {
            TransactionLogSystem::RecordEntry(Gain(txId, kPlayerA));
        }

        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), kCap);
        EXPECT_EQ(Dropped(DropReason::kOverflow) - before, 2u) << "挤掉一条计一次";

        // 丢的是最旧的两条:第一笔从第 3 条带起;逐笔交完,最后一条是最新的。
        const uint64_t mark = AuditOutbox::Mark();
        uint64_t nextTxId = 3;
        for (int saves = 0;; ++saves)
        {
            ASSERT_LT(saves, 64);
            const auto attached = Attach(kPlayerA, mark);
            ASSERT_GT(attached.logs_size(), 0);
            EXPECT_EQ(attached.logs(0).tx_id(), nextTxId);
            nextTxId = attached.logs(attached.logs_size() - 1).tx_id() + 1;
            if (!attached.result.more)
            {
                break;
            }
            AuditOutbox::AckThrough(kPlayerA, attached.result.through);
        }
        EXPECT_EQ(nextTxId, kCap + 3) << "最新的一条(kCap + 2)还在";
    }

    // 字节上限同样只为封住内存:远大于一笔的预算,到了才丢最旧的。
    TEST_F(AuditOutboxTest, OverflowByBytesDropsTheOldest)
    {
        AuditOutbox::Enable(true);
        constexpr uint64_t kRecords = 200; // 200 条约 60 KiB 的流水 ≈ 12 MiB,超过每人 8 MiB
        const uint64_t before = Dropped(DropReason::kOverflow);
        for (uint64_t txId = 1; txId <= kRecords; ++txId)
        {
            TransactionLogSystem::RecordEntry(BigGain(txId, kPlayerA, 60 * 1024));
        }

        EXPECT_LE(AuditOutbox::PendingBytes(kPlayerA), AuditOutbox::kMaxBytesPerPlayer);
        EXPECT_GT(AuditOutbox::PendingBytes(kPlayerA), AuditOutbox::kMaxBytesPerSave) << "队列上限远大于一笔的预算";
        const std::size_t kept = AuditOutbox::PendingCount(kPlayerA);
        EXPECT_LT(kept, kRecords);
        EXPECT_EQ(Dropped(DropReason::kOverflow) - before, kRecords - kept) << "挤掉几条计几次";
        const auto attached = Attach(kPlayerA, AuditOutbox::Mark());
        ASSERT_GT(attached.logs_size(), 0);
        EXPECT_EQ(attached.logs(0).tx_id(), kRecords - kept + 1) << "丢的是最旧的,留下的从这条开始";
    }

    TEST_F(AuditOutboxTest, OversizedRecordIsRefused)
    {
        AuditOutbox::Enable(true);
        const uint64_t before = Dropped(DropReason::kTooLarge);

        TransactionLogSystem::RecordEntry(BigGain(1, kPlayerA, AuditOutbox::kMaxRecordBytes));

        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u) << "宁可缺一条,也不让一条坏记录卡住发件箱";
        EXPECT_EQ(Dropped(DropReason::kTooLarge) - before, 1u);
    }

    // 资产变动按那名玩家通知一次(总体方案 §4.5):scene 用它请求合并存盘,货币、物品变动约 1 秒内提交,不靠每个玩法各自记得触发。
    // 通知跟着「资产变了」走:流水收下要通知,被拒(没开发件箱、号段没号、单条超限)也要通知 —— 货币、物品照样已经在内存里变了。
    // 收下时通知发生在入队之后,被通知的一方此刻取快照就带得上这一条。
    TEST_F(AuditOutboxTest, AssetChangeNotifiesTheHookWhetherTheEntryIsQueuedOrRefused)
    {
        struct Notice
        {
            uint64_t player;
            std::size_t pendingWhenNotified;
            bool operator==(const Notice &other) const { return player == other.player && pendingWhenNotified == other.pendingWhenNotified; }
        };
        std::vector<Notice> notices;
        TransactionLogSystem::SetAssetChangedHook([&notices](uint64_t playerId)
        {
            notices.push_back(Notice{playerId, AuditOutbox::PendingCount(playerId)});
        });

        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        ASSERT_EQ(notices, (std::vector<Notice>{{kPlayerA, 0}})) << "没开发件箱:流水被拒收,资产照样变了,照样通知";

        AuditOutbox::Enable(true);
        TransactionLogSystem::RecordEntry(Gain(2, kPlayerA));
        TransactionLogSystem::RecordEntry(Spend(3, kPlayerB));
        TransactionLogSystem::RecordEntry(Gain(0, kPlayerB));
        TransactionLogSystem::RecordEntry(BigGain(4, kPlayerA, AuditOutbox::kMaxRecordBytes));

        EXPECT_EQ(notices, (std::vector<Notice>{{kPlayerA, 0}, {kPlayerA, 1}, {kPlayerB, 1}, {kPlayerB, 1}, {kPlayerA, 1}}))
            << "每条通知一次;收下的那条通知时已在队列里,号段没号与超限被拒的同样通知;合并成一笔存盘是请求存盘那一方的事";
    }

    // 没有单一玩家的流水不通知(不知道该请谁存盘);快照请求不走这个钩子,要不要尽快提交由请求方自己决定。
    TEST_F(AuditOutboxTest, EntriesWithoutASingleOwnerAndSnapshotsDoNotNotifyTheHook)
    {
        AuditOutbox::Enable(true);
        int notified = 0;
        TransactionLogSystem::SetAssetChangedHook([&notified](uint64_t) { ++notified; });

        TransactionLogEntry transfer = Gain(1, kPlayerA);
        transfer.set_from_player(kPlayerB);
        TransactionLogSystem::RecordEntry(transfer);
        TransactionLogEntry system;
        system.set_tx_id(2);
        system.set_tx_type(TX_CURRENCY_ADD);
        TransactionLogSystem::RecordEntry(system);
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 0u) << "两边都有玩家的流水跟不了某一次提交,拒收";
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerB), 0u);
        EXPECT_TRUE(AuditOutbox::EnqueueSnapshot(kPlayerA, SnapshotRequest(3, kPlayerA)));
        EXPECT_EQ(notified, 0);

        // 清掉钩子之后照常入队,只是不再通知。
        TransactionLogSystem::SetAssetChangedHook(nullptr);
        TransactionLogSystem::RecordEntry(Gain(4, kPlayerA));
        EXPECT_EQ(AuditOutbox::PendingCount(kPlayerA), 2u) << "快照请求和这条流水";
        EXPECT_EQ(notified, 0);
    }

    // 钩子里换掉钩子自己(scene 重新装配存盘状态机时会重设)不能毁掉正在执行的函数对象。
    TEST_F(AuditOutboxTest, HookMayReplaceItselfWhileRunning)
    {
        AuditOutbox::Enable(true);
        int first = 0;
        int second = 0;
        TransactionLogSystem::SetAssetChangedHook([&first, &second](uint64_t)
        {
            TransactionLogSystem::SetAssetChangedHook([&second](uint64_t) { ++second; });
            ++first;
        });

        TransactionLogSystem::RecordEntry(Gain(1, kPlayerA));
        TransactionLogSystem::RecordEntry(Gain(2, kPlayerA));

        EXPECT_EQ(first, 1);
        EXPECT_EQ(second, 1);
    }
} // namespace
