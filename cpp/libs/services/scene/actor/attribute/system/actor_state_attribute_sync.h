#pragma once

//https://youxiputao.com/article/24673.html
//https://dev.epicgames.com/documentation/zh-cn/unreal-engine/replication-graph-in-unreal-engine

#include "engine/core/type_define/type_define.h"

class ActorStateAttributeSyncSystem
{
public:
    static void Update(double delta);

    static void SyncAttributes(entt::entity entity, const EntityVector& receivingObservers, uint32_t syncFrequency);
};