#include <gtest/gtest.h>

#include <cstdint>
#include <filesystem>
#include <iostream>
#include <string>
#include <vector>

#include <yaml-cpp/yaml.h>

#include "node/system/node/node_command_route.h"
#include "node/system/node/node_command_topic.h"
#include "node/system/node/node_kafka_command_filter.h"

#include "proto/contracts/kafka/gate_command.pb.h"
#include "proto/contracts/kafka/scene_command.pb.h"

#include "../test_config_helper.h"

// ---------------------------------------------------------------------------
// kafka_command_test — 控制面命令寻址与目标过滤的单元测试。
// 设计文档:docs/design/control-plane-topic-partitioning-20260908.md
//
// 两件事必须被盯住:
//   1) partition = node_id % P 在 C++ 与 Go 两侧逐位一致。不一致的表现是命令
//      落到没人 assign 的分区上,消费者收不到、Kafka 也不报错 —— 编译器和
//      集成测试都看不见。所以两侧读同一份向量文件
//      go/shared/kafkacmd/testdata/command_partition_vectors.json。
//   2) 同一个分区上坐着几百个节点,ValidateCommandTarget 从"防僵尸的保险"
//      变成了路由的一部分:发给别人的必须丢、发给自己的必须过。
// ---------------------------------------------------------------------------

namespace {

namespace fs = std::filesystem;

constexpr const char *kVectorRelativePath =
    "go/shared/kafkacmd/testdata/command_partition_vectors.json";

// 向上找仓库根:测试可执行文件从 build/cpp/tests/ 跑,而 test_config_helper
// 又会把 cwd 换到 bin/。两种情况都靠"从若干个起点逐级向上找同一个相对路径"解决,
// 不写死任何一条绝对路径。
fs::path FindVectorFile(const std::vector<fs::path> &startPoints)
{
    for (const auto &start : startPoints)
    {
        if (start.empty())
        {
            continue;
        }
        fs::path dir = fs::absolute(start);
        for (int depth = 0; depth < 8; ++depth)
        {
            const fs::path candidate = dir / kVectorRelativePath;
            std::error_code ec;
            if (fs::exists(candidate, ec))
            {
                return candidate;
            }
            if (!dir.has_parent_path() || dir.parent_path() == dir)
            {
                break;
            }
            dir = dir.parent_path();
        }
    }
    return {};
}

fs::path gVectorFile;

} // namespace

// ---------------------------------------------------------------------------
// 1. 与 Go 的分区映射对拍
// ---------------------------------------------------------------------------

TEST(CommandPartitionVectors, MatchesSharedVectorFile)
{
    ASSERT_FALSE(gVectorFile.empty())
        << "找不到共享向量文件 " << kVectorRelativePath
        << ";它是 C++/Go 对拍的唯一凭据,缺了就等于没测。";

    const YAML::Node root = YAML::LoadFile(gVectorFile.string());
    ASSERT_TRUE(root["cases"]) << "向量文件缺 cases";

    // 向量文件里写死的默认值必须与代码常量一致,否则"两边都按向量过了"
    // 却和线上跑的默认契约不是一回事。
    EXPECT_EQ(root["default_partitions"].as<uint32_t>(),
              node::kafka::kDefaultCommandTopicPartitions);
    EXPECT_EQ(root["default_generation"].as<uint32_t>(),
              node::kafka::kDefaultCommandTopicGeneration);

    std::size_t caseCount = 0;
    for (const auto &c : root["cases"])
    {
        const std::string name = c["name"].as<std::string>();
        const std::string base = c["base"].as<std::string>();
        const uint64_t nodeId = c["node_id"].as<uint64_t>();
        const uint32_t partitions = c["partitions"].as<uint32_t>();
        const uint32_t generation = c["generation"].as<uint32_t>();
        const int32_t expectPartition = c["expect_partition"].as<int32_t>();
        const std::string expectTopic = c["expect_topic"].as<std::string>();

        EXPECT_EQ(node::kafka::CommandPartitionForNode(nodeId, partitions), expectPartition)
            << "case " << name;
        EXPECT_EQ(node::kafka::CommandTopicName(base, generation), expectTopic)
            << "case " << name;
        ++caseCount;
    }
    EXPECT_GT(caseCount, 0u) << "向量文件没有用例";
}

TEST(CommandTopicNaming, DerivesBaseFromNodeShortName)
{
    EXPECT_EQ(node::kafka::CommandTopicBaseName("gate"), "gate-cmd");
    EXPECT_EQ(node::kafka::CommandTopicBaseName("scene"), "scene-cmd");
    // 空短名不许拼出一个只有后缀的名字,交给调用方 fail-closed。
    EXPECT_EQ(node::kafka::CommandTopicBaseName(""), "");
    EXPECT_EQ(node::kafka::CommandTopicName("", 1), "");
}

