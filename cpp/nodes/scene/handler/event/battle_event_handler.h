#pragma once

#include "proto/common/event/battle_event.pb.h"

class BattleEventHandler
{
public:
    static void Register();

    static void UnRegister();
    static void BattleSettlementEventHandler(const BattleSettlementEvent& event);
};
