#pragma once
#include <optional>
#include "engine/core/type_define/type_define.h"

namespace google::protobuf
{
    class Message;
}

class ActorCreateS2C;
class Vector3;

class ViewSystem
{
public:
    // Check whether to send NPC-enter message (e.g., skip in multiplayer dungeons without NPC quests)
    static bool ShouldSendNpcEnterMessage(entt::entity observer, entt::entity entrant);

    // 调用方须保证 entrant 有效且 netId 为已分配的非零网络号；本函数只填消息，不创建或校验身份。
    static void FillActorCreateMessageInfo(entt::entity entrant, uint64_t netId, ActorCreateS2C& createMessage);

    // 广播给客户端正在显示 entity 的观察者(AOI 反向索引中可见的那些,不含 entity 自己)。
    // 显式关注可以在网格范围外,这些观察者同样收到;没有关注关系的附近实体不收。
    static void BroadcastMessageToVisiblePlayers(entt::entity entity, const uint32_t message_id,
    const google::protobuf::Message& message);

    static bool IsWithinViewRadius(entt::entity observer, entt::entity entrant, double view_radius);
    static bool IsWithinViewRadius(entt::entity observer, entt::entity entrant);

    // 空间可见判定:距离 + 隐身。目标隐身时,只有观察者对它的显式关注授予了侦测能力
    // (AoiFollowGrant::revealsStealth)才算可见。两个半径见 spatial/constants/view.h:
    //   CanSee:        建立空间关注,距离不超过视野半径;
    //   CanKeepSeeing: 维持已有的空间关注,距离不超过离开半径(视野半径 × kAoiLeaveRadiusFactor)。
    // 缺 Transform 的一方无法量距离,两者都返回 false。
    static bool CanSee(entt::entity observer, entt::entity target);
    static bool CanKeepSeeing(entt::entity observer, entt::entity target);

    // Check whether the target entity has an active stealth buff.
    static bool IsStealthed(entt::entity entity);

    static std::optional<double> GetDistanceBetweenEntities(entt::entity entity1, entt::entity entity2);

    static double GetMaxViewRadius(entt::entity observer);

    static void LookAtPosition(entt::entity entity, const Vector3& pos);
private:
    static bool BothAreNpcs(entt::entity observer, entt::entity entrant);
    // 隐身规则:目标没隐身,或观察者对它有授予侦测的显式来源。
    static bool PassesStealthRule(entt::entity observer, entt::entity target);
};

