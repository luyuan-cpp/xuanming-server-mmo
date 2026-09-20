#pragma once

#include "proto/common/event/team_event.pb.h"

class TeamEventHandler
{
public:
    static void Register();

    static void UnRegister();
    static void PlayerTeamRefreshEventHandler(const PlayerTeamRefreshEvent& event);
};
