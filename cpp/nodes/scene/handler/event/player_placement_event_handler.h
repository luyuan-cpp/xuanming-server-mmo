#pragma once

#include "proto/common/event/player_placement_event.pb.h"

class PlayerPlacementEventHandler
{
public:
    static void PlayerHostedPlacementEventHandler(const PlayerHostedPlacementEvent& event);
};
