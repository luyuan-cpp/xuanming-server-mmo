#pragma once

// 控制面命令的**目标过滤**(从 node_kafka_command_handler.h 拆出来的纯逻辑)。
//
// 拆出来的理由:改成 `<类型>-cmd_gN` + `partition = node_id % P` 之后,同一个分区上
// 会坐着几百个节点,每个节点都读到分区里全部命令 —— 过滤从"防僵尸的保险"变成了
// 路由本身的一部分,必须能被单元测试直接盯住(cpp/tests/kafka_command_test)。
// 原来的写法把它埋在 RegisterKafkaCommandHandler 里,要测就得把 Node、
// node_config_manager、rpc 注册表整条链拖进测试工程。
//
// 两级过滤,顺序有讲究:
//   1) target_node_id / target_<类型>_id —— 数字比较,先跑,便宜。同分区上的
//      几百个节点里只有一个能过。
//   2) target_instance_id —— 进程 uuid(node.cpp 每次启动重新生成),防的是
//      "node_id 被回收、前任还没死透"这一类僵尸命令。字符串比较,后跑。
//
// 两级都必须**被生产者填上**才有效:字段留 0 / 留空时这一级直接放行
// (老生产者的兼容位)。所以 Go 侧生产者补齐 target_gate_id 是这轮改造的一部分,
// 见 docs/design/control-plane-topic-partitioning-20260908.md。

#include <atomic>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include <google/protobuf/descriptor.h>
#include <google/protobuf/message.h>

#include "muduo/base/Logging.h"

namespace node::kafka {

// 命令 proto 里承载"发给谁"的字段名。按节点类型派生,
// 例如 gate 是 {"target_gate_id", "target_node_id"} / {"target_instance_id"}。
struct KafkaCommandTargetFilter {
    std::vector<std::string> nodeIdFieldNames{ "target_node_id" };
    std::vector<std::string> instanceIdFieldNames{ "target_instance_id" };
    // 非空 = 打开「僵尸实例」WARN 的采样(ValidateCommandTarget):第一条必打,此后每 1024 条一行,累计数存在这里,
    // 拷贝过滤器共用同一份。空 = 逐条打(默认):控制命令是偶发的,每一条都值得看。
    // 给持续流的 topic 用:观战帧按回合持续发,gate 重启拿回同一 node_id 后,battle 的中继表还指着前任实例,
    // 这些场的每一帧都会走到这条分支,不采样就是在主 loop 上同步写一行日志。
    std::shared_ptr<std::atomic<uint64_t>> staleInstanceDropCounter;
};

namespace detail {

inline bool TryReadUintField(const google::protobuf::Message& msg,
    const std::vector<std::string>& candidateFields,
    uint64_t& outValue,
    std::string& outFieldName)
{
    const auto* descriptor = msg.GetDescriptor();
    const auto* reflection = msg.GetReflection();
    if (!descriptor || !reflection) {
        return false;
    }

    for (const auto& fieldName : candidateFields) {
        const auto* field = descriptor->FindFieldByName(fieldName);
        if (!field) {
            continue;
        }
        if (field->is_repeated()) {
            continue;
        }

        switch (field->cpp_type()) {
        case google::protobuf::FieldDescriptor::CPPTYPE_UINT32:
            outValue = reflection->GetUInt32(msg, field);
            outFieldName = fieldName;
            return true;
        case google::protobuf::FieldDescriptor::CPPTYPE_UINT64:
            outValue = reflection->GetUInt64(msg, field);
            outFieldName = fieldName;
            return true;
        default:
            continue;
        }
    }

    return false;
}

inline bool TryReadStringField(const google::protobuf::Message& msg,
    const std::vector<std::string>& candidateFields,
    std::string& outValue,
    std::string& outFieldName)
{
    const auto* descriptor = msg.GetDescriptor();
    const auto* reflection = msg.GetReflection();
    if (!descriptor || !reflection) {
        return false;
    }

    for (const auto& fieldName : candidateFields) {
        const auto* field = descriptor->FindFieldByName(fieldName);
        if (!field) {
            continue;
        }
        if (field->is_repeated()) {
            continue;
        }
        if (field->cpp_type() != google::protobuf::FieldDescriptor::CPPTYPE_STRING) {
            continue;
        }

        outValue = reflection->GetString(msg, field);
        outFieldName = fieldName;
        return true;
    }

    return false;
}

// true = 这条命令是发给本进程的,可以派发;false = 丢弃。
//
// 共享分区之后这个函数每条消息都跑,而且大多数消息都不是给自己的
// —— 所以 mismatch 走 LOG_DEBUG 而不是 LOG_WARN:一个分区上几百个节点,
// 每人对每条别人的命令打一行 WARN,日志量会是控制面消息量的几百倍。
// 真正需要报警的是"发给我的 node_id、但 instance 对不上"(僵尸残留),
// 那一条仍然是 WARN(持续流的 topic 可以打开采样,见 KafkaCommandTargetFilter)。
template <typename CommandT>
bool ValidateCommandTarget(const std::string& topic,
    const CommandT& command,
    uint32_t nodeId,
    const std::string& nodeInstanceId,
    const KafkaCommandTargetFilter& filter)
{
    uint64_t targetNodeId = 0;
    std::string nodeIdFieldName;
    const bool hasNodeIdField =
        TryReadUintField(command, filter.nodeIdFieldNames, targetNodeId, nodeIdFieldName);
    if (hasNodeIdField && targetNodeId != 0 && targetNodeId != nodeId) {
        LOG_DEBUG << "Kafka command for another node ignored. field=" << nodeIdFieldName
            << ", target=" << targetNodeId << ", current=" << nodeId << ", topic=" << topic;
        return false;
    }

    std::string targetInstanceId;
    std::string instanceFieldName;
    if (TryReadStringField(command, filter.instanceIdFieldNames, targetInstanceId, instanceFieldName)) {
        if (!targetInstanceId.empty() && targetInstanceId != nodeInstanceId) {
            // target_node_id 也没填(老生产者)时,这条命令本来就可能是同分区上
            // 别人的,降级成 DEBUG;填了且等于自己才是真的"僵尸命令"。
            if (hasNodeIdField && targetNodeId == nodeId) {
                // sampled_total = 0:这个过滤器没开采样,逐条打。
                const auto& dropCounter = filter.staleInstanceDropCounter;
                const uint64_t sampledTotal = dropCounter ? dropCounter->fetch_add(1, std::memory_order_relaxed) + 1 : 0;
                if (!dropCounter || (sampledTotal & 0x3FF) == 1) {
                    LOG_WARN << "Kafka command stale instance ignored. field=" << instanceFieldName
                        << ", target=" << targetInstanceId << ", current=" << nodeInstanceId
                        << ", node_id=" << nodeId << ", topic=" << topic
                        << ", sampled_total=" << sampledTotal;
                }
            }
            else {
                LOG_DEBUG << "Kafka command for another instance ignored. field=" << instanceFieldName
                    << ", target=" << targetInstanceId << ", current=" << nodeInstanceId
                    << ", topic=" << topic;
            }
            return false;
        }
    }

    return true;
}

} // namespace detail

} // namespace node::kafka
