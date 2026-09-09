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
    // 旧的一节点一 topic 的前缀(gate / scene);只在迁移窗口里还用得到。
    std::string topicPrefix;
    // 新方案的 topic 基名(gate-cmd / scene-cmd),有效名再加 _g<代号>。
    std::string commandTopicBase;
    // 旧路径的 consumer group 前缀。新路径不进 group(见下面的长注释)。
    std::string groupPrefix;
    // 目标过滤用的字段名(node_kafka_command_filter.h)。
    KafkaCommandTargetFilter targetFilter;
};

// Derive KafkaCommandHandlerOptions from node type.
// e.g. GateNodeService -> topicPrefix="gate", commandTopicBase="gate-cmd",
//      groupPrefix="gate-group", nodeIdFieldNames={"target_gate_id","target_node_id"}.
inline KafkaCommandHandlerOptions BuildDefaultKafkaOptions(uint32_t nodeType)
{
    const std::string name = NodeUtils::NodeTypeToShortName(nodeType);

    KafkaCommandHandlerOptions options;
    options.topicPrefix = name;
    options.commandTopicBase = CommandTopicBaseName(name);
    options.groupPrefix = name + "-group";
    options.targetFilter.nodeIdFieldNames = { "target_" + name + "_id", "target_node_id" };
    options.targetFilter.instanceIdFieldNames = { "target_instance_id" };
    return options;
}

// 注册控制面命令消费。
//
// 新路径(`<类型>-cmd_g<N>` 的 node_id % P 号分区):**assign 自己的分区,不 subscribe**。
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
// group id 在新路径上是**惰性**的:不 subscribe 就不会 JoinGroup,又关掉了 auto.commit
// (KafkaPartitionAssignPolicy::ownOffsets),所以 broker 上根本不会出现这个 group。
// 仍然按 node_id 拼一个名字只为日志可读。
//
// 迁移窗口:Kafka.DisableLegacyPerNodeTopic 为 false(默认)时**同时**消费旧的
// `<类型>-<node_id>`,因为生产者是分批切的(Go login 由另一路发布切换)。
// 老路径维持原来的 group 订阅语义,也就是说 P1 在老路径上仍然存在 ——
// 全部生产者切完、把开关置 true 之后,P1 才算彻底关闭。
template <typename CommandT, typename DispatchFn>
bool RegisterKafkaCommandHandler(Node& node,
    const KafkaCommandHandlerOptions& options,
    DispatchFn&& dispatchFn)
{
    const uint32_t nodeId = node.GetNodeId();
    const uint32_t partitionCount = ConfiguredCommandTopicPartitions();
    const std::string commandTopic = CommandTopicName(options.commandTopicBase,
        ConfiguredCommandTopicGeneration());
    if (commandTopic.empty()) {
        LOG_ERROR << "Kafka command handler: empty command topic base for node_type="
                  << node.GetNodeType();
        return false;
    }
    const int32_t partition = CommandPartitionForNode(nodeId, partitionCount);

    // 新旧两条路径共用同一份 dispatch。先存一份可拷贝的副本再按需构造回调,
    // 免得"第一次注册把它 move 空了,第二次注册拿到空 std::function"。
    using Dispatch = std::decay_t<DispatchFn>;
    Dispatch dispatch = std::forward<DispatchFn>(dispatchFn);
    const KafkaCommandTargetFilter filter = options.targetFilter;

    auto makeCallback = [&node, dispatch, filter]() {
        return [&node, dispatch, filter](const std::string& t, const std::string& payload) {
            auto command = DecodeKafkaProtoPayload<CommandT>(t, payload);
            if (!command) {
                return;
            }

            if (!detail::ValidateCommandTarget(t, *command, node.GetNodeId(),
                    node.GetNodeInfo().node_uuid(), filter)) {
                return;
            }

            dispatch(t, *command);
        };
    };

    LOG_INFO << "Registering Kafka command handler. topic=" << commandTopic
             << ", partition=" << partition << "/" << partitionCount
             << ", node_id=" << nodeId;

    KafkaPartitionAssignPolicy assignPolicy;
    assignPolicy.expectedTopicPartitions = static_cast<int32_t>(partitionCount);
    assignPolicy.startAtEnd = true;
    assignPolicy.ownOffsets = true;

    if (!node.RegisterKafkaMessageHandler({ commandTopic },
            options.commandTopicBase + "-" + std::to_string(nodeId),
            makeCallback(), { partition }, assignPolicy)) {
        return false;
    }

    if (LegacyPerNodeTopicDisabled()) {
        return true;
    }

    // ---- 迁移窗口:旧的一节点一 topic ----
    const std::string legacyTopic = options.topicPrefix + "-" + std::to_string(nodeId);
    auto& kafkaConfig = tlsNodeConfigManager.GetBaseDeployConfig().kafka();
    std::string legacyGroupId = kafkaConfig.group_id();
    if (legacyGroupId.empty()) {
        legacyGroupId = options.groupPrefix + "-" + std::to_string(nodeId);
    }

    LOG_INFO << "Registering legacy per-node Kafka command handler (migration window). topic="
             << legacyTopic << ", group_id=" << legacyGroupId
             << ". 生产者全部切到 " << commandTopic
             << " 之后把 Kafka.DisableLegacyPerNodeTopic 置 true 关掉本路径。";

    return node.RegisterKafkaMessageHandler({ legacyTopic }, legacyGroupId, makeCallback());
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

// Default dispatch: validate event_id + payload, then DispatchProtoEvent.
// Works for any command proto with event_id() and payload() accessors.
template <typename CommandT>
void DefaultKafkaCommandDispatch(const std::string& topic, const CommandT& command)
{
    if (command.event_id() == 0) {
        LOG_WARN << "Kafka command missing event_id, topic=" << topic;
        return;
    }
    if (command.payload().empty()) {
        LOG_WARN << "Kafka command payload is empty, topic=" << topic
                 << ", event_id=" << command.event_id();
        return;
    }
    if (!DispatchProtoEvent(command.event_id(), command.payload())) {
        LOG_WARN << "Kafka command dispatch failed, topic=" << topic
                 << ", event_id=" << command.event_id();
    }
}

// Fully-automatic overload: auto-derive options + use default dispatch.
template <typename CommandT>
bool RegisterKafkaCommandHandler(Node& node)
{
    return RegisterKafkaCommandHandler<CommandT>(
        node, BuildDefaultKafkaOptions(node.GetNodeType()),
        DefaultKafkaCommandDispatch<CommandT>);
}

} // namespace node::kafka
