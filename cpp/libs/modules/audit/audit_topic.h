#pragma once

// 审计 topic(transaction_log / player_snapshot)的**名字契约**:有效名 = `<基名>_g<世代号>`。
//
// 基名是各自模块的常量(transaction_log_system.h / snapshot_system.h),世代号来自部署配置
// BaseDeployConfig.audit_topic_generation(bin/etc/base_deploy_config.yaml 的 AuditTopicGeneration)。
//
// 为什么这里只有**一份**实现:C++ 是这两个 topic 唯一的生产者,go/data_service 是唯一的消费者,
// topic 名是两边唯一的会合点(go/data_service/internal/config/config.go 的 topicForGeneration,
// 由 Kafka.TopicGeneration 拼)。两个生产者各拼各的,就等于把"同一个契约"抄了两份 ——
// 抄两份必然漂移,而漂移的症状是静默的:消息进了一个没人消费的 topic,保留期(默认 30 天)一到就没了,
// Kafka 不报错、生产者也不报错。所以两个模块都必须走这个函数,不许自己拼后缀。
//
// 2026-09-09 之前后缀是编译期常量(`"transaction_log_topic_g1"`),换代要手改两个头文件 + 重出镜像,
// 漏一处就是"生产者写 _g1、消费者读 _g2"。现在收敛成一个部署键,与
// node_command_topic.h 的 command_topic_generation 是同一套纪律。

#include <cstdint>
#include <string>

#include <node_config_manager.h>

namespace audit {

// 第一代也带后缀。理由:broker 是 auto.create.topics.enable + num.partitions=1,生产者一发消息
// 就把**裸名字**的 topic 自动建成 1 分区,消费端 EnsureTopics 拿 6 / 3 的分区契约去比永远得到
// "partition contract mismatch",两条落库消费者一条都起不来。带代号的名字与那两个自动建出来的
// 裸名 topic 互不相干。与 Go 侧 topicForGeneration 的口径逐字一致。
inline constexpr uint32_t kDefaultAuditTopicGeneration = 1;

// 0 一律当"没配":proto3 没有 presence,配置里不写就是 0。这条规则必须与 Go 侧
// topicForGeneration 的 `if generation == 0 { generation = 1 }` 逐字一致。
inline constexpr uint32_t NormalizeAuditTopicGeneration(uint32_t configured)
{
    return configured == 0 ? kDefaultAuditTopicGeneration : configured;
}

inline uint32_t ConfiguredAuditTopicGeneration()
{
    return NormalizeAuditTopicGeneration(
        tlsNodeConfigManager.GetBaseDeployConfig().audit_topic_generation());
}

// 基名为空返回空,交调用方 fail-closed,而不是拼出一个只有后缀的名字
// (与 Go 的 topicForGeneration、node_command_topic.h 的 CommandTopicName 同样处理)。
inline std::string AuditTopicName(const std::string& baseName, uint32_t generation)
{
    if (baseName.empty()) {
        return {};
    }
    return baseName + "_g" + std::to_string(NormalizeAuditTopicGeneration(generation));
}

// 生产者的入口:传基名,拿当前部署配置下的有效名。
inline std::string AuditTopicName(const std::string& baseName)
{
    return AuditTopicName(baseName, ConfiguredAuditTopicGeneration());
}

} // namespace audit
