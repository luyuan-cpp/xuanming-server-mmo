#pragma once

// 控制面命令的**寻址落地层**:把 node_command_topic.h 的纯契约接上 base_deploy_config.yaml,
// 给生产者(battle / scene)一个只需要 "目标节点类型 + 目标 node_id" 的入口。
//
// 设计文档:docs/design/control-plane-topic-partitioning-20260908.md
//
// 生产者必须走 ResolveCommandRoute,不许再自己拼 "gate-" + id:
// topic 名与分区号是一对不可分的东西,分开写就会漂移(名字改了、分区还按旧的算,
// 消息落到没人 assign 的分区上,静默全丢,而且 Kafka 不报错)。

#include <cstdint>
#include <string>

#include "node/system/node/node_command_topic.h"
#include "node/system/node/node_util.h"

#include <node_config_manager.h>

namespace node::kafka {

// 一条命令的完整寻址:topic + 显式分区号。
struct CommandRoute {
    std::string topic;
    int32_t partition = -1;

    bool Valid() const { return !topic.empty() && partition >= 0; }
};

inline uint32_t ConfiguredCommandTopicPartitions()
{
    return NormalizeCommandTopicPartitions(
        tlsNodeConfigManager.GetBaseDeployConfig().kafka().command_topic_partitions());
}

inline uint32_t ConfiguredCommandTopicGeneration()
{
    return NormalizeCommandTopicGeneration(
        tlsNodeConfigManager.GetBaseDeployConfig().kafka().command_topic_generation());
}

// gate -> "gate-cmd_g1"、scene -> "scene-cmd_g1"。
inline std::string CommandTopicForNodeType(uint32_t nodeType)
{
    return CommandTopicName(CommandTopicBaseName(NodeUtils::NodeTypeToShortName(nodeType)),
        ConfiguredCommandTopicGeneration());
}


inline CommandRoute ResolveCommandRoute(uint32_t nodeType, uint32_t targetNodeId)
{
    CommandRoute route;
    route.topic = CommandTopicForNodeType(nodeType);
    route.partition = CommandPartitionForNode(targetNodeId, ConfiguredCommandTopicPartitions());
    return route;
}

} // namespace node::kafka