TEST(CommandTopicNaming, ZeroMeansUnconfigured)
{
    EXPECT_EQ(node::kafka::NormalizeCommandTopicPartitions(0),
              node::kafka::kDefaultCommandTopicPartitions);
    EXPECT_EQ(node::kafka::NormalizeCommandTopicGeneration(0),
              node::kafka::kDefaultCommandTopicGeneration);
    EXPECT_EQ(node::kafka::CommandTopicName("gate-cmd", 0), "gate-cmd_g1");
}

// 同一个 node_id 在两个不同代号下算出同一个分区号:代号只换 topic 名,
// 不动映射函数 —— 这正是"换代号扩分区"能安全并存的前提。
TEST(CommandTopicNaming, GenerationDoesNotAffectPartition)
{
    const uint64_t nodeId = 100000;
    EXPECT_EQ(node::kafka::CommandPartitionForNode(nodeId, 256),
              node::kafka::CommandPartitionForNode(nodeId, 256));
    EXPECT_NE(node::kafka::CommandTopicName("gate-cmd", 1),
              node::kafka::CommandTopicName("gate-cmd", 2));
}

// 分区号必须始终落在 [0, P)。越界会被 assign 成一个不存在的分区,
// 消费者随后永远收不到任何东西。
TEST(CommandPartitionRange, AlwaysWithinContract)
{
    for (uint64_t nodeId = 0; nodeId <= 131071; nodeId += 997)
    {
        const int32_t p = node::kafka::CommandPartitionForNode(nodeId, 256);
        ASSERT_GE(p, 0) << "node_id=" << nodeId;
        ASSERT_LT(p, 256) << "node_id=" << nodeId;
    }
}

// ---------------------------------------------------------------------------
// 2. 目标过滤:发给别人的丢、发给自己的过
// ---------------------------------------------------------------------------

namespace {

node::kafka::KafkaCommandTargetFilter GateFilter()
{
    node::kafka::KafkaCommandTargetFilter filter;
    filter.nodeIdFieldNames = {"target_gate_id", "target_node_id"};
    filter.instanceIdFieldNames = {"target_instance_id"};
    return filter;
}

constexpr const char *kSelfInstance = "11111111-1111-1111-1111-111111111111";
constexpr const char *kPredecessorInstance = "22222222-2222-2222-2222-222222222222";
constexpr uint32_t kSelfNodeId = 5;

bool ValidateGate(const contracts::kafka::GateCommand &command)
{
    return node::kafka::detail::ValidateCommandTarget(
        "gate-cmd_g1", command, kSelfNodeId, kSelfInstance, GateFilter());
}

} // namespace

TEST(ValidateCommandTarget, DispatchesCommandAddressedToThisInstance)
{
    contracts::kafka::GateCommand command;
    command.set_target_gate_id(kSelfNodeId);
    command.set_target_instance_id(kSelfInstance);
    EXPECT_TRUE(ValidateGate(command));
}

// P1 的核心用例:node_id 被回收,前任(僵尸)与继任者共用同一个 node_id。
// 发给前任的命令带着前任的实例 uuid,继任者必须丢掉它 —— 否则会拿别人的
// 会话去绑定/踢人。
TEST(ValidateCommandTarget, DropsCommandForRecycledNodeIdOfPredecessorInstance)
{
    contracts::kafka::GateCommand command;
    command.set_target_gate_id(kSelfNodeId);
    command.set_target_instance_id(kPredecessorInstance);
    EXPECT_FALSE(ValidateGate(command));
}

// 共享分区带来的新常态:分区里绝大多数消息是发给同分区其它节点的。
TEST(ValidateCommandTarget, DropsCommandForAnotherNodeSharingThePartition)
{
    contracts::kafka::GateCommand command;
    command.set_target_gate_id(kSelfNodeId + 256); // 同一个分区上的另一个 gate
    command.set_target_instance_id(kSelfInstance); // 就算 uuid 撞了也得靠第一级挡住
    EXPECT_FALSE(ValidateGate(command));
}

