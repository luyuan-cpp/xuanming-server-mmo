#include "battle_spectate_relay.h"

#include <utility>

#include "muduo/base/Logging.h"

#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "node/system/node/node_command_route.h"

namespace battle_spectate_relay
{

bool SendFrameViaKafka(const FrameRecord &record)
{
    return KafkaProducer::Instance().send(record.topic, record.payload, record.key, record.partition) ==
           RdKafka::ERR_NO_ERROR;
}

SpectateFramePublisher::SpectateFramePublisher(FrameSink sink)
{
    SetSink(std::move(sink));
}

void SpectateFramePublisher::SetSink(FrameSink sink)
{
    sink_ = sink ? std::move(sink) : FrameSink(SendFrameViaKafka);
}

void SpectateFramePublisher::PublishToRelayGate(::contracts::kafka::SpectateFrameEvent &frame, const uint32_t gateNodeId,
                                                const std::string &gateInstanceId)
{
    frame.set_target_gate_id(gateNodeId);
    frame.set_target_instance_id(gateInstanceId);
    Send("relay", frame,
         node::kafka::CommandTopicName(node::kafka::kSpectateRelayTopicBase, node::kafka::ConfiguredCommandTopicGeneration()),
         node::kafka::CommandPartitionForNode(gateNodeId, node::kafka::ConfiguredCommandTopicPartitions()));
}

void SpectateFramePublisher::PublishToBroadcast(::contracts::kafka::SpectateFrameEvent &frame)
{
    frame.clear_target_gate_id();
    frame.clear_target_instance_id();
    Send("broadcast", frame,
         node::kafka::CommandTopicName(node::kafka::kSpectateBroadcastTopicBase, node::kafka::ConfiguredCommandTopicGeneration()),
         node::kafka::SpectateBroadcastPartitionForBattle(frame.battle_id()));
}

void SpectateFramePublisher::Send(const char *channel, const ::contracts::kafka::SpectateFrameEvent &frame,
                                  std::string topic, const int32_t partition)
{
    const FrameRecord record{std::move(topic), partition, std::to_string(frame.battle_id()), frame.SerializeAsString()};
    if (sink_(record))
    {
        return;
    }
    // 首次必打,之后每 1024 次一行(与 battle 其它采样日志同口径):broker 故障期间每回合每台 gate 都会走到这里。
    // battle 没有 Prometheus 端点,failed_total 就是计数;标签只有低基数枚举,不带 battle_id。
    if ((sendFailures_++ & 0x3FF) == 0)
    {
        LOG_WARN << "metric=battle_spectate_frame_send_failed channel=" << channel
                 << " kind=" << ::contracts::kafka::eSpectateFrameKind_Name(frame.kind())
                 << " failed_total=" << sendFailures_
                 << ",frame dropped without retry;the next frame carries full state";
    }
}

} // namespace battle_spectate_relay
