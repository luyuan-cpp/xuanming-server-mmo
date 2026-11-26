#pragma once
#include <cstdint>

#include "proto/scene/scene.pb.h"

constexpr uint32_t ScenePlayerEnterGameNodeMessageId = 83;
constexpr uint32_t ScenePlayerEnterGameNodeIndex = 0;
#define ScenePlayerEnterGameNodeMethod  ::Scene_Stub::descriptor()->method(0)

constexpr uint32_t SceneProcessClientPlayerMessageMessageId = 10;
constexpr uint32_t SceneProcessClientPlayerMessageIndex = 1;
#define SceneProcessClientPlayerMessageMethod  ::Scene_Stub::descriptor()->method(1)

constexpr uint32_t SceneUpdateSessionDetailMessageId = 24;
constexpr uint32_t SceneUpdateSessionDetailIndex = 2;
#define SceneUpdateSessionDetailMethod  ::Scene_Stub::descriptor()->method(2)

constexpr uint32_t SceneCreateSceneMessageId = 36;
constexpr uint32_t SceneCreateSceneIndex = 3;
#define SceneCreateSceneMethod  ::Scene_Stub::descriptor()->method(3)

constexpr uint32_t SceneDestroySceneMessageId = 121;
constexpr uint32_t SceneDestroySceneIndex = 4;
#define SceneDestroySceneMethod  ::Scene_Stub::descriptor()->method(4)

constexpr uint32_t ScenePrepareBattleMessageId = 141;
constexpr uint32_t ScenePrepareBattleIndex = 5;
#define ScenePrepareBattleMethod  ::Scene_Stub::descriptor()->method(5)

constexpr uint32_t SceneCancelBattlePrepareMessageId = 155;
constexpr uint32_t SceneCancelBattlePrepareIndex = 6;
#define SceneCancelBattlePrepareMethod  ::Scene_Stub::descriptor()->method(6)

constexpr uint32_t SceneNodeHandshakeMessageId = 62;
constexpr uint32_t SceneNodeHandshakeIndex = 7;
#define SceneNodeHandshakeMethod  ::Scene_Stub::descriptor()->method(7)

constexpr uint32_t SceneCheckJianghuDuelEligibilityMessageId = 564;
constexpr uint32_t SceneCheckJianghuDuelEligibilityIndex = 8;
#define SceneCheckJianghuDuelEligibilityMethod  ::Scene_Stub::descriptor()->method(8)

// 节点间 RPC0 的方法 tag(docs/design/muduo-rpc-channel.md §10):RpcChannel / RpcClient / RpcSession 的
// CallMethod<Tag> / SendRequest<Tag> 按它取消息号与请求 / 响应类型。只含类型信息、不依赖 core;
// 请求 / 响应类型在此前向声明,tag 本身不要求完整类型。
class CancelBattlePrepareRequest;
class CheckJianghuDuelEligibilityRequest;
class CheckJianghuDuelEligibilityResponse;
class CreateSceneRequest;
class CreateSceneResponse;
class DestroySceneRequest;
class Empty;
class NodeHandshakeRequest;
class NodeHandshakeResponse;
class PlayerEnterGameNodeRequest;
class PrepareBattleRequest;
class PrepareBattleResponse;
class ProcessClientPlayerMessageRequest;
class ProcessClientPlayerMessageResponse;
class RegisterPlayerSessionRequest;

struct ScenePlayerEnterGameNodeRpc
{
    static constexpr uint32_t kMessageId = ScenePlayerEnterGameNodeMessageId;
    using Request = ::PlayerEnterGameNodeRequest;
    using Response = ::Empty;
};

struct SceneProcessClientPlayerMessageRpc
{
    static constexpr uint32_t kMessageId = SceneProcessClientPlayerMessageMessageId;
    using Request = ::ProcessClientPlayerMessageRequest;
    using Response = ::ProcessClientPlayerMessageResponse;
};

struct SceneUpdateSessionDetailRpc
{
    static constexpr uint32_t kMessageId = SceneUpdateSessionDetailMessageId;
    using Request = ::RegisterPlayerSessionRequest;
    using Response = ::Empty;
};

struct SceneCreateSceneRpc
{
    static constexpr uint32_t kMessageId = SceneCreateSceneMessageId;
    using Request = ::CreateSceneRequest;
    using Response = ::CreateSceneResponse;
};

struct SceneDestroySceneRpc
{
    static constexpr uint32_t kMessageId = SceneDestroySceneMessageId;
    using Request = ::DestroySceneRequest;
    using Response = ::Empty;
};

struct ScenePrepareBattleRpc
{
    static constexpr uint32_t kMessageId = ScenePrepareBattleMessageId;
    using Request = ::PrepareBattleRequest;
    using Response = ::PrepareBattleResponse;
};

struct SceneCancelBattlePrepareRpc
{
    static constexpr uint32_t kMessageId = SceneCancelBattlePrepareMessageId;
    using Request = ::CancelBattlePrepareRequest;
    using Response = ::Empty;
};

struct SceneNodeHandshakeRpc
{
    static constexpr uint32_t kMessageId = SceneNodeHandshakeMessageId;
    using Request = ::NodeHandshakeRequest;
    using Response = ::NodeHandshakeResponse;
};

struct SceneCheckJianghuDuelEligibilityRpc
{
    static constexpr uint32_t kMessageId = SceneCheckJianghuDuelEligibilityMessageId;
    using Request = ::CheckJianghuDuelEligibilityRequest;
    using Response = ::CheckJianghuDuelEligibilityResponse;
};
