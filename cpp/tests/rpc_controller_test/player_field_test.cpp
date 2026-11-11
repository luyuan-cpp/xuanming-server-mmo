#include <gtest/gtest.h>

#include <cstdint>
#include <string>

#include "core/tracing/player_field.h"
#include "proto/common/base/rpc_message.pb.h"
#include "proto/contracts/kafka/gate_command.pb.h"
#include "proto/contracts/kafka/scene_command.pb.h"

// ---------------------------------------------------------------------------
// player_field_test — 日志上下文里的玩家取自消息顶层 player_id(docs/design/distributed-tracing.md §5.5)。
//
// gRPC 服务端(GrpcUnaryCall::Proceed)与控制面命令消费(node_kafka_command_handler.h)都经 TopLevelPlayerId
// 取值。钉住三件事:
//   1) 真实的命令信封与请求取得到原值(64 位不截断);
//   2) 没有 player_id 的消息返回 0,而且能编过 —— 不是每种消息都带这个字段;
//   3) 同名但形态不对的字段(字符串、repeated 要下标的访问器)一律不取,只有能转成 uint64 的才取。
// ---------------------------------------------------------------------------

namespace {

struct Uint32Player {
    uint32_t player_id() const { return 7; }
};

struct StringPlayer {
    std::string player_id() const { return "42"; }
};

// repeated 字段的生成访问器要下标,无参调用不成立。
struct RepeatedPlayer {
    uint64_t player_id(int index) const { return static_cast<uint64_t>(index) + 1; }
};

struct NoPlayer {
    uint32_t target_node_id() const { return 3; }
};

TEST(TopLevelPlayerId, ReadsTheEnvelopeValueWithoutTruncation)
{
    contracts::kafka::GateCommand gate;
    gate.set_player_id(4611686018427387905ULL);
    contracts::kafka::SceneCommand scene;
    scene.set_player_id(42);

    EXPECT_EQ(tracing::TopLevelPlayerId(gate), 4611686018427387905ULL);
    EXPECT_EQ(tracing::TopLevelPlayerId(scene), 42u);
    EXPECT_EQ(tracing::TopLevelPlayerId(contracts::kafka::GateCommand{}), 0u) << "不针对单个玩家的命令信封里是 0";
}

TEST(TopLevelPlayerId, NarrowerIntegerFieldsAreWidened)
{
    EXPECT_EQ(tracing::TopLevelPlayerId(Uint32Player{}), 7u);
}

TEST(TopLevelPlayerId, MessagesWithoutAUsablePlayerFieldYieldZero)
{
    EXPECT_EQ(tracing::TopLevelPlayerId(NoPlayer{}), 0u);
    EXPECT_EQ(tracing::TopLevelPlayerId(StringPlayer{}), 0u);
    EXPECT_EQ(tracing::TopLevelPlayerId(RepeatedPlayer{}), 0u);
    // 没有 player_id 字段的真实 proto 消息。
    EXPECT_EQ(tracing::TopLevelPlayerId(GameRpcMessage{}), 0u);
}

} // namespace
