#pragma once

#include "proto/contracts/kafka/gate_event.pb.h"

class GateEventHandler
{
public:
    static void RoutePlayerEventHandler(const contracts::kafka::RoutePlayerEvent& event);
    static void KickPlayerEventHandler(const contracts::kafka::KickPlayerEvent& event);
    static void PlayerLeaseExpiredEventHandler(const contracts::kafka::PlayerLeaseExpiredEvent& event);
    static void BindSessionEventHandler(const contracts::kafka::BindSessionEvent& event);
    static void RedirectToGateEventHandler(const contracts::kafka::RedirectToGateEvent& event);
    static void GateDrainMigrationEventHandler(const contracts::kafka::GateDrainMigrationEvent& event);
    static void PushToPlayerEventHandler(const contracts::kafka::PushToPlayerEvent& event);
    static void BroadcastToPlayersEventHandler(const contracts::kafka::BroadcastToPlayersEvent& event);
    static void BroadcastToSceneEventHandler(const contracts::kafka::BroadcastToSceneEvent& event);
    static void BroadcastToAllEventHandler(const contracts::kafka::BroadcastToAllEvent& event);
};