// 兼容位:老生产者不填 target_gate_id(留 0)。第一级过滤放行,
// 只剩 instance 这一级 —— 这正是本轮要把 Go 生产者补齐的原因。
TEST(ValidateCommandTarget, UnsetNodeIdFallsThroughToInstanceCheck)
{
    contracts::kafka::GateCommand passing;
    passing.set_target_instance_id(kSelfInstance);
    EXPECT_TRUE(ValidateGate(passing));

    contracts::kafka::GateCommand dropped;
    dropped.set_target_instance_id(kPredecessorInstance);
    EXPECT_FALSE(ValidateGate(dropped));
}

// 两个字段都没填时只能放行:没有任何寻址信息可用。留在这里是为了把
// "生产者不填 = 过滤被关掉" 这条事实钉住,而不是让它无声退化。
TEST(ValidateCommandTarget, FullyUnaddressedCommandIsAccepted)
{
    contracts::kafka::GateCommand command;
    EXPECT_TRUE(ValidateGate(command));
}

TEST(ValidateCommandTarget, SceneCommandUsesItsOwnNodeIdField)
{
    node::kafka::KafkaCommandTargetFilter filter;
    filter.nodeIdFieldNames = {"target_scene_id", "target_node_id"};
    filter.instanceIdFieldNames = {"target_instance_id"};

    contracts::kafka::SceneCommand mine;
    mine.set_target_scene_id(kSelfNodeId);
    mine.set_target_instance_id(kSelfInstance);
    EXPECT_TRUE(node::kafka::detail::ValidateCommandTarget(
        "scene-cmd_g1", mine, kSelfNodeId, kSelfInstance, filter));

    contracts::kafka::SceneCommand other;
    other.set_target_scene_id(kSelfNodeId + 512);
    other.set_target_instance_id(kSelfInstance);
    EXPECT_FALSE(node::kafka::detail::ValidateCommandTarget(
        "scene-cmd_g1", other, kSelfNodeId, kSelfInstance, filter));
}

// ---------------------------------------------------------------------------
// 3. 配置驱动的落地层(拿得到 base_deploy_config.yaml 时才跑)
// ---------------------------------------------------------------------------

namespace {
bool gConfigLoaded = false;
}

TEST(ResolveCommandRoute, MatchesTheContractInDeployConfig)
{
    if (!gConfigLoaded)
    {
        GTEST_SKIP() << "没找到 etc/base_deploy_config.yaml,跳过配置驱动的用例";
    }

    const uint32_t partitions = node::kafka::ConfiguredCommandTopicPartitions();
    const uint32_t generation = node::kafka::ConfiguredCommandTopicGeneration();
    ASSERT_GT(partitions, 0u);
    ASSERT_GT(generation, 0u);

    const auto route = node::kafka::ResolveCommandRoute(GateNodeService, 300);
    EXPECT_TRUE(route.Valid());
    EXPECT_EQ(route.topic, node::kafka::CommandTopicName("gate-cmd", generation));
    EXPECT_EQ(route.partition, node::kafka::CommandPartitionForNode(300, partitions));

    const auto sceneRoute = node::kafka::ResolveCommandRoute(SceneNodeService, 300);
    EXPECT_EQ(sceneRoute.topic, node::kafka::CommandTopicName("scene-cmd", generation));
    // 两个类型的 topic 不同名,但同一个 node_id 的分区号算法是同一个。
    EXPECT_NE(sceneRoute.topic, route.topic);
    EXPECT_EQ(sceneRoute.partition, route.partition);
}

// 迁移窗口没关掉之前,旧的一节点一 topic 名字必须还能拼出来。
TEST(ResolveCommandRoute, LegacyPerNodeTopicNameIsUnchanged)
{
    EXPECT_EQ(node::kafka::LegacyPerNodeTopic(GateNodeService, 5), "gate-5");
    EXPECT_EQ(node::kafka::LegacyPerNodeTopic(SceneNodeService, 77), "scene-77");
}

int main(int argc, char **argv)
{
    // 先在换 cwd 之前记下起点,test_config_helper 会把 cwd 换到 bin/。
    std::vector<fs::path> startPoints;
    startPoints.emplace_back(fs::current_path());
    if (argc > 0 && argv[0] != nullptr)
    {
        startPoints.emplace_back(fs::absolute(argv[0]).parent_path());
    }

    // 配置是可选的:纯函数与过滤用例不需要它。拿得到就顺带把
    // base_deploy_config.yaml 里的契约也一起验了。
    gConfigLoaded = test_config::FindAndLoadTestConfig(argc, argv);
    if (!gConfigLoaded)
    {
        std::cerr << "warning: base_deploy_config.yaml not found; "
                     "config-driven cases will be skipped\n";
    }
    startPoints.emplace_back(fs::current_path());

    gVectorFile = FindVectorFile(startPoints);

    testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}
