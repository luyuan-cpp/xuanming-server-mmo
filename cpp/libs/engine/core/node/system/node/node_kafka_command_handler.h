#pragma once

#include <string>
#include <type_traits>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"
#include "messaging/kafka/kafka_proto_decoder.h"
#include "node/system/node/node.h"
#include "node/system/node/node_command_route.h"
#include "node/system/node/node_kafka_command_filter.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include <node_config_manager.h>

namespace node::kafka {

struct KafkaCommandHandlerOptions {
    // topic 基名(gate-cmd / scene-cmd),有效名再加 _g<代号>。
    std::string commandTopicBase;
    // 目标过滤用的字段名与僵尸实例日志的采样开关(node_kafka_command_filter.h)。
    KafkaCommandTargetFilter targetFilter;
    // >0:不按 node_id 选分区,assign 这个 topic 的全部分区(0..N-1),并按 N 核对分区契约。给每个节点都要读全量的
    // 广播 topic 用(观战 L2),N 是该 topic 自己的契约常量,不是命令 topic 的分区数。默认 0 = 现有调用方行为不变。
    // 注意维护控制模式(MMORPG_MAINTENANCE_CONTROL_DIR)只接受单分区的 manual consumer,Node 会拒绝这种注册,
    // 调用方要在该模式下自己决定是否注册。
    uint32_t assignAllPartitions{0};
};

// Derive KafkaCommandHandlerOptions from node type.
// e.g. GateNodeService -> commandTopicBase="gate-cmd", nodeIdFieldNames={"target_gate_id","target_node_id"}.
inline KafkaCommandHandlerOptions BuildDefaultKafkaOptions(uint32_t nodeType)
{
    const std::string name = NodeUtils::NodeTypeToShortName(nodeType);

    KafkaCommandHandlerOptions options;
    options.commandTopicBase = CommandTopicBaseName(name);
    options.targetFilter.nodeIdFieldNames = { "target_" + name + "_id", "target_node_id" };
    options.targetFilter.instanceIdFieldNames = { "target_instance_id" };
    return options;
}

// 注册控制面命令消费。
//
// `<类型>-cmd_g<N>` 的 node_id % P 号分区:**assign 自己的分区,不 subscribe**。
// 这一条就是 P1 的修复本身:
//   老写法里 topic 名和 group 名都带 node_id,而 node_id 被 node_allocator 立刻回收。
//   冻结/被隔离但仍是 group 成员的前任 A,与拿到同一个 node_id 的继任者 B,
//   会一起出现在 `gate-group-5` 里;Kafka 只把这个分区判给其中一个成员。
//   判给 A 时 A 消费掉 B 的命令、再按 target_instance_id 丢弃,B 一条也收不到
//   —— 登录绑定、进世界、踢人、战斗绑定与结算全部静默消失。
//   assign() 走的是 librdkafka 的 simple consumer 模式:不发 JoinGroup、
//   不参与任何协调,谁 assign 谁读,前任的存在与本进程无关。顺带把 10 万消费者
//   规模下的 rebalance 成本也一并去掉了。
//
// group id 是**惰性**的:不 subscribe 就不会 JoinGroup,又关掉了 auto.commit
// (KafkaPartitionAssignPolicy::ownOffsets),所以 broker 上根本不会出现这个 group。
// 仍然按 node_id 拼一个名字只为日志可读。
template <typename CommandT, typename DispatchFn>
bool RegisterKafkaCommandHandler(Node& node,
    const KafkaCommandHandlerOptions& options,
    DispatchFn&& dispatchFn)
{
    const uint32_t nodeId = node.GetNodeId();
    const bool assignAll = options.assignAllPartitions > 0;
    const uint32_t partitionCount = assignAll ? options.assignAllPartitions : ConfiguredCommandTopicPartitions();
    const std::string commandTopic = CommandTopicName(options.commandTopicBase,
        ConfiguredCommandTopicGeneration());
    if (commandTopic.empty()) {
        LOG_ERROR << "Kafka command handler: empty command topic base for node_type="
                  << node.GetNodeType();
        return false;
    }
    std::vector<int32_t> partitions;
    if (assignAll) {
        partitions.reserve(partitionCount);
        for (uint32_t partition = 0; partition < partitionCount; ++partition) {
            partitions.push_back(static_cast<int32_t>(partition));
        }
    } else {
        partitions.push_back(CommandPartitionForNode(nodeId, partitionCount));
    }

    using Dispatch = std::decay_t<DispatchFn>;
    Dispatch dispatch = std::forward<DispatchFn>(dispatchFn);
    const KafkaCommandTargetFilter filter = options.targetFilter;

    auto callback = [&node, dispatch, filter](const std::string& t, const std::string& payload) {
        auto command = DecodeKafkaProtoPayload<CommandT>(t, payload);
        if (!command) {
            return;
        }

        if (node.RequiresMaintenanceTargetIdentity()) {
            uint64_t targetNode=0;std::string nodeField,targetInstance,instanceField;
            if(!detail::TryReadUintField(*command,filter.nodeIdFieldNames,targetNode,nodeField) ||
               !detail::TryReadStringField(*command,filter.instanceIdFieldNames,targetInstance,instanceField) ||
               targetNode!=node.GetNodeId() || targetInstance!=node.GetNodeInfo().node_uuid()) return;
            // P8之后共享manual分区必须精确目标boot；不把空兼容位视为本实例命令。
        }
        if (!detail::ValidateCommandTarget(t, *command, node.GetNodeId(),
                node.GetNodeInfo().node_uuid(), filter)) {
            return;
        }

        dispatch(t, *command);
    };

    LOG_INFO << "Registering Kafka command handler. topic=" << commandTopic
             << ", partition=" << (assignAll ? std::string("all") : std::to_string(partitions.front()))
             << "/" << partitionCount
             << ", node_id=" << nodeId;

    KafkaPartitionAssignPolicy assignPolicy;
    assignPolicy.expectedTopicPartitions = static_cast<int32_t>(partitionCount);
    assignPolicy.startAtEnd = true;
    assignPolicy.ownOffsets = true;

    return node.RegisterKafkaMessageHandler({ commandTopic },
            options.commandTopicBase + "-" + std::to_string(nodeId),
            std::move(callback), partitions, assignPolicy);
}

// Convenience overload: auto-derive options from node type.
template <typename CommandT, typename DispatchFn>
bool RegisterKafkaCommandHandler(Node& node,
    DispatchFn&& dispatchFn)
{
    return RegisterKafkaCommandHandler<CommandT>(
        node, BuildDefaultKafkaOptions(node.GetNodeType()),
        std::forward<DispatchFn>(dispatchFn));
}

} // namespace node::kafka
