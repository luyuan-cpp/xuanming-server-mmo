#pragma once

#include "proto/common/event/battle_event.pb.h"

class BattleEventHandler
{
public:
    static void BattleConfirmedEventHandler(const BattleConfirmedEvent& event);
};
