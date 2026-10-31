#pragma once

// 观战中继(L1)与全服广播(L2)的 Kafka 出口(docs/design/battle-spectate-tiered-fanout.md §15.3 / §15.4)。
//
// 职责只有一件:把一帧 SpectateFrameEvent 按通道落成一条 Kafka 记录(topic / 分区 / key / 字节)交给出口,并记下发送失败。
// 「本场挂了哪些 gate、是否已升广播、这一帧走哪些通道」是房间的事实,归 BattleRoomManager,这里不持有房间状态。
//
// 寻址(配置口径与 battle 现有的 gate-cmd 相同,见 node_command_route.h):
//   L1 中继:gate-spectate_g<N>,分区 = gate node_id % 命令 topic 分区数;事件填 target_gate_id / target_instance_id
//            (AGENTS.md §7.2:同分区上的其它 gate 与目标 gate 的僵尸实例都按这两个字段丢弃);
//   L2 广播:spectate-bcast_g<N>,分区 = battle_id % 16;两个目标字段留空(广播没有单一目标,gate 按 battle_instance_id 防僵尸)。
//   key 一律是 battle_id 十进制(AGENTS.md §7.3)。消息体就是 SpectateFrameEvent 本身,不套 GateCommand 信封。
//
// 失败语义:没进生产者队列的帧只计数 + 采样日志(metric=battle_spectate_frame_send_failed),不重试、不阻塞回合推进:
// 每帧都带全量状态,下一帧自愈。
//
// 线程模型:只在 battle 的 loop 线程调用,无锁。

#include <cstdint>
#include <functional>
#include <string>

#include "proto/contracts/kafka/gate_event.pb.h"

namespace battle_spectate_relay
{

// 一条待发的 Kafka 记录:往哪个 topic、哪个分区、用什么 key、发什么字节。
struct FrameRecord
{
    std::string topic;
    int32_t partition = 0;
    std::string key;
    std::string payload;
};

// 观战帧出口(显式依赖,默认 SendFrameViaKafka,测试注入捕获函数)。
// 返回 true = 记录已交给生产者;false = 没发出去,调用方只计数、不重试。
// 同步调用:实现不得阻塞,也不得回调 BattleRoomManager(调用方此刻可能正在遍历房间的中继表)。
using FrameSink = std::function<bool(const FrameRecord &)>;

// 默认出口:走本线程的 KafkaProducer。失败原因由生产者自己限流记日志(队列满、无 broker、fatal 冷却期等)。
bool SendFrameViaKafka(const FrameRecord &record);

class SpectateFramePublisher
{
public:
    // 空 sink = 默认出口(SendFrameViaKafka)。
    explicit SpectateFramePublisher(FrameSink sink = {});

    // 换出口;传空恢复默认。只在 loop 线程调用,不能在出口的调用栈里调用。
    void SetSink(FrameSink sink);

    // 发给一台中继 gate。只改写 frame 的两个目标字段:同一帧发多台 gate 时复用同一个对象,client_frame 不随 gate 数多拷一份。
    // gateNodeId 非 0、gateInstanceId 非空由调用方保证(空目标会关掉 gate 侧的目标过滤),
    // 房间的中继表只经 BattleRoomManager::HandleAttachRelayGate 校验后写入。
    void PublishToRelayGate(::contracts::kafka::SpectateFrameEvent &frame, uint32_t gateNodeId,
                            const std::string &gateInstanceId);

    // 发到全服广播 topic。会清掉两个目标字段:调用方可能复用发过中继的同一个对象,目标字段留着的话每台 gate 都会按目标过滤丢掉。
    void PublishToBroadcast(::contracts::kafka::SpectateFrameEvent &frame);

    // 累计没发出去的帧数(出口返回 false 的次数)。已入队、后来投递失败的不在内,那部分归 kafka_producer_stats。
    uint64_t SendFailures() const { return sendFailures_; }

private:
    void Send(const char *channel, const ::contracts::kafka::SpectateFrameEvent &frame, std::string topic,
              int32_t partition);

    FrameSink sink_;
    uint64_t sendFailures_ = 0;
};

} // namespace battle_spectate_relay
