#pragma once

#include "proto/contracts/kafka/gate_event.pb.h"

class GateEventHandler
{
public:
    static void Register();

    static void UnRegister();
    static void RoutePlayerEventHandler(const contracts::kafka::RoutePlayerEvent& event);
    static void KickPlayerEventHandler(const contracts::kafka::KickPlayerEvent& event);
    static void PlayerDisconnectedEventHandler(const contracts::kafka::PlayerDisconnectedEvent& event);
    static void PlayerLeaseExpiredEventHandler(const contracts::kafka::PlayerLeaseExpiredEvent& event);
    static void BindSessionEventHandler(const contracts::kafka::BindSessionEvent& event);
    static void RedirectToGateEventHandler(const contracts::kafka::RedirectToGateEvent& event);
    static void PushToPlayerEventHandler(const contracts::kafka::PushToPlayerEvent& event);
    static void BroadcastToPlayersEventHandler(const contracts::kafka::BroadcastToPlayersEvent& event);
    static void BroadcastToSceneEventHandler(const contracts::kafka::BroadcastToSceneEvent& event);
    static void BroadcastToAllEventHandler(const contracts::kafka::BroadcastToAllEvent& event);
    static void BindBattleEventHandler(const contracts::kafka::BindBattleEvent& event);
    static void UnbindBattleEventHandler(const contracts::kafka::UnbindBattleEvent& event);
};
