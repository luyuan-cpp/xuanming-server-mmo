#include <gtest/gtest.h>

#include <string>
#include <thread>

#include "messaging/kafka/kafka_manager.h"
#include "messaging/kafka/kafka_producer.h"
#include "proto/common/base/config.pb.h"

// ---------------------------------------------------------------------------
// kafka_manager_init_test — KafkaManager::Init 的生产端 broker 登记契约。
// 缺陷与修复设计:docs/handoff/global-singletons-p0-findings-20261014.md 的 P0-B。
//
// 要盯住的两件事:
//   1) 生产端 broker 的登记与 Kafka.GroupID 无关。2026-10 之前 GroupID 非空时 Init
//      走一次不带回调的 Subscribe,它在登记 broker 之前就返回 false;只生产不消费的
//      battle 在 K8s(node ConfigMap 写死 GroupID: "game-consumer-group")上因此拿不到
//      broker,每次 send() 都被拒。
//   2) Kafka.Brokers 为空时 Init 返回 false(Node::InitKafka 据此拒启),而且不得把
//      空串登记给生产者。
//
// 断言的是缺陷本身(生产者有没有拿到 broker),不只是 Init 的返回值:返回值以前被唯一的
// 调用者丢掉,只测它测的是代理量。
//
// 确定、可重复:setBrokers 只存字符串,librdkafka 句柄要到第一次 send() 才惰性创建,
// 这里从不 send,所以不连 broker、不起 librdkafka 线程、不读墙钟。KafkaManager 析构时
// 的 flush 在没有句柄时直接返回。
//
// main() 在 kafka_command_test.cpp 里,本文件不再定义。
// ---------------------------------------------------------------------------

namespace {

constexpr const char *kUnreachableBroker = "127.0.0.1:1";
// K8s node ConfigMap 在 P0-B 批 2(2026-10-15 改为从 bin/etc 镜像)之前写死的组名,即触发缺陷的形态。
constexpr const char *kLegacyK8sGroupId = "game-consumer-group";

struct InitOutcome
{
    bool initOk = false;
    bool producerHasBrokers = false;
};

KafkaConfig MakeConfig(const std::string &broker, const std::string &groupId)
{
    KafkaConfig config;
    if (!broker.empty())
    {
        config.add_brokers(broker);
    }
    config.set_group_id(groupId);
    return config;
}

// KafkaProducer::Instance() 是 thread_local:每个场景放进独立的新线程,生产者从空白状态开始,
// 互不沾染,也不受用例执行顺序影响。Init 与读取必须在同一个线程上,和节点里 Init 与 send()
// 同在 loop 线程是同一个约束。断言放回主线程做。
InitOutcome RunInitOnFreshThread(const KafkaConfig &config)
{
    InitOutcome outcome;
    std::thread worker([&outcome, &config] {
        KafkaManager manager;
        outcome.initOk = manager.Init(config);
        outcome.producerHasBrokers = KafkaProducer::Instance().brokersConfigured();
    });
    worker.join();
    return outcome;
}

} // namespace

// 本机形态(GroupID 为空):修复前后都应通过,作对照。
TEST(KafkaManagerInit, ProducerBrokersConfiguredWhenGroupIdIsEmpty)
{
    const InitOutcome outcome = RunInitOnFreshThread(MakeConfig(kUnreachableBroker, ""));
    EXPECT_TRUE(outcome.initOk);
    EXPECT_TRUE(outcome.producerHasBrokers);
}

// K8s + battle 形态(GroupID 非空、本节点没有消费回调):P0-B 缺陷本身。
TEST(KafkaManagerInit, ProducerBrokersConfiguredWhenGroupIdIsSet)
{
    const InitOutcome outcome = RunInitOnFreshThread(MakeConfig(kUnreachableBroker, kLegacyK8sGroupId));
    EXPECT_TRUE(outcome.initOk);
    EXPECT_TRUE(outcome.producerHasBrokers);
}

// Brokers 为空 + GroupID 为空:必须报失败,否则节点带着发不出消息的生产者启动。
TEST(KafkaManagerInit, EmptyBrokersRejectedWhenGroupIdIsEmpty)
{
    const InitOutcome outcome = RunInitOnFreshThread(MakeConfig("", ""));
    EXPECT_FALSE(outcome.initOk);
    EXPECT_FALSE(outcome.producerHasBrokers);
}

// Brokers 为空 + GroupID 非空:修复前也返回 false,但那是「没有消费回调」这个无关原因;
// 这里守住修复后的语义 —— 拒绝的理由只能是 Brokers 为空,与 GroupID 无关。
TEST(KafkaManagerInit, EmptyBrokersRejectedWhenGroupIdIsSet)
{
    const InitOutcome outcome = RunInitOnFreshThread(MakeConfig("", kLegacyK8sGroupId));
    EXPECT_FALSE(outcome.initOk);
    EXPECT_FALSE(outcome.producerHasBrokers);
}

// 不创建 broker 句柄也必须关闭后拒绝 Publish；幂等 Shutdown 不能重新开放 producer。
TEST(KafkaManagerInit, ShutdownJoinsIdleOwnerAndRejectsFurtherPublication)
{
    bool first = false, second = false, joined = false, localJoined = false, rejected = false, frontier = false;
    std::thread worker([&] {
        KafkaManager manager;
        first = manager.Shutdown(std::chrono::milliseconds(0));
        second = manager.Shutdown(std::chrono::milliseconds(0));
        joined = manager.ConsumersJoined();
        localJoined = manager.LocalWorkJoined();
        rejected = !manager.Publish("closed-topic", "must-not-send");
        frontier = manager.ClosedPublicationFrontier().has_value();
    });
    worker.join();
    EXPECT_TRUE(first);
    EXPECT_TRUE(second);
    EXPECT_TRUE(joined);
    EXPECT_TRUE(localJoined);
    EXPECT_TRUE(rejected);
    EXPECT_TRUE(frontier);
}

TEST(KafkaManagerInit, ConsumerlessPreparedBootStillRequiresActualSourceGateAndOpensOnce)
{
    bool prepared=false, rejectedWrong=false, opened=false, duplicateRejected=false;int callbacks=0;
    std::thread worker([&]{
        KafkaManager manager;ASSERT_TRUE(manager.PrepareOnlyBeforeSubscribe(true));
        sql_writer_maintenance_receipt::Plan plan;
        auto readiness=manager.PrepareReadiness(plan,std::string(32,'m'),std::string(16,'i'),std::string(16,'b')).get();
        ASSERT_TRUE(readiness.has_value());prepared=readiness->groups().empty()&&readiness->manual_assignments().empty();
        auto changed=*readiness;changed.add_groups()->set_group("not-configured");
        rejectedWrong=!manager.OpenPrepared(changed,[&]{++callbacks;return true;});
        EXPECT_EQ(callbacks,0);
        EXPECT_FALSE(manager.OpenPrepared(*readiness,[&]{++callbacks;return false;}));
        opened=manager.OpenPrepared(*readiness,[&]{++callbacks;return true;});
        duplicateRejected=!manager.OpenPrepared(*readiness,[&]{++callbacks;return true;});
    });worker.join();
    EXPECT_TRUE(prepared);EXPECT_TRUE(rejectedWrong);EXPECT_TRUE(opened);EXPECT_TRUE(duplicateRejected);EXPECT_EQ(callbacks,2);
}
