#pragma once

// 控制面命令 topic 的**寻址契约**(唯一真源之一;另一份在 go/shared/kafkacmd/command_topic.go,
// 两边由 go/shared/kafkacmd/testdata/command_partition_vectors.json 对拍)。
//
// 设计文档:docs/design/control-plane-topic-partitioning-20260908.md
//
// 为什么不再用"一个节点一个 topic":
//   * 正确性:老方案 topic 名和 consumer group 名里都带 node_id,而 node_id 由
//     node_allocator 的"最小空闲扫描"立刻回收。冻结/网络分区但仍是 group 成员的
//     前任 A 和继任者 B 会同时出现在 `gate-group-5` 里,Kafka 只把分区判给其中一个。
//     判给 A 时,A 把 B 的命令消费掉、再按 target_instance_id 丢弃,B 永远收不到
//     —— 登录绑定 / 进世界 / 踢人 / 战斗绑定与结算静默消失。per-message 的
//     target_instance_id 只能防"错执行",防不住这种"饿死"。
//   * 规模:目标是 ~10 万个 scene 进程、每天重启。10 万个 topic 在一个集群里,
//     controller 元数据、每分区内存、rebalance 时间三条全部塌方。
//
// 改成:一个节点类型一个 topic、固定分区数 P,**partition = node_id % P**;
// 消费端 assign 自己那个分区(不进 consumer group,没有协调、没有 rebalance),
// 生产端显式指定分区。同一分区上有多个节点,各自读全量再按
// target_node_id + target_instance_id 过滤(读放大是刻意接受的成本,
// 控制面总量是每秒几百条量级)。
//
// 本头文件**只放纯函数**:不 include 配置、日志、protobuf,任何测试工程都能直接包。
// 读配置的那层在 node_command_route.h。

#include <cstdint>
#include <string>

namespace node::kafka {

// P 的编译期默认值。改这个数 = 改不可变契约,必须同时:
//   1) cpp 这里、2) go/shared/kafkacmd/command_topic.go、
//   3) deploy/docker-compose.yml 的 kafka-topic-init、
//   4) deploy/k8s/manifests/infra/kafka-topic-init.yaml(经 k8s_deploy.ps1 注入),
// 并且**换一个代号**(kDefaultCommandTopicGeneration +1)换一批新 topic。
// 绝不在同名 topic 上扩分区:扩分区会把 node_id % P 重映射,命令落到没人 assign
// 的分区上静默全丢。
inline constexpr uint32_t kDefaultCommandTopicPartitions = 256;

// 代号。与 data_service 的 transaction_log_topic_g1 / player_snapshot_topic_g1 同形:
// **第一代也带后缀**,这样 broker 的 auto.create(num.partitions=1)抢先建出来的
// 裸名字 topic 与契约 topic 互不相干。
inline constexpr uint32_t kDefaultCommandTopicGeneration = 1;

// 0 一律当"没配",回落到编译期默认。proto3 没有 presence,配置里不写就是 0,
// 这条规则让存量 yaml 不改也能起,并且必须与 Go 侧逐字一致。
inline constexpr uint32_t NormalizeCommandTopicPartitions(uint32_t configured)
{
    return configured == 0 ? kDefaultCommandTopicPartitions : configured;
}

inline constexpr uint32_t NormalizeCommandTopicGeneration(uint32_t configured)
{
    return configured == 0 ? kDefaultCommandTopicGeneration : configured;
}

// 基名:gate -> "gate-cmd"、scene -> "scene-cmd"。
// 沿用 BuildDefaultKafkaOptions 的"按节点类型短名派生"写法,新节点类型不用另配。
inline std::string CommandTopicBaseName(const std::string& nodeShortName)
{
    if (nodeShortName.empty()) {
        return {};
    }
    return nodeShortName + "-cmd";
}

// 有效名:`<基名>_g<代号>`。基名为空返回空,交给调用方 fail-closed,
// 而不是拼出一个只有后缀的名字(与 data_service topicForGeneration 同样的处理)。
inline std::string CommandTopicName(const std::string& baseName, uint32_t generation)
{
    if (baseName.empty()) {
        return {};
    }
    return baseName + "_g" + std::to_string(NormalizeCommandTopicGeneration(generation));
}

// 寻址函数本体。node_id 是路由号(node_allocator 发的 [1, 2^17)),
// 全局唯一(跨 zone 也唯一,见 docs/design/cross-zone-matchmaking.md),
// 所以取模结果在生产端和消费端一定一致。
//
// 用取模而不是哈希:两边实现必须逐位一致,取模是唯一不会因语言/库版本漂移的映射;
// node_id 由分配器顺序发放,分布本来就均匀,哈希没有额外收益。
inline int32_t CommandPartitionForNode(uint64_t nodeId, uint32_t partitions)
{
    return static_cast<int32_t>(nodeId % NormalizeCommandTopicPartitions(partitions));
}

// ---- 观战帧 topic(docs/design/battle-spectate-tiered-fanout.md §15.3)----
// 两条 topic 都与命令 topic 共用代号(CommandTopicGeneration),名字一律 `<基名>_g<代号>`。
//
// L1 中继:`gate-spectate_g<N>`,与 gate 命令 topic 同一分区契约(partition = gate node_id % 命令 topic 分区数),
// 生产端(battle)用 CommandPartitionForNode 定分区,消费端(gate)assign 自己那个分区。
// 单独开 topic 是为了不让每回合的观战帧挤占踢人、路由这类控制命令的通道。
inline constexpr const char* kSpectateRelayTopicBase = "gate-spectate";

// L2 广播:`spectate-bcast_g<N>`,固定 16 分区,partition = battle_id % 16;每台 gate assign 全部分区。
// 分区数同样是不可变契约:要改就换代号,不在同名 topic 上扩分区(改了 battle_id % P 的映射,在途帧会落到
// 旧分区之外,gate 当前状态可以自愈,但这条纪律与命令 topic 保持一致,不开例外)。
inline constexpr const char* kSpectateBroadcastTopicBase = "spectate-bcast";
inline constexpr uint32_t kSpectateBroadcastTopicPartitions = 16;

inline int32_t SpectateBroadcastPartitionForBattle(uint64_t battleId)
{
    return static_cast<int32_t>(battleId % kSpectateBroadcastTopicPartitions);
}

} // namespace node::kafka
