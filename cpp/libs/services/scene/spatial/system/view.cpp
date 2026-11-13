#include "view.h"
#include "muduo/base/Logging.h"
#include "engine/core/type_define/player_guid_comp.h"

#include "interest.h"
#include "proto/common/component/base_comp.pb.h"
#include "proto/common/component/player_network_comp.pb.h"  // PlayerSessionSnapshotComp:只有有网关会话的玩家才能收广播
#include "Detour/DetourCommon.h"
#include "spatial/constants/view.h"
#include "spatial/system/facing.h"
#include "generated/attribute/actorbaseattributess2c_attribute_sync.h"
#include "proto/scene/player_scene.pb.h"
#include "proto/scene/player_state_attribute_sync.pb.h"  // ActorBaseAttributesS2C:朝向变化标脏
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/npc_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "combat/buff/comp/buff_comp.h"
#include "spatial/comp/scene_node_scene_comp.h"

#include "engine/core/type_define/type_define.h"
#include "network/client_message_sender.h"
#include "thread_context/ecs_context.h"

bool ViewSystem::ShouldSendNpcEnterMessage(entt::entity observer, entt::entity entrant)
{
    return !BothAreNpcs(observer, entrant);
}

bool ViewSystem::BothAreNpcs(entt::entity observer, entt::entity entrant)
{
    return tlsEcs.actorRegistry.any_of<Npc>(observer) && tlsEcs.actorRegistry.any_of<Npc>(entrant);
}

double ViewSystem::GetMaxViewRadius(entt::entity observer)
{
    double viewRadius = kMaxViewRadius;

    if (const auto observerViewRadius = tlsEcs.actorRegistry.try_get<ViewRadius>(observer))
    {
        viewRadius = observerViewRadius->radius();
    }

    return viewRadius;
}

bool ViewSystem::IsWithinViewRadius(entt::entity viewer, entt::entity targetEntity, double visionRadius)
{
    const auto *viewerTransformPtr = tlsEcs.actorRegistry.try_get<Transform>(viewer);
    const auto *targetTransformPtr = tlsEcs.actorRegistry.try_get<Transform>(targetEntity);
    if (!viewerTransformPtr || !targetTransformPtr) return false;

    const auto &viewerTransform = *viewerTransformPtr;
    const auto &targetTransform = *targetTransformPtr;

    const dtReal viewerLocation[] = {
        viewerTransform.location().x(),
        viewerTransform.location().y(),
        viewerTransform.location().z()};
    const dtReal targetLocation[] = {
        targetTransform.location().x(),
        targetTransform.location().y(),
        targetTransform.location().z()};

    // Compute distance and check whether it's within view range
    return dtVdist(viewerLocation, targetLocation) <= visionRadius;
}

bool ViewSystem::IsWithinViewRadius(entt::entity observer, entt::entity entrant)
{
    const double viewRadius = GetMaxViewRadius(observer);

    return IsWithinViewRadius(observer, entrant, viewRadius);
}

std::optional<double> ViewSystem::GetDistanceBetweenEntities(entt::entity entity1, entt::entity entity2)
{
    const auto *transform1 = tlsEcs.actorRegistry.try_get<Transform>(entity1);
    const auto *transform2 = tlsEcs.actorRegistry.try_get<Transform>(entity2);
    if (!transform1 || !transform2)
        return std::nullopt;

    const dtReal location1[] = {
        transform1->location().x(),
        transform1->location().y(),
        transform1->location().z()};
    const dtReal location2[] = {
        transform2->location().x(),
        transform2->location().y(),
        transform2->location().z()};

    return dtVdist(location1, location2);
}

