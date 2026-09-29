#pragma once
#include <cstdint>
#include <string>

// 玩家存盘 DBTask 的 Kafka topic 名。规则与 go/db、go/login 的
// config.DbTaskTopicForGeneration 逐字一致(C++ scene 是生产者,go/db 是唯一的消费者,
// topic 名是两边唯一的会合点;规则漂移不报任何错,存盘静默写进没人消费的 topic):
//   generation <= 1 → "db_task_zone_{zoneId}"
//   generation >= 2 → "db_task_zone_{zoneId}_g{generation}"
//
// 为什么第一代**不带**后缀,而审计 topic(modules/audit/audit_topic.h)第一代就带 _g1:
// db_task_zone_{zone} 这个名字在引入世代号之前就已上线、承载着 applied cursor 的 topic namespace,
// 给第一代补后缀等于换 topic,只能沿用 Go 侧早已定下的口径。0 与 1 同义(proto3 没有 presence,
// 配置里不写就是 0)。
//
// 本函数只做纯拼接、不读配置,方便测试工程直接包;调用方从
// BaseDeployConfig.db_task_topic_generation(yaml 键 DbTaskTopicGeneration)取世代号传进来。
// 换代流程见 docs/design/db-task-kafka-partition-contract.md。
inline std::string GetDbTaskTopic(uint32_t zoneId, uint32_t generation)
{
    std::string topic = "db_task_zone_" + std::to_string(zoneId);
    if (generation <= 1)
    {
        return topic;
    }
    return topic + "_g" + std::to_string(generation);
}
