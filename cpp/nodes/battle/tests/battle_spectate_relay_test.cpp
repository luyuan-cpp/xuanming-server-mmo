// 观战中继 / 广播出口的单元测试:只盯「一帧落成什么 Kafka 记录」与失败计数,不起 Node / EventLoop / Kafka。
// 房间状态(挂接、升档、每回合走哪条通道)的用例在 battle_observer_contract_test.cpp。
#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <vector>

#include "logic/battle_spectate_relay.h"
#include "node_config_manager.h"
#include "proto/contracts/kafka/gate_event.pb.h"

namespace {
using battle_spectate_relay::FrameRecord;
using battle_spectate_relay::SpectateFramePublisher;

constexpr uint64_t kBattleId = 12345;
constexpr char kClientFrame[] = "already-encoded-client-frame";

contracts::kafka::SpectateFrameEvent SampleFrame() {
    contracts::kafka::SpectateFrameEvent frame;
    frame.set_battle_id(kBattleId);
    frame.set_battle_instance_id("battle-instance");
    frame.set_round_index(7);
    frame.set_kind(contracts::kafka::SPECTATE_FRAME_TURN);
    frame.set_client_frame(kClientFrame);
    frame.set_deadline_ms(1800000000000);
    return frame;
}

contracts::kafka::SpectateFrameEvent ParseFrame(const FrameRecord& record) {
    contracts::kafka::SpectateFrameEvent frame;
    EXPECT_TRUE(frame.ParseFromString(record.payload));
    return frame;
}

class BattleSpectateRelay : public testing::Test {
protected:
    void SetUp() override { saved_ = tlsNodeConfigManager; }
    void TearDown() override { tlsNodeConfigManager = saved_; }

    void ConfigureCommandTopics(const uint32_t generation, const uint32_t partitions) {
        auto& kafka = *tlsNodeConfigManager.GetBaseDeployConfig().mutable_kafka();
        kafka.set_command_topic_generation(generation);
        kafka.set_command_topic_partitions(partitions);
    }

    NodeConfigManager saved_;
    std::vector<FrameRecord> records_;
    bool accepts_ = true;
    SpectateFramePublisher publisher_{[this](const FrameRecord& record) {
        records_.push_back(record);
        return accepts_;
    }};
};

// 中继:与 gate 命令 topic 同一分区契约(node_id % 配置的分区数),代号来自配置,事件带目标字段,key 是 battle_id 十进制。
TEST_F(BattleSpectateRelay, RelayRecordAddressesTargetGateOverConfiguredRoute) {
    ConfigureCommandTopics(3, 8);
    auto frame = SampleFrame();
    publisher_.PublishToRelayGate(frame, 21, "gate-instance");

    ASSERT_EQ(1u, records_.size());
    EXPECT_EQ("gate-spectate_g3", records_[0].topic);
    EXPECT_EQ(21 % 8, records_[0].partition);
    EXPECT_EQ(std::to_string(kBattleId), records_[0].key);
    const auto sent = ParseFrame(records_[0]);
    EXPECT_EQ(21u, sent.target_gate_id());
    EXPECT_EQ("gate-instance", sent.target_instance_id());
    EXPECT_EQ(kBattleId, sent.battle_id());
    EXPECT_EQ("battle-instance", sent.battle_instance_id());
    EXPECT_EQ(7u, sent.round_index());
    EXPECT_EQ(contracts::kafka::SPECTATE_FRAME_TURN, sent.kind());
    EXPECT_EQ(kClientFrame, sent.client_frame());
    EXPECT_EQ(1800000000000u, sent.deadline_ms());
}

// proto3 配置不写就是 0:与命令 topic 同一回落规则(代号 1、256 分区)。
TEST_F(BattleSpectateRelay, UnconfiguredRouteFallsBackToCommandTopicDefaults) {
    ConfigureCommandTopics(0, 0);
    auto frame = SampleFrame();
    publisher_.PublishToRelayGate(frame, 300, "gate-instance");
    publisher_.PublishToBroadcast(frame);

    ASSERT_EQ(2u, records_.size());
    EXPECT_EQ("gate-spectate_g1", records_[0].topic);
    EXPECT_EQ(300 % 256, records_[0].partition);
    EXPECT_EQ("spectate-bcast_g1", records_[1].topic);
}

// 广播:固定 16 分区,按 battle_id 定分区,不随命令 topic 的分区数变;同一帧刚发过中继时目标字段也要清掉,
// 否则每台 gate 都会按目标过滤把广播帧丢掉。
TEST_F(BattleSpectateRelay, BroadcastRecordClearsTargetsAndPartitionsByBattle) {
    ConfigureCommandTopics(3, 8);
    auto frame = SampleFrame();
    publisher_.PublishToRelayGate(frame, 21, "gate-instance");
    publisher_.PublishToBroadcast(frame);

    ASSERT_EQ(2u, records_.size());
    EXPECT_EQ("spectate-bcast_g3", records_[1].topic);
    EXPECT_EQ(static_cast<int32_t>(kBattleId % 16), records_[1].partition);
    EXPECT_EQ(std::to_string(kBattleId), records_[1].key);
    const auto sent = ParseFrame(records_[1]);
    EXPECT_EQ(0u, sent.target_gate_id());
    EXPECT_TRUE(sent.target_instance_id().empty());
    EXPECT_EQ(kClientFrame, sent.client_frame());
}

// 同一帧逐台发:每条记录的目标是各自的 gate,前一台的目标不残留。
TEST_F(BattleSpectateRelay, ReusedFrameCarriesEachGatesOwnTarget) {
    auto frame = SampleFrame();
    publisher_.PublishToRelayGate(frame, 21, "first-gate");
    publisher_.PublishToRelayGate(frame, 22, "second-gate");

    ASSERT_EQ(2u, records_.size());
    EXPECT_EQ(21u, ParseFrame(records_[0]).target_gate_id());
    EXPECT_EQ("first-gate", ParseFrame(records_[0]).target_instance_id());
    EXPECT_EQ(22u, ParseFrame(records_[1]).target_gate_id());
    EXPECT_EQ("second-gate", ParseFrame(records_[1]).target_instance_id());
}

// 出口没接住只计数,不重试:每帧只调一次出口;接住的帧不计数。
TEST_F(BattleSpectateRelay, RefusedSendIsCountedWithoutRetry) {
    auto frame = SampleFrame();
    accepts_ = false;
    publisher_.PublishToBroadcast(frame);
    publisher_.PublishToRelayGate(frame, 21, "gate-instance");
    EXPECT_EQ(2u, records_.size());
    EXPECT_EQ(2u, publisher_.SendFailures());

    accepts_ = true;
    publisher_.PublishToBroadcast(frame);
    EXPECT_EQ(3u, records_.size());
    EXPECT_EQ(2u, publisher_.SendFailures());
}
} // namespace