void ViewSystem::FillActorCreateMessageInfo(entt::entity entrant, uint64_t netId, ActorCreateS2C &createMessage)
{
    createMessage.set_entity(netId);

    const auto *entrantTransformPtr = tlsEcs.actorRegistry.try_get<Transform>(entrant);
    if (entrantTransformPtr)
    {
        createMessage.mutable_transform()->mutable_location()->CopyFrom(
            entrantTransformPtr->location());
    }

    if (tlsEcs.actorRegistry.any_of<Player>(entrant))
    {
        createMessage.set_actor_type(ACTOR_TYPE_PLAYER);
        if (const auto* profile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(entrant))
        {
            createMessage.set_appearance_id(profile->appearance_id());
            createMessage.set_gender(profile->gender());
        }
        if (const auto* identity = tlsEcs.actorRegistry.try_get<PlayerUint32Comp>(entrant))
        {
            createMessage.set_class_id(identity->class_());
        }
    }
    else if (tlsEcs.actorRegistry.any_of<Npc>(entrant))
    {
        createMessage.set_actor_type(ACTOR_TYPE_NPC);
    }

    const auto *guidPtr = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(entrant);
    if (guidPtr)
    {
        createMessage.set_guid(guidPtr->value);
    }
}

void ViewSystem::BroadcastMessageToVisiblePlayers(entt::entity entity, const uint32_t message_id,
                                                  const google::protobuf::Message &message)
{
    // 客户端只显示关注列表里的对象:没显示 entity 的人收到它的技能消息既无用,也会泄露隐身者的行动。
    EntityVector receivers;
    InterestSystem::GetVisibleWatchers(entity, receivers);
    // 没有会话快照的实体(NPC 等)没有客户端可发,留在名单里只会让广播函数逐个报错。
    std::erase_if(receivers, [](entt::entity watcher)
                  { return !tlsEcs.actorRegistry.any_of<PlayerSessionSnapshotComp>(watcher); });
    if (receivers.empty())
    {
        return;
    }
    BroadcastToPlayerClients(message_id, message, receivers);
}

void ViewSystem::LookAtPosition(entt::entity entity, const Vector3 &pos)
{
    auto *transform = tlsEcs.actorRegistry.try_get<Transform>(entity);
    if (transform == nullptr) return;

    // 朝向口径见 facing.h:只转偏航、单位是度,与客户端上报同一份契约。
    // 2026-11-12 前这里写的是弧度、俯仰放在 x(客户端把 x 当翻滚),新进视野的人看到的施法者朝向是错的。
    const auto yaw = facing::YawDegreesToward(pos.x() - transform->location().x(),
                                              pos.y() - transform->location().y());
    if (!yaw)
    {
        // 目标就在脚下或正上方:没有水平方向,保留原朝向。
        return;
    }
    facing::SetYaw(*transform->mutable_rotation(), *yaw);
    // 与其它写 Transform 的入口一致:标脏后由属性同步发给正在显示他的观察者。
    SetActorBaseAttributesS2CAttrDirtyBit(entity, ActorBaseAttributesS2C::kTransformFieldNumber);
}

bool ViewSystem::IsStealthed(entt::entity entity)
{
    return tlsEcs.actorRegistry.any_of<StealthedTagComp>(entity);
}

bool ViewSystem::CanSee(entt::entity observer, entt::entity target)
{
    // Distance check first (cheapest).
    return IsWithinViewRadius(observer, target, GetMaxViewRadius(observer)) && PassesStealthRule(observer, target);
}

bool ViewSystem::CanKeepSeeing(entt::entity observer, entt::entity target)
{
    return IsWithinViewRadius(observer, target, GetMaxViewRadius(observer) * kAoiLeaveRadiusFactor) &&
           PassesStealthRule(observer, target);
}

bool ViewSystem::PassesStealthRule(entt::entity observer, entt::entity target)
{
    if (!IsStealthed(target))
    {
        return true;
    }
    // 隐身目标只对持有侦测授权的观察者可见:授权登记在观察者对它的显式关注来源上,
    // 由技能等玩法明确给出;普通关注、队伍关系或空间邻近都不能看穿隐身。
    const auto *aoiList = tlsEcs.actorRegistry.try_get<AoiListComp>(observer);
    if (aoiList == nullptr)
    {
        return false;
    }
    const auto it = aoiList->entries.find(target);
    return it != aoiList->entries.end() && it->second.RevealsStealth();
}
